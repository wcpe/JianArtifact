package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

func TestNotificationLessPrioritizesSeverityAfterFailure(t *testing.T) {
	now := time.Now().UTC()
	critical := AuditAttentionPreview{AttentionId: "critical", FailureCount: 1, Severity: AuditSeverityCritical, LatestOccurredAt: now.Add(-time.Hour)}
	high := AuditAttentionPreview{AttentionId: "high", FailureCount: 1, Severity: AuditSeverityHigh, LatestOccurredAt: now}
	if !notificationLess(critical, high) {
		t.Fatal("同为失败批次时，critical 必须优先于较新的 high")
	}
	if auditSeverity(repository.ObservabilityEvent{Action: "repository.delete", Result: "ok"}) != AuditSeverityCritical {
		t.Fatal("危险删除必须归为 critical")
	}
}

func TestNotificationOrderingSemantics(t *testing.T) {
	now := time.Now().UTC()
	newerSuccess := AuditAttentionPreview{AttentionId: "b-newer", FailureCount: 0, LatestOccurredAt: now}
	olderFailure := AuditAttentionPreview{AttentionId: "a-older", FailureCount: 2, LatestOccurredAt: now.Add(-time.Hour)}
	// 缺省请求（页眉预览）失败优先；显式请求（消息中心）时间倒序。
	if !notificationLess(olderFailure, newerSuccess) {
		t.Fatal("缺省请求必须失败批次优先，即使其更旧")
	}
	if !notificationTimeLess(newerSuccess, olderFailure) {
		t.Fatal("显式请求必须按最新时间倒序，不因失败置顶")
	}
	tieA := AuditAttentionPreview{AttentionId: "same-1", LatestOccurredAt: now}
	tieB := AuditAttentionPreview{AttentionId: "same-0", LatestOccurredAt: now}
	if !notificationTimeLess(tieA, tieB) {
		t.Fatal("时间相同时必须以 attentionId 倒序打破平局")
	}
}

// TestAuditSummaryHandlesRealisticDailyVolume 是线上 bug 的回归测试：
// 默认 24h 窗口的事件量超过「全量投影 + 5000 上限」后，审计总览返回 409
// audit_query_too_large，前端整个工作台变成「当前节点审计暂时不可用」。
// 实测线上节点上线第 3 天，24h 就已有 5900 条审计事件 —— 默认视图必然踩中。
// 修复后聚合走窄投影、上限提高到 50000，默认 24h 必须正常返回并统计全量。
func TestAuditSummaryHandlesRealisticDailyVolume(t *testing.T) {
	db := openAPITestDB(t)
	const seeded = 6000
	seedAuditEvents(t, db, seeded)

	repo := repository.NewAuditObservabilityRepo(db)
	handler := NewHandlers(Deps{AuditObservability: repo, AuditSourceNode: "node-a"})

	from := time.Now().UTC().Add(-24 * time.Hour)
	to := time.Now().UTC().Add(time.Minute)
	rec := serveAuditSummary(handler, GetAuditObservabilitySummaryParams{From: &from, To: &to})
	if rec.Code != http.StatusOK {
		t.Fatalf("默认 24h 总览应返回 200，得 %d，响应 = %s", rec.Code, rec.Body.String())
	}
	var summary AuditObservabilitySummary
	if err := json.Unmarshal(rec.Body.Bytes(), &summary); err != nil {
		t.Fatalf("解析总览响应：%v", err)
	}
	if summary.TotalCount != seeded {
		t.Fatalf("总览应统计全部 %d 条事件，得 %d", seeded, summary.TotalCount)
	}
}

// serveAuditSummary 以管理员身份调用 GET /observability/audit/summary。
func serveAuditSummary(handler *Handlers, params GetAuditObservabilitySummaryParams) *httptest.ResponseRecorder {
	router := gin.New()
	router.GET("/summary", func(c *gin.Context) {
		c.Set("auth.principal", &auth.Principal{Role: "admin"})
		handler.GetAuditObservabilitySummary(c, params)
	})
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/summary", nil))
	return rec
}

// seedAuditEvents 单事务批量写入 n 条审计事件；detail / body_preview 故意写成大字段，
// 用来证明聚合读取不会把这些体积大又不参与统计的列读进内存。
func seedAuditEvents(t *testing.T, db *persistence.DB, n int) {
	t.Helper()
	tx, err := db.Beginx()
	if err != nil {
		t.Fatalf("开启事务：%v", err)
	}
	stmt, err := tx.Prepare(`INSERT INTO audit_log
		(ts, actor, action, entity_type, entity_key, repo, detail, result, ip, source_node,
		 http_method, http_path, status_code, duration_ms, user_agent, body_preview)
		VALUES (?, 'admin', 'asset.put', 'asset', ?, 'release', ?, 'ok', '203.0.113.7', '',
		 'PUT', '/repository/release/pkg.jar', 200, 12, 'agent/1.0', ?)`)
	if err != nil {
		t.Fatalf("准备插入语句：%v", err)
	}
	now := time.Now().UTC()
	for i := 0; i < n; i++ {
		at := now.Add(-time.Duration(i) * time.Second).Format(time.RFC3339Nano)
		if _, err := stmt.Exec(
			at,
			fmt.Sprintf("release/pkg-%d.jar", i),
			strings.Repeat("x", 2048),
			strings.Repeat("y", 2048),
		); err != nil {
			t.Fatalf("插入第 %d 条审计事件：%v", i, err)
		}
	}
	if err := stmt.Close(); err != nil {
		t.Fatalf("关闭插入语句：%v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("提交事务：%v", err)
	}
}
