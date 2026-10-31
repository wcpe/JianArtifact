package httpserver_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/api"
	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// newSettingHistoryRouter 构造带审计仓储的设置路由（历史端点依赖审计落库）。
func newSettingHistoryRouter(t *testing.T) (*gin.Engine, repository.AuditLogRepo) {
	t.Helper()
	db := openMigratedDB(t, filepath.Join(t.TempDir(), "settings-history.db"))
	auditLogs := repository.NewAuditLogRepo(db)
	handlers := api.NewHandlers(api.Deps{
		Settings:  domain.NewSettingService(repository.NewSettingRepo(db)),
		AuditLogs: auditLogs,
	})

	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		if c.GetHeader("X-Test-Role") == "admin" {
			c.Set("auth.principal", &auth.Principal{Role: "admin", Username: "admin", UserID: 1})
		} else {
			c.Set("auth.principal", &auth.Principal{Role: "user", Username: "u", UserID: 2})
		}
		c.Next()
	})
	r.GET("/api/v1/settings", handlers.GetSettings)
	r.PUT("/api/v1/settings", handlers.PutSettings)
	r.GET("/api/v1/settings/history", handlers.GetSettingHistory)
	return r, *auditLogs
}

func putSettingsAsAdmin(t *testing.T, r *gin.Engine, body string) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/v1/settings", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Test-Role", "admin")
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT %s 应 200，得 %d（体：%s）", body, rec.Code, rec.Body.String())
	}
}

// TestSettingHistoryRecordsChangePerKey 设置写入要逐键落审计，
// 且历史端点能按设置键精确查到前后值。
//
// 这条链路此前是断的：recordChange 对节点本地设置直接跳过，而 ChangeRecorder
// 在复制退役后已换成空实现——设置页即便做了历史面板也查不到任何记录。
func TestSettingHistoryRecordsChangePerKey(t *testing.T) {
	t.Parallel()
	r, _ := newSettingHistoryRouter(t)

	// 先改一次，形成「旧值 → 新值」。
	putSettingsAsAdmin(t, r, `{"blobGcInterval":5}`)
	// 再改一次，验证历史按时间倒序且能累积。
	putSettingsAsAdmin(t, r, `{"blobGcInterval":9}`)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/settings/history?key=blob_gc_interval", nil)
	req.Header.Set("X-Test-Role", "admin")
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("历史查询应 200，得 %d（体：%s）", rec.Code, rec.Body.String())
	}
	var body struct {
		Key     string                           `json:"key"`
		Entries []repository.SettingHistoryEntry `json:"entries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析响应：%v", err)
	}
	if body.Key != "blob_gc_interval" {
		t.Errorf("key = %q，期望 blob_gc_interval", body.Key)
	}
	if len(body.Entries) != 2 {
		t.Fatalf("应有 2 条历史，得 %d（体：%s）", len(body.Entries), rec.Body.String())
	}
	// 新→旧：最近一次是 5 → 9。
	if body.Entries[0].OldVal != "5" || body.Entries[0].NewVal != "9" {
		t.Errorf("最近一条应为 5 → 9，得 %q → %q", body.Entries[0].OldVal, body.Entries[0].NewVal)
	}
	if body.Entries[0].Actor != "admin" {
		t.Errorf("操作者应为 admin，得 %q", body.Entries[0].Actor)
	}
	// 第一次是从「当前生效值」改到 5。旧值取的是真实生效值（代码默认 86400），
	// 而不是空串——历史要能回答「从多少改到多少」，给出「未设置」会让人无从判断。
	if body.Entries[1].NewVal != "5" || body.Entries[1].OldVal != "86400" {
		t.Errorf("最早一条应为 86400 → 5，得 %q → %q", body.Entries[1].OldVal, body.Entries[1].NewVal)
	}
}

// TestSettingHistoryIsolatesKeys 历史必须按键隔离：
// 查 A 键不能把 B 键的改动混进来（模糊匹配正是这里容易出错的地方）。
func TestSettingHistoryIsolatesKeys(t *testing.T) {
	t.Parallel()
	r, _ := newSettingHistoryRouter(t)

	putSettingsAsAdmin(t, r, `{"blobGcInterval":5,"searchTimeoutSeconds":7}`)

	queryKeys := func(key string) []repository.SettingHistoryEntry {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/settings/history?key="+key, nil)
		req.Header.Set("X-Test-Role", "admin")
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("历史查询 %s 应 200，得 %d", key, rec.Code)
		}
		var body struct {
			Entries []repository.SettingHistoryEntry `json:"entries"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("解析响应：%v", err)
		}
		return body.Entries
	}

	if got := len(queryKeys("blob_gc_interval")); got != 1 {
		t.Errorf("blob_gc_interval 应有 1 条，得 %d", got)
	}
	if got := len(queryKeys("search_timeout_seconds")); got != 1 {
		t.Errorf("search_timeout_seconds 应有 1 条，得 %d", got)
	}
	// 未改动的键不应有历史。
	if got := len(queryKeys("trash_max_bytes")); got != 0 {
		t.Errorf("未改动项不应有历史，得 %d 条", got)
	}
}

// TestSettingHistoryRequiresKeyAndAdmin 鉴权与参数校验。
func TestSettingHistoryRequiresKeyAndAdmin(t *testing.T) {
	t.Parallel()
	r, _ := newSettingHistoryRouter(t)

	// 缺 key → 400。
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/settings/history", nil)
	req.Header.Set("X-Test-Role", "admin")
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("缺 key 应 400，得 %d", rec.Code)
	}

	// 非 admin → 403。
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/settings/history?key=blob_gc_interval", nil)
	req.Header.Set("X-Test-Role", "user")
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("非 admin 应 403，得 %d", rec.Code)
	}

	// 非法 limit → 400。
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/settings/history?key=blob_gc_interval&limit=abc", nil)
	req.Header.Set("X-Test-Role", "admin")
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("非法 limit 应 400，得 %d", rec.Code)
	}
}
