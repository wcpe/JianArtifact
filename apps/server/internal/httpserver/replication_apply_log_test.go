package httpserver_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/api"
	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/httpserver"
	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

func TestReplicationApplyLogsPermissionPaginationAndFilter(t *testing.T) {
	db, err := persistence.Open(filepath.Join(t.TempDir(), "apply-log-api.db"))
	if err != nil {
		t.Fatalf("打开数据库：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("迁移：%v", err)
	}
	logs := repository.NewReplicationApplyLogRepo(db)
	for i, result := range []string{"failed", "applied"} {
		if err := logs.Observe(repository.ReplicationApplyLog{
			SourceNode:  "node-a",
			SourceSeq:   int64(i + 1),
			PeerURL:     "https://peer.example",
			EntityType:  "asset",
			EntityKey:   "asset:raw/a.txt",
			Op:          "put",
			Result:      result,
			Detail:      "详情",
			FirstSeenAt: "2026-08-18T00:00:00Z",
			LastSeenAt:  "2026-08-18T00:00:00Z",
		}); err != nil {
			t.Fatalf("写入审计：%v", err)
		}
	}
	handlers := api.NewHandlers(api.Deps{ReplicationApplyLogs: logs})
	server := httpserver.New("test",
		httpserver.WithHandlers(handlers),
		httpserver.WithMiddleware(func(c *gin.Context) {
			role := c.GetHeader("X-Test-Role")
			if role != "" {
				c.Set("auth.principal", &auth.Principal{Role: role, Username: role, UserID: 1})
			}
			c.Next()
		}),
	)
	r := server.Handler(nil)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/replication-apply-logs", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("未认证应 401，得 %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/replication-apply-logs", nil)
	req.Header.Set("X-Test-Role", "user")
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("非管理员应 403，得 %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/replication-apply-logs?sourceSeq=-1", nil)
	req.Header.Set("X-Test-Role", "admin")
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("负 sourceSeq 应 400，得 %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/replication-apply-logs?result=invalid", nil)
	req.Header.Set("X-Test-Role", "admin")
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非法 result 应 400，得 %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/replication-apply-logs?result=failed&limit=1&offset=0", nil)
	req.Header.Set("X-Test-Role", "admin")
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("管理员查询应 200，得 %d（体：%s）", rec.Code, rec.Body.String())
	}
	var out api.ReplicationApplyLogList
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析响应：%v", err)
	}
	if out.Total != 1 || len(out.Items) != 1 || out.Items[0].Result != "failed" {
		t.Fatalf("分页筛选结果不符：%+v", out)
	}
}
