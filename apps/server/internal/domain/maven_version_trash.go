package domain

// 本文件实现 Maven 版本的**清理计划**：把「一个坐标下的某个版本」展开成完整文件清单，
// 按语义类别分组，供预览与删除共用同一份计算结果。
//
// 三条必须守住的口径：
//
//  1. **按目录前缀展开，不按文件名模式匹配**。同一版本目录下会出现大量命名变体：
//     主档（jar/war/pom）、classifier（sources/javadoc/tests）、校验和（.sha1/.md5/.sha256/
//     .sha512）、版本级 maven-metadata.xml 及其校验和；SNAPSHOT 还会同时存在字面
//     `-SNAPSHOT` 与若干 `-<timestamp>-<buildNumber>` 历史构建。命名变体无法穷举，
//     任何"列出后缀白名单"的做法都迟早漏掉一个新的变体，而漏掉一个文件就会让 metadata、
//     校验和或同级文件指向不存在的内容。前缀才是可靠的边界。
//
//  2. **预览与执行共用同一套候选计算**。两处各写一遍会漂移成"预览说 8 个、实际删 11 个"，
//     而这里是删除动作，漂移的代价是删掉用户没看见的东西。实现上 Preview 与 Delete
//     都调 planMavenVersion。
//
//  3. **分批读取，不受单次操作 500 条上限约束**。SNAPSHOT 版本积累几十个历史构建后，
//     文件数很容易超过 500；沿用 maxAssetOperationItems 会让用户永远删不掉最该删的那个
//     版本。读取端分批取全量，提交端按批原子提交（见 DeleteMavenVersion）。

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// mavenPlanBatchSize 是版本清单的分批读取页大小。
//
// 之所以要分批而不是一次 ListByRepo(全部)：SQLite 单次查询把上万行一次性装进内存，
// 与架构红线「大对象不得整体入内存」相冲突；而这里只需要逐行做分类，没有跨行聚合，
// 分批是纯收益。
const mavenPlanBatchSize = 500

// MavenVersionCoordinate 是一个 Maven 坐标。Version 为 SNAPSHOT 版本时 BuildID 可指定
// 单个时间戳构建（例如 `20260317.215705-3`），留空表示整个版本。
type MavenVersionCoordinate struct {
	GroupID    string `json:"groupId"`
	ArtifactID string `json:"artifactId"`
	Version    string `json:"version"`
	// BuildID 仅对 SNAPSHOT 版本有效，形如 `20260317.215705-3`。
	// 指定后只清理该次构建产生的文件，保留其余历史构建。
	BuildID string `json:"buildId,omitempty"`
}

// Dir 返回该坐标对应的仓库内目录（不含尾部斜杠）。
func (c MavenVersionCoordinate) Dir() string {
	group := strings.ReplaceAll(c.GroupID, ".", "/")
	return group + "/" + c.ArtifactID + "/" + c.Version
}

// IsSnapshot 判断该坐标是否为 SNAPSHOT 版本。
func (c MavenVersionCoordinate) IsSnapshot() bool {
	return strings.HasSuffix(c.Version, "-SNAPSHOT")
}

// Validate 校验坐标可用性：各段非空、不含路径分隔符与 `..`。
//
// 这是删除路径，绝不能容忍任何"也许能拼出合法路径"的输入：一个 `..` 段就能让前缀
// 逃逸到别的 artifact 甚至别的层级，而 ListByRepo 是 LIKE 前缀匹配，逃逸不会被拦。
func (c MavenVersionCoordinate) Validate() error {
	for name, value := range map[string]string{
		"groupId": c.GroupID, "artifactId": c.ArtifactID, "version": c.Version,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%w: %s 不能为空", ErrValidation, name)
		}
		if strings.ContainsAny(value, `/\`) {
			return fmt.Errorf("%w: %s 不能包含路径分隔符", ErrValidation, name)
		}
		if strings.Contains(value, "..") {
			return fmt.Errorf("%w: %s 不能包含 ..", ErrValidation, name)
		}
	}
	if c.BuildID != "" {
		if strings.ContainsAny(c.BuildID, `/\`) || strings.Contains(c.BuildID, "..") {
			return fmt.Errorf("%w: buildId 含非法字符", ErrValidation)
		}
		// BuildID 只在 SNAPSHOT 上才有意义：release 版本的时间戳构建不存在，
		// 静默忽略会让用户以为"只删了这一次构建"，实际删了整个版本。
		if !c.IsSnapshot() {
			return fmt.Errorf("%w: buildId 仅适用于 SNAPSHOT 版本", ErrValidation)
		}
	}
	return nil
}

// MavenVersionFileCategory 是文件在版本内的语义类别。
//
// 分类只用于**展示与核对**（让用户在删之前一眼看出 jar 有几个、classifier 有哪些），
// 不参与"删不删"的判定——判定永远是"在不在这个前缀下"。
type MavenVersionFileCategory string

const (
	// MavenFilePrimary 是主档：jar / war / pom / aar 等（无 classifier 的同名主文件）。
	MavenFilePrimary MavenVersionFileCategory = "primary"
	// MavenFileClassifier 是带 classifier 的衍生文件：sources / javadoc / tests 等。
	MavenFileClassifier MavenVersionFileCategory = "classifier"
	// MavenFileChecksum 是校验和文件（.sha1/.md5/.sha256/.sha512）。
	MavenFileChecksum MavenVersionFileCategory = "checksum"
	// MavenFileMetadata 是版本级 maven-metadata.xml 及其校验和。
	MavenFileMetadata MavenVersionFileCategory = "metadata"
	// MavenFileOther 是既非以上类别、又确实位于版本目录下的文件（如 .module / .txt）。
	MavenFileOther MavenVersionFileCategory = "other"
)

// MavenVersionFile 是清单中的一行。
type MavenVersionFile struct {
	Path      string                   `json:"path"`
	Size      int64                    `json:"size"`
	Category  MavenVersionFileCategory `json:"category"`
	BlobHash  string                   `json:"blobHash"`
	Checksum  string                   `json:"checksumType,omitempty"`
	BuildID   string                   `json:"buildId,omitempty"`
	UpdatedAt string                   `json:"updatedAt"`
}

// MavenSnapshotBuild 是一次 SNAPSHOT 历史构建的聚合视图。
type MavenSnapshotBuild struct {
	// BuildID 形如 `20260317.215705-3`；字面 `-SNAPSHOT` 文件归入 LiteralSnapshot 组。
	BuildID   string `json:"buildId"`
	FileCount int    `json:"fileCount"`
	Bytes     int64  `json:"bytes"`
}

// MavenVersionPlan 是一个版本的完整清理计划。
type MavenVersionPlan struct {
	Coordinate MavenVersionCoordinate `json:"coordinate"`
	Repository string                 `json:"repository"`
	// Directory 是该版本在仓库内的目录，所有 Files 都以它为前缀。
	Directory string             `json:"directory"`
	Files     []MavenVersionFile `json:"files"`
	// Categories 是各类别的文件数与字节汇总，顺序稳定，便于前端直接渲染。
	Categories []MavenVersionCategoryStat `json:"categories"`
	// SnapshotBuilds 仅在 SNAPSHOT 版本上非空：列出全部历史构建。
	// 字面 `-SNAPSHOT` 文件（Gradle 直接发布形态）以 BuildID="SNAPSHOT" 单列一组。
	SnapshotBuilds []MavenSnapshotBuild `json:"snapshotBuilds,omitempty"`
	// LiteralSnapshotFiles 统计字面 `-SNAPSHOT` 文件数，与时间戳构建区分开。
	LiteralSnapshotFiles int   `json:"literalSnapshotFiles"`
	TotalFiles           int   `json:"totalFiles"`
	TotalBytes           int64 `json:"totalBytes"`
	// Exists 表示目录下确实有文件；false 时 TotalFiles 为 0，调用方据此报 404。
	Exists bool `json:"exists"`
}

// MavenVersionCategoryStat 是单个类别的汇总。
type MavenVersionCategoryStat struct {
	Category  MavenVersionFileCategory `json:"category"`
	FileCount int                      `json:"fileCount"`
	Bytes     int64                    `json:"bytes"`
}

// checksumSuffixes 是 Maven 生态实际出现的校验和后缀，长的在前以便正确剥离。
var checksumSuffixes = []string{".sha512", ".sha256", ".sha1", ".md5"}

// classifyMavenVersionFile 判定文件在版本内的类别与所属构建。
//
// 判定顺序有意义：先判 metadata（它同时也是 .xml），再剥校验和后判主档/classifier，
// 否则 `maven-metadata.xml.sha1` 会被误判成"名为 maven-metadata.xml 的主档"。
func classifyMavenVersionFile(artifactID, version, filename string) (MavenVersionFileCategory, string, string) {
	// metadata 优先：artifact 级与版本级都以 maven-metadata.xml 结尾。
	base := filename
	var checksumType string
	for _, suffix := range checksumSuffixes {
		if strings.HasSuffix(base, suffix) {
			base = strings.TrimSuffix(base, suffix)
			checksumType = suffix
			break
		}
	}
	if strings.HasPrefix(base, "maven-metadata") && strings.HasSuffix(base, ".xml") {
		// 版本级 metadata 归 metadata 类；其校验和也归同一类，
		// 因为用户心智里"删版本"就是连 metadata 带校验和一起删。
		return MavenFileMetadata, "", checksumType
	}

	// 先剥离 SNAPSHOT 时间戳构建号：文件名形如
	//   <artifactId>-<version-without-SNAPSHOT>-<timestamp>-<build>[-<classifier>].<ext>
	// 例如 demo-1.17-20260317.215705-3-sources.jar（classifier 在后）
	// 或   demo-1.17-20260317.215705-3.jar（无 classifier）。
	// 构建号必须最先剥，否则后面的「去 version」和「找 classifier」都会错位。
	// 校验和文件也要算：demo-...-3.jar.sha1 属于第 3 次构建，
	// 否则删单个构建时它的校验和会被留下，成为指向已删文件的残骸。
	buildID := extractSnapshotBuildID(base)
	if checksumType != "" {
		// 校验和只需知道归属构建，无需区分主档/classifier。
		return MavenFileChecksum, buildID, checksumType
	}

	withinBuild := base
	if buildID != "" {
		withinBuild = strings.Replace(base, "-"+buildID, "", 1)
	}

	// 去掉尾部 version。对时间戳构建，version 在文件名里表现为去 SNAPSHOT 的形态
	// （1.17-SNAPSHOT → 1.17），因此两种形态都要试。
	stem := strings.TrimSuffix(withinBuild, mavenExtension(withinBuild))
	if strings.HasPrefix(stem, artifactID+"-") {
		stem = strings.TrimPrefix(stem, artifactID+"-")
	}
	for _, v := range []string{version, strings.TrimSuffix(version, "-SNAPSHOT")} {
		if trimmed := trimVersionAndBuild(stem, v); trimmed != stem {
			stem = trimmed
			break
		}
	}
	// stem 现在只剩 classifier（可能为空）。注意 classifier 位于版本号之后，
	// 因此上面剥离后若还有内容，它就是 classifier。
	stem = strings.Trim(stem, "-")
	if stem == "" {
		return MavenFilePrimary, buildID, ""
	}
	return MavenFileClassifier, buildID, ""
}

// mavenExtension 返回文件名最后一个扩展名（含点）；无扩展名返回空串。
func mavenExtension(name string) string {
	if i := strings.LastIndex(name, "."); i >= 0 && i > 0 {
		return name[i:]
	}
	return ""
}

// trimVersionAndBuild 从尾部去掉 `-<version>`；不匹配时原样返回。
func trimVersionAndBuild(name, version string) string {
	suffix := "-" + version
	if strings.HasSuffix(name, suffix) {
		return strings.TrimSuffix(name, suffix)
	}
	return name
}

// extractSnapshotBuildID 从文件名中识别 Maven SNAPSHOT 的 `<timestamp>-<buildNumber>`。
//
// 时间戳形如 20260317.215705（8 位日期 . 6 位时间），构建号是其后紧跟的整数。
// classifier 有两种位置，都必须支持：
//   - 构建号在末尾：demo-1.17-20260317.215705-3.jar
//   - classifier 在构建号之后：demo-1.17-20260317.215705-3-sources.jar
//
// 做法：去掉扩展名与校验和后缀，从右往左扫描每一个 `-` 分段，把相邻的
// `<timestamp>` + `<digits>` 组合识别为构建号。只在相邻两段同时满足时才认定，
// 避免把普通 classifier（如 `-rc1`、`-1`）误判成构建号。
func extractSnapshotBuildID(filename string) string {
	base := filename
	// 先去校验和后缀，再去扩展名；顺序不能反（.jar.sha1 需先去掉 .sha1）。
	for _, suffix := range checksumSuffixes {
		if strings.HasSuffix(base, suffix) {
			base = strings.TrimSuffix(base, suffix)
			break
		}
	}
	base = strings.TrimSuffix(base, mavenExtension(base))

	segments := strings.Split(base, "-")
	// 相邻两段 (timestamp, buildNumber) 构成构建号；从右往左取第一个命中，
	// 因为 classifier 只会出现在构建号**之后**，从右扫能跳过 classifier。
	for i := len(segments) - 2; i >= 1; i-- {
		if isMavenTimestamp(segments[i]) && allDigits(segments[i+1]) {
			return segments[i] + "-" + segments[i+1]
		}
	}
	return ""
}

// isMavenTimestamp 判断是否为 Maven 的 `yyyyMMdd.HHmmss` 形态。
func isMavenTimestamp(s string) bool {
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

func allDigits(s string) bool {
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

// isLiteralSnapshotFile 判断文件是否属于「字面 -SNAPSHOT」形态（Gradle 直接发布）。
//
// 判定必须排除时间戳构建：时间戳文件名（如 demo-1.17-20260317.215705-3.jar）的
// version 段虽然没有 `-SNAPSHOT` 字样，但同目录下的文件仍可能命中
// `strings.Contains(filename, "-SNAPSHOT")` 之外的情况；真正的区分点是
// **文件名里是否带时间戳构建号**。带构建号的归该构建，不带的才是字面 SNAPSHOT。
func isLiteralSnapshotFile(filename, version string) bool {
	if !strings.HasSuffix(version, "-SNAPSHOT") {
		return false
	}
	if !strings.Contains(filename, "-SNAPSHOT") {
		return false
	}
	// 已能识别出时间戳构建号的文件不属于字面 SNAPSHOT 组。
	if extractSnapshotBuildID(filename) != "" {
		return false
	}
	return true
}

// planMavenVersion 展开版本目录下的全部文件并分类。
//
// 返回的 plan 是**预览与删除共用的唯一真相**：DeleteMavenVersion 直接消费它选中的路径集合，
// 不再自己重新枚举一遍。
func (s *AssetService) planMavenVersion(repo *repository.Repository, coord MavenVersionCoordinate) (*MavenVersionPlan, error) {
	if err := coord.Validate(); err != nil {
		return nil, err
	}
	dir := coord.Dir()
	prefix := dir + "/"

	plan := &MavenVersionPlan{
		Coordinate: coord,
		Repository: repo.Name,
		Directory:  dir,
	}

	// 分批读取全量：必须读完才能给出准确的 TotalFiles/TotalBytes，
	// 也才能保证"预览看见的就是会删的"。
	offset := 0
	for {
		batch, err := s.assets.ListByRepo(repo.ID, prefix, mavenPlanBatchSize, offset)
		if err != nil {
			return nil, err
		}
		if len(batch) == 0 {
			break
		}
		for _, asset := range batch {
			filename := strings.TrimPrefix(asset.Path, prefix)
			// 防御性检查：LIKE 前缀理论上不会带出更深层的子目录内容，
			// 但 artifact 目录下可能存在同名前缀的兄弟版本（如 1.17 与 1.17.1），
			// 若路径里还含 `/` 则说明不在本版本目录内，必须跳过。
			if strings.Contains(filename, "/") {
				continue
			}
			category, buildID, checksumType := classifyMavenVersionFile(coord.ArtifactID, coord.Version, filename)
			if isLiteralSnapshotFile(filename, coord.Version) {
				buildID = "SNAPSHOT"
				plan.LiteralSnapshotFiles++
			}
			plan.Files = append(plan.Files, MavenVersionFile{
				Path:      asset.Path,
				Size:      asset.Size,
				Category:  category,
				BlobHash:  asset.BlobHash,
				Checksum:  checksumType,
				BuildID:   buildID,
				UpdatedAt: asset.UpdatedAt,
			})
			plan.TotalFiles++
			plan.TotalBytes += asset.Size
		}
		offset += len(batch)
		if len(batch) < mavenPlanBatchSize {
			break
		}
	}

	plan.Exists = plan.TotalFiles > 0
	if !plan.Exists {
		return plan, nil
	}

	// 分类汇总：顺序固定（primary → classifier → checksum → metadata → other），
	// 让前端不依赖 map 迭代顺序。
	order := []MavenVersionFileCategory{
		MavenFilePrimary, MavenFileClassifier, MavenFileChecksum, MavenFileMetadata, MavenFileOther,
	}
	counts := make(map[MavenVersionFileCategory][2]int64, len(order))
	for _, file := range plan.Files {
		cur := counts[file.Category]
		cur[0]++
		cur[1] += file.Size
		counts[file.Category] = cur
	}
	for _, category := range order {
		stat := counts[category]
		if stat[0] == 0 {
			continue
		}
		plan.Categories = append(plan.Categories, MavenVersionCategoryStat{
			Category: category, FileCount: int(stat[0]), Bytes: stat[1],
		})
	}

	// SNAPSHOT 历史构建汇总：只对 SNAPSHOT 版本有意义。
	if coord.IsSnapshot() {
		builds := make(map[string]*MavenSnapshotBuild)
		for _, file := range plan.Files {
			if file.BuildID == "" {
				continue
			}
			entry, ok := builds[file.BuildID]
			if !ok {
				entry = &MavenSnapshotBuild{BuildID: file.BuildID}
				builds[file.BuildID] = entry
			}
			entry.FileCount++
			entry.Bytes += file.Size
		}
		for _, entry := range builds {
			plan.SnapshotBuilds = append(plan.SnapshotBuilds, *entry)
		}
		// 按 BuildID 降序：时间戳构建天然按时间递增，降序即"最新在前"，
		// 字面 SNAPSHOT 排在最后（它没有时间戳，且通常是应被清理的那个）。
		sort.Slice(plan.SnapshotBuilds, func(i, j int) bool {
			li, lj := plan.SnapshotBuilds[i].BuildID, plan.SnapshotBuilds[j].BuildID
			if li == "SNAPSHOT" {
				return false
			}
			if lj == "SNAPSHOT" {
				return true
			}
			return li > lj
		})
	}

	return plan, nil
}

// selectMavenPlanFiles 按坐标范围从计划中选出要删除的文件，并单独返回需要重建的 metadata。
//
// 范围语义：
//   - BuildID 为空（删整个版本）：目录下全部文件都删，metadata 一并删除；
//   - BuildID 非空（只删某次构建）：只选该构建的文件，**元数据不进删除集合**——
//     它由调用方按剩余构建重建。把旧 metadata 一起删掉再重建是多余的两步，
//     而保留它则会指向已不存在的构建，客户端会反复 404。
//
// 返回值 metadata 是"本次必须重建的版本级 metadata 文件"，可能与删除集合并存。
func selectMavenPlanFiles(plan *MavenVersionPlan) (files []MavenVersionFile, metadata []MavenVersionFile) {
	if plan.Coordinate.BuildID == "" {
		return plan.Files, nil
	}
	out := make([]MavenVersionFile, 0, len(plan.Files))
	for _, file := range plan.Files {
		switch {
		case file.BuildID == plan.Coordinate.BuildID:
			out = append(out, file)
		case file.Category == MavenFileMetadata:
			// 只删某次构建时，版本级 metadata 必须按剩余构建重写。
			// 它的校验和文件同样要重写，因此一并纳入重建集合。
			metadata = append(metadata, file)
		}
	}
	return out, metadata
}

// ErrMavenVersionEmpty 表示坐标目录下没有任何文件。
var ErrMavenVersionEmpty = errors.New("该 Maven 版本下没有制品")
