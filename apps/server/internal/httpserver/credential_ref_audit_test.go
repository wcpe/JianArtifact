package httpserver_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/api"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// FR-109：凭据引用的配置与使用必须进入审计；审计只记录逻辑名称，绝不记录明文或解析结果。
func TestCredentialRefUsageIsAuditedWithoutSecrets(t *testing.T) {
	env := newTestEnv(t)
	var bootstrap api.LoginResponse
	if code := env.do(t, http.MethodPost, "/api/v1/auth/bootstrap", "", api.BootstrapRequest{Username: "admin", Password: "admin-pass-123"}, &bootstrap); code != http.StatusCreated {
		t.Fatalf("自举状态码=%d", code)
	}

	// 1) proxy 仓库创建携带 credentialRef → repo.create 审计含逻辑名称。
	var created api.Repository
	credentialRef := "LEGACY_UPSTREAM"
	if code := env.do(t, http.MethodPost, "/api/v1/repositories", bootstrap.Token, api.CreateRepositoryRequest{
		Name:          "cred-proxy",
		Format:        "raw",
		Type:          "proxy",
		RemoteUrl:     ptr("https://upstream.example.test/repo"),
		CredentialRef: &credentialRef,
	}, &created); code != http.StatusCreated {
		t.Fatalf("创建 proxy 仓库状态码=%d", code)
	}
	rows, err := env.audits.List(repository.AuditFilter{Action: "repo.create", Limit: 10})
	if err != nil || len(rows) != 1 {
		t.Fatalf("读取 repo.create 审计：rows=%+v err=%v", rows, err)
	}
	if !strings.Contains(rows[0].Detail, "credentialRef=LEGACY_UPSTREAM") {
		t.Fatalf("repo.create 审计应含 credentialRef 逻辑名称，实得 %q", rows[0].Detail)
	}

	// 2) 更新清除 credentialRef → repo.update 审计记录清除。
	var updated api.Repository
	if code := env.do(t, http.MethodPatch, "/api/v1/repositories/cred-proxy", bootstrap.Token, api.UpdateRepositoryRequest{
		CredentialRef: ptr(""),
	}, &updated); code != http.StatusOK {
		t.Fatalf("更新仓库状态码=%d", code)
	}
	updates, err := env.audits.List(repository.AuditFilter{Action: "repo.update", Limit: 10})
	if err != nil || len(updates) != 1 {
		t.Fatalf("读取 repo.update 审计：rows=%+v err=%v", updates, err)
	}
	if !strings.Contains(updates[0].Detail, "credentialRef=(cleared)") {
		t.Fatalf("repo.update 审计应记录凭据清除，实得 %q", updates[0].Detail)
	}

	// 3) 迁移任务携带 credentialRef 创建并启动 → migration.start 审计含逻辑名称。
	// credentialRef 在创建时即校验 JIAN_UPSTREAM_CREDENTIAL_<引用名> 存在；明文绝不入审计。
	const credentialValue = "upstream-secret-material"
	t.Setenv("JIAN_UPSTREAM_CREDENTIAL_LEGACY_UPSTREAM", credentialValue)
	var task api.MigrationTask
	if code := env.do(t, http.MethodPost, "/api/v1/migrations", bootstrap.Token, api.CreateMigrationRequest{
		SourceType:    api.OnlineRest,
		SourceConfig:  ptrMigrationOnlineURLConfig(t, "https://nexus.example.test/repository"),
		CredentialRef: &credentialRef,
	}, &task); code != http.StatusCreated {
		t.Fatalf("创建迁移任务状态码=%d", code)
	}
	var started api.MigrationTask
	if code := env.do(t, http.MethodPost, "/api/v1/migrations/"+itoa64(task.Id)+"/start", bootstrap.Token, nil, &started); code != http.StatusOK {
		t.Fatalf("启动迁移任务状态码=%d", code)
	}
	starts, err := env.audits.List(repository.AuditFilter{Action: "migration.start", Limit: 10})
	if err != nil || len(starts) != 1 {
		t.Fatalf("读取 migration.start 审计：rows=%+v err=%v", starts, err)
	}
	if !strings.Contains(starts[0].Detail, "credentialRef=LEGACY_UPSTREAM") {
		t.Fatalf("migration.start 审计应含 credentialRef 逻辑名称，实得 %q", starts[0].Detail)
	}

	// 4) 全部审计条目不得出现明文凭据、样例值或环境变量名。
	all, err := env.audits.List(repository.AuditFilter{Limit: 200})
	if err != nil {
		t.Fatalf("读取全部审计：%v", err)
	}
	for _, row := range all {
		if strings.Contains(row.Detail, credentialValue) || strings.Contains(row.Detail, "JIAN_UPSTREAM_CREDENTIAL_") {
			t.Fatalf("审计不得记录明文凭据或环境变量名：action=%s detail=%q", row.Action, row.Detail)
		}
	}
}
