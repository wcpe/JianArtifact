// 搜索结果包级聚合与命中精度分层（FR-150）。
//
// 规格：docs/specs/search-package-aggregation.md §3.2 ~ §3.7。本文件是**纯函数**集合：
// 输入是已经取回的文件行与用户关键词，输出是「包 → 版本 → 文件」三层聚合结果，不碰数据库、
// 不碰 ctx。因此同一套规则既可以在服务层用真实候选行驱动，也可以在单测里用构造行驱动。
//
// 坐标与版本都是 path 的派生量（asset 表没有 version 列），所以这里的解析规则就是聚合口径的
// 唯一定义点：要改口径就改这里，不要在调用方另做判断。
package domain

import (
	"sort"
	"strconv"
	"strings"

	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// maxAggregateRows 是单次包级聚合的扫描预算（规格 §3.7）。
//
// 按 path ASC 确定性截断：超出预算时聚合结果是「下界」，但仍可用——total 始终来自
// SearchByFilter 的 COUNT(*)，不受截断影响。
const maxAggregateRows = 5000

// 精度分层标记（规格 §3.4）。字符串取值即响应里的 bestPrecision / precision。
const (
	PrecisionExact      = "exact"      // 版本段与关键词完全相等
	PrecisionPrerelease = "prerelease" // 命中串是版本前缀，后面还有标记段（-rc1 / -SNAPSHOT / +build）
	PrecisionSubstring  = "substring"  // 纯子串巧合（1.17.120 / 1.17.12.1）
	PrecisionNone       = "none"       // 无版本相关度信号（或不含坐标的降级条目）
)

// 包节点类型（规格 §3.2 / §3.7）：maven | npm | file。
const (
	PackageKindMaven = "maven"
	PackageKindNpm   = "npm"
	PackageKindFile  = "file"
)

// 精度分层的编号：越小越精确，正是 §3.5 比较器的第一键（升序）。
const (
	precisionRankExact      = 0
	precisionRankPrerelease = 1
	precisionRankSubstring  = 2
	precisionRankNone       = 3
)

// precisionRank 把精度标记还原为比较用编号；非法/空值一律落到 none。
func precisionRank(label string) int {
	switch label {
	case PrecisionExact:
		return precisionRankExact
	case PrecisionPrerelease:
		return precisionRankPrerelease
	case PrecisionSubstring:
		return precisionRankSubstring
	default:
		return precisionRankNone
	}
}

// precisionLabel 是 precisionRank 的逆映射。
func precisionLabel(rank int) string {
	switch rank {
	case precisionRankExact:
		return PrecisionExact
	case precisionRankPrerelease:
		return PrecisionPrerelease
	case precisionRankSubstring:
		return PrecisionSubstring
	default:
		return PrecisionNone
	}
}

// asciiLower 只折叠 ASCII 的 A-Z。
//
// 精度比较必须与 SQLite LIKE 的默认大小写行为一致（规格 §3.4）：LIKE 只折叠 ASCII，因此这里
// 不能用 strings.ToLower / EqualFold 的 Unicode 折叠——那会让 Go 侧判定比 SQL 更宽松，出现
// 「SQL 未命中却被算作命中」的错位。逐字节处理对 UTF-8 安全：多字节序列的每个字节都 >= 0x80，
// 不会被改写。
func asciiLower(s string) string {
	b := []byte(s)
	for i := 0; i < len(b); i++ {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}

// startsWithDigit 报告 s 是否以 ASCII 数字起首（规格 §3.2 的版本段判定条件 `^[0-9]`）。
func startsWithDigit(s string) bool {
	return s != "" && s[0] >= '0' && s[0] <= '9'
}

// Coordinates 是一条 path 解析出的包坐标（规格 §3.2）。
//
// 解析受仓库 format **门控**：只有 maven / npm 两种格式启用对应规则，其余格式（raw 等）
// 一律降级——宁可少聚合，也不把 docs/a/b/c 误判成 a:b:c（规格 §6 的取舍）。
type Coordinates struct {
	Kind       string // maven / npm / file（file = 未解析出坐标的降级条目）
	Coordinate string // 展示坐标：maven 为 group:artifact，npm 为包名；降级时为空
	Version    string // 版本段；空表示未版本化（npm packument 等）
	// AtPackageRoot 标记该 path 本身就是包根（npm packument），供 fileKind 归类使用。
	AtPackageRoot bool
}

// HasPackage 报告是否解析出了包坐标（降级条目为 false）。
func (c Coordinates) HasPackage() bool { return c.Kind != PackageKindFile }

// ParseCoordinates 按仓库格式解析一条 path 的包坐标（规格 §3.2）。
// 解析不出坐标时返回 Kind=PackageKindFile 的零值结果，调用方据此降级为文件级条目。
func ParseCoordinates(format, path string) Coordinates {
	segs := strings.Split(path, "/")
	switch format {
	case PackageKindMaven:
		if c, ok := parseMavenCoordinates(segs); ok {
			return c
		}
	case PackageKindNpm:
		if c, ok := parseNpmCoordinates(segs); ok {
			return c
		}
	}
	return Coordinates{Kind: PackageKindFile}
}

// parseMavenCoordinates 解析 Maven 的 group/artifact/version/file（规格 §3.2）。
//
// 判定条件：段数 >= 4 **且** 倒数第二段（版本目录段）以数字起首。只取末三段，因此深层 group
// （com/example/sub/module/...）不需要任何特判。
// 不满足条件的（如 n == 3 的 group/artifact/file 扁平发布）返回 false → 降级。
func parseMavenCoordinates(segs []string) (Coordinates, bool) {
	n := len(segs)
	if n < 4 {
		return Coordinates{}, false
	}
	version := segs[n-2]
	artifact := segs[n-3]
	// group 是 artifact 之前的全部段（Go 切片上界取 n-3，即"去掉末三段"）。
	groupSegs := segs[:n-3]
	if !startsWithDigit(version) || artifact == "" || len(groupSegs) == 0 {
		return Coordinates{}, false
	}
	group := strings.Join(groupSegs, ".")
	if group == "" {
		return Coordinates{}, false
	}
	return Coordinates{
		Kind:       PackageKindMaven,
		Coordinate: group + ":" + artifact,
		Version:    version,
	}, true
}

// parseNpmCoordinates 解析 npm 的三种形态（规格 §3.2）：
//
//	@scope/pkg                     scoped packument（版本为空）
//	@scope/pkg/<version>/...       scoped 版本目录
//	pkg                            unscoped packument（版本为空）
//	pkg/<version>/...              unscoped 版本目录
//	pkg/-/pkg-<version>.tgz        unscoped tarball（版本从文件名剥离）
//
// scoped 的 tarball（@scope/pkg/-/...）按规格不剥离版本，归入「未版本化」组，不丢条目。
func parseNpmCoordinates(segs []string) (Coordinates, bool) {
	if len(segs) == 0 || segs[0] == "" {
		return Coordinates{}, false
	}
	if strings.HasPrefix(segs[0], "@") {
		if len(segs) < 2 || segs[1] == "" {
			return Coordinates{}, false // 只有 scope 段，构不成包名
		}
		c := Coordinates{Kind: PackageKindNpm, Coordinate: segs[0] + "/" + segs[1]}
		switch {
		case len(segs) == 2:
			c.AtPackageRoot = true
		case startsWithDigit(segs[2]):
			c.Version = segs[2]
		}
		return c, true
	}
	c := Coordinates{Kind: PackageKindNpm, Coordinate: segs[0]}
	switch {
	case len(segs) == 1:
		c.AtPackageRoot = true
	case segs[1] == "-":
		// tarball 形态：版本不在独立路径段里，从文件名剥离。
		c.Version = npmTarballVersion(segs[len(segs)-1], segs[0])
	case startsWithDigit(segs[1]):
		c.Version = segs[1]
	}
	return c, true
}

// npmTarballVersion 从 tarball 文件名 `<pkg>-<version>.tgz` 剥离版本（规格 §3.2）。
// 剥离失败（后缀或前缀不匹配）返回空串，条目归入包根的「未版本化」组而不是被丢弃。
func npmTarballVersion(name, pkg string) string {
	rest, ok := strings.CutSuffix(name, ".tgz")
	if !ok {
		return ""
	}
	prefix := pkg + "-"
	if !strings.HasPrefix(rest, prefix) {
		return ""
	}
	return strings.TrimPrefix(rest, prefix)
}

// MatchPrecision 判定版本段 version 相对关键词集合的命中精度（规格 §3.4）。
// 多个关键词取**编号最小**者（最有利精度）；没有关键词命中或版本段为空时返回 none。
func MatchPrecision(terms []string, version string) string {
	return precisionLabel(matchPrecisionRank(terms, version))
}

// matchPrecisionRank 是 MatchPrecision 的编号形态，供聚合阶段直接比较。
//
// 判定对每个关键词**自上而下取首个命中项**：exact → prerelease（命中串是前缀且余下部分以
// `-` / `+` 起首）→ substring（含但不满足前两条）→ none。用结构判定而不是预发布后缀白名单，
// 是因为 `-` / `+` 起首已覆盖 Maven 与 semver 的全部预发布/构建元数据形态，而封闭名单必然
// 漏项（规格 §3.4 的论证）。
func matchPrecisionRank(terms []string, version string) int {
	if version == "" {
		return precisionRankNone
	}
	v := asciiLower(version)
	best := precisionRankNone
	for _, t := range terms {
		tt := asciiLower(t)
		if tt == "" {
			continue
		}
		switch {
		case v == tt:
			// 已是最高精度，无需再看其它关键词。
			return precisionRankExact
		case strings.HasPrefix(v, tt):
			rest := v[len(tt):]
			if rest != "" && (rest[0] == '-' || rest[0] == '+') {
				if best > precisionRankPrerelease {
					best = precisionRankPrerelease
				}
			} else if best > precisionRankSubstring {
				// 是前缀但不是预发布标记（如 1.17.12.1 的 `.1`）：纯子串巧合。
				best = precisionRankSubstring
			}
		case strings.Contains(v, tt):
			if best > precisionRankSubstring {
				best = precisionRankSubstring
			}
		}
	}
	return best
}

// versionVector 把版本串切成数字向量（规格 §3.5）：按 `.` / `-` / `+` 分段，遇到非数字段即停。
//
//	1.17.12                  → [1 17 12]
//	1.17.12-rc1              → [1 17 12]
//	1.17.12+build.7          → [1 17 12]
//	1.17.12-20240101.1-3     → [1 17 12 20240101 1 3]
//	latest                   → 空向量（等价于全 0）
//
// 这是"足够好"的数字比较，不是严格 semver 语义（规格 §2.3 明确不做）。
func versionVector(version string) []int64 {
	if version == "" {
		return nil
	}
	var out []int64
	for _, seg := range strings.FieldsFunc(version, func(r rune) bool {
		return r == '.' || r == '-' || r == '+'
	}) {
		n, err := strconv.ParseInt(seg, 10, 64)
		if err != nil {
			break // 非数字段：向量到此为止
		}
		out = append(out, n)
	}
	return out
}

// compareVersionVectors 逐位数值比较两个版本向量（缺失位视为 0），返回 -1 / 0 / 1。
func compareVersionVectors(a, b []int64) int {
	n := len(a)
	if len(b) > n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		var av, bv int64
		if i < len(a) {
			av = a[i]
		}
		if i < len(b) {
			bv = b[i]
		}
		if av != bv {
			if av < bv {
				return -1
			}
			return 1
		}
	}
	return 0
}

// fileExt 取文件名最后一个点之后的部分并小写化（不含点）；无扩展名返回空串。
func fileExt(name string) string {
	if i := strings.LastIndex(name, "."); i >= 0 && i < len(name)-1 {
		return asciiLower(name[i+1:])
	}
	return ""
}

// checksumExts 与前端 CHECKSUM_EXTS 保持同一集合（规格附录 A）：
// 「一键隐藏校验和」与「树内校验和折叠」必须同一口径，否则两处会打架。
var checksumExts = map[string]bool{
	"sha1": true, "md5": true, "sha256": true, "sha512": true, "asc": true,
}

// classifyFile 返回文件的 fileKind 与其排序优先级（规格附录 A）。
//
// 判定顺序自上而下、首个命中即停；优先级越小越靠前（主构件在前、校验和垫底）。
// checksum 写在 default 之前是必须的：附录 A 的 other 是"以上都不匹配"的兜底，
// 而校验和的扩展名不会被任何更早的分支命中。
func classifyFile(coords Coordinates, path string) (string, int) {
	name := path
	if i := strings.LastIndex(path, "/"); i >= 0 {
		name = path[i+1:]
	}
	ext := fileExt(name)
	switch {
	case ext == "jar" && !strings.Contains(name, "-sources") &&
		!strings.Contains(name, "-javadoc") && !strings.Contains(name, "-tests"):
		return "jar", 0
	case ext == "war" || ext == "ear" || ext == "aar":
		return ext, 1
	case ext == "tgz":
		return "tgz", 1
	case ext == "jar" && strings.Contains(name, "-sources"):
		return "sources", 2
	case ext == "jar" && strings.Contains(name, "-javadoc"):
		return "javadoc", 3
	case ext == "jar" && strings.Contains(name, "-tests"):
		return "tests", 4
	case ext == "pom":
		return "pom", 5
	case coords.AtPackageRoot:
		// packument：路径本身就是 npm 包根（`pkg` 或 `@scope/pkg`）。
		// 附录 A 的判定还写了"扩展名 json"，但本仓库的 packument 路径就是裸包名、
		// 不带 .json 后缀（见 npm 协议 `assets.Get(repo, pkg)`），因此扩展名不是可用维度；
		// AtPackageRoot 才是"路径为 npm 包根"的精确表达。
		return "packument", 5
	case name == "maven-metadata.xml" || ext == "module":
		return "metadata", 6
	case checksumExts[ext]:
		return "checksum", 9
	default:
		return "other", 7
	}
}

// PackageFile 是聚合结果里的一个文件行（响应 packages[].versions[].files[]）。
type PackageFile struct {
	Name       string // 文件名（path 的最后一段）
	Path       string // 仓库内完整路径
	Repository string // 仓库名（跨仓库合并时用于消歧）
	Size       int64
	Sha1       string
	UpdatedAt  string
	FileKind   string
	// kindPriority 是 fileKind 的排序优先级（规格附录 A），只参与组内排序、不对外输出。
	kindPriority int
}

// PackageVersion 是包内的一个版本组（版本标识 ASCII 大小写不敏感归并）。
type PackageVersion struct {
	Version         string // 展示版本串（取首次出现的原文）；空串表示「未版本化」组
	Precision       string // exact / prerelease / substring / none
	FileCount       int
	Size            int64  // 组内文件体积合计
	LatestUpdatedAt string // 组内最新 updated_at
	Files           []PackageFile
}

// SearchPackage 是一个包节点（响应 packages[]）。
type SearchPackage struct {
	Coordinate   string // 展示坐标；无坐标条目为仓库名
	Kind         string // maven | npm | file
	Repositories []string
	VersionCount int // 版本组数（不含空版本组）
	FileCount    int // 组内文件总数（含校验和文件）
	// BestPrecision 是组内所有版本精度的最小值（规格 §3.4）；无坐标条目恒为 none。
	BestPrecision   string
	LatestUpdatedAt string
	TopVersion      string // 组内排序后的首个版本；用于 relevance 的第 2 键
	Versions        []PackageVersion
	// Files 只用于「无坐标」伪包：规格 §3.2 要求这类条目**跳过版本层**、文件直接挂在包下。
	// 有坐标的包不使用本字段（文件挂在 Versions[].Files 里）。
	Files []PackageFile
}

// PackageSource 是聚合的输入行：一条文件级搜索命中 + 它所属仓库的名字与格式。
// 格式决定坐标解析规则（规格 §3.2 的门控），名字是降级条目的伪包维度。
type PackageSource struct {
	RepoName string
	Format   string
	Asset    repository.Asset
}

// packageKey 是包的身份键：**不含仓库**（同坐标跨仓库合并为一个包节点，规格 §3.3）。
// kind 参与键是为了让「某个 maven 坐标恰好与某个仓库同名」不会误合并。
type packageKey struct {
	kind       string
	coordinate string
}

// BuildPackageGroups 把候选行聚合为「包 → 版本 → 文件」三层（规格 §3.3）。
//
// 入参 rows 应已按 path 升序（SearchAllByFilter 的确定性顺序），出参保持首次出现顺序，
// 因此本函数自身也是确定性输出（版本/文件层在函数内完成组内排序）。
// 包与版本层的先后由调用方按 relevance 比较器排序（SortSearchPackages）。
func BuildPackageGroups(rows []PackageSource, includeTerms []string) []SearchPackage {
	type acc struct {
		pkg      SearchPackage
		repoSeen map[string]bool
		verSeen  map[string]int // 小写版本串 → Versions 下标
		latest   string
	}
	index := make(map[packageKey]*acc, len(rows))
	accs := make([]*acc, 0, len(rows))

	for _, row := range rows {
		coords := ParseCoordinates(row.Format, row.Asset.Path)
		key := packageKey{kind: coords.Kind, coordinate: coords.Coordinate}
		if !coords.HasPackage() {
			// 裸路径：按**仓库**归为伪包（每个仓库一个），避免每个路径都成一个节点。
			key = packageKey{kind: PackageKindFile, coordinate: row.RepoName}
		}
		a, ok := index[key]
		if !ok {
			a = &acc{
				pkg: SearchPackage{
					Coordinate:    key.coordinate,
					Kind:          key.kind,
					BestPrecision: PrecisionNone,
				},
				repoSeen: make(map[string]bool, 1),
				verSeen:  make(map[string]int, 4),
			}
			if coords.HasPackage() {
				a.pkg.Versions = make([]PackageVersion, 0, 4)
			} else {
				a.pkg.Files = make([]PackageFile, 0, 4)
			}
			index[key] = a
			accs = append(accs, a)
		}
		if !a.repoSeen[row.RepoName] {
			a.repoSeen[row.RepoName] = true
			a.pkg.Repositories = append(a.pkg.Repositories, row.RepoName)
		}
		kind, priority := classifyFile(coords, row.Asset.Path)
		file := PackageFile{
			Name:         baseName(row.Asset.Path),
			Path:         row.Asset.Path,
			Repository:   row.RepoName,
			Size:         row.Asset.Size,
			Sha1:         row.Asset.Sha1,
			UpdatedAt:    row.Asset.UpdatedAt,
			FileKind:     kind,
			kindPriority: priority,
		}
		a.pkg.FileCount++
		if row.Asset.UpdatedAt > a.latest {
			a.latest = row.Asset.UpdatedAt
		}
		if !coords.HasPackage() {
			// 版本层跳过：文件直接挂在伪包下。
			a.pkg.Files = append(a.pkg.Files, file)
			continue
		}
		lowerVersion := asciiLower(coords.Version)
		vi, seen := a.verSeen[lowerVersion]
		if !seen {
			vi = len(a.pkg.Versions)
			a.verSeen[lowerVersion] = vi
			a.pkg.Versions = append(a.pkg.Versions, PackageVersion{
				Version:   coords.Version, // 展示取首次出现的原文
				Precision: precisionLabel(matchPrecisionRank(includeTerms, coords.Version)),
				Files:     make([]PackageFile, 0, 4),
			})
		}
		v := &a.pkg.Versions[vi]
		v.Files = append(v.Files, file)
	}

	out := make([]SearchPackage, 0, len(accs))
	for _, a := range accs {
		p := a.pkg
		p.LatestUpdatedAt = a.latest
		// 仓库列表排序：跨仓库合并后的展示顺序不能取决于行到达顺序。
		sort.Strings(p.Repositories)
		bestRank := precisionRankNone
		for i := range p.Versions {
			v := &p.Versions[i]
			sortPackageFiles(v.Files)
			v.FileCount = len(v.Files)
			v.Size = 0
			v.LatestUpdatedAt = ""
			for _, f := range v.Files {
				v.Size += f.Size
				if f.UpdatedAt > v.LatestUpdatedAt {
					v.LatestUpdatedAt = f.UpdatedAt
				}
			}
			if v.Version != "" {
				p.VersionCount++
			}
			if r := precisionRank(v.Precision); r < bestRank {
				bestRank = r
			}
		}
		// 组内版本排序（精度升序 → 版本向量降序 → 版本串升序）后再取 topVersion。
		sortPackageVersions(p.Versions)
		if len(p.Versions) > 0 {
			p.TopVersion = p.Versions[0].Version
		}
		p.BestPrecision = precisionLabel(bestRank)
		sortPackageFiles(p.Files)
		out = append(out, p)
	}
	return out
}

// baseName 取 path 的最后一段。
func baseName(path string) string {
	if i := strings.LastIndex(path, "/"); i >= 0 {
		return path[i+1:]
	}
	return path
}

// sortPackageFiles 按「fileKind 优先级升序 → path 升序」排列（规格 §3.5）：
// 主构件在最前、校验和垫底，同类型内按路径稳定。
func sortPackageFiles(files []PackageFile) {
	sort.Slice(files, func(i, j int) bool {
		if files[i].kindPriority != files[j].kindPriority {
			return files[i].kindPriority < files[j].kindPriority
		}
		return files[i].Path < files[j].Path
	})
}

// sortPackageVersions 按「精度升序 → 版本向量降序 → 版本串升序」排列（规格 §3.5）。
func sortPackageVersions(versions []PackageVersion) {
	sort.Slice(versions, func(i, j int) bool {
		ri, rj := precisionRank(versions[i].Precision), precisionRank(versions[j].Precision)
		if ri != rj {
			return ri < rj
		}
		if c := compareVersionVectors(versionVector(versions[i].Version), versionVector(versions[j].Version)); c != 0 {
			return c > 0
		}
		return versions[i].Version < versions[j].Version
	})
}

// SortSearchPackages 按相关度比较器对包排序（规格 §3.5 的字典序，逐键比较至分出胜负）：
//
//  1. bestPrecisionRank **升序**（0 精确 → 1 预发布 → 2 子串 → 3 无信号）；
//  2. topVersionKey **降序**，**仅当 bestPrecisionRank < 3 时参与**，否则跳过；
//  3. latestUpdatedAt **降序**；
//  4. coordinate **升序**（稳定兜底）。
//
// 第 2 键是"同精度下新版本优先"；第 3 键承接"没有任何版本信号时新的改动在前"——它恰好等于
// 改造前的默认排序 updated desc，因此无版本信号的查询（绝大多数关键词搜索）在 relevance 下
// 的体验与现状一致。第 4 键 + kind 保证分页稳定、可复现。
//
// 验收示例（q=1.17.12，同包三个版本全部命中）：1.17.12 → 1.17.12-rc1 → 1.17.120。
func SortSearchPackages(pkgs []SearchPackage) {
	sort.Slice(pkgs, func(i, j int) bool { return packageLess(pkgs[i], pkgs[j]) })
}

// packageLess 是 SortSearchPackages 的比较器本体。
func packageLess(a, b SearchPackage) bool {
	ra, rb := precisionRank(a.BestPrecision), precisionRank(b.BestPrecision)
	if ra != rb {
		return ra < rb
	}
	if ra < precisionRankNone {
		if c := compareVersionVectors(versionVector(a.TopVersion), versionVector(b.TopVersion)); c != 0 {
			return c > 0
		}
	}
	if a.LatestUpdatedAt != b.LatestUpdatedAt {
		return a.LatestUpdatedAt > b.LatestUpdatedAt
	}
	if a.Coordinate != b.Coordinate {
		return a.Coordinate < b.Coordinate
	}
	return a.Kind < b.Kind
}
