package httpserver_test

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/api"
	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/blobstore"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// syncTokenMiddleware 复刻 main 包中间件（FR-84）：校验 Bearer 令牌，常量时间比较。
func syncTokenMiddleware(want string) gin.HandlerFunc {
	return func(c *gin.Context) {
		got := ""
		if h := c.GetHeader("Authorization"); strings.HasPrefix(h, "Bearer ") {
			got = strings.TrimPrefix(h, "Bearer ")
		}
		if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}
		c.Next()
	}
}

// newReplicationServer 装配复制端点（ReplicationService + pull/blob 路由 + 同步令牌中间件）。
// 返回服务端 handler 与数据句柄，供构造数据后测试。
func newReplicationServer(t *testing.T, syncToken string) (http.Handler, *repository.ReplChangeRepo, *blobstore.Store, *domain.ReplicationService) {
	t.Helper()
	db, err := persistence.Open(filepath.Join(t.TempDir(), "repl-handler.db"))
	if err != nil {
		t.Fatalf("打开数据库：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("迁移：%v", err)
	}
	userRepo := repository.NewUserRepo(db)
	repoRepo := repository.NewRepoRepo(db)
	assetRepo := repository.NewAssetRepo(db)
	aclRepo := repository.NewAclRepo(db)
	tokenRepo := repository.NewTokenRepo(db)
	repl := repository.NewReplChangeRepo(db)
	blobs := blobstore.NewStore(filepath.Join(t.TempDir(), "blobs"))
	replSvc := domain.NewReplicationService(repl, assetRepo, repoRepo, aclRepo, userRepo, tokenRepo, repository.NewSettingRepo(db), blobs)

	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	if syncToken != "" {
		sync := r.Group("/api/v1/cluster/sync", syncTokenMiddleware(syncToken))
		sync.GET("/pull", func(c *gin.Context) {
			changes, err := replSvc.ListSince(0, 500)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}
			latest, _ := replSvc.LatestSeq()
			c.JSON(http.StatusOK, gin.H{"changes": changes, "latestSeq": latest})
		})
		sync.GET("/blob/:hash", func(c *gin.Context) {
			rc, _, err := replSvc.OpenBlob(c.Param("hash"))
			if err != nil {
				c.Status(http.StatusNotFound)
				return
			}
			defer func() { _ = rc.Close() }()
			c.DataFromReader(http.StatusOK, -1, "application/octet-stream", rc, nil)
		})
	}
	return r, repl, blobs, replSvc
}

// TestReplicationEndpointAuth 鉴权：无 token / 错 token 401；正确 token 200；未配置 token 端点 404。
func TestReplicationEndpointAuth(t *testing.T) {
	// 未配置同步令牌：复制端点不注册 → 404。
	h, _, _, _ := newReplicationServer(t, "")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/cluster/sync/pull", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("未配置同步令牌应 404，得 %d", rec.Code)
	}

	// 配置令牌：无 token → 401。
	h, _, _, _ = newReplicationServer(t, "secret-token")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/cluster/sync/pull", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("无 token 应 401，得 %d", rec.Code)
	}

	// 错误 token → 401。
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/cluster/sync/pull", nil)
	req.Header.Set("Authorization", "Bearer wrong-token")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("错误 token 应 401，得 %d", rec.Code)
	}

	// 正确 token → 200。
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/cluster/sync/pull", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("正确 token 应 200，得 %d", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应解析失败：%v", err)
	}
	if body["latestSeq"].(float64) != 0 {
		t.Errorf("空库 latestSeq 应为 0，得 %v", body["latestSeq"])
	}
}

// TestReplicationEndpointPullAndBlob pull 返回变更、blob 流式（200/404）。
func TestReplicationEndpointPullAndBlob(t *testing.T) {
	h, repl, blobs, replSvc := newReplicationServer(t, "secret-token")

	// 构造一条变更日志与一个 blob。
	if _, err := repl.Append("node-a", "put", "setting", "setting:k", `{"key":"k","value":"v"}`, "2026-08-13T00:00:00Z"); err != nil {
		t.Fatalf("Append：%v", err)
	}
	hash, _, _, _, err := blobs.Put(strings.NewReader("blob-payload"))
	if err != nil {
		t.Fatalf("写 blob：%v", err)
	}

	// pull：返回 1 条变更。
	req := httptest.NewRequest(http.MethodGet, "/api/v1/cluster/sync/pull", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("pull 应 200，得 %d", rec.Code)
	}
	var body struct {
		Changes []struct {
			Seq  int64  `json:"seq"`
			Data string `json:"data"`
		} `json:"changes"`
		LatestSeq int64 `json:"latestSeq"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应解析：%v", err)
	}
	if len(body.Changes) != 1 || body.LatestSeq != 1 || body.Changes[0].Seq != 1 {
		t.Errorf("pull 结果不符：%+v", body)
	}

	// blob：存在的 hash 200 且内容一致。
	req = httptest.NewRequest(http.MethodGet, "/api/v1/cluster/sync/blob/"+hash, nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("blob 应 200，得 %d", rec.Code)
	}
	if rec.Body.String() != "blob-payload" {
		t.Errorf("blob 内容不符：%q", rec.Body.String())
	}

	// blob：不存在的 hash 404。
	req = httptest.NewRequest(http.MethodGet, "/api/v1/cluster/sync/blob/0000000000000000000000000000000000000000000000000000000000000000", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("不存在 blob 应 404，得 %d", rec.Code)
	}

	_ = replSvc
}

// TestClusterEndpoints 集群管理端点（FR-86）：GET 返回状态、PUT 写 enabled、非 admin 403。
func TestClusterEndpoints(t *testing.T) {
	_, _, _, replSvc := newReplicationServer(t, "secret")
	handlers := api.NewHandlers(api.Deps{
		Version:         "test",
		Replication:     replSvc,
		ClusterPeerURL:  "http://peer.example",
		ClusterTokenSet: true,
	})

	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	// 测试用鉴权中间件：按请求头模拟 admin / user 主体。
	r.Use(func(c *gin.Context) {
		if c.GetHeader("X-Test-Role") == "admin" {
			c.Set("auth.principal", &auth.Principal{Role: "admin", Username: "admin", UserID: 1})
		} else {
			c.Set("auth.principal", &auth.Principal{Role: "user", Username: "u", UserID: 2})
		}
		c.Next()
	})
	r.GET("/api/v1/cluster", handlers.GetClusterStatus)
	r.PUT("/api/v1/cluster", handlers.PutClusterStatus)

	// 非 admin GET → 403。
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/cluster", nil)
	req.Header.Set("X-Test-Role", "user")
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("非 admin GET 应 403，得 %d", rec.Code)
	}

	// admin PUT 配置对端（FR-88：peerUrl + peerToken 入库）。
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPut, "/api/v1/cluster", strings.NewReader(`{"peerUrl":"http://peer.example","peerToken":"secret"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Test-Role", "admin")
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin PUT 配置对端应 200，得 %d", rec.Code)
	}

	// admin GET → 200 且状态字段正确（对端自 setting 读，FR-88）。
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/cluster", nil)
	req.Header.Set("X-Test-Role", "admin")
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin GET 应 200，得 %d", rec.Code)
	}
	var st struct {
		NodeID    string `json:"nodeId"`
		PeerURL   string `json:"peerUrl"`
		TokenSet  bool   `json:"tokenSet"`
		Enabled   bool   `json:"enabled"`
		Watermark int64  `json:"watermark"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("解析响应：%v", err)
	}
	if st.PeerURL != "http://peer.example" || !st.TokenSet || !st.Enabled || st.NodeID == "" {
		t.Errorf("集群状态字段不符：%+v", st)
	}

	// admin PUT enabled=false → 200 且 enabled=false。
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPut, "/api/v1/cluster", strings.NewReader(`{"enabled":false}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Test-Role", "admin")
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin PUT 应 200，得 %d", rec.Code)
	}
	var after struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &after); err != nil {
		t.Fatalf("解析响应：%v", err)
	}
	if after.Enabled {
		t.Error("PUT enabled=false 后状态应 enabled=false")
	}

	// 非 admin PUT → 403。
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPut, "/api/v1/cluster", strings.NewReader(`{"enabled":true}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Test-Role", "user")
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("非 admin PUT 应 403，得 %d", rec.Code)
	}
}

// TestClusterSyncLogs 同步历史端点（FR-88）：非 admin 403，admin 返回分页列表（含实体构成）。
func TestClusterSyncLogs(t *testing.T) {
	db, err := persistence.Open(filepath.Join(t.TempDir(), "sync-logs-handler.db"))
	if err != nil {
		t.Fatalf("打开数据库：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("迁移：%v", err)
	}
	syncLogs := repository.NewSyncLogRepo(db)
	logID, err := syncLogs.Start("https://repo.wcpe.top", 0)
	if err != nil {
		t.Fatalf("Start：%v", err)
	}
	if err := syncLogs.Finish(logID, true, 3, 3, 3, 0, 1, map[string]int{"repository": 3, "user": 1}, ""); err != nil {
		t.Fatalf("Finish：%v", err)
	}

	handlers := api.NewHandlers(api.Deps{SyncLogs: syncLogs})

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
	r.GET("/api/v1/cluster/sync-logs", handlers.GetClusterSyncLogs)

	// 非 admin → 403。
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/cluster/sync-logs", nil)
	req.Header.Set("X-Test-Role", "user")
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("非 admin 应 403，得 %d", rec.Code)
	}

	// admin → 200，返回记录列表与总数。
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/cluster/sync-logs", nil)
	req.Header.Set("X-Test-Role", "admin")
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin 应 200，得 %d", rec.Code)
	}
	var out struct {
		Items []repository.SyncLogEntry `json:"items"`
		Total int                       `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析响应：%v", err)
	}
	if out.Total != 1 || len(out.Items) != 1 {
		t.Fatalf("应 1 条记录 total=%d items=%d", out.Total, len(out.Items))
	}
	e := out.Items[0]
	if e.PeerURL != "https://repo.wcpe.top" || e.Success == nil || !*e.Success {
		t.Errorf("记录字段不符：peer=%s success=%v", e.PeerURL, e.Success)
	}
	if e.Changes != 3 || e.Blobs != 1 {
		t.Errorf("统计不符：changes=%d blobs=%d", e.Changes, e.Blobs)
	}
	var counts map[string]int
	if err := json.Unmarshal([]byte(e.EntityCounts), &counts); err != nil || counts["repository"] != 3 {
		t.Errorf("实体构成不符：%v err=%v", counts, err)
	}

	// 分页参数：limit=1 只回 1 条。
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/cluster/sync-logs?limit=1", nil)
	req.Header.Set("X-Test-Role", "admin")
	r.ServeHTTP(rec, req)
	var page struct {
		Items []repository.SyncLogEntry `json:"items"`
		Total int                       `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("解析分页响应：%v", err)
	}
	if len(page.Items) != 1 || page.Total != 1 {
		t.Errorf("limit=1 应回 1 条且 total=1，得 items=%d total=%d", len(page.Items), page.Total)
	}
}

// TestClusterSyncLogsEmpty 空数据（无同步记录）时 items 应为 JSON 数组 [] 而非 null——
// 前端同步历史渲染依赖数组（list.items.length），null 会导致页面崩溃。
func TestClusterSyncLogsEmpty(t *testing.T) {
	db, err := persistence.Open(filepath.Join(t.TempDir(), "sync-logs-empty.db"))
	if err != nil {
		t.Fatalf("打开数据库：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("迁移：%v", err)
	}
	handlers := api.NewHandlers(api.Deps{SyncLogs: repository.NewSyncLogRepo(db)})

	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("auth.principal", &auth.Principal{Role: "admin", Username: "admin", UserID: 1})
		c.Next()
	})
	r.GET("/api/v1/cluster/sync-logs", handlers.GetClusterSyncLogs)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/cluster/sync-logs", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200，得 %d", rec.Code)
	}
	// 断言 items 为 JSON 数组（[]）而非 null。
	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("解析响应：%v", err)
	}
	items, ok := raw["items"].([]any)
	if !ok {
		t.Fatalf("items 应为数组 []，得 %v（体：%s）", raw["items"], rec.Body.String())
	}
	if len(items) != 0 {
		t.Errorf("空库 items 长度应为 0，得 %d", len(items))
	}
}
