package httpserver_test

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
	"github.com/wcpe/jianartifact/apps/server/internal/httpserver"
	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// seedAuditEvents 播种带完整 HTTP 上下文的审计事件，返回其 TS 便于断言顺序。
func seedAuditEvents(t *testing.T, db *persistence.DB) {
	t.Helper()
	audits := repository.NewAuditLogRepo(db)
	now := time.Now().UTC()
	entries := []repository.AuditLogEntry{
		{
			TS:    now.Add(-30 * time.Second).Format(time.RFC3339Nano),
			Actor: "admin-a", ActorEmail: "admin@example.com", Action: "asset.delete",
			EntityType: "asset", EntityKey: "release/a.jar", Repo: "release", Result: "error",
			IP: "203.0.113.7", AuthSource: "jwt", UserAgent: "Mozilla/5.0 (Windows NT 10.0)",
			RequestID: "req-seed-1", SourceNode: "node-a",
			HTTPMethod: "DELETE", HTTPPath: "/api/v1/artifacts/:id", StatusCode: 500, DurationMs: 1240,
			TokenPreview: "Bearer eyJhbG****1dnM", BodyPreview: "{\n  \"force\": true\n}",
		},
		{
			TS:    now.Add(-20 * time.Second).Format(time.RFC3339Nano),
			Actor: "admin-a", ActorEmail: "admin@example.com", Action: "setting.update",
			EntityType: "setting", EntityKey: "public_url", Result: "ok",
			IP: "10.12.3.44", AuthSource: "jwt",
			RequestID: "req-seed-2", SourceNode: "node-a",
			HTTPMethod: "PUT", HTTPPath: "/api/v1/settings", StatusCode: 200, DurationMs: 74,
		},
		{
			TS:    now.Add(-10 * time.Second).Format(time.RFC3339Nano),
			Actor: "sync-service", Action: "replication.apply", EntityType: "batch",
			EntityKey: "repl-batch-118", Result: "ok", IP: "192.168.1.27", AuthSource: "system",
			SourceNode: "node-a", HTTPMethod: "POST", HTTPPath: "/api/v1/replication/apply",
			StatusCode: 204, DurationMs: 8420,
		},
	}
	for _, entry := range entries {
		if err := audits.Insert(entry); err != nil {
			t.Fatalf("写入审计：%v", err)
		}
	}
}

func newAuditRouter(t *testing.T) (http.Handler, *persistence.DB) {
	t.Helper()
	db, err := persistence.Open(filepath.Join(t.TempDir(), "audit-http-context.db"))
	if err != nil {
		t.Fatalf("打开数据库：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("迁移数据库：%v", err)
	}
	handlers := api.NewHandlers(api.Deps{
		AuditLogs:          repository.NewAuditLogRepo(db),
		AuditObservability: repository.NewAuditObservabilityRepo(db),
		AuditSourceNode:    "node-a",
		AuditAttentionKey:  []byte("audit-http-context-test-key"),
	})
	server := httpserver.New("test",
		httpserver.WithHandlers(handlers),
		httpserver.WithManagementSecurityAudit(handlers.AuditLog),
		httpserver.WithMiddleware(func(c *gin.Context) {
			c.Set("auth.principal", &auth.Principal{UserID: 11, Username: "admin-a", Role: "admin", AuthSource: auth.AuthSourceWebJWT})
			c.Next()
		}),
	)
	return server.Handler(nil), db
}

func TestAuditEventsExposeHTTPContextAndFilters(t *testing.T) {
	router, db := newAuditRouter(t)
	seedAuditEvents(t, db)

	type eventPage struct {
		Items      []api.AuditEvent `json:"items"`
		TotalCount int              `json:"totalCount"`
	}
	get := func(path string) eventPage {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s 期望 200，实际 %d：%s", path, rec.Code, rec.Body.String())
		}
		var page eventPage
		if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
			t.Fatalf("解析 %s：%v", path, err)
		}
		return page
	}

	t.Run("列表返回脱敏 HTTP 上下文与身份快照", func(t *testing.T) {
		page := get("/api/v1/observability/audit/events?limit=10")
		if page.TotalCount != 3 || len(page.Items) != 3 {
			t.Fatalf("期望 3 条事件，实际 total=%d items=%d", page.TotalCount, len(page.Items))
		}
		newest := page.Items[0]
		if newest.Http == nil {
			t.Fatal("最新事件应带 HTTP 上下文")
		}
		if newest.Http.Method != api.AuditHttpContextMethodPOST {
			t.Fatalf("期望方法 POST，实际 %q", newest.Http.Method)
		}
		if newest.Http.Path != "/api/v1/replication/apply" {
			t.Fatalf("期望路由模板，实际 %q", newest.Http.Path)
		}
		if newest.Http.StatusCode == nil || *newest.Http.StatusCode != 204 {
			t.Fatalf("期望状态码 204，实际 %v", newest.Http.StatusCode)
		}
		if newest.DurationMs == nil || *newest.DurationMs != 8420 {
			t.Fatalf("期望耗时 8420ms，实际 %v", newest.DurationMs)
		}
		if newest.ClientIp == nil || *newest.ClientIp != "192.168.1.27" {
			t.Fatalf("期望客户端 IP 192.168.1.27，实际 %v", newest.ClientIp)
		}

		oldest := page.Items[2]
		if oldest.Http == nil || oldest.Http.TokenPreview == nil || *oldest.Http.TokenPreview != "Bearer eyJhbG****1dnM" {
			t.Fatalf("期望令牌脱敏预览，实际 %+v", oldest.Http)
		}
		if oldest.Http.BodyPreview == nil || !strings.Contains(*oldest.Http.BodyPreview, "force") {
			t.Fatalf("期望脱敏请求体，实际 %v", oldest.Http.BodyPreview)
		}
		if oldest.Actor.Email == nil || *oldest.Actor.Email != "admin@example.com" {
			t.Fatalf("期望操作者邮箱快照，实际 %v", oldest.Actor.Email)
		}
	})

	t.Run("按方法/动作/邮箱/客户端 IP 前缀筛选", func(t *testing.T) {
		if page := get("/api/v1/observability/audit/events?limit=10&method=POST"); page.TotalCount != 1 {
			t.Fatalf("method=POST 期望 1 条，实际 %d", page.TotalCount)
		}
		if page := get("/api/v1/observability/audit/events?limit=10&action=asset.delete"); page.TotalCount != 1 {
			t.Fatalf("action=asset.delete 期望 1 条，实际 %d", page.TotalCount)
		}
		if page := get("/api/v1/observability/audit/events?limit=10&actorEmail=admin@example.com"); page.TotalCount != 2 {
			t.Fatalf("actorEmail 期望 2 条，实际 %d", page.TotalCount)
		}
		if page := get("/api/v1/observability/audit/events?limit=10&clientIp=10.12."); page.TotalCount != 1 {
			t.Fatalf("clientIp 前缀期望 1 条，实际 %d", page.TotalCount)
		}
		if page := get("/api/v1/observability/audit/events?limit=10&authSource=system"); page.TotalCount != 1 {
			t.Fatalf("authSource=system 期望 1 条，实际 %d", page.TotalCount)
		}
	})

	t.Run("关键字命中请求路径", func(t *testing.T) {
		page := get("/api/v1/observability/audit/events?limit=10&q=artifacts")
		if page.TotalCount != 1 {
			t.Fatalf("q=artifacts 期望 1 条，实际 %d", page.TotalCount)
		}
		if page.Items[0].Http == nil || !strings.Contains(page.Items[0].Http.Path, "artifacts") {
			t.Fatalf("命中事件应含目标路径，实际 %+v", page.Items[0].Http)
		}
	})

	t.Run("非法方法与批次状态返回 400", func(t *testing.T) {
		for _, path := range []string{
			"/api/v1/observability/audit/events?method=TRACE",
			"/api/v1/observability/audit/events?attention=done",
		} {
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("GET %s 期望 400，实际 %d", path, rec.Code)
			}
		}
	})

	t.Run("显式 offset 直达指定页", func(t *testing.T) {
		first := get("/api/v1/observability/audit/events?limit=1")
		second := get("/api/v1/observability/audit/events?limit=1&offset=1")
		if len(first.Items) != 1 || len(second.Items) != 1 {
			t.Fatal("分页应各返回 1 条")
		}
		if first.Items[0].EventId == second.Items[0].EventId {
			t.Fatal("offset=1 应返回不同的第二条事件")
		}
		if second.Items[0].Http == nil || second.Items[0].Http.Path != "/api/v1/settings" {
			t.Fatalf("第二条应为 settings 事件，实际 %+v", second.Items[0].Http)
		}
	})

	t.Run("概览指标统计耗时/慢请求/错误码/来源 IP", func(t *testing.T) {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/observability/audit/summary", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET summary 期望 200，实际 %d：%s", rec.Code, rec.Body.String())
		}
		var summary api.AuditObservabilitySummary
		if err := json.Unmarshal(rec.Body.Bytes(), &summary); err != nil {
			t.Fatalf("解析 summary：%v", err)
		}
		if summary.AverageDurationMs == nil || *summary.AverageDurationMs != (1240+74+8420)/3 {
			t.Fatalf("期望平均耗时 %d，实际 %v", (1240+74+8420)/3, summary.AverageDurationMs)
		}
		if summary.SlowRequestCount == nil || *summary.SlowRequestCount != 2 {
			t.Fatalf("期望慢请求 2（1240ms 与 8420ms），实际 %v", summary.SlowRequestCount)
		}
		if summary.ServerErrorCount == nil || *summary.ServerErrorCount != 1 {
			t.Fatalf("期望 5xx 计数 1，实际 %v", summary.ServerErrorCount)
		}
		if summary.ClientErrorCount == nil || *summary.ClientErrorCount != 0 {
			t.Fatalf("期望 4xx 计数 0，实际 %v", summary.ClientErrorCount)
		}
		if summary.DistinctClientIpCount == nil || *summary.DistinctClientIpCount != 3 {
			t.Fatalf("期望来源 IP 去重 3，实际 %v", summary.DistinctClientIpCount)
		}
	})
}

// newUnauthenticatedAuditRouter 只挂审计写入中间件、不注入主体：
// 管理写请求会被认证层拒绝并记录审计，用于验证请求上下文的采集链路。
func newUnauthenticatedAuditRouter(t *testing.T) (http.Handler, *persistence.DB) {
	t.Helper()
	db, err := persistence.Open(filepath.Join(t.TempDir(), "audit-write-context.db"))
	if err != nil {
		t.Fatalf("打开数据库：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("迁移数据库：%v", err)
	}
	handlers := api.NewHandlers(api.Deps{
		AuditLogs:          repository.NewAuditLogRepo(db),
		AuditObservability: repository.NewAuditObservabilityRepo(db),
		AuditSourceNode:    "node-a",
		AuditAttentionKey:  []byte("audit-http-context-test-key"),
	})
	server := httpserver.New("test",
		httpserver.WithHandlers(handlers),
		httpserver.WithManagementSecurityAudit(handlers.AuditLog),
	)
	return server.Handler(nil), db
}

func TestAuditWriteCapturesRequestContext(t *testing.T) {
	router, db := newUnauthenticatedAuditRouter(t)

	// 未认证的管理写请求会被拒绝并记录审计：验证中间件确实抓到了方法与请求体。
	body := `{"name":"release","password":"s3cret-value"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/repositories", strings.NewReader(body))
	req.Header.Set("User-Agent", "audit-test-agent/1.0")
	req.Header.Set("X-Request-ID", "req-http-context-1")
	req.Header.Set("Authorization", "Bearer eyJhbGciOiJIUzI1NiJ9.payloadpart.signaturepart")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
		t.Fatalf("未认证写请求期望 401/403，实际 %d：%s", rec.Code, rec.Body.String())
	}

	entries, err := repository.NewAuditLogRepo(db).List(repository.AuditFilter{Limit: 10})
	if err != nil {
		t.Fatalf("读取审计：%v", err)
	}
	if len(entries) == 0 {
		t.Fatal("管理写请求应产生审计记录")
	}
	entry := entries[0]
	if entry.HTTPMethod != http.MethodPost {
		t.Fatalf("期望记录请求方法 POST，实际 %q", entry.HTTPMethod)
	}
	if !strings.Contains(entry.HTTPPath, "/api/v1/repositories") {
		t.Fatalf("期望记录请求路径，实际 %q", entry.HTTPPath)
	}
	if entry.StatusCode != rec.Code {
		t.Fatalf("期望状态码 %d，实际 %d", rec.Code, entry.StatusCode)
	}
	if entry.RequestID != "req-http-context-1" {
		t.Fatalf("期望请求 ID 透传，实际 %q", entry.RequestID)
	}
	if entry.UserAgent != "audit-test-agent/1.0" {
		t.Fatalf("期望 User-Agent 记录，实际 %q", entry.UserAgent)
	}
	if !strings.HasPrefix(entry.TokenPreview, "Bearer ") || !strings.Contains(entry.TokenPreview, "****") {
		t.Fatalf("期望令牌脱敏预览，实际 %q", entry.TokenPreview)
	}
	if !strings.Contains(entry.BodyPreview, "****") || strings.Contains(entry.BodyPreview, "s3cret-value") {
		t.Fatalf("请求体应脱敏且不含明文凭据，实际 %q", entry.BodyPreview)
	}
}
