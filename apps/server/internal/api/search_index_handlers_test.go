package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// 本文件钉住的两个运维端点：
//   - GET  /api/v1/search/index        —— 字段与口径（ready/indexed/total/lastId/updatedAt
//     以及漂移四元组 indexedRows/assetRows/missingRows/orphanRows）
//   - POST /api/v1/search/index/rebuild —— 202 受理 / 运行中重复触发 409 / 非管理员 403 / 匿名 401

// searchIndexStatusStub 是状态端点用的桩：直接给出快照，避免为了一个纯展示端点去造真实回填。
type searchIndexStatusStub struct {
	status domain.SearchIndexStatus
	err    error
}

func (s searchIndexStatusStub) Status() (domain.SearchIndexStatus, error) { return s.status, s.err }
func (s searchIndexStatusStub) Rebuild() error                            { return nil }

// searchIndexRebuildStub 是重建端点用的桩：固定返回给定错误，用于确定性地覆盖 202 / 409。
type searchIndexRebuildStub struct {
	err      error
	rejected bool
}

func (s *searchIndexRebuildStub) Status() (domain.SearchIndexStatus, error) {
	return domain.SearchIndexStatus{}, nil
}

func (s *searchIndexRebuildStub) Rebuild() error {
	if s.err != nil {
		s.rejected = true
		return s.err
	}
	return nil
}

// serveSearchIndex 以给定主体（nil 为匿名）经与生产一致的路由形状发起请求：
// authMW 只注入主体（匿名不注入），admin 校验在 handler 内完成。
func serveSearchIndex(h *Handlers, principal *auth.Principal, method, path string) *httptest.ResponseRecorder {
	router := gin.New()
	router.Use(func(c *gin.Context) {
		if principal != nil {
			c.Set("auth.principal", principal)
		}
	})
	router.GET("/api/v1/search/index", func(c *gin.Context) { h.GetSearchIndexStatus(c) })
	router.POST("/api/v1/search/index/rebuild", func(c *gin.Context) { h.RebuildSearchIndex(c) })
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

// TestGetSearchIndexStatusFields 校验状态端点的字段与口径（回填中 / 已完成 / 仅孤儿行三种形态）。
func TestGetSearchIndexStatusFields(t *testing.T) {
	gin.SetMode(gin.ReleaseMode)

	// 回填中：ready=false，indexed < total；索引行数也还落后于资产行数（存在漏索引）。
	inProgress := NewHandlers(Deps{SearchIndex: searchIndexStatusStub{status: domain.SearchIndexStatus{
		Ready: false, Indexed: 4000, Total: 10000, LastID: 4000, UpdatedAt: "2026-10-19 10:00:00",
		IndexRows: 4000, AssetRows: 10000, MissingRows: 6000,
	}}})
	rec := serveSearchIndex(inProgress, adminPrincipal(), http.MethodGet, "/api/v1/search/index")
	if rec.Code != http.StatusOK {
		t.Fatalf("回填中状态码 = %d，期望 200：%s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("解析响应：%v（body=%s）", err, rec.Body.String())
	}
	for key, want := range map[string]any{
		"ready":       false,
		"indexed":     float64(4000),
		"total":       float64(10000),
		"lastId":      float64(4000),
		"updatedAt":   "2026-10-19 10:00:00",
		"indexedRows": float64(4000),
		"assetRows":   float64(10000),
		"missingRows": float64(6000),
		"orphanRows":  float64(0),
	} {
		if got[key] != want {
			t.Fatalf("字段 %s = %v，期望 %v（body=%s）", key, got[key], want, rec.Body.String())
		}
	}
	if len(got) != 9 {
		t.Fatalf("响应字段数 = %d，期望 9（多余字段会扩大非契约面）：%s", len(got), rec.Body.String())
	}

	// 已完成：ready=true，indexed == total，索引行数与资产行数一致（无漂移）。
	done := NewHandlers(Deps{SearchIndex: searchIndexStatusStub{status: domain.SearchIndexStatus{
		Ready: true, Indexed: 10000, Total: 10000, LastID: 10000, UpdatedAt: "2026-10-19 11:00:00",
		IndexRows: 10000, AssetRows: 10000,
	}}})
	rec = serveSearchIndex(done, adminPrincipal(), http.MethodGet, "/api/v1/search/index")
	if rec.Code != http.StatusOK {
		t.Fatalf("已完成状态码 = %d，期望 200", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("解析响应：%v", err)
	}
	if got["ready"] != true || got["indexed"] != float64(10000) || got["total"] != float64(10000) {
		t.Fatalf("已完成字段不符：%s", rec.Body.String())
	}
	if got["indexedRows"] != float64(10000) || got["assetRows"] != float64(10000) ||
		got["missingRows"] != float64(0) || got["orphanRows"] != float64(0) {
		t.Fatalf("已完成时应无漂移：%s", rec.Body.String())
	}

	// 仅孤儿索引行（仓库删除后的常态）：ready 仍为 true，但 orphanRows 必须报出来——
	// 监控按 missingRows 告警时不会被它误触，按 orphanRows 才知道要不要清理空间。
	orphan := NewHandlers(Deps{SearchIndex: searchIndexStatusStub{status: domain.SearchIndexStatus{
		Ready: true, Indexed: 10000, Total: 10000, LastID: 10000, UpdatedAt: "2026-10-19 12:00:00",
		IndexRows: 10003, AssetRows: 10000, OrphanRows: 3,
	}}})
	rec = serveSearchIndex(orphan, adminPrincipal(), http.MethodGet, "/api/v1/search/index")
	if rec.Code != http.StatusOK {
		t.Fatalf("孤儿行状态码 = %d，期望 200", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("解析响应：%v", err)
	}
	if got["ready"] != true || got["orphanRows"] != float64(3) || got["missingRows"] != float64(0) {
		t.Fatalf("孤儿索引行不应影响 ready，且要单独可见：%s", rec.Body.String())
	}
}

// TestGetSearchIndexStatusRejectsNonAdmin 校验权限：匿名 401、非管理员 403。
func TestGetSearchIndexStatusRejectsNonAdmin(t *testing.T) {
	gin.SetMode(gin.ReleaseMode)
	h := NewHandlers(Deps{SearchIndex: searchIndexStatusStub{status: domain.SearchIndexStatus{}}})

	if rec := serveSearchIndex(h, nil, http.MethodGet, "/api/v1/search/index"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("匿名状态码 = %d，期望 401", rec.Code)
	}
	if rec := serveSearchIndex(h, userPrincipal(), http.MethodGet, "/api/v1/search/index"); rec.Code != http.StatusForbidden {
		t.Fatalf("非管理员状态码 = %d，期望 403", rec.Code)
	}
}

// TestRebuildSearchIndexAcceptedThenConflict 校验重建端点：首次受理 202，运行中重复触发 409。
func TestRebuildSearchIndexAcceptedThenConflict(t *testing.T) {
	gin.SetMode(gin.ReleaseMode)

	// 首次受理：202（只表示已受理，真正的清空与重灌在后台）。
	ok := NewHandlers(Deps{SearchIndex: &searchIndexRebuildStub{}})
	rec := serveSearchIndex(ok, adminPrincipal(), http.MethodPost, "/api/v1/search/index/rebuild")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("首次受理状态码 = %d，期望 202：%s", rec.Code, rec.Body.String())
	}

	// 运行中重复触发：409 + job_running（对齐既有运维接口口径）。
	busy := &searchIndexRebuildStub{err: domain.ErrSearchIndexRunning}
	h := NewHandlers(Deps{SearchIndex: busy})
	rec = serveSearchIndex(h, adminPrincipal(), http.MethodPost, "/api/v1/search/index/rebuild")
	if rec.Code != http.StatusConflict {
		t.Fatalf("运行中重复触发状态码 = %d，期望 409：%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析错误响应：%v（body=%s）", err, rec.Body.String())
	}
	if body.Error.Code != "job_running" {
		t.Fatalf("错误码 = %q，期望 job_running（body=%s）", body.Error.Code, rec.Body.String())
	}
	if !busy.rejected {
		t.Fatal("运行中重复触发应真的走到 Rebuild（桩未被调用）")
	}
}

// TestRebuildSearchIndexRejectsNonAdmin 校验权限：匿名 401、非管理员 403，且都不得触发重建。
func TestRebuildSearchIndexRejectsNonAdmin(t *testing.T) {
	gin.SetMode(gin.ReleaseMode)
	stub := &searchIndexRebuildStub{}
	h := NewHandlers(Deps{SearchIndex: stub})

	if rec := serveSearchIndex(h, nil, http.MethodPost, "/api/v1/search/index/rebuild"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("匿名状态码 = %d，期望 401", rec.Code)
	}
	if rec := serveSearchIndex(h, userPrincipal(), http.MethodPost, "/api/v1/search/index/rebuild"); rec.Code != http.StatusForbidden {
		t.Fatalf("非管理员状态码 = %d，期望 403", rec.Code)
	}
	if stub.rejected {
		t.Fatal("被权限拦下的请求不应触达重建逻辑")
	}
}

// TestSearchIndexEndpointsWithRealService 用真实服务 + 真实库跑一遍读端点：把「历史行回填 →
// 状态端点报告 ready=true、indexed=total、lastId=最大 id」这条运维主链路端到端钉住。
func TestSearchIndexEndpointsWithRealService(t *testing.T) {
	gin.SetMode(gin.ReleaseMode)
	db := openAPITestDB(t)
	repoID, err := repository.NewRepoRepo(db).Create("api-index", "raw", "hosted", "public", "")
	if err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	for _, path := range []string{"a/one-1.0.0.jar", "a/two-2.0.0.jar", "a/three-3.0.0.jar"} {
		if _, err := db.Exec(`INSERT INTO asset (repository_id, path, blob_hash, size, content_type, sha1, md5)
			VALUES (?, ?, 'h', 1, 'text/plain', '', '')`, repoID, path); err != nil {
			t.Fatalf("写入历史行：%v", err)
		}
	}
	svc := domain.NewSearchIndexService(repository.NewSearchIndexStateRepo(db))
	h := NewHandlers(Deps{SearchIndex: svc})

	// 回填前：ready=false、indexed=0、total=0（迁移不回填，总量快照要等回填开始才写）。
	rec := serveSearchIndex(h, adminPrincipal(), http.MethodGet, "/api/v1/search/index")
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("解析回填前响应：%v", err)
	}
	if got["ready"] != false || got["indexed"] != float64(0) || got["total"] != float64(0) {
		t.Fatalf("回填前字段不符：%s", rec.Body.String())
	}

	if err := svc.Run(context.Background()); err != nil {
		t.Fatalf("回填：%v", err)
	}
	rec = serveSearchIndex(h, adminPrincipal(), http.MethodGet, "/api/v1/search/index")
	if rec.Code != http.StatusOK {
		t.Fatalf("回填后状态码 = %d：%s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("解析回填后响应：%v", err)
	}
	var maxID int64
	if err := db.Get(&maxID, `SELECT MAX(id) FROM asset`); err != nil {
		t.Fatalf("取最大 id：%v", err)
	}
	if got["ready"] != true || got["indexed"] != float64(3) || got["total"] != float64(3) || got["lastId"] != float64(maxID) {
		t.Fatalf("回填后字段不符：%s（期望 lastId=%d）", rec.Body.String(), maxID)
	}
	if got["updatedAt"] == "" {
		t.Fatalf("updatedAt 不应为空：%s", rec.Body.String())
	}
	// 三行历史行全部入了索引 → 索引行数与资产行数一致，无漂移。
	if got["indexedRows"] != float64(3) || got["assetRows"] != float64(3) ||
		got["missingRows"] != float64(0) || got["orphanRows"] != float64(0) {
		t.Fatalf("回填完成后应无漂移：%s", rec.Body.String())
	}

	// 重建受理后必须最终回到 ready=true（后台异步，轮询到稳定态）。
	if err := svc.Rebuild(); err != nil && !errors.Is(err, domain.ErrSearchIndexRunning) {
		t.Fatalf("受理重建：%v", err)
	}
	deadline := 0
	for {
		rec = serveSearchIndex(h, adminPrincipal(), http.MethodGet, "/api/v1/search/index")
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("解析重建后响应：%v", err)
		}
		if got["ready"] == true && got["indexed"] == float64(3) {
			break
		}
		deadline++
		if deadline > 600 {
			t.Fatalf("等待重建完成超时：%s", rec.Body.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// searchIndexBlockingStub 模拟「不支持 context、且计数很慢」的真实实现：
// Status() 一直阻塞到用例结束，用来验证 API 层的等待侧收口。
type searchIndexBlockingStub struct {
	release chan struct{}
}

func (s *searchIndexBlockingStub) Status() (domain.SearchIndexStatus, error) {
	<-s.release
	return domain.SearchIndexStatus{Ready: true}, nil
}

func (s *searchIndexBlockingStub) Rebuild() error { return nil }

// serveSearchIndexCtx 与 serveSearchIndex 同路由形状，但允许指定请求上下文。
func serveSearchIndexCtx(h *Handlers, principal *auth.Principal, ctx context.Context) *httptest.ResponseRecorder {
	router := gin.New()
	router.Use(func(c *gin.Context) {
		if principal != nil {
			c.Set("auth.principal", principal)
		}
	})
	router.GET("/api/v1/search/index", func(c *gin.Context) { h.GetSearchIndexStatus(c) })
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/search/index", nil).WithContext(ctx))
	return rec
}

// TestGetSearchIndexStatusBoundedByContext 钉住状态端点的等待侧收口：
//
//   - 请求上下文的 deadline 已过 → 不再等底层计数，直接 504 + search_index_status_timeout；
//   - 请求上下文被取消（客户端断开）→ 静默丢弃：不写响应体，也不是 5xx。
//
// 用「已过期的 deadline / 已取消的上下文」构造，是确定性的：不需要把真实预算改小，
// 也不会与查询抢时序。底层计数在这两个用例里始终阻塞，因此能确定地验证「没有被等」。
func TestGetSearchIndexStatusBoundedByContext(t *testing.T) {
	gin.SetMode(gin.ReleaseMode)

	// 预算耗尽：父上下文 deadline 已过，handler 派生的 ctx 立刻是 DeadlineExceeded。
	expired, cancelExpired := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	t.Cleanup(cancelExpired)
	blocked := &searchIndexBlockingStub{release: make(chan struct{})}
	t.Cleanup(func() { close(blocked.release) })
	h := NewHandlers(Deps{SearchIndex: blocked})

	rec := serveSearchIndexCtx(h, adminPrincipal(), expired)
	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("预算耗尽状态码 = %d（体：%s），期望 504", rec.Code, rec.Body.String())
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析错误响应：%v（体：%s）", err, rec.Body.String())
	}
	// 不复用 search_timeout：那个的错误码语义是「检索范围太大 / 该查索引」，处置方向不同。
	if body.Error.Code != "search_index_status_timeout" {
		t.Fatalf("错误码 = %q，期望 search_index_status_timeout", body.Error.Code)
	}

	// 客户端取消：连接已断，不写响应体，也不记「内部错误」。
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	rec = serveSearchIndexCtx(h, adminPrincipal(), cancelled)
	if rec.Code >= 500 {
		t.Fatalf("客户端取消不应映射为 5xx，实得 %d（体：%s）", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("客户端取消不应写响应体，实得：%s", rec.Body.String())
	}
}

// TestGetSearchIndexStatusPrefersContextualStatus 钉住可选扩展的优先级：实现方若支持
// context，就必须走 StatusContext（并把 ctx 传下去），而不是退化成后台 goroutine。
func TestGetSearchIndexStatusPrefersContextualStatus(t *testing.T) {
	gin.SetMode(gin.ReleaseMode)
	stub := &searchIndexContextualStub{}
	h := NewHandlers(Deps{SearchIndex: stub})
	rec := serveSearchIndex(h, adminPrincipal(), http.MethodGet, "/api/v1/search/index")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d（体：%s），期望 200", rec.Code, rec.Body.String())
	}
	if !stub.contextualCalled {
		t.Fatal("实现方支持 context 时应调用 StatusContext，而不是回落到 Status")
	}
	if stub.legacyCalled {
		t.Fatal("不应同时调用不带 context 的 Status")
	}
}

// searchIndexContextualStub 同时实现 Status 与 StatusContext，用于验证分派优先级。
type searchIndexContextualStub struct {
	contextualCalled bool
	legacyCalled     bool
}

func (s *searchIndexContextualStub) Status() (domain.SearchIndexStatus, error) {
	s.legacyCalled = true
	return domain.SearchIndexStatus{}, nil
}

func (s *searchIndexContextualStub) StatusContext(ctx context.Context) (domain.SearchIndexStatus, error) {
	s.contextualCalled = true
	return domain.SearchIndexStatus{Ready: true}, nil
}

func (s *searchIndexContextualStub) Rebuild() error { return nil }

// TestRebuildSearchIndexWritesAudit 校验重建留痕：受理成功后必须写一条 search.index_rebuild，
// 且 409 / 未授权路径都不得留下「重建了」的假账。
func TestRebuildSearchIndexWritesAudit(t *testing.T) {
	gin.SetMode(gin.ReleaseMode)
	db := openAPITestDB(t)
	auditLogs := repository.NewAuditLogRepo(db)

	// 受理成功 → 1 条审计。
	h := NewHandlers(Deps{SearchIndex: &searchIndexRebuildStub{}, AuditLogs: auditLogs})
	if rec := serveSearchIndex(h, adminPrincipal(), http.MethodPost, "/api/v1/search/index/rebuild"); rec.Code != http.StatusAccepted {
		t.Fatalf("受理状态码 = %d（体：%s），期望 202", rec.Code, rec.Body.String())
	}
	entries, err := auditLogs.List(repository.AuditFilter{Action: "search.index_rebuild"})
	if err != nil {
		t.Fatalf("查审计：%v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("受理成功应写 1 条审计，实际 %d 条", len(entries))
	}
	entry := entries[0]
	if entry.Action != "search.index_rebuild" || entry.EntityType != "search_index" ||
		entry.Result != "ok" || entry.Actor != "admin" {
		t.Fatalf("审计内容不符：%+v", entry)
	}

	// 运行中重复触发（409）→ 不新增审计。
	busy := NewHandlers(Deps{SearchIndex: &searchIndexRebuildStub{err: domain.ErrSearchIndexRunning}, AuditLogs: auditLogs})
	if rec := serveSearchIndex(busy, adminPrincipal(), http.MethodPost, "/api/v1/search/index/rebuild"); rec.Code != http.StatusConflict {
		t.Fatalf("冲突状态码 = %d，期望 409", rec.Code)
	}
	// 非管理员（403）→ 同样不新增审计。
	if rec := serveSearchIndex(h, userPrincipal(), http.MethodPost, "/api/v1/search/index/rebuild"); rec.Code != http.StatusForbidden {
		t.Fatalf("非管理员状态码 = %d，期望 403", rec.Code)
	}
	entries, err = auditLogs.List(repository.AuditFilter{Action: "search.index_rebuild"})
	if err != nil {
		t.Fatalf("查审计：%v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("未真正触发重建的请求不应写审计，实际 %d 条", len(entries))
	}
}
