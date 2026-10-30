package domain

// 本文件实现「按 Maven 包名搜索版本 → 批量清理」。
//
// 要解决的问题（运维视角）：
//   批量维护时用户脑子里想的是包（`com.tbao:tbaoolib`），不是某个具体版本的目录路径。
//   既有的资产浏览只能逐层进目录、逐个版本删，清理几十个陈旧版本要点几十次，
//   而且每次都得重新找位置——这在实际维护里不可接受。
//
// 因此这里的入口是**包名 + 筛选条件**：
//   输入 `group:artifact`（支持 `*` 通配），给出该包下所有版本及其文件数/字节/时间，
//   再叠加「含 SNAPSHOT」「时间早于」「体积大于」等条件把目标圈出来，
//   勾选后一次性移入回收站（可恢复），最终再统一清理。
//
// 三条必须守住的边界：
//
//  1. **搜索只读、绝不删**。预览与实际删除是两个请求，且删除请求必须回传
//     用户在预览里看到的具体版本目录——服务端重新计算后做子集校验，
//     不允许"凭筛选条件直接删"（条件可能在两次请求之间匹配到新版本）。
//
//  2. **通配符只作用在坐标上，不作用在路径上**。用户输入的 `*` 经转义后
//     不会变成 SQL 通配，否则一个 `%` 就能扫遍全仓。
//
//  3. **每批自身原子、批间不保证**。与单版本删除同一口径：宁可"删了 80/100 且
//     能看到已完成部分"，也不要"一个都删不掉"。

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// MavenPackageQuery 是按包名搜索版本的条件。
//
// GroupID / ArtifactID 支持 `*` 通配（如 `com.tbao.*`、`*`），
// 二者组合即"包名"；Version 支持 `*` 通配（如 `1.17*`）。
type MavenPackageQuery struct {
	GroupID    string `json:"groupId"`
	ArtifactID string `json:"artifactId"`
	// Version 为空表示不限版本；支持 `*`（如 `1.17*`）。
	Version string `json:"version,omitempty"`
	// SnapshotOnly 只保留含字面 `-SNAPSHOT` 文件的版本（污染检测的主用途）。
	SnapshotOnly bool `json:"snapshotOnly,omitempty"`
	// UpdatedBefore 只保留 MaxUpdated 早于该时间的版本（"YYYY-MM-DD" 或完整时间）。
	UpdatedBefore string `json:"updatedBefore,omitempty"`
	// MinBytes / MaxBytes 按版本总字节筛选；0 表示不限。
	MinBytes int64 `json:"minBytes,omitempty"`
	MaxBytes int64 `json:"maxBytes,omitempty"`
	// Limit 限制返回的版本数，0 取默认（500）。
	Limit int `json:"limit,omitempty"`
}

// Validate 校验查询条件，防止把明显无效的输入带进 SQL。
func (q MavenPackageQuery) Validate() error {
	if strings.TrimSpace(q.GroupID) == "" && strings.TrimSpace(q.ArtifactID) == "" {
		return fmt.Errorf("%w: 至少需要 groupId 或 artifactId", ErrValidation)
	}
	for name, v := range map[string]string{
		"groupId": q.GroupID, "artifactId": q.ArtifactID, "version": q.Version,
	} {
		// 通配符之外只允许坐标合法字符：这些值最终会拼成路径前缀，
		// 出现 `/` 或 `..` 就能逃逸到别的目录。
		sanitized := strings.ReplaceAll(v, "*", "")
		if strings.ContainsAny(sanitized, `/\`) || strings.Contains(sanitized, "..") {
			return fmt.Errorf("%w: %s 含非法字符", ErrValidation, name)
		}
	}
	if q.MinBytes < 0 || q.MaxBytes < 0 {
		return fmt.Errorf("%w: 体积区间不能为负", ErrValidation)
	}
	if q.MinBytes > 0 && q.MaxBytes > 0 && q.MinBytes > q.MaxBytes {
		return fmt.Errorf("%w: 最小体积不能大于最大体积", ErrValidation)
	}
	if q.UpdatedBefore != "" {
		if _, err := normalizeTimeBound(q.UpdatedBefore); err != nil {
			return err
		}
	}
	return nil
}

// MavenPackageVersion 是搜索结果中的一个版本。
type MavenPackageVersion struct {
	// Coordinate 是完整坐标，供展示与后续删除直接回传。
	Coordinate MavenVersionCoordinate `json:"coordinate"`
	// VersionDir 是该版本目录（仓库内路径）。
	VersionDir           string `json:"versionDir"`
	FileCount            int64  `json:"fileCount"`
	Bytes                int64  `json:"bytes"`
	UpdatedAt            string `json:"updatedAt"`
	CreatedAt            string `json:"createdAt"`
	LiteralSnapshotFiles int64  `json:"literalSnapshotFiles"`
	TimestampBuilds      int    `json:"timestampBuilds"`
	// Suspect 标记「疑似幽灵快照」：含字面 SNAPSHOT 文件但没有任何时间戳构建。
	// 这正是 Nexus 事故里那种「releases 仓里只有字面 -SNAPSHOT.jar」的形态。
	Suspect bool `json:"suspect"`
}

// MavenPackageSearchResult 是一次包名搜索的结果。
type MavenPackageSearchResult struct {
	Query MavenPackageQuery `json:"query"`
	// Repository 是被搜索的仓库名。
	Repository string `json:"repository"`
	// ArtifactDir 是解析出的 artifact 目录（用于提示用户实际搜的是哪个包）。
	ArtifactDirs []string              `json:"artifactDirs"`
	Versions     []MavenPackageVersion `json:"versions"`
	// TotalVersions 是筛选前的候选版本数，TotalBytes 是筛选后合计。
	TotalVersions int   `json:"totalVersions"`
	MatchedBytes  int64 `json:"matchedBytes"`
	// Truncated 表示结果被 Limit 截断，前端应提示"收窄条件"。
	Truncated bool `json:"truncated"`
}

// SearchMavenPackageVersions 按包名与筛选条件列出候选版本（只读）。
func (s *AssetService) SearchMavenPackageVersions(repoName string, q MavenPackageQuery) (*MavenPackageSearchResult, error) {
	if err := q.Validate(); err != nil {
		return nil, err
	}
	repo, err := s.repos.GetByName(repoName)
	if err != nil {
		return nil, mapNotFound(err)
	}
	if repo.Format != "maven" {
		return nil, fmt.Errorf("%w: 仅 Maven 仓库支持按包名搜索", ErrOperationUnsupported)
	}
	// proxy 仓的资产是缓存副本，其"存在"取决于历史回源；
	// 批量清理针对的是**自己发布的内容**，因此只对 hosted 开放写语义的搜索。
	// 但读搜索本身无害，这里不拒绝 proxy，只在实际删除时校验 hosted。
	prefix, err := mavenArtifactSearchPrefix(q)
	if err != nil {
		return nil, err
	}

	rows, err := s.assets.ListMavenVersionDirs(repo.ID, prefix)
	if err != nil {
		return nil, err
	}

	result := &MavenPackageSearchResult{Query: q, Repository: repoName}
	result.TotalVersions = len(rows)

	// 后置筛选：通配与时间/体积条件在 Go 侧判定。
	// 之所以不全部下推到 SQL：通配符语义（`*` 匹配任意段）与 SQL LIKE 不同，
	// 混用会让「看起来该匹配却没匹配」变成难查的问题；这里统一在一处判定，
	// 只有 artifact 前缀这一个能精确表达的条件下推到 SQL 做粗筛。
	dirSeen := make(map[string]struct{})
	limit := q.Limit
	if limit <= 0 || limit > 2000 {
		limit = 500
	}
	updatedBefore := ""
	if q.UpdatedBefore != "" {
		updatedBefore, err = normalizeTimeBound(q.UpdatedBefore)
		if err != nil {
			return nil, err
		}
	}

	matched := make([]MavenPackageVersion, 0, len(rows))
	for _, row := range rows {
		groupID := deriveGroupID(row.ArtifactDir, row.ArtifactID)
		if !wildcardMatch(q.GroupID, groupID) || !wildcardMatch(q.ArtifactID, row.ArtifactID) {
			continue
		}
		if q.Version != "" && !wildcardMatch(q.Version, row.Version) {
			continue
		}
		if q.SnapshotOnly && !isSnapshotVersionRow(row) {
			continue
		}
		if updatedBefore != "" && row.MaxUpdated >= updatedBefore {
			continue
		}
		if q.MinBytes > 0 && row.Bytes < q.MinBytes {
			continue
		}
		if q.MaxBytes > 0 && row.Bytes > q.MaxBytes {
			continue
		}
		// Suspect：有字面 SNAPSHOT 文件但没有任何时间戳构建 → 幽灵快照形态。
		suspect := row.LiteralSnapshotFiles > 0 && row.TimestampBuilds == 0
		matched = append(matched, MavenPackageVersion{
			Coordinate: MavenVersionCoordinate{
				GroupID: groupID, ArtifactID: row.ArtifactID, Version: row.Version,
			},
			VersionDir:           row.VersionDir,
			FileCount:            row.FileCount,
			Bytes:                row.Bytes,
			UpdatedAt:            row.MaxUpdated,
			CreatedAt:            row.MinUpdated,
			LiteralSnapshotFiles: row.LiteralSnapshotFiles,
			TimestampBuilds:      row.TimestampBuilds,
			Suspect:              suspect,
		})
		if _, ok := dirSeen[row.ArtifactDir]; !ok {
			dirSeen[row.ArtifactDir] = struct{}{}
			result.ArtifactDirs = append(result.ArtifactDirs, row.ArtifactDir)
		}
	}

	// 排序：疑似问题版本置顶，其次按更新时间降序——运维第一眼要看到最该处理的那批。
	sort.SliceStable(matched, func(i, j int) bool {
		if matched[i].Suspect != matched[j].Suspect {
			return matched[i].Suspect
		}
		return matched[i].UpdatedAt > matched[j].UpdatedAt
	})
	sort.Strings(result.ArtifactDirs)

	if len(matched) > limit {
		matched = matched[:limit]
		result.Truncated = true
	}
	result.Versions = matched
	for _, v := range matched {
		result.MatchedBytes += v.Bytes
	}
	return result, nil
}

// isSnapshotVersionRow 判断一个版本目录是否属于 SNAPSHOT 版本。
//
// 判据是「版本目录名以 -SNAPSHOT 结尾」**或**「目录内存在字面 -SNAPSHOT 文件」，
// 而不是只看文件名：正规快照的文件名是时间戳形态
// （`real-1.17-20260317.215705-3.jar`，不含 `-SNAPSHOT` 字样），
// 只看文件名会把它们全部漏掉——而"含 SNAPSHOT 的版本"显然应当包含这些。
//
// 反过来，字面 SNAPSHOT 文件出现在非 SNAPSHOT 目录下也是一种真实污染形态
// （有人把快照内容直接发进 release 版本号），因此两个条件取并集。
func isSnapshotVersionRow(row repository.MavenVersionRow) bool {
	if strings.HasSuffix(row.Version, "-SNAPSHOT") {
		return true
	}
	return row.LiteralSnapshotFiles > 0
}

// artifactDir 形如 `com/tbao/tbaoolib`，去掉最后一段即为 group 路径，
// 转成点分即 groupId。目录层数不足以构成 group 时返回空串（由通配判定处理）。
func deriveGroupID(artifactDir, artifactID string) string {
	dir := strings.TrimSuffix(artifactDir, "/")
	if artifactID != "" {
		dir = strings.TrimSuffix(dir, "/"+artifactID)
	}
	dir = strings.Trim(dir, "/")
	if dir == "" {
		return ""
	}
	return strings.ReplaceAll(dir, "/", ".")
}

// mavenArtifactSearchPrefix 把查询条件转成可用于 SQL 粗筛的路径前缀。
//
// 只有「groupId 与 artifactId 都不含通配符」时才能精确计算前缀；
// 含通配时返回空前缀（扫全仓后由 Go 侧过滤）——这是有意的取舍：
// 通配搜索本就要看很多版本，粗筛帮不上忙，强行拼前缀反而会漏匹配。
func mavenArtifactSearchPrefix(q MavenPackageQuery) (string, error) {
	group := strings.TrimSpace(q.GroupID)
	artifact := strings.TrimSpace(q.ArtifactID)
	if strings.ContainsAny(group, "*") || strings.ContainsAny(artifact, "*") {
		return "", nil
	}
	if group == "" || artifact == "" {
		// 只给一个维度时无法定位 artifact 目录，退回全仓扫描。
		return "", nil
	}
	if err := validateCoordinateSegment(group); err != nil {
		return "", err
	}
	if err := validateCoordinateSegment(artifact); err != nil {
		return "", err
	}
	return strings.ReplaceAll(group, ".", "/") + "/" + artifact, nil
}

// validateCoordinateSegment 校验单个坐标段不含路径分隔符与 `..`。
func validateCoordinateSegment(v string) error {
	if strings.ContainsAny(v, `/\`) || strings.Contains(v, "..") {
		return fmt.Errorf("%w: 坐标含非法字符", ErrValidation)
	}
	return nil
}

// wildcardMatch 做「`*` 匹配任意字符」的简单通配匹配（不区分大小写）。
//
// pattern 为空时视为匹配全部：调用方已用空值表示"该维度不限"。
// 只支持 `*`，不支持 `?` —— Maven 坐标里 `?` 是合法字符，把它当通配会误伤。
func wildcardMatch(pattern, value string) bool {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" || pattern == "*" {
		return true
	}
	return wildcardMatchFold(strings.ToLower(pattern), strings.ToLower(value))
}

// wildcardMatchFold 是 wildcardMatch 的大小写已归一化实现。
//
// 用双指针 + 单星回溯：只需支持 `*`，无需完整 glob 的回溯表。
// 这样既是线性最坏情况（O(n·m)），也不引入正则编译开销。
func wildcardMatchFold(pattern, value string) bool {
	var pi, vi int
	star := -1
	match := 0
	for vi < len(value) {
		if pi < len(pattern) && (pattern[pi] == value[vi]) {
			pi++
			vi++
			continue
		}
		if pi < len(pattern) && pattern[pi] == '*' {
			star = pi
			match = vi
			pi++
			continue
		}
		if star != -1 {
			// 回溯：让上一个 `*` 多吃一个字符。
			pi = star + 1
			match++
			vi = match
			continue
		}
		return false
	}
	for pi < len(pattern) && pattern[pi] == '*' {
		pi++
	}
	return pi == len(pattern)
}

// normalizeTimeBound 把用户输入的时间归一成 "YYYY-MM-DD HH:MM:SS" 以便与
// updated_at 直接做字符串比较（该列就是此格式，定长可直接比较）。
//
// 只接受 `YYYY-MM-DD` 与 `YYYY-MM-DD HH:MM:SS` 两种：接受更多格式意味着
// 要处理时区，而库里的时间一律是 UTC，静默按本地时区解释会让"早于某日"
// 的结果与用户预期差几小时。
func normalizeTimeBound(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	switch len(raw) {
	case 10: // YYYY-MM-DD
		if raw[4] != '-' || raw[7] != '-' {
			return "", fmt.Errorf("%w: 时间格式应为 YYYY-MM-DD", ErrValidation)
		}
		return raw + " 00:00:00", nil
	case 19: // YYYY-MM-DD HH:MM:SS
		if raw[4] != '-' || raw[7] != '-' || raw[10] != ' ' {
			return "", fmt.Errorf("%w: 时间格式应为 YYYY-MM-DD HH:MM:SS", ErrValidation)
		}
		return raw, nil
	default:
		return "", fmt.Errorf("%w: 时间格式应为 YYYY-MM-DD 或 YYYY-MM-DD HH:MM:SS", ErrValidation)
	}
}

// ErrMavenBatchEmpty 表示批量清理没有选中任何版本。
var ErrMavenBatchEmpty = errors.New("没有选中任何版本")

// MavenBatchTrashRequest 是批量移入回收站的请求。
type MavenBatchTrashRequest struct {
	// Versions 是用户在预览里确认过的具体版本，逐条带完整坐标。
	// 刻意不接受"筛选条件"作为删除依据：条件在两次请求之间可能匹配到新版本，
	// 凭条件删除等于删掉用户没看见的东西。
	Versions []MavenVersionCoordinate `json:"versions"`
}

// MavenBatchTrashResult 是批量清理的结果。
type MavenBatchTrashResult struct {
	// OperationID 是本批次的总操作号。
	OperationID string `json:"operationId"`
	// TrashedVersions 是成功移入回收站的版本数，TrashedFiles 是文件数。
	TrashedVersions int   `json:"trashedVersions"`
	TrashedFiles    int   `json:"trashedFiles"`
	FreedBytes      int64 `json:"freedBytes"`
	// Failures 是逐版本失败明细；部分成功时仍有意义。
	Failures []MavenBatchFailure `json:"failures,omitempty"`
}

// MavenBatchFailure 是批量清理中单个版本的失败。
type MavenBatchFailure struct {
	Coordinate MavenVersionCoordinate `json:"coordinate"`
	Reason     string                 `json:"reason"`
}

// TrashMavenVersionsBatch 把多个版本一次移入回收站。
//
// 逐版本复用 DeleteMavenVersion：批量不是一个新的事务语义，而是"多次单版本删除"，
// 因此单个版本失败不影响其余（部分成功可接受且必须如实报告）。
// 之所以不追求整批原子：一次清理上百个版本时，任何一个版本被并发改过就整批回滚，
// 会让用户反复重试仍无法清理——那比"部分成功 + 明细"糟糕得多。
func (s *AssetService) TrashMavenVersionsBatch(repoName string, req MavenBatchTrashRequest, actor string) (*MavenBatchTrashResult, error) {
	if len(req.Versions) == 0 {
		return nil, ErrMavenBatchEmpty
	}
	// 上限与资产操作条目数一致，避免一次请求打爆服务端。
	const maxBatch = 200
	if len(req.Versions) > maxBatch {
		return nil, fmt.Errorf("%w: 单次最多清理 %d 个版本", ErrValidation, maxBatch)
	}
	result := &MavenBatchTrashResult{OperationID: newOperationID()}
	for _, coord := range req.Versions {
		// 用版本级删除的同一个入口，确保语义完全一致（含 metadata 处理与原子性）。
		del, err := s.DeleteMavenVersion(repoName, coord, nil, actor)
		if err != nil {
			result.Failures = append(result.Failures, MavenBatchFailure{
				Coordinate: coord, Reason: err.Error(),
			})
			continue
		}
		result.TrashedVersions++
		result.TrashedFiles += del.Trashed
		result.FreedBytes += del.Bytes
	}
	// 全部失败时返回错误，而不是一个"成功的空结果"：
	// 那会让界面显示"清理完成 0 个"，掩盖了实际全失败。
	if result.TrashedVersions == 0 && len(result.Failures) > 0 {
		return result, fmt.Errorf("%w: 全部 %d 个版本清理失败，首个原因：%s",
			ErrValidation, len(result.Failures), result.Failures[0].Reason)
	}
	return result, nil
}
