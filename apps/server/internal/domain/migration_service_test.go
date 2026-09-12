package domain_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/migration/credential"
	"github.com/wcpe/jianartifact/apps/server/internal/migration/discover"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
	"github.com/wcpe/jianartifact/apps/server/internal/upstream"
)

func TestMigrationServiceCreatePlannedAndStart(t *testing.T) {
	db := newTestDB(t)
	repo := repository.NewMigrationTaskRepo(db)
	svc := domain.NewMigrationService(repo, nil)

	task, err := svc.Create(domain.MigrationCreateInput{
		SourceType:     repository.MigrationSourceOfflineDir,
		SourceConfig:   map[string]any{"path": "/data/nexus"},
		ConflictPolicy: repository.MigrationConflictSkip,
		PlanJSON:       `{"repositories":[]}`,
	})
	if err != nil {
		t.Fatalf("Create：%v", err)
	}
	if task.Status != repository.MigrationStatusPlanned {
		t.Fatalf("创建后 status = %q，期望 planned", task.Status)
	}

	started, err := svc.Start(task.ID, nil)
	if err != nil {
		t.Fatalf("Start：%v", err)
	}
	if started.Status != repository.MigrationStatusRunning {
		t.Fatalf("start 后 status = %q", started.Status)
	}

	// 再次 start → 409 语义 ErrConflict
	if _, err := svc.Start(task.ID, nil); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("二次 start err = %v，期望 ErrConflict", err)
	}
}

func TestMigrationServicePersistsInitiatorForCreateAndDiscover(t *testing.T) {
	db := newTestDB(t)
	svc := domain.NewMigrationService(repository.NewMigrationTaskRepo(db), nil)
	initiator := domain.MigrationInitiator{Username: "migration-admin", UserID: 42, AuthSource: "web_jwt"}

	created, err := svc.Create(domain.MigrationCreateInput{
		SourceType:   repository.MigrationSourceOfflineBundle,
		SourceConfig: map[string]any{"path": t.TempDir()},
		Initiator:    initiator,
	})
	if err != nil {
		t.Fatalf("Create：%v", err)
	}
	assertMigrationInitiator(t, created, initiator)

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "manifest.json"), []byte(`{"repositories":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	discovered, err := svc.Discover(context.Background(), domain.MigrationDiscoverInput{
		SourceType:   repository.MigrationSourceOfflineBundle,
		SourceConfig: map[string]any{"path": root},
		Initiator:    initiator,
	})
	if err != nil {
		t.Fatalf("Discover：%v", err)
	}
	assertMigrationInitiator(t, discovered.Task, initiator)
}

func assertMigrationInitiator(t *testing.T, task *repository.MigrationTask, want domain.MigrationInitiator) {
	t.Helper()
	if task.InitiatorUsername != want.Username || !task.InitiatorUserID.Valid || task.InitiatorUserID.Int64 != want.UserID || task.InitiatorAuthSource != want.AuthSource {
		t.Fatalf("任务发起人 = %+v，期望 %+v", task, want)
	}
}

func TestMigrationServiceCredentialRef(t *testing.T) {
	db := newTestDB(t)
	svc := domain.NewMigrationService(repository.NewMigrationTaskRepo(db), nil)

	// 未知引用
	_, err := svc.Create(domain.MigrationCreateInput{
		SourceType:    repository.MigrationSourceOnlineREST,
		SourceConfig:  map[string]any{"url": "http://n"},
		CredentialRef: "MISSING_CREDENTIAL",
	})
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("未知 credentialRef err = %v，期望 ErrValidation", err)
	}

	t.Setenv("JIAN_UPSTREAM_CREDENTIAL_NEXUS_PRIMARY", "user:pass")
	task, err := svc.Create(domain.MigrationCreateInput{
		SourceType:    repository.MigrationSourceOnlineREST,
		SourceConfig:  map[string]any{"sourceRef": "NEXUS_PRIMARY"},
		CredentialRef: "NEXUS_PRIMARY",
	})
	if err != nil {
		t.Fatalf("Create with ref：%v", err)
	}
	if !task.CredentialRef.Valid || task.CredentialRef.String != "NEXUS_PRIMARY" {
		t.Errorf("credential_ref = %+v", task.CredentialRef)
	}
	// 库中不应出现明文
	if task.SourceConfig == "user:pass" || task.PlanJSON == "user:pass" {
		t.Fatal("明文密钥出现在持久化字段")
	}
}

func TestMigrationServicePersistsDirectSourceAuthEncrypted(t *testing.T) {
	db := newTestDB(t)
	svc := domain.NewMigrationService(repository.NewMigrationTaskRepo(db), nil)
	sealer, err := credential.NewSealer([]byte("01234567890123456789012345678901"))
	if err != nil {
		t.Fatal(err)
	}
	svc.SetCredentialSealer(sealer)
	const password = "nexus-user-token-passcode"
	task, err := svc.Create(domain.MigrationCreateInput{
		SourceType:   repository.MigrationSourceOnlineREST,
		SourceConfig: map[string]any{"url": "https://nexus.example/", "sourceAuth": password},
		SourceAuth: &domain.SourceAuth{
			Type:     credential.TypeBasic,
			Username: "nexus-user-token-name",
			Password: password,
		},
	})
	if err != nil {
		t.Fatalf("Create direct auth：%v", err)
	}
	assertSourceConfig(t, task.SourceConfig, map[string]any{"url": "https://nexus.example"})
	if !task.SourceAuthType.Valid || task.SourceAuthType.String != credential.TypeBasic {
		t.Fatalf("sourceAuthType = %+v", task.SourceAuthType)
	}
	if len(task.SourceAuthCiphertext) == 0 || strings.Contains(string(task.SourceAuthCiphertext), password) {
		t.Fatal("直接认证应仅以不含明文的密文持久化")
	}
	if strings.Contains(task.SourceConfig, password) || strings.Contains(task.PlanJSON, password) {
		t.Fatal("认证材料不得进入任务安全字段")
	}
	if _, err := sealer.Open(task.SourceAuthCiphertext, domain.MigrationCredentialAAD(task)); err != nil {
		t.Fatalf("任务密文不可恢复：%v", err)
	}
}

func TestMigrationServiceDiscoverPersistsDirectBearerAuthOnlyAfterSuccess(t *testing.T) {
	const token = "discover-bearer-token"
	var requests atomic.Int32
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if got, want := r.Header.Get("Authorization"), "Bearer "+token; got != want {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`[{"name":"maven-releases","format":"maven2","type":"hosted"}]`))
	}))
	t.Cleanup(source.Close)

	db := newTestDB(t)
	svc := domain.NewMigrationService(repository.NewMigrationTaskRepo(db), nil, func(string) (discover.Source, error) {
		return discover.NewOnlineREST(upstream.NewTestClient(time.Second)), nil
	})
	svc.SetCredentialSealer(mustMigrationCredentialSealer(t))
	result, err := svc.Discover(context.Background(), domain.MigrationDiscoverInput{
		SourceType:   repository.MigrationSourceOnlineREST,
		SourceConfig: map[string]any{"url": source.URL},
		SourceAuth:   &domain.SourceAuth{Type: credential.TypeBearer, Token: token},
	})
	if err != nil {
		t.Fatalf("Discover：%v", err)
	}
	if requests.Load() != 1 || len(result.Plan.Repositories) != 1 {
		t.Fatalf("发现请求=%d，plan=%#v", requests.Load(), result.Plan)
	}
	if result.Task.SourceAuthType.String != credential.TypeBearer || len(result.Task.SourceAuthCiphertext) == 0 {
		t.Fatalf("发现任务未保存直接认证密文：%+v", result.Task)
	}
	if strings.Contains(result.Task.SourceConfig, token) || strings.Contains(result.Task.PlanJSON, token) {
		t.Fatal("发现后的任务安全字段不得含 Bearer 明文")
	}
}

func TestMigrationServiceRejectsDirectAuthWithCredentialRef(t *testing.T) {
	db := newTestDB(t)
	svc := domain.NewMigrationService(repository.NewMigrationTaskRepo(db), nil)
	svc.SetCredentialSealer(mustMigrationCredentialSealer(t))
	t.Setenv("JIAN_UPSTREAM_CREDENTIAL_NEXUS_PRIMARY", "reader:password")
	_, err := svc.Create(domain.MigrationCreateInput{
		SourceType:    repository.MigrationSourceOnlineREST,
		SourceConfig:  map[string]any{"url": "https://nexus.example"},
		CredentialRef: "NEXUS_PRIMARY",
		SourceAuth:    &domain.SourceAuth{Type: credential.TypeBearer, Token: "token"},
	})
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("直接认证与 credentialRef 并存应拒绝：%v", err)
	}
}

func mustMigrationCredentialSealer(t *testing.T) *credential.Sealer {
	t.Helper()
	sealer, err := credential.NewSealer([]byte("01234567890123456789012345678901"))
	if err != nil {
		t.Fatal(err)
	}
	return sealer
}

func TestMigrationServiceListRemoteRepositoriesUsesSourceRef(t *testing.T) {
	source := newNexusRepositoryServer(t, "reader", "password")
	t.Setenv("JIAN_MIGRATION_SOURCE_NEXUS_TEST", source.URL)
	t.Setenv("JIAN_UPSTREAM_CREDENTIAL_NEXUS_READ", "reader:password")

	svc := newOnlineMigrationService(t)
	items, err := svc.ListRemoteRepositories(context.Background(), "NEXUS_TEST", "NEXUS_READ")
	if err != nil {
		t.Fatalf("按 sourceRef 枚举仓库：%v", err)
	}
	if len(items) != 1 || items[0].Name != "raw-hosted" {
		t.Fatalf("仓库索引 = %#v", items)
	}
}

func TestMigrationServiceListRemoteRepositoriesUsesDirectBearerAuth(t *testing.T) {
	const token = "jwt:contains-colon"
	var requests atomic.Int32
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if got, want := r.Header.Get("Authorization"), "Bearer "+token; got != want {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`[{"name":"maven-releases","format":"maven2","type":"hosted"}]`))
	}))
	t.Cleanup(source.Close)

	svc := newOnlineMigrationService(t)
	items, err := svc.ListRemoteRepositoriesWithSource(context.Background(), map[string]any{"url": source.URL}, "", &domain.SourceAuth{
		Type: credential.TypeBearer, Token: token,
	})
	if err != nil {
		t.Fatalf("直接 URL 远程索引：%v", err)
	}
	if len(items) != 1 || items[0].Name != "maven-releases" || requests.Load() != 1 {
		t.Fatalf("仓库索引 = %#v，requests=%d", items, requests.Load())
	}
}

func TestMigrationServiceListRemoteRepositoriesRejectsRawURLWithoutRequest(t *testing.T) {
	factoryCalls := 0
	svc := domain.NewMigrationService(nil, nil, func(string) (discover.Source, error) {
		factoryCalls++
		return discover.NewOnlineREST(upstream.NewTestClient(time.Second)), nil
	})

	_, err := svc.ListRemoteRepositories(context.Background(), "https://public.example.test", "")
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("原始 URL 应被拒绝，得 %v", err)
	}
	if factoryCalls != 0 {
		t.Fatalf("原始 URL 被拒绝前不得创建来源客户端，实际 %d 次", factoryCalls)
	}
}

func TestMigrationServiceListRemoteRepositoriesDoesNotReadOtherEnvironmentSecrets(t *testing.T) {
	source := newNexusRepositoryServer(t, "", "")
	t.Setenv("JIAN_MIGRATION_SOURCE_NEXUS_TEST", source.URL)
	t.Setenv("JIAN_SYNC_TOKEN", "不得外带的同步秘密")

	svc := newOnlineMigrationService(t)
	_, err := svc.ListRemoteRepositories(context.Background(), "NEXUS_TEST", "SYNC_TOKEN")
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("非专用凭据引用应被拒绝，得 %v", err)
	}
	if source.Requests() != 0 {
		t.Fatalf("非专用凭据引用不得触发来源请求，实际 %d 次", source.Requests())
	}
	if strings.Contains(err.Error(), "不得外带") || strings.Contains(err.Error(), "SYNC_TOKEN") {
		t.Fatalf("错误不得回显进程秘密或引用：%v", err)
	}
}

type nexusRepositoryTestServer struct {
	URL      string
	server   *httptest.Server
	requests atomic.Int32
}

func newNexusRepositoryServer(t *testing.T, username, password string) *nexusRepositoryTestServer {
	t.Helper()
	result := &nexusRepositoryTestServer{}
	result.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		result.requests.Add(1)
		if username != "" {
			gotUser, gotPassword, ok := r.BasicAuth()
			if !ok || gotUser != username || gotPassword != password {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
		}
		if r.URL.Path != "/service/rest/v1/repositories" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"name":"raw-hosted","format":"raw","type":"hosted"}]`))
	}))
	result.URL = result.server.URL
	t.Cleanup(result.server.Close)
	return result
}

func (s *nexusRepositoryTestServer) Requests() int32 {
	return s.requests.Load()
}

func newOnlineMigrationService(t *testing.T) *domain.MigrationService {
	t.Helper()
	return domain.NewMigrationService(nil, nil, func(string) (discover.Source, error) {
		return discover.NewOnlineREST(upstream.NewTestClient(time.Second)), nil
	})
}

func TestMigrationServicePersistsOnlyAllowedSourceConfig(t *testing.T) {
	db := newTestDB(t)
	svc := domain.NewMigrationService(repository.NewMigrationTaskRepo(db), nil)

	if _, err := svc.Create(domain.MigrationCreateInput{
		SourceType:   repository.MigrationSourceOnlineREST,
		SourceConfig: map[string]any{"url": "https://nexus.example.invalid", "sourceRef": "NEXUS_PRIMARY"},
	}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("online_rest 带 url err = %v，期望 ErrValidation", err)
	}

	online, err := svc.Create(domain.MigrationCreateInput{
		SourceType: repository.MigrationSourceOnlineREST,
		SourceConfig: map[string]any{
			"sourceRef":           "NEXUS_PRIMARY",
			"includeRepositories": []any{"raw"},
			"repositoryFormats":   map[string]any{"raw": "raw"},
			"credential":          "不得落库",
			"token":               "不得落库",
			"unknown":             "不得落库",
		},
		PlanJSON: `{"repositories":[{"name":"raw","format":"raw"},{"name":"maven","format":"maven"}]}`,
	})
	if err != nil {
		t.Fatalf("创建 online_rest：%v", err)
	}
	assertSourceConfig(t, online.SourceConfig, map[string]any{"sourceRef": "NEXUS_PRIMARY"})
	assertPlanRepositories(t, online.PlanJSON, []string{"raw"})

	offline, err := svc.Create(domain.MigrationCreateInput{
		SourceType: repository.MigrationSourceOfflineDir,
		SourceConfig: map[string]any{
			"path":                "/data/nexus",
			"includeRepositories": []any{"raw"},
			"repositoryTypes":     map[string]any{"raw": "hosted"},
			"password":            "不得落库",
			"unknown":             "不得落库",
		},
		PlanJSON: `{"repositories":[{"name":"raw","format":"raw"}]}`,
	})
	if err != nil {
		t.Fatalf("创建 offline_dir：%v", err)
	}
	assertSourceConfig(t, offline.SourceConfig, map[string]any{"path": "/data/nexus"})
}

func assertSourceConfig(t *testing.T, raw string, want map[string]any) {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("解析 sourceConfig：%v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sourceConfig = %#v，期望 %#v", got, want)
	}
}

func assertPlanRepositories(t *testing.T, raw string, want []string) {
	t.Helper()
	var plan struct {
		Repositories []struct {
			Name string `json:"name"`
		} `json:"repositories"`
	}
	if err := json.Unmarshal([]byte(raw), &plan); err != nil {
		t.Fatalf("解析 plan：%v", err)
	}
	got := make([]string, 0, len(plan.Repositories))
	for _, repo := range plan.Repositories {
		got = append(got, repo.Name)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("plan repositories = %#v，期望 %#v", got, want)
	}
}

func TestMigrationServiceCancelAndResume(t *testing.T) {
	db := newTestDB(t)
	svc := domain.NewMigrationService(repository.NewMigrationTaskRepo(db), nil)
	task, err := svc.Create(domain.MigrationCreateInput{
		SourceType:   repository.MigrationSourceOfflineBundle,
		SourceConfig: map[string]any{"path": "/b"},
	})
	if err != nil {
		t.Fatalf("Create：%v", err)
	}
	cancelled, err := svc.Cancel(task.ID)
	if err != nil {
		t.Fatalf("Cancel planned：%v", err)
	}
	if cancelled.Status != repository.MigrationStatusCancelled {
		t.Fatalf("status = %q", cancelled.Status)
	}
	resumed, err := svc.Resume(task.ID)
	if err != nil {
		t.Fatalf("Resume：%v", err)
	}
	if resumed.Status != repository.MigrationStatusRunning {
		t.Fatalf("resume 后 status = %q", resumed.Status)
	}
}

func TestMigrationServiceIllegalTransitions(t *testing.T) {
	db := newTestDB(t)
	repo := repository.NewMigrationTaskRepo(db)
	svc := domain.NewMigrationService(repo, nil)
	task, _ := svc.Create(domain.MigrationCreateInput{
		SourceType:   repository.MigrationSourceOfflineDir,
		SourceConfig: map[string]any{"path": "/x"},
	})
	// 直接标 completed 后 resume/start 应冲突
	msg := "done"
	if err := repo.UpdateStatus(task.ID, repository.MigrationStatusCompleted, &msg, true, true); err != nil {
		t.Fatalf("UpdateStatus：%v", err)
	}
	if _, err := svc.Start(task.ID, nil); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("completed start：%v", err)
	}
	if _, err := svc.Resume(task.ID); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("completed resume：%v", err)
	}
	if _, err := svc.Cancel(task.ID); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("completed cancel：%v", err)
	}
}

func TestMigrationServiceFailInterrupted(t *testing.T) {
	db := newTestDB(t)
	repo := repository.NewMigrationTaskRepo(db)
	svc := domain.NewMigrationService(repo, nil)
	task, _ := svc.Create(domain.MigrationCreateInput{
		SourceType:   repository.MigrationSourceOfflineDir,
		SourceConfig: map[string]any{"path": "/x"},
	})
	if _, err := svc.Start(task.ID, nil); err != nil {
		t.Fatalf("Start：%v", err)
	}
	n, err := svc.FailInterruptedRunning()
	if err != nil || n != 1 {
		t.Fatalf("FailInterrupted n=%d err=%v", n, err)
	}
	got, _ := svc.Get(task.ID)
	if got.Status != repository.MigrationStatusFailed {
		t.Fatalf("status = %q", got.Status)
	}
}

func TestMigrationServiceInvalidSource(t *testing.T) {
	db := newTestDB(t)
	svc := domain.NewMigrationService(repository.NewMigrationTaskRepo(db), nil)
	_, err := svc.Create(domain.MigrationCreateInput{
		SourceType:   "docker",
		SourceConfig: map[string]any{},
	})
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("err = %v", err)
	}
}
