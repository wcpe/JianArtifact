package httpserver_test

// FR-38 审计写路径：仓库更新与集群配置变更须写入审计日志（回归测试）。
import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/api"
	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// auditTestRouter 装配仓库与集群配置端点 + 审计存储，返回数据句柄。
func auditTestRouter(t *testing.T) (http.Handler, *repository.AuditLogRepo, *repository.RepoRepo, *repository.SettingRepo) {
	t.Helper()
	db, err := persistence.Open(filepath.Join(t.TempDir(), "audit-write.db"))
	if err != nil {
		t.Fatalf("打开数据库：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("迁移：%v", err)
	}
	repoRepo := repository.NewRepoRepo(db)
	settings := repository.NewSettingRepo(db)
	auditLogs := repository.NewAuditLogRepo(db)
	handlers := api.NewHandlers(api.Deps{
		Repos: domain.NewRepositoryService(repoRepo, repository.NewAclRepo(db), repository.NewAssetRepo(db),
			domain.NewSettingService(repository.NewSettingRepo(db)), repository.NewUserRepo(db)),
		Replication: domain.NewReplicationService(
			repository.NewReplChangeRepo(db), repository.NewAssetRepo(db), repoRepo,
			repository.NewAclRepo(db), repository.NewUserRepo(db), repository.NewTokenRepo(db), settings, nil,
		),
		AuditLogs: auditLogs,
	})

	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("auth.principal", &auth.Principal{Role: "admin", Username: "admin", UserID: 1})
		c.Next()
	})
	r.PATCH("/api/v1/repositories/:name", func(c *gin.Context) {
		handlers.UpdateRepository(c, c.Param("name"))
	})
	r.PUT("/api/v1/cluster", handlers.PutClusterStatus)
	return r, auditLogs, repoRepo, settings
}

// TestAuditRepoUpdate 仓库更新成功后应写入 repo.update 审计。
func TestAuditRepoUpdate(t *testing.T) {
	r, auditLogs, repoRepo, _ := auditTestRouter(t)
	if _, err := repoRepo.Create("raw-audit", "raw", "hosted", "public", "{}"); err != nil {
		t.Fatalf("创建仓库：%v", err)
	}

	body := `{"description":"更新后的描述"}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/repositories/raw-audit", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("更新仓库应 200，得 %d（体：%s）", rec.Code, rec.Body.String())
	}
	rows, err := auditLogs.List(repository.AuditFilter{Limit: 50})
	if err != nil {
		t.Fatalf("查询审计：%v", err)
	}
	var found bool
	for _, row := range rows {
		if row.Action == "repo.update" && row.EntityKey == "raw-audit" && row.Result == "ok" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("仓库更新应写入 repo.update 审计，得 %+v", rows)
	}
}

// TestAuditClusterConfig 集群配置变更成功后应写入 cluster.config 审计。
func TestAuditClusterConfig(t *testing.T) {
	r, auditLogs, _, settings := auditTestRouter(t)
	if err := settings.Set("repl:enabled", "true"); err != nil {
		t.Fatalf("预置配置：%v", err)
	}

	body := `{"enabled":false}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/v1/cluster", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("集群配置应 200，得 %d（体：%s）", rec.Code, rec.Body.String())
	}
	rows, err := auditLogs.List(repository.AuditFilter{Limit: 50})
	if err != nil {
		t.Fatalf("查询审计：%v", err)
	}
	var found bool
	for _, row := range rows {
		if row.Action == "cluster.config" && row.Result == "ok" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("集群配置变更应写入 cluster.config 审计，得 %+v", rows)
	}
}
