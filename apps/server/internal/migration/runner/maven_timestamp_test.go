package runner_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// TestRunnerOnlineMavenPreservesSourceTimestamps 是 maven 格式的端到端时间戳回归：
// 真实 sqlite + 假 Nexus 源，断言 maven 资产行的 created_at/updated_at 等于
// 源端 lastModified 转 UTC 的 "YYYY-MM-DD HH:MM:SS"，而不是迁移执行时刻。
// 只测 default 分支的桩测试无法覆盖 maven 这条真实路径。
func TestRunnerOnlineMavenPreservesSourceTimestamps(t *testing.T) {
	const sourceLastModified = "2021-10-19T02:14:21.187+00:00"
	const wantTimestamp = "2021-10-19 02:14:21"

	mux := http.NewServeMux()
	mux.HandleFunc("/service/rest/v1/assets", func(w http.ResponseWriter, r *http.Request) {
		host := "http://" + r.Host
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]string{
			{
				"path":         "com/example/lib/1.0/lib-1.0.jar",
				"downloadUrl":  host + "/dl/lib.jar",
				"contentType":  "application/java-archive",
				"lastModified": sourceLastModified,
			},
			{
				"path":         "com/example/lib/1.0/lib-1.0.pom",
				"downloadUrl":  host + "/dl/lib.pom",
				"contentType":  "application/xml",
				"lastModified": sourceLastModified,
			},
		}})
	})
	mux.HandleFunc("/dl/", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("maven-bytes"))
	})
	source := httptest.NewServer(mux)
	t.Cleanup(source.Close)

	mig, assets, repos, _, _ := setup(t)
	if _, err := repos.Create("maven-releases", "maven", "hosted", "private", "", repository.RepositoryConfig{}); err != nil {
		t.Fatalf("创建 maven 目标仓库：%v", err)
	}

	created, err := mig.Create(domain.MigrationCreateInput{
		SourceType:   repository.MigrationSourceOnlineREST,
		SourceConfig: map[string]any{"url": source.URL, "allowPrivateSource": true},
		PlanJSON:     `{"repositories":[{"name":"maven-releases","format":"maven"}]}`,
	})
	if err != nil {
		t.Fatalf("Create：%v", err)
	}
	if _, err := mig.Start(created.ID, nil); err != nil {
		t.Fatalf("Start：%v", err)
	}
	task := waitStatus(t, mig, created.ID, repository.MigrationStatusCompleted)

	report := struct {
		Copied  int `json:"copied"`
		Skipped int `json:"skipped"`
		Failed  int `json:"failed"`
	}{}
	if err := json.Unmarshal([]byte(task.ReportJSON), &report); err != nil {
		t.Fatalf("解析 report：%v", err)
	}
	if report.Copied != 2 || report.Failed != 0 {
		t.Fatalf("maven 资产应全部复制成功：report=%+v", report)
	}

	for _, path := range []string{
		"com/example/lib/1.0/lib-1.0.jar",
		"com/example/lib/1.0/lib-1.0.pom",
	} {
		asset, rc, err := assets.Get("maven-releases", path)
		if err != nil {
			t.Fatalf("读取 %s：%v", path, err)
		}
		_ = rc.Close()
		if asset.CreatedAt != wantTimestamp {
			t.Errorf("%s created_at = %q，期望源端时间 %q", path, asset.CreatedAt, wantTimestamp)
		}
		if asset.UpdatedAt != wantTimestamp {
			t.Errorf("%s updated_at = %q，期望源端时间 %q", path, asset.UpdatedAt, wantTimestamp)
		}
	}
}
