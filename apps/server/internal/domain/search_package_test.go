package domain

import (
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// 本文件钉住的**纯函数规则**（规格 §3.2 ~ §3.5）：
// 包坐标解析、命中精度分层、版本向量与 relevance 排序。
//
// 这些都是"口径"本身，出错的表现是排序莫名其妙或把噪声顶到前面，且不会报错——
// 所以必须逐条对照规格的示例表断言，而不是只测"能跑通"。

// TestParseCoordinates 覆盖规格 §3.2 的三种形态与降级规则。
func TestParseCoordinates(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		format    string
		path      string
		wantKind  string
		wantCoord string
		wantVer   string
		wantRoot  bool
	}{
		{
			name:      "Maven 常规三层 group",
			format:    "maven",
			path:      "com/example/foo/1.17.12/foo-1.17.12.jar",
			wantKind:  PackageKindMaven,
			wantCoord: "com.example:foo",
			wantVer:   "1.17.12",
		},
		{
			name:      "Maven 深层 group 只取末三段之前的全部段",
			format:    "maven",
			path:      "com/example/sub/module/bar/2.0.0/bar-2.0.0.pom",
			wantKind:  PackageKindMaven,
			wantCoord: "com.example.sub.module:bar",
			wantVer:   "2.0.0",
		},
		{
			name:      "Maven 单层 group（n==4）",
			format:    "maven",
			path:      "org/demo/1.0.0/demo-1.0.0.jar",
			wantKind:  PackageKindMaven,
			wantCoord: "org:demo",
			wantVer:   "1.0.0",
		},
		{
			name:     "Maven n==3 扁平发布降级为无坐标",
			format:   "maven",
			path:     "group/artifact/file-1.0.0.jar",
			wantKind: PackageKindFile,
		},
		{
			name:     "Maven 版本段不以数字起首则降级",
			format:   "maven",
			path:     "com/example/foo/latest/foo-latest.jar",
			wantKind: PackageKindFile,
		},
		{
			name:      "npm unscoped 版本目录",
			format:    "npm",
			path:      "pkg-a/1.0.0/pkg-a-1.0.0.tgz",
			wantKind:  PackageKindNpm,
			wantCoord: "pkg-a",
			wantVer:   "1.0.0",
		},
		{
			name:      "npm unscoped packument（版本为空）",
			format:    "npm",
			path:      "pkg-a",
			wantKind:  PackageKindNpm,
			wantCoord: "pkg-a",
			wantRoot:  true,
		},
		{
			name:      "npm scoped packument",
			format:    "npm",
			path:      "@scope/pkg",
			wantKind:  PackageKindNpm,
			wantCoord: "@scope/pkg",
			wantRoot:  true,
		},
		{
			name:      "npm scoped 版本目录",
			format:    "npm",
			path:      "@scope/pkg/2.1.0/pkg-2.1.0.tgz",
			wantKind:  PackageKindNpm,
			wantCoord: "@scope/pkg",
			wantVer:   "2.1.0",
		},
		{
			name:      "npm tarball 形态剥出版本",
			format:    "npm",
			path:      "pkg-a/-/pkg-a-3.2.1.tgz",
			wantKind:  PackageKindNpm,
			wantCoord: "pkg-a",
			wantVer:   "3.2.1",
		},
		{
			name:      "npm tarball 剥离失败归未版本化",
			format:    "npm",
			path:      "pkg-a/-/other-name.tgz",
			wantKind:  PackageKindNpm,
			wantCoord: "pkg-a",
		},
		{
			name:      "npm 非数字第二段归未版本化",
			format:    "npm",
			path:      "pkg-a/README.md",
			wantKind:  PackageKindNpm,
			wantCoord: "pkg-a",
		},
		{
			name:     "raw 仓库一律不解析（格式门控）",
			format:   "raw",
			path:     "com/example/foo/1.17.12/foo-1.17.12.jar",
			wantKind: PackageKindFile,
		},
		{
			name:     "裸路径降级",
			format:   "raw",
			path:     "docs/a/b.txt",
			wantKind: PackageKindFile,
		},
		{
			name:     "未知格式降级",
			format:   "",
			path:     "com/example/foo/1.17.12/foo-1.17.12.jar",
			wantKind: PackageKindFile,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseCoordinates(tc.format, tc.path)
			if got.Kind != tc.wantKind {
				t.Fatalf("kind = %q，期望 %q（path=%s）", got.Kind, tc.wantKind, tc.path)
			}
			if got.Coordinate != tc.wantCoord {
				t.Fatalf("coordinate = %q，期望 %q（path=%s）", got.Coordinate, tc.wantCoord, tc.path)
			}
			if got.Version != tc.wantVer {
				t.Fatalf("version = %q，期望 %q（path=%s）", got.Version, tc.wantVer, tc.path)
			}
			if got.AtPackageRoot != tc.wantRoot {
				t.Fatalf("atPackageRoot = %v，期望 %v（path=%s）", got.AtPackageRoot, tc.wantRoot, tc.path)
			}
			if (got.Kind != PackageKindFile) != got.HasPackage() {
				t.Fatalf("HasPackage 与 Kind 不一致：%+v", got)
			}
		})
	}
}

// TestMatchPrecision 逐条对照规格附录 B 的精度用例表（T = 1.17.12）。
func TestMatchPrecision(t *testing.T) {
	t.Parallel()
	terms := []string{"1.17.12"}
	cases := []struct {
		version string
		want    string
	}{
		{"1.17.12", PrecisionExact},
		{"1.17.12.1", PrecisionSubstring},                  // R = .1，不是预发布标记
		{"1.17.12-rc1", PrecisionPrerelease},               // R = -rc1
		{"1.17.12-SNAPSHOT", PrecisionPrerelease},          // R = -SNAPSHOT
		{"1.17.12-alpha.3", PrecisionPrerelease},           // R = -alpha.3
		{"1.17.12-20240101.123456-3", PrecisionPrerelease}, // Maven 时间戳快照
		{"1.17.12+build.7", PrecisionPrerelease},           // 构建元数据
		{"1.17.120", PrecisionSubstring},                   // R = 0（关键回归用例）
		{"11.17.12", PrecisionSubstring},                   // 命中在尾部
		{"1.17.1200", PrecisionSubstring},
		{"1.18.0", PrecisionNone},
		{"", PrecisionNone}, // 无坐标/未版本化
	}
	for _, tc := range cases {
		t.Run(tc.version, func(t *testing.T) {
			if got := MatchPrecision(terms, tc.version); got != tc.want {
				t.Fatalf("MatchPrecision(%q) = %q，期望 %q", tc.version, got, tc.want)
			}
		})
	}
}

// TestMatchPrecisionMultipleTerms 覆盖「多个关键词取编号最小者」与大小写口径。
func TestMatchPrecisionMultipleTerms(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		terms   []string
		version string
		want    string
	}{
		{"多词取最有利精度（精确优先）", []string{"1.17.120", "1.17.12"}, "1.17.120", PrecisionExact},
		{"多词取最有利精度（预发布优于子串）", []string{"1.17.120", "1.17.12"}, "1.17.12-rc1", PrecisionPrerelease},
		{"无关键词则不构成版本信号", nil, "1.17.12", PrecisionNone},
		{"关键词为空串时忽略", []string{""}, "1.17.12", PrecisionNone},
		{"ASCII 大小写不敏感（关键词大写）", []string{"1.17.12-SNAPSHOT"}, "1.17.12-snapshot", PrecisionExact},
		{"ASCII 大小写不敏感（版本大写）", []string{"1.17.12"}, "1.17.12-RC1", PrecisionPrerelease},
		{"只命中包名不命中版本段 → none", []string{"foo"}, "1.17.12", PrecisionNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := MatchPrecision(tc.terms, tc.version); got != tc.want {
				t.Fatalf("MatchPrecision(%v, %q) = %q，期望 %q", tc.terms, tc.version, got, tc.want)
			}
		})
	}
}

// TestVersionVectorNumericOrder 钉住版本向量是**数值**比较而不是字符串比较：
// 1.9.0 必须小于 1.10.0（字符串比较会得出相反结论）。
func TestVersionVectorNumericOrder(t *testing.T) {
	t.Parallel()
	if c := compareVersionVectors(versionVector("1.9.0"), versionVector("1.10.0")); c >= 0 {
		t.Fatalf("1.9.0 应小于 1.10.0（数值比较），得 %d", c)
	}
	cases := []struct {
		a, b string
		want int
	}{
		{"1.17.12", "1.17.120", -1},
		{"1.17.12", "1.17.12", 0},
		{"1.17.12-rc1", "1.17.12", 0}, // 预发布段不参与向量
		{"1.17.12+build.7", "1.17.12", 0},
		{"2.0.0", "1.99.99", 1},
		{"1.0", "1.0.0", 0}, // 缺位视为 0
		{"latest", "0.0.0", 0},
		{"latest", "1.0.0", -1},
	}
	for _, tc := range cases {
		if got := compareVersionVectors(versionVector(tc.a), versionVector(tc.b)); got != tc.want {
			t.Fatalf("compare(%q, %q) = %d，期望 %d", tc.a, tc.b, got, tc.want)
		}
	}
}

// TestBuildPackageGroupsOrder 钉住规格 §3.5 的核心验收顺序：
// 同包 1.17.12（精确）→ 1.17.12-rc1（预发布）→ 1.17.120（子串）。
func TestBuildPackageGroupsOrder(t *testing.T) {
	t.Parallel()
	rows := []PackageSource{
		mavenRow("maven-releases", "com/example/foo/1.17.12/foo-1.17.12.jar", "2026-10-18 09:12:33"),
		mavenRow("maven-releases", "com/example/foo/1.17.12/foo-1.17.12.pom", "2026-10-18 09:12:33"),
		mavenRow("maven-releases", "com/example/foo/1.17.12/foo-1.17.12.jar.sha1", "2026-10-18 09:12:33"),
		mavenRow("maven-releases", "com/example/foo/1.17.12-rc1/foo-1.17.12-rc1.jar", "2026-10-17 09:12:33"),
		mavenRow("maven-releases", "com/example/foo/1.17.120/foo-1.17.120.jar", "2026-10-16 09:12:33"),
	}
	pkgs := BuildPackageGroups(rows, []string{"1.17.12"})
	if len(pkgs) != 1 {
		t.Fatalf("应聚合为 1 个包，得 %d", len(pkgs))
	}
	p := pkgs[0]
	if p.Coordinate != "com.example:foo" || p.Kind != PackageKindMaven {
		t.Fatalf("包身份 = %s/%s，期望 com.example:foo/maven", p.Kind, p.Coordinate)
	}
	if p.BestPrecision != PrecisionExact {
		t.Fatalf("包精度 = %q，期望 exact（组内最小值）", p.BestPrecision)
	}
	if p.FileCount != 5 || p.VersionCount != 3 {
		t.Fatalf("计数 = %d 文件 / %d 版本，期望 5 / 3", p.FileCount, p.VersionCount)
	}
	wantVersions := []struct{ version, precision string }{
		{"1.17.12", PrecisionExact},
		{"1.17.12-rc1", PrecisionPrerelease},
		{"1.17.120", PrecisionSubstring},
	}
	if len(p.Versions) != len(wantVersions) {
		t.Fatalf("版本组数 = %d，期望 %d", len(p.Versions), len(wantVersions))
	}
	for i, want := range wantVersions {
		if p.Versions[i].Version != want.version || p.Versions[i].Precision != want.precision {
			t.Fatalf("第 %d 个版本 = %s[%s]，期望 %s[%s]", i,
				p.Versions[i].Version, p.Versions[i].Precision, want.version, want.precision)
		}
	}
	if p.TopVersion != "1.17.12" {
		t.Fatalf("topVersion = %q，期望 1.17.12", p.TopVersion)
	}
	// 组内文件排序：主构件在前、校验和垫底。
	files := p.Versions[0].Files
	if len(files) != 3 {
		t.Fatalf("1.17.12 组文件数 = %d，期望 3", len(files))
	}
	if files[0].FileKind != "jar" || files[1].FileKind != "pom" || files[2].FileKind != "checksum" {
		t.Fatalf("文件排序 = %s,%s,%s，期望 jar,pom,checksum",
			files[0].FileKind, files[1].FileKind, files[2].FileKind)
	}
}

// TestBuildPackageGroupsMergesAcrossRepos 钉住「包身份键不含仓库」（规格 §3.3）：
// 同一坐标出现在两个仓库时合并为一个包节点，repositories 记录两个仓库。
func TestBuildPackageGroupsMergesAcrossRepos(t *testing.T) {
	t.Parallel()
	rows := []PackageSource{
		mavenRow("maven-releases", "com/example/foo/1.0.0/foo-1.0.0.jar", "2026-10-18 09:12:33"),
		mavenRow("maven-proxy", "com/example/foo/1.0.0/foo-1.0.0.jar", "2026-10-18 10:12:33"),
		mavenRow("maven-proxy", "com/example/foo/2.0.0/foo-2.0.0.jar", "2026-10-18 11:12:33"),
	}
	pkgs := BuildPackageGroups(rows, nil)
	if len(pkgs) != 1 {
		t.Fatalf("跨仓库同坐标应合并为 1 个包，得 %d", len(pkgs))
	}
	p := pkgs[0]
	if p.FileCount != 3 || p.VersionCount != 2 {
		t.Fatalf("计数 = %d 文件 / %d 版本，期望 3 / 2", p.FileCount, p.VersionCount)
	}
	if len(p.Repositories) != 2 || p.Repositories[0] != "maven-proxy" || p.Repositories[1] != "maven-releases" {
		t.Fatalf("repositories = %v，期望排序后的 [maven-proxy maven-releases]", p.Repositories)
	}
	// 同一版本同时存在于两个仓库 → 该版本组下出现两条文件行（靠文件行的 repository 消歧）。
	// 版本组顺序按版本向量降序：2.0.0 在前。
	if got := len(p.Versions[0].Files); got != 1 {
		t.Fatalf("2.0.0 组文件数 = %d，期望 1", got)
	}
	if got := len(p.Versions[1].Files); got != 2 {
		t.Fatalf("1.0.0 组文件数 = %d，期望 2（两个仓库各一份）", got)
	}
}

// TestBuildPackageGroupsDegraded 钉住裸路径的降级口径（规格 §3.2）：
// 每仓库一个「无坐标」伪包、跳过版本层、精度恒为 none。
func TestBuildPackageGroupsDegraded(t *testing.T) {
	t.Parallel()
	rows := []PackageSource{
		{RepoName: "raw-a", Format: "raw", Asset: repository.Asset{Path: "docs/a/b.txt", Size: 10, UpdatedAt: "2026-10-18 09:00:00"}},
		{RepoName: "raw-a", Format: "raw", Asset: repository.Asset{Path: "docs/a/c.txt", Size: 20, UpdatedAt: "2026-10-18 10:00:00"}},
		{RepoName: "raw-b", Format: "raw", Asset: repository.Asset{Path: "other.txt", Size: 30, UpdatedAt: "2026-10-18 11:00:00"}},
	}
	pkgs := BuildPackageGroups(rows, []string{"b.txt"})
	if len(pkgs) != 2 {
		t.Fatalf("每个仓库一个伪包，期望 2 个，得 %d", len(pkgs))
	}
	byCoord := map[string]SearchPackage{}
	for _, p := range pkgs {
		byCoord[p.Coordinate] = p
	}
	a, ok := byCoord["raw-a"]
	if !ok {
		t.Fatalf("缺少 raw-a 伪包：%+v", byCoord)
	}
	if a.Kind != PackageKindFile {
		t.Fatalf("伪包 kind = %q，期望 file", a.Kind)
	}
	if a.FileCount != 2 || len(a.Files) != 2 {
		t.Fatalf("raw-a 文件数 = %d/%d，期望 2/2（文件直挂包下）", a.FileCount, len(a.Files))
	}
	if a.VersionCount != 0 || len(a.Versions) != 0 {
		t.Fatalf("伪包应跳过版本层，得 versionCount=%d versions=%d", a.VersionCount, len(a.Versions))
	}
	if a.BestPrecision != PrecisionNone {
		t.Fatalf("伪包精度 = %q，期望 none", a.BestPrecision)
	}
	if a.LatestUpdatedAt != "2026-10-18 10:00:00" {
		t.Fatalf("伪包 latestUpdatedAt = %q，期望组内最大值", a.LatestUpdatedAt)
	}
}

// TestSortSearchPackages 钉住 §3.5 的包级比较器各键，含「版本向量是数值比较」这一关键点。
func TestSortSearchPackages(t *testing.T) {
	t.Parallel()
	mk := func(coord, precision, top, updated string) SearchPackage {
		return SearchPackage{Coordinate: coord, Kind: PackageKindMaven, BestPrecision: precision,
			TopVersion: top, LatestUpdatedAt: updated}
	}
	cases := []struct {
		name string
		pkgs []SearchPackage
		want []string
	}{
		{
			name: "第 1 键：精度升序",
			pkgs: []SearchPackage{
				mk("b:sub", PrecisionSubstring, "1.0.0", "2026-10-18 00:00:00"),
				mk("a:exact", PrecisionExact, "1.0.0", "2026-10-18 00:00:00"),
				mk("c:pre", PrecisionPrerelease, "1.0.0", "2026-10-18 00:00:00"),
				mk("d:none", PrecisionNone, "1.0.0", "2026-10-18 00:00:00"),
			},
			want: []string{"a:exact", "c:pre", "b:sub", "d:none"},
		},
		{
			name: "第 2 键：同精度下版本向量数值降序（1.10.0 先于 1.9.0）",
			pkgs: []SearchPackage{
				mk("old:lib", PrecisionExact, "1.9.0", "2026-10-18 00:00:00"),
				mk("new:lib", PrecisionExact, "1.10.0", "2026-10-18 00:00:00"),
			},
			want: []string{"new:lib", "old:lib"},
		},
		{
			name: "第 2 键在 rank=3 时不参与（退回第 3 键）",
			pkgs: []SearchPackage{
				mk("a:none-old", PrecisionNone, "", "2026-10-17 00:00:00"),
				mk("b:none-new", PrecisionNone, "", "2026-10-18 00:00:00"),
			},
			want: []string{"b:none-new", "a:none-old"},
		},
		{
			name: "第 3 键：更新时间降序",
			pkgs: []SearchPackage{
				mk("a:old", PrecisionNone, "", "2026-10-17 00:00:00"),
				mk("b:new", PrecisionNone, "", "2026-10-19 00:00:00"),
				mk("c:mid", PrecisionNone, "", "2026-10-18 00:00:00"),
			},
			want: []string{"b:new", "c:mid", "a:old"},
		},
		{
			name: "第 4 键：坐标升序兜底",
			pkgs: []SearchPackage{
				mk("b:same", PrecisionNone, "", "2026-10-18 00:00:00"),
				mk("a:same", PrecisionNone, "", "2026-10-18 00:00:00"),
			},
			want: []string{"a:same", "b:same"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			SortSearchPackages(tc.pkgs)
			got := make([]string, 0, len(tc.pkgs))
			for _, p := range tc.pkgs {
				got = append(got, p.Coordinate)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("结果数 = %d，期望 %d", len(got), len(tc.want))
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("顺序 = %v，期望 %v", got, tc.want)
				}
			}
		})
	}
}

// TestClassifyFile 覆盖规格附录 A 的 fileKind 归类与优先级。
func TestClassifyFile(t *testing.T) {
	t.Parallel()
	cases := []struct {
		format string
		path   string
		want   string
	}{
		{"maven", "com/example/foo/1.0.0/foo-1.0.0.jar", "jar"},
		{"maven", "com/example/foo/1.0.0/foo-1.0.0.war", "war"},
		{"maven", "com/example/foo/1.0.0/foo-1.0.0-aar.aar", "aar"},
		{"npm", "pkg-a/-/pkg-a-1.0.0.tgz", "tgz"},
		{"maven", "com/example/foo/1.0.0/foo-1.0.0-sources.jar", "sources"},
		{"maven", "com/example/foo/1.0.0/foo-1.0.0-javadoc.jar", "javadoc"},
		{"maven", "com/example/foo/1.0.0/foo-1.0.0-tests.jar", "tests"},
		{"maven", "com/example/foo/1.0.0/foo-1.0.0.pom", "pom"},
		{"npm", "pkg-a", "packument"},
		{"npm", "@scope/pkg", "packument"},
		{"maven", "com/example/foo/maven-metadata.xml", "metadata"},
		{"maven", "com/example/foo/1.0.0/foo-1.0.0.module", "metadata"},
		{"raw", "docs/a/b.txt", "other"},
		{"maven", "com/example/foo/1.0.0/foo-1.0.0.jar.sha1", "checksum"},
		{"maven", "com/example/foo/1.0.0/foo-1.0.0.jar.md5", "checksum"},
		{"maven", "com/example/foo/1.0.0/foo-1.0.0.jar.asc", "checksum"},
		{"maven", "com/example/foo/1.0.0/foo-1.0.0.jar.sha512", "checksum"},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			coords := ParseCoordinates(tc.format, tc.path)
			got, _ := classifyFile(coords, tc.path)
			if got != tc.want {
				t.Fatalf("classifyFile(%s) = %q，期望 %q", tc.path, got, tc.want)
			}
		})
	}
}

// mavenRow 构造一行 Maven 仓库的聚合输入。
func mavenRow(repo, path, updated string) PackageSource {
	return PackageSource{
		RepoName: repo,
		Format:   "maven",
		Asset:    repository.Asset{Path: path, Size: 1, Sha1: "s", UpdatedAt: updated},
	}
}
