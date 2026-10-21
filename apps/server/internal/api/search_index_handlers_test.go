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
//   - GET  /api/v1/search/index        —— 字段与口径（ready/indexed/total/lastId/updatedAt）
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

// TestGetSearchIndexStatusFields 校验状态端点的字段与口径（回填中 vs 已完成两种形态）。
func TestGetSearchIndexStatusFields(t *testing.T) {
	gin.SetMode(gin.ReleaseMode)

	// 回填中：ready=false，indexed < total。
	inProgress := NewHandlers(Deps{SearchIndex: searchIndexStatusStub{status: domain.SearchIndexStatus{
		Ready: false, Indexed: 4000, Total: 10000, LastID: 4000, UpdatedAt: "2026-10-19 10:00:00",
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
		"ready":     false,
		"indexed":   float64(4000),
		"total":     float64(10000),
		"lastId":    float64(4000),
		"updatedAt": "2026-10-19 10:00:00",
	} {
		if got[key] != want {
			t.Fatalf("字段 %s = %v，期望 %v（body=%s）", key, got[key], want, rec.Body.String())
		}
	}
	if len(got) != 5 {
		t.Fatalf("响应字段数 = %d，期望 5（多余字段会扩大非契约面）：%s", len(got), rec.Body.String())
	}

	// 已完成：ready=true，indexed == total。
	done := NewHandlers(Deps{SearchIndex: searchIndexStatusStub{status: domain.SearchIndexStatus{
		Ready: true, Indexed: 10000, Total: 10000, LastID: 10000, UpdatedAt: "2026-10-19 11:00:00",
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
