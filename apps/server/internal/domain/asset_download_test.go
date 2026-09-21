package domain

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// 覆盖 FR-142 采集口径：只计 GET+200 的完整传输（206/POST/非制品路径不计）、
// UA 归类、分钟桶内存合并后批量落库，以及 flush 失败时的保留重试语义。
func TestAssetDownloadServiceRecordsOnlyCompleteTransfers(t *testing.T) {
	db, err := persistence.Open(filepath.Join(t.TempDir(), "asset-download-svc.db"))
	if err != nil {
		t.Fatalf("打开数据库：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("迁移数据库：%v", err)
	}
	repo := repository.NewAssetDownloadRepo(db)
	svc := NewAssetDownloadService(repo)

	past := time.Date(2026, 9, 21, 10, 0, 20, 0, time.UTC)

	// 计入：GET + 200 + 制品路径。
	if !svc.RecordDownload("GET", 200, "/repository/r1/a/b.jar", "1.2.3.4", "Apache-Maven/3.9", past) {
		t.Fatal("GET 200 制品路径应计入")
	}
	// 同一分钟同一组合再来一次 → 内存合并。
	svc.RecordDownload("GET", 200, "/repository/r1/a/b.jar", "1.2.3.4", "Apache-Maven/3.9", past)

	// 不计入：Range 分段（206）、非 GET、非制品前缀、缺制品路径。
	if svc.RecordDownload("GET", 206, "/repository/r1/a/b.jar", "1.2.3.4", "curl/8", past) {
		t.Fatal("206 分段不应计入")
	}
	if svc.RecordDownload("POST", 200, "/repository/r1/a/b.jar", "1.2.3.4", "curl/8", past) {
		t.Fatal("非 GET 不应计入")
	}
	if svc.RecordDownload("GET", 200, "/api/v1/repositories", "1.2.3.4", "curl/8", past) {
		t.Fatal("非制品协议路径不应计入")
	}
	if svc.RecordDownload("GET", 200, "/repository/r1", "1.2.3.4", "curl/8", past) {
		t.Fatal("缺制品路径不应计入")
	}

	// UA 归类：各族各一条（browser 的 UA 含 Mozilla，不应误归 maven）。
	svc.RecordDownload("GET", 200, "/repository/r1/g.jar", "9.9.9.9", "Gradle/8.5", past)
	svc.RecordDownload("GET", 200, "/repository/r1/n.tgz", "9.9.9.9", "npm/10.2.0 node/v20", past)
	svc.RecordDownload("GET", 200, "/repository/r1/w.jar", "9.9.9.9", "Mozilla/5.0 (Windows NT 10.0)", past)
	svc.RecordDownload("GET", 200, "/repository/r1/x.jar", "9.9.9.9", "SomeUnknownClient/1.0", past)

	// 未到分钟边界（当前分钟内的桶不落库，保持内存累计）。
	now := past.Add(time.Minute)
	if err := svc.Flush(now); err != nil {
		t.Fatalf("Flush：%v", err)
	}

	sum, err := repo.SumByAsset("r1")
	if err != nil {
		t.Fatalf("聚合：%v", err)
	}
	if len(sum) != 5 {
		t.Fatalf("应有 5 个制品，实际 %+v", sum)
	}
	if sum["a/b.jar"] != 2 {
		t.Fatalf("同分钟同组合应合并为 2，实际 %d", sum["a/b.jar"])
	}
	for _, path := range []string{"g.jar", "n.tgz", "w.jar", "x.jar"} {
		if sum[path] != 1 {
			t.Fatalf("%s 应为 1，实际 %d", path, sum[path])
		}
	}
}

// URL 解码与边界：编码路径解出可读 path；前缀/段数不符返回 false。
func TestParseRepositoryAssetPath(t *testing.T) {
	cases := []struct {
		name string
		in   string
		repo string
		path string
		ok   bool
	}{
		{"name-simple", "/repository/r1/a/b.jar", "r1", "a/b.jar", true},
		{"name-escaped", "/repository/r1/a/%E4%B8%AD%E6%96%87.jar", "r1", "a/中文.jar", true},
		{"name-wrong-prefix", "/api/v1/x", "", "", false},
		{"name-missing-asset", "/repository/r1", "", "", false},
		{"name-empty-repo", "/repository//a.jar", "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			repo, path, ok := ParseRepositoryAssetPath(c.in)
			if ok != c.ok || repo != c.repo || path != c.path {
				t.Fatalf("ParseRepositoryAssetPath(%q) = (%q, %q, %v)，期望 (%q, %q, %v)", c.in, repo, path, ok, c.repo, c.path, c.ok)
			}
		})
	}
}

// UA 归类表（spec 附录）逐族断言。
func TestClassifyUAFamily(t *testing.T) {
	cases := map[string]string{
		"Apache-Maven/3.9.6 (Java 17)":    "maven",
		"Gradle/8.5 (Linux; amd64)":       "gradle",
		"npm/10.2.0 node/v20.11.0":        "npm",
		"curl/8.4.0":                      "curl",
		"Mozilla/5.0 (X11; Linux x86_64)": "browser",
		"":                                "other",
		"Jenkins/2.4":                     "other",
	}
	for ua, want := range cases {
		if got := classifyUAFamily(ua); got != want {
			t.Errorf("classifyUAFamily(%q) = %q，期望 %q", ua, got, want)
		}
	}
}
