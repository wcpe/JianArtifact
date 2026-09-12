package httpserver_test

// FR-38 审计写路径：仓库更新与集群配置变更须写入审计日志（回归测试）。
import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/api"
	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/blobstore"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
	"github.com/wcpe/jianartifact/apps/server/internal/upstream"
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
	assetRepo := repository.NewAssetRepo(db)
	settings := repository.NewSettingRepo(db)
	auditLogs := repository.NewAuditLogRepo(db)
	handlers := api.NewHandlers(api.Deps{
		Repos: domain.NewRepositoryService(repoRepo, repository.NewAclRepo(db), repository.NewAssetRepo(db),
			domain.NewSettingService(repository.NewSettingRepo(db)), repository.NewUserRepo(db)),
		Assets:          domain.NewAssetService(repoRepo, assetRepo, blobstore.NewStore(t.TempDir()), upstream.NewTestClient(time.Second)),
		AuditLogs:       auditLogs,
		AuditSourceNode: "management-test-node",
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
	r.POST("/api/v1/repositories/:name/recheck-connection", func(c *gin.Context) {
		handlers.RecheckRepositoryConnection(c, c.Param("name"))
	})
	r.GET("/api/v1/audit-logs", handlers.GetAuditLogs)
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

func TestManagementAuditSourceNodePersistsAndIsAvailableThroughAPI(t *testing.T) {
	r, _, repoRepo, _ := auditTestRouter(t)
	if _, err := repoRepo.Create("raw-source-node", "raw", "hosted", "public", "{}"); err != nil {
		t.Fatalf("创建仓库：%v", err)
	}

	update := httptest.NewRecorder()
	updateReq := httptest.NewRequest(http.MethodPatch, "/api/v1/repositories/raw-source-node", strings.NewReader(`{"description":"节点审计"}`))
	updateReq.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(update, updateReq)
	if update.Code != http.StatusOK {
		t.Fatalf("管理更新状态码 = %d，期望 200：%s", update.Code, update.Body.String())
	}

	list := httptest.NewRecorder()
	r.ServeHTTP(list, httptest.NewRequest(http.MethodGet, "/api/v1/audit-logs", nil))
	if list.Code != http.StatusOK {
		t.Fatalf("读取审计状态码 = %d，期望 200：%s", list.Code, list.Body.String())
	}
	var body api.AuditLogListResponse
	if err := json.Unmarshal(list.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析审计响应：%v", err)
	}
	for _, entry := range body.Items {
		if entry.Action == "repo.update" && entry.EntityKey == "raw-source-node" {
			if entry.SourceNode != "management-test-node" {
				t.Fatalf("管理审计来源节点 = %q，期望 management-test-node", entry.SourceNode)
			}
			return
		}
	}
	t.Fatalf("API 响应缺少管理审计记录：%+v", body.Items)
}

func TestAuditRecheckDoesNotRecordLegacyURLUserinfo(t *testing.T) {
	r, auditLogs, repoRepo, _ := auditTestRouter(t)
	const secret = "private-password"
	if _, err := repoRepo.Create("legacy-userinfo", "raw", "proxy", "private", `{"remoteUrl":"https://release-user:private-password@repo.example.com/raw"}`); err != nil {
		t.Fatalf("写入遗留 proxy 配置：%v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/repositories/legacy-userinfo/recheck-connection", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("遗留仓库重测应返回 200，得 %d（体：%s）", rec.Code, rec.Body.String())
	}
	rows, err := auditLogs.List(repository.AuditFilter{Action: "repo.recheck", Limit: 50})
	if err != nil {
		t.Fatalf("查询重测审计：%v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("重测审计条数 = %d，期望 1", len(rows))
	}
	if strings.Contains(rows[0].Detail, secret) || strings.Contains(rows[0].Detail, "release-user") {
		t.Fatalf("重测审计不得包含 URL userinfo：%q", rows[0].Detail)
	}
	if rows[0].Detail != "上游已配置" {
		t.Fatalf("重测审计 detail = %q，期望固定脱敏状态", rows[0].Detail)
	}
}
