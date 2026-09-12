package runner_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/formats"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

type synchronizedLogBuffer struct {
	mu   sync.Mutex
	data bytes.Buffer
}

func (b *synchronizedLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.data.Write(p)
}

func (b *synchronizedLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.data.String()
}

func TestRunnerOnlineRESTWithIncludeFilter(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/service/rest/v1/repositories", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]string{
			{"name": "raw-small", "format": "raw", "type": "hosted"},
			{"name": "huge-repo", "format": "maven2", "type": "hosted"},
		})
	})
	mux.HandleFunc("/service/rest/v1/assets", func(w http.ResponseWriter, r *http.Request) {
		repo := r.URL.Query().Get("repository")
		// downloadUrl 指向本测试服务器
		host := "http://" + r.Host
		items := []map[string]string{}
		switch repo {
		case "raw-small":
			items = []map[string]string{
				{"path": "a.bin", "downloadUrl": host + "/dl/a.bin", "contentType": "application/octet-stream"},
			}
		case "huge-repo":
			items = []map[string]string{
				{"path": "big.jar", "downloadUrl": host + "/dl/big.jar", "contentType": "application/java-archive"},
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": items, "continuationToken": ""})
	})
	mux.HandleFunc("/dl/", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/dl/a.bin":
			_, _ = w.Write([]byte("payload-a"))
		case "/dl/big.jar":
			_, _ = w.Write([]byte("should-not-download"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	t.Setenv("JIAN_MIGRATION_SOURCE_NEXUS_TEST", srv.URL)

	mig, assets, _, _, _ := setup(t)
	result, err := mig.Create(domain.MigrationCreateInput{
		SourceType: repository.MigrationSourceOnlineREST,
		SourceConfig: map[string]any{
			"sourceRef":           "NEXUS_TEST",
			"includeRepositories": []any{"raw-small"},
		},
		PlanJSON: `{"repositories":[{"name":"raw-small","format":"raw"},{"name":"huge-repo","format":"maven"}]}`,
	})
	if err != nil {
		t.Fatalf("Create：%v", err)
	}

	if _, err := mig.Start(result.ID, nil); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		task, _ := mig.Get(result.ID)
		if task.Status == repository.MigrationStatusCompleted {
			break
		}
		if task.Status == repository.MigrationStatusFailed {
			t.Fatalf("失败：%s", task.ErrorMessage.String)
		}
		time.Sleep(20 * time.Millisecond)
	}
	task, _ := mig.Get(result.ID)
	if task.Status != repository.MigrationStatusCompleted {
		t.Fatalf("status=%s", task.Status)
	}

	_, rc, err := assets.Get("raw-small", "a.bin")
	if err != nil {
		t.Fatalf("Get：%v", err)
	}
	body, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(body) != "payload-a" {
		t.Fatalf("内容 = %q", body)
	}
	// huge-repo 不应被创建
	if _, _, err := assets.Get("huge-repo", "big.jar"); err == nil {
		t.Fatal("不应迁移 huge-repo")
	}
}

func TestRunnerOnlineRESTSkipsPyPIMetadataSidecar(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/service/rest/v1/assets", func(w http.ResponseWriter, r *http.Request) {
		host := "http://" + r.Host
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]string{
			{"path": "packages/demo/demo-1.0.0-py3-none-any.whl", "downloadUrl": host + "/wheel"},
			{"path": "packages/demo/demo-1.0.0-py3-none-any.whl.metadata", "downloadUrl": host + "/sidecar"},
		}})
	})
	mux.HandleFunc("/wheel", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("wheel")) })
	mux.HandleFunc("/sidecar", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("metadata")) })
	source := httptest.NewServer(mux)
	t.Cleanup(source.Close)
	t.Setenv("JIAN_MIGRATION_SOURCE_NEXUS_PYPI_SIDECAR", source.URL)

	mig, assets, repos, _, _ := setup(t)
	repos.SetEnabledFormats(formats.New(formats.Known...))
	created, err := mig.Create(domain.MigrationCreateInput{
		SourceType:   repository.MigrationSourceOnlineREST,
		SourceConfig: map[string]any{"sourceRef": "NEXUS_PYPI_SIDECAR"},
		PlanJSON:     `{"repositories":[{"name":"pypi-source","format":"pypi","type":"hosted"}]}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mig.Start(created.ID, nil); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, mig, created.ID, repository.MigrationStatusCompleted)
	if _, body, err := assets.Get("pypi-source", "pypi/packages/demo/demo-1.0.0-py3-none-any.whl"); err != nil {
		t.Fatalf("wheel 未迁入：%v", err)
	} else if err := body.Close(); err != nil {
		t.Fatalf("关闭 wheel 读取流：%v", err)
	}
	if _, _, err := assets.Get("pypi-source", "pypi/packages/demo/demo-1.0.0-py3-none-any.whl.metadata"); err == nil {
		t.Fatal("PyPI metadata 旁车文件不得作为制品迁入")
	}
}

func TestRunnerOnlineRESTUsesNuGetMetadataWhenNexusPathHasNoFilename(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/service/rest/v1/assets", func(w http.ResponseWriter, r *http.Request) {
		host := "http://" + r.Host
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]string{{
			"path": "fr31.nexus.fixture/1.0.0", "downloadUrl": host + "/package",
		}}})
	})
	mux.HandleFunc("/package", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(testNuGetPackage(t, "Fr31.Nexus.Fixture", "1.0.0"))
	})
	source := httptest.NewServer(mux)
	t.Cleanup(source.Close)
	t.Setenv("JIAN_MIGRATION_SOURCE_NEXUS_NUGET_PATH", source.URL)

	mig, assets, repos, _, _ := setup(t)
	repos.SetEnabledFormats(formats.New(formats.Known...))
	created, err := mig.Create(domain.MigrationCreateInput{
		SourceType:   repository.MigrationSourceOnlineREST,
		SourceConfig: map[string]any{"sourceRef": "NEXUS_NUGET_PATH"},
		PlanJSON:     `{"repositories":[{"name":"nuget-source","format":"nuget","type":"hosted"}]}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mig.Start(created.ID, nil); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, mig, created.ID, repository.MigrationStatusCompleted)
	if _, body, err := assets.Get("nuget-source", "nuget/fr31.nexus.fixture/1.0.0/Fr31.Nexus.Fixture.1.0.0.nupkg"); err != nil {
		t.Fatalf("Nexus NuGet 包未迁入：%v", err)
	} else if err := body.Close(); err != nil {
		t.Fatalf("关闭 NuGet 读取流：%v", err)
	}
}

func TestRunnerOnlineRESTSkipsCargoConfigAsset(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/service/rest/v1/assets", func(w http.ResponseWriter, r *http.Request) {
		host := "http://" + r.Host
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]string{{
			"path": "config.json", "downloadUrl": host + "/config.json",
		}}})
	})
	source := httptest.NewServer(mux)
	t.Cleanup(source.Close)
	t.Setenv("JIAN_MIGRATION_SOURCE_NEXUS_CARGO_CONFIG", source.URL)

	mig, _, repos, _, _ := setup(t)
	repos.SetEnabledFormats(formats.New(formats.Known...))
	created, err := mig.Create(domain.MigrationCreateInput{
		SourceType:   repository.MigrationSourceOnlineREST,
		SourceConfig: map[string]any{"sourceRef": "NEXUS_CARGO_CONFIG"},
		PlanJSON:     `{"repositories":[{"name":"cargo-source","format":"cargo","type":"hosted"}]}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mig.Start(created.ID, nil); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, mig, created.ID, repository.MigrationStatusCompleted)
}

func TestRunnerDirectBasicAuthSupportsStartResumeAndFinalize(t *testing.T) {
	const username = "nexus-user-token-name"
	const password = "nexus-user-token-passcode"
	var authenticated atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/service/rest/v1/assets", func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != username || pass != password {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		authenticated.Add(1)
		host := "http://" + r.Host
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]string{{
			"path": "a.bin", "downloadUrl": host + "/download",
		}}, "continuationToken": ""})
	})
	mux.HandleFunc("/download", func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != username || pass != password {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		authenticated.Add(1)
		_, _ = w.Write([]byte("direct-auth-payload"))
	})
	source := httptest.NewServer(mux)
	t.Cleanup(source.Close)

	mig, assets, _, tasks, _ := setup(t)
	created, err := mig.Create(domain.MigrationCreateInput{
		SourceType:   repository.MigrationSourceOnlineREST,
		SourceConfig: map[string]any{"url": source.URL},
		SourceAuth: &domain.SourceAuth{
			Type: "basic", Username: username, Password: password,
		},
		PlanJSON: `{"repositories":[{"name":"raw","format":"raw"}]}`,
	})
	if err != nil {
		t.Fatalf("Create：%v", err)
	}
	if strings.Contains(created.SourceConfig, password) || strings.Contains(string(created.SourceAuthCiphertext), password) {
		t.Fatal("任务持久化字段不得含认证明文")
	}
	if _, err := mig.Start(created.ID, nil); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, mig, created.ID, repository.MigrationStatusCompleted)
	if _, rc, err := assets.Get("raw", "a.bin"); err != nil {
		t.Fatalf("首次迁移资产：%v", err)
	} else {
		_ = rc.Close()
	}
	if _, err := mig.Finalize(context.Background(), created.ID); err != nil {
		t.Fatalf("Finalize：%v", err)
	}
	failed := "测试恢复"
	if err := tasks.UpdateStatus(created.ID, repository.MigrationStatusFailed, &failed, false, true); err != nil {
		t.Fatal(err)
	}
	if _, err := mig.Resume(created.ID); err != nil {
		t.Fatalf("Resume：%v", err)
	}
	waitStatus(t, mig, created.ID, repository.MigrationStatusCompleted)
	if authenticated.Load() < 4 {
		t.Fatalf("start/finalize/resume 均应使用 Basic 认证，实际请求 %d", authenticated.Load())
	}
}

func TestRunnerOnlineRESTAllowsPrivateSourceWhenFlagged(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/service/rest/v1/assets", func(w http.ResponseWriter, r *http.Request) {
		host := "http://" + r.Host
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]string{
			{"path": "a.bin", "downloadUrl": host + "/dl/a.bin", "contentType": "application/octet-stream"},
		}})
	})
	mux.HandleFunc("/dl/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("payload-a"))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	// setupSecure 默认使用安全出站客户端（拒绝私网）；来源显式声明 allowPrivateSource 后应放行。
	mig, assets, _, _, _ := setupSecure(t)
	created, err := mig.Create(domain.MigrationCreateInput{
		SourceType:   repository.MigrationSourceOnlineREST,
		SourceConfig: map[string]any{"url": srv.URL, "allowPrivateSource": true},
		PlanJSON:     `{"repositories":[{"name":"raw","format":"raw"}]}`,
	})
	if err != nil {
		t.Fatalf("Create：%v", err)
	}
	if _, err := mig.Start(created.ID, nil); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, mig, created.ID, repository.MigrationStatusCompleted)
	if _, rc, err := assets.Get("raw", "a.bin"); err != nil {
		t.Fatalf("私网来源资产未迁入：%v", err)
	} else {
		_ = rc.Close()
	}
}

func TestRunnerOnlineRESTRejectsPrivateSourceByDefault(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/service/rest/v1/assets", func(w http.ResponseWriter, r *http.Request) {})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	mig, _, _, _, _ := setupSecure(t)
	created, err := mig.Create(domain.MigrationCreateInput{
		SourceType:   repository.MigrationSourceOnlineREST,
		SourceConfig: map[string]any{"url": srv.URL},
		PlanJSON:     `{"repositories":[{"name":"raw","format":"raw"}]}`,
	})
	if err != nil {
		t.Fatalf("Create：%v", err)
	}
	if _, err := mig.Start(created.ID, nil); err != nil {
		t.Fatal(err)
	}
	task := waitStatus(t, mig, created.ID, repository.MigrationStatusFailed)
	if !strings.Contains(task.ErrorMessage.String, "出站安全策略拒绝") {
		t.Fatalf("默认应拒绝私网来源，error=%q", task.ErrorMessage.String)
	}
}

func TestRunnerDropsLegacyInlineTokenBeforeExecutingURL(t *testing.T) {
	mig, _, _, tasks, _ := setup(t)
	const secret = "旧任务密钥不得执行或泄露"
	id, err := tasks.Create(repository.MigrationTaskCreate{
		Status:     repository.MigrationStatusPlanned,
		SourceType: repository.MigrationSourceOnlineREST,
		SourceConfig: `{"url":"http://127.0.0.1:1","token":"` + secret + `",` +
			`"includeRepositories":["raw"]}`,
		ConflictPolicy: repository.MigrationConflictSkip,
		PlanJSON:       `{"repositories":[{"name":"raw","format":"raw"}]}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mig.Start(id, nil); err != nil {
		t.Fatal(err)
	}
	task := waitStatus(t, mig, id, repository.MigrationStatusFailed)
	if !strings.Contains(task.ErrorMessage.String, "Nexus 来源网络连接失败") {
		t.Fatalf("遗留来源错误 = %q", task.ErrorMessage.String)
	}
	if strings.Contains(task.ErrorMessage.String, secret) || strings.Contains(task.ReportJSON, secret) {
		t.Fatalf("遗留密钥不得进入错误或报告：error=%q report=%q", task.ErrorMessage.String, task.ReportJSON)
	}
}

func TestRunnerResumeRejectsUnsafeSourceRefWithoutCredentialLeak(t *testing.T) {
	mig, _, _, tasks, _ := setupSecure(t)
	const ref = "NEXUS_UNSAFE"
	t.Setenv("JIAN_MIGRATION_SOURCE_"+ref, "http://127.0.0.1:8081")
	t.Setenv("JIAN_UPSTREAM_CREDENTIAL_MIGRATION_TEST", "迁移凭据不得泄露")

	created, err := mig.Create(domain.MigrationCreateInput{
		SourceType:    repository.MigrationSourceOnlineREST,
		SourceConfig:  map[string]any{"sourceRef": ref},
		CredentialRef: "MIGRATION_TEST",
		PlanJSON:      `{"repositories":[{"name":"raw","format":"raw"}]}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	failed := "中断后待恢复"
	if err := tasks.UpdateStatus(created.ID, repository.MigrationStatusFailed, &failed, false, true); err != nil {
		t.Fatal(err)
	}
	if _, err := mig.Resume(created.ID); err != nil {
		t.Fatal(err)
	}
	task := waitStatus(t, mig, created.ID, repository.MigrationStatusFailed)
	if !strings.Contains(task.ErrorMessage.String, "出站安全") {
		t.Fatalf("恢复路径应由出站安全策略拒绝，得 %q", task.ErrorMessage.String)
	}
	if strings.Contains(task.ErrorMessage.String, "迁移凭据不得泄露") || strings.Contains(task.ErrorMessage.String, "127.0.0.1") || strings.Contains(task.ReportJSON, "迁移凭据不得泄露") {
		t.Fatalf("任务错误和报告不得泄露凭据或来源地址：error=%q report=%q", task.ErrorMessage.String, task.ReportJSON)
	}
}

func TestRunnerRejectsDangerousDownloadRedirectWithoutCredentialLeak(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/service/rest/v1/assets", func(w http.ResponseWriter, r *http.Request) {
		host := "http://" + r.Host
		_ = json.NewEncoder(w).Encode(map[string]any{
			"items": []map[string]string{{
				"path":        "a.bin",
				"downloadUrl": host + "/download",
			}},
		})
	})
	mux.HandleFunc("/download", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "ftp://127.0.0.1/artifact", http.StatusFound)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	mig, _, _, _, _ := setup(t)
	t.Setenv("JIAN_MIGRATION_SOURCE_NEXUS_DOWNLOAD_TEST", srv.URL)
	t.Setenv("JIAN_UPSTREAM_CREDENTIAL_MIGRATION_DOWNLOAD", "下载凭据不得泄露")
	created, err := mig.Create(domain.MigrationCreateInput{
		SourceType:    repository.MigrationSourceOnlineREST,
		SourceConfig:  map[string]any{"sourceRef": "NEXUS_DOWNLOAD_TEST"},
		CredentialRef: "MIGRATION_DOWNLOAD",
		PlanJSON:      `{"repositories":[{"name":"raw","format":"raw"}]}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mig.Start(created.ID, nil); err != nil {
		t.Fatal(err)
	}
	task := waitStatus(t, mig, created.ID, repository.MigrationStatusFailed)
	if !strings.Contains(task.ErrorMessage.String, "出站安全") {
		t.Fatalf("下载重定向应由出站安全策略拒绝，得 %q", task.ErrorMessage.String)
	}
	if strings.Contains(task.ErrorMessage.String, "下载凭据不得泄露") || strings.Contains(task.ReportJSON, "下载凭据不得泄露") {
		t.Fatalf("任务错误和报告不得泄露下载凭据：error=%q report=%q", task.ErrorMessage.String, task.ReportJSON)
	}
}

func TestRunnerRedactsConnectionFailureFromTaskReportAndLog(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	sourceURL := srv.URL
	srv.Close()

	oldOutput := log.Writer()
	var logs synchronizedLogBuffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(oldOutput) })

	mig, _, _, _, _ := setup(t)
	t.Setenv("JIAN_MIGRATION_SOURCE_NEXUS_CONNECTION_FAILURE", sourceURL)
	created, err := mig.Create(domain.MigrationCreateInput{
		SourceType:   repository.MigrationSourceOnlineREST,
		SourceConfig: map[string]any{"sourceRef": "NEXUS_CONNECTION_FAILURE"},
		PlanJSON:     `{"repositories":[{"name":"raw","format":"raw"}]}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mig.Start(created.ID, nil); err != nil {
		t.Fatal(err)
	}
	task := waitStatus(t, mig, created.ID, repository.MigrationStatusFailed)
	if !strings.Contains(task.ErrorMessage.String, "Nexus 来源网络连接失败") {
		t.Fatalf("错误分类 = %q", task.ErrorMessage.String)
	}
	for _, value := range []string{task.ErrorMessage.String, task.ReportJSON, logs.String()} {
		if strings.Contains(value, sourceURL) || strings.Contains(value, "127.0.0.1") {
			t.Fatalf("任务错误、报告和日志不得泄露来源地址：%q", value)
		}
	}
}

func TestRunnerRejectsCrossOriginDownloadURLBeforeSendingCredential(t *testing.T) {
	var attackRequests atomic.Int32
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attackRequests.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(attacker.Close)

	sourceMux := http.NewServeMux()
	sourceMux.HandleFunc("/service/rest/v1/assets", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"items": []map[string]string{{
				"path":        "a.bin",
				"downloadUrl": attacker.URL + "/artifact",
			}},
		})
	})
	source := httptest.NewServer(sourceMux)
	t.Cleanup(source.Close)

	mig, _, _, _, _ := setup(t)
	t.Setenv("JIAN_MIGRATION_SOURCE_NEXUS_CROSS_ORIGIN", source.URL)
	t.Setenv("JIAN_UPSTREAM_CREDENTIAL_MIGRATION_CROSS_ORIGIN", "publisher:secret")
	created, err := mig.Create(domain.MigrationCreateInput{
		SourceType:    repository.MigrationSourceOnlineREST,
		SourceConfig:  map[string]any{"sourceRef": "NEXUS_CROSS_ORIGIN"},
		CredentialRef: "MIGRATION_CROSS_ORIGIN",
		PlanJSON:      `{"repositories":[{"name":"raw","format":"raw"}]}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mig.Start(created.ID, nil); err != nil {
		t.Fatal(err)
	}
	task := waitStatus(t, mig, created.ID, repository.MigrationStatusFailed)
	if !strings.Contains(task.ErrorMessage.String, "出站安全策略拒绝") {
		t.Fatalf("跨站下载错误 = %q", task.ErrorMessage.String)
	}
	if got := attackRequests.Load(); got != 0 {
		t.Fatalf("跨站下载地址不应收到请求，实际 %d 次", got)
	}
}
