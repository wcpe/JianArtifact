package repository

import (
	"sort"
	"strings"
)

// MavenVersionRow 是一个「版本目录」的聚合行。
//
// 为什么聚合在 Go 侧做而不是纯 SQL：Maven 版本目录的位置不是固定偏移
// （group 可由任意多段组成），SQLite 没有内置 reverse / split_part，
// 用 instr + substr 拼「取最后两段」的表达式既难读又容易在路径含特殊字符时出错。
// 这里改用**流式游标逐行扫描**：只把 (path, size, updated_at) 三列流式读进来、
// 边读边聚合成「版本目录 → 统计」，内存占用是「版本数」而非「资产数」，
// 既守住「不把全量资产装进内存」的红线，也没有 SQL 表达式的脆弱性。
type MavenVersionRow struct {
	// VersionDir 是版本目录路径（不含尾部斜杠），形如 com/example/demo/1.17-SNAPSHOT。
	VersionDir string `json:"versionDir"`
	// ArtifactDir 是 artifact 目录路径，形如 com/example/demo。
	ArtifactDir string `json:"artifactDir"`
	// ArtifactID / Version 是从路径末两段解析出的坐标分量。
	ArtifactID string `json:"artifactId"`
	Version    string `json:"version"`
	FileCount  int64  `json:"fileCount"`
	Bytes      int64  `json:"bytes"`
	// MinUpdated / MaxUpdated 是该版本内最早/最晚的更新时间。
	MinUpdated string `json:"minUpdated"`
	MaxUpdated string `json:"maxUpdated"`
	// LiteralSnapshotFiles 是文件名含 `-SNAPSHOT` 的文件数（字面快照形态）。
	// 它没有时间戳、不可复现，是污染检测的关键信号。
	LiteralSnapshotFiles int64 `json:"literalSnapshotFiles"`
	// TimestampBuilds 是该版本内识别出的 SNAPSHOT 时间戳构建数（去重后的数量）。
	// 与 LiteralSnapshotFiles 一起可区分「正规快照」与「幽灵字面快照」。
	TimestampBuilds int `json:"timestampBuilds"`
}

// mavenVersionAggregator 把逐行资产聚合成版本目录统计。
type mavenVersionAggregator struct {
	order []string
	byDir map[string]*MavenVersionRow
	// builds 记录每个版本目录已见到的构建号，用于去重计数。
	builds map[string]map[string]struct{}
}

func newMavenVersionAggregator() *mavenVersionAggregator {
	return &mavenVersionAggregator{
		byDir:  make(map[string]*MavenVersionRow),
		builds: make(map[string]map[string]struct{}),
	}
}

// add 并入一条资产。path 必须是仓库内相对路径。
func (a *mavenVersionAggregator) add(path string, size int64, updatedAt string) {
	slash := strings.LastIndex(path, "/")
	if slash <= 0 {
		return
	}
	versionDir := path[:slash]
	filename := path[slash+1:]

	// 版本目录至少要 group/artifact/version 三段；不足三段的是扁平发布或异常路径，跳过。
	parts := strings.Split(versionDir, "/")
	if len(parts) < 3 {
		return
	}
	version := parts[len(parts)-1]
	artifact := parts[len(parts)-2]

	row, ok := a.byDir[versionDir]
	if !ok {
		row = &MavenVersionRow{
			VersionDir:  versionDir,
			ArtifactDir: strings.Join(parts[:len(parts)-1], "/"),
			ArtifactID:  artifact,
			Version:     version,
			MinUpdated:  updatedAt,
			MaxUpdated:  updatedAt,
		}
		a.byDir[versionDir] = row
		a.order = append(a.order, versionDir)
	}
	row.FileCount++
	row.Bytes += size
	// updated_at 是 "YYYY-MM-DD HH:MM:SS" 定长格式，可直接按字符串比较。
	if updatedAt != "" {
		if row.MinUpdated == "" || updatedAt < row.MinUpdated {
			row.MinUpdated = updatedAt
		}
		if updatedAt > row.MaxUpdated {
			row.MaxUpdated = updatedAt
		}
	}
	if strings.Contains(filename, "-SNAPSHOT") {
		row.LiteralSnapshotFiles++
	}
	if build := extractSnapshotBuildFromName(filename); build != "" {
		set, ok := a.builds[versionDir]
		if !ok {
			set = make(map[string]struct{})
			a.builds[versionDir] = set
		}
		set[build] = struct{}{}
	}
}

// result 返回按路径升序的聚合结果，并回填时间戳构建数。
func (a *mavenVersionAggregator) result() []MavenVersionRow {
	out := make([]MavenVersionRow, 0, len(a.order))
	for _, dir := range a.order {
		row := a.byDir[dir]
		row.TimestampBuilds = len(a.builds[dir])
		out = append(out, *row)
	}
	// 按 VersionDir 升序：保证同一查询的返回稳定，便于分页与断言。
	sortMavenRows(out)
	return out
}

// extractSnapshotBuildFromName 从文件名识别 `<timestamp>-<buildNumber>`。
//
// 与 domain 层的实现保持同一判据（相邻两段 <timestamp> + <digits>），
// 但不引入 domain 依赖：repository 层不得反向依赖 domain（见分层约束）。
// 因此这里保留一份最小实现，两处任何一方调整都必须同步——
// 已有测试 TestMavenBuildIdParsersAgree 锁住两者一致。
func extractSnapshotBuildFromName(filename string) string {
	base := filename
	for _, suffix := range []string{".sha512", ".sha256", ".sha1", ".md5"} {
		if strings.HasSuffix(base, suffix) {
			base = strings.TrimSuffix(base, suffix)
			break
		}
	}
	if i := strings.LastIndex(base, "."); i > 0 {
		base = base[:i]
	}
	segments := strings.Split(base, "-")
	// 从右往左找第一个 (timestamp, buildNumber) 相邻对。
	for i := len(segments) - 2; i >= 1; i-- {
		if isMavenTimestampShape(segments[i]) && isAllDigits(segments[i+1]) {
			return segments[i] + "-" + segments[i+1]
		}
	}
	return ""
}

// isMavenTimestampShape 判断 `yyyyMMdd.HHmmss` 形态。
func isMavenTimestampShape(s string) bool {
	if len(s) != 15 || s[8] != '.' {
		return false
	}
	for i, r := range s {
		if i == 8 {
			continue
		}
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// ListMavenVersionDirs 返回仓库内全部「版本目录」的聚合视图。
//
// artifactPrefix 非空时只统计 artifact 目录以它起首的版本（形如 com/example/demo），
// 用于「按包名搜索」：先由 group:artifact 算出目录前缀，再交给 SQL 前缀过滤，
// 避免把整仓资产都流进来。
func (r *AssetRepo) ListMavenVersionDirs(repoID int64, artifactPrefix string) ([]MavenVersionRow, error) {
	query := `SELECT path, size, updated_at FROM asset WHERE repository_id = ?`
	args := []any{repoID}
	if artifactPrefix != "" {
		// 带尾部斜杠，避免 com/example/dev 误匹配 com/example/device。
		query += ` AND path LIKE ? ESCAPE '\'`
		args = append(args, likePrefix(strings.TrimSuffix(artifactPrefix, "/")+"/"))
	}
	query += ` ORDER BY path`

	rows, err := r.db.Queryx(query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	agg := newMavenVersionAggregator()
	for rows.Next() {
		var path, updatedAt string
		var size int64
		if err := rows.Scan(&path, &size, &updatedAt); err != nil {
			return nil, err
		}
		agg.add(path, size, updatedAt)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return agg.result(), nil
}

// ListMavenVersionDirsByArtifact 按 artifact 目录精确列出其下所有版本。
//
// 与 ListMavenVersionDirs 的区别是**精确到 artifact**（不是前缀）：
// 批量清理时用户指定的是 `com.example:demo` 这一个包，不该把
// `com/example/demo-extra` 或 `com/example/demo2` 也带进来。
func (r *AssetRepo) ListMavenVersionDirsByArtifact(repoID int64, artifactDir string) ([]MavenVersionRow, error) {
	artifactDir = strings.TrimSuffix(artifactDir, "/")
	if artifactDir == "" {
		return nil, nil
	}
	query := `SELECT path, size, updated_at FROM asset WHERE repository_id = ? AND path LIKE ? ESCAPE '\' ORDER BY path`
	rows, err := r.db.Queryx(query, repoID, likePrefix(artifactDir+"/"))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	agg := newMavenVersionAggregator()
	for rows.Next() {
		var path, updatedAt string
		var size int64
		if err := rows.Scan(&path, &size, &updatedAt); err != nil {
			return nil, err
		}
		// 只接受 artifactDir 的**直接**下一级为 version 的资产：
		// 更深层（多出一段目录）说明是别的 artifact，必须排除。
		rel := strings.TrimPrefix(path, artifactDir+"/")
		if strings.Count(rel, "/") != 1 {
			continue
		}
		agg.add(path, size, updatedAt)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return agg.result(), nil
}

// sortMavenRows 按 VersionDir 升序排序。
func sortMavenRows(rows []MavenVersionRow) {
	sort.Slice(rows, func(i, j int) bool { return rows[i].VersionDir < rows[j].VersionDir })
}
