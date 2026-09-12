package httpserver_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/api"
	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/httpserver"
	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

func TestAuditObservabilitySnapshotAndAtomicAcknowledgement(t *testing.T) {
	db, err := persistence.Open(filepath.Join(t.TempDir(), "audit-observability.db"))
	if err != nil {
		t.Fatalf("打开数据库：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("迁移数据库：%v", err)
	}
	audits := repository.NewAuditLogRepo(db)
	now := time.Now().UTC()
	batchTime := now.Truncate(time.Minute).Add(-40 * time.Second)
	for _, entry := range []repository.AuditLogEntry{
		{TS: batchTime.Format(time.RFC3339Nano), Actor: "admin-a", Action: "repository.delete", EntityType: "repository", EntityKey: "release", Repo: "release", Result: "ok", SourceNode: "node-a", CorrelationID: "op-1"},
		{TS: batchTime.Add(10 * time.Second).Format(time.RFC3339Nano), Actor: "admin-a", Action: "user.update", EntityType: "user", EntityKey: "release-user", Result: "rejected", SourceNode: "node-a", CorrelationID: "op-1"},
	} {
		if err := audits.Insert(entry); err != nil {
			t.Fatalf("写入审计：%v", err)
		}
	}
	handlers := api.NewHandlers(api.Deps{AuditLogs: audits, AuditObservability: repository.NewAuditObservabilityRepo(db), AuditSourceNode: "node-a", AuditAttentionKey: []byte("audit-observability-test-key")})
	server := httpserver.New("test", httpserver.WithHandlers(handlers), httpserver.WithMiddleware(func(c *gin.Context) {
		c.Set("auth.principal", &auth.Principal{UserID: 11, Username: "admin-a", Role: "admin", AuthSource: auth.AuthSourceWebJWT})
		c.Next()
	}))
	router := server.Handler(nil)

	get := func(path string, out any) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if out != nil && rec.Code == http.StatusOK {
			if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
				t.Fatalf("解析 %s：%v", path, err)
			}
		}
		return rec
	}
	var summary api.AuditObservabilitySummary
	if rec := get("/api/v1/observability/audit/summary", &summary); rec.Code != http.StatusOK {
		t.Fatalf("summary 状态码=%d，体=%s", rec.Code, rec.Body.String())
	}
	if summary.TotalCount != 2 || summary.FailureCount != 1 || summary.Snapshot == "" || summary.SnapshotAt.IsZero() {
		t.Fatalf("summary 统计不符：%+v", summary)
	}
	var events api.AuditEventPage
	if rec := get("/api/v1/observability/audit/events", &events); rec.Code != http.StatusOK {
		t.Fatalf("events 状态码=%d，体=%s", rec.Code, rec.Body.String())
	}
	if events.Snapshot == "" || len(events.Items) != 2 {
		t.Fatalf("事件快照或条目不符：%+v", events)
	}
	attentionID := ""
	for _, event := range events.Items {
		if event.Attention != nil {
			attentionID = event.Attention.AttentionId
			break
		}
	}
	if attentionID == "" {
		t.Fatal("风险事件应获得服务端 attentionId")
	}
	requestBody := []byte(`{"attentionId":"` + attentionID + `"}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/v1/observability/audit/attention-acknowledgements", bytes.NewReader(requestBody))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("首次确认状态码=%d，体=%s", rec.Code, rec.Body.String())
	}
	var acknowledged api.AcknowledgeAuditAttentionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &acknowledged); err != nil {
		t.Fatal(err)
	}
	if acknowledged.NewlyAcknowledgedCount == 0 || acknowledged.UnacknowledgedRiskEventCount != 0 {
		t.Fatalf("首次确认结果不符：%+v", acknowledged)
	}
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/api/v1/observability/audit/attention-acknowledgements", bytes.NewReader(requestBody)))
	if rec.Code != http.StatusOK {
		t.Fatalf("重复确认状态码=%d，体=%s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &acknowledged); err != nil {
		t.Fatal(err)
	}
	if acknowledged.NewlyAcknowledgedCount != 0 || acknowledged.FirstAcknowledgement.AcknowledgedBy.DisplayName != "admin-a" {
		t.Fatalf("重复确认必须保留首个确认人：%+v", acknowledged)
	}
	var notifications api.AuditAttentionNotificationList
	if rec := get("/api/v1/observability/audit/notifications", &notifications); rec.Code != http.StatusOK || notifications.TotalUnacknowledged == nil || *notifications.TotalUnacknowledged != 0 {
		t.Fatalf("确认后通知未清除：status=%d result=%+v", rec.Code, notifications)
	}
}

// FR-117：消息中心依赖 notifications 端点的筛选、分页与排序扩展；缺省请求保持页眉口径。
func TestAuditNotificationCenterPaginationAndStatusFilter(t *testing.T) {
	db, err := persistence.Open(filepath.Join(t.TempDir(), "notification-center.db"))
	if err != nil {
		t.Fatalf("打开数据库：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("迁移数据库：%v", err)
	}
	audits := repository.NewAuditLogRepo(db)
	now := time.Now().UTC().Truncate(time.Minute)
	// 五个风险批次：一个稍旧的失败批次（将被确认）+ 四个未确认批次（失败/高风险混合）。
	for _, entry := range []repository.AuditLogEntry{
		{TS: now.Add(-time.Hour).Format(time.RFC3339Nano), Actor: "admin-a", Action: "asset.delete", EntityType: "asset", EntityKey: "old-acked", Repo: "release", Result: "rejected", SourceNode: "node-a"},
		{TS: now.Add(-30 * time.Minute).Format(time.RFC3339Nano), Actor: "admin-a", Action: "repository.delete", EntityType: "repository", EntityKey: "critical", Repo: "release", Result: "ok", SourceNode: "node-a"},
		{TS: now.Add(-20 * time.Minute).Format(time.RFC3339Nano), Actor: "admin-b", Action: "asset.delete", EntityType: "asset", EntityKey: "failed", Repo: "release", Result: "failed", SourceNode: "node-a"},
		{TS: now.Add(-10 * time.Minute).Format(time.RFC3339Nano), Actor: "admin-c", Action: "user.update", EntityType: "user", EntityKey: "rejected", Result: "rejected", SourceNode: "node-a"},
		{TS: now.Add(-5 * time.Minute).Format(time.RFC3339Nano), Actor: "admin-a", Action: "settings.update", EntityType: "setting", EntityKey: "tuned", Result: "ok", SourceNode: "node-a"},
	} {
		if err := audits.Insert(entry); err != nil {
			t.Fatalf("写入审计：%v", err)
		}
	}
	handlers := api.NewHandlers(api.Deps{AuditLogs: audits, AuditObservability: repository.NewAuditObservabilityRepo(db), AuditSourceNode: "node-a", AuditAttentionKey: []byte("notification-center-test-key")})
	server := httpserver.New("test", httpserver.WithHandlers(handlers), httpserver.WithMiddleware(func(c *gin.Context) {
		c.Set("auth.principal", &auth.Principal{UserID: 11, Username: "admin-a", Role: "admin", AuthSource: auth.AuthSourceWebJWT})
		c.Next()
	}))
	router := server.Handler(nil)

	getNotifications := func(query string) (api.AuditAttentionNotificationList, int) {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/observability/audit/notifications"+query, nil))
		var body api.AuditAttentionNotificationList
		if rec.Code == http.StatusOK {
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("解析通知响应：%v", err)
			}
		}
		return body, rec.Code
	}

	// 风险批次共 5 个，先把最旧的失败批次确认掉。
	all, code := getNotifications("?status=all&limit=100")
	if code != http.StatusOK || all.Total != 5 {
		t.Fatalf("status=all 应返回全部 5 个风险批次：code=%d body=%+v", code, all)
	}
	acked := all.Items[len(all.Items)-1]
	if acked.State != api.AuditAttentionStateUnacknowledged {
		t.Fatalf("确认前列表应全部未确认：%+v", acked)
	}
	ackBody := []byte(`{"attentionId":"` + acked.AttentionId + `"}`)
	rec := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPut, "/api/v1/observability/audit/attention-acknowledgements", bytes.NewReader(ackBody))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, request)
	if rec.Code != http.StatusOK {
		t.Fatalf("确认最旧批次状态码=%d，体=%s", rec.Code, rec.Body.String())
	}

	// 缺省请求：保持页眉口径 —— 仅未确认、totalUnacknowledged=total、失败优先排序。
	var def api.AuditAttentionNotificationList
	if body, code := getNotifications(""); code != http.StatusOK || body.TotalUnacknowledged == nil || body.Total != 4 || *body.TotalUnacknowledged != 4 {
		t.Fatalf("缺省请求口径不符：code=%d body=%+v", code, body)
	} else {
		def = body
	}
	for _, item := range def.Items {
		if item.State != api.AuditAttentionStateUnacknowledged {
			t.Fatalf("缺省请求不得混入已确认批次：%+v", item)
		}
	}
	if len(def.Items) == 0 || def.Items[0].FailureCount == 0 {
		t.Fatalf("缺省请求必须失败优先置顶：%+v", def.Items)
	}

	// status=acknowledged：只含已确认批次，且展示首个确认人快照。
	if body, code := getNotifications("?status=acknowledged"); code != http.StatusOK || body.Total != 1 || len(body.Items) != 1 {
		t.Fatalf("status=acknowledged 应只含 1 个已确认批次：code=%d body=%+v", code, body)
	} else if body.Items[0].FirstAcknowledgement == nil || body.Items[0].FirstAcknowledgement.AcknowledgedBy.DisplayName != "admin-a" {
		t.Fatalf("已确认批次必须携带确认人快照：%+v", body.Items)
	}

	// 显式请求时间排序：最新批次在前，不因失败置顶重排。
	if body, code := getNotifications("?status=all&limit=5"); code != http.StatusOK || len(body.Items) != 5 {
		t.Fatalf("status=all 时间排序应返回 5 条：code=%d body=%+v", code, body)
	} else if body.Items[0].Action != "settings.update" || body.Items[4].Action != "asset.delete" {
		t.Fatalf("显式请求必须按最新时间倒序：%+v", body.Items)
	}

	// limit/cursor 分页：逐页推进，nextCursor 终止时 hasMore=false。
	var collected []string
	cursor := ""
	for page := 0; ; page++ {
		query := "?status=all&limit=1" + cursor
		body, code := getNotifications(query)
		if code != http.StatusOK {
			t.Fatalf("分页请求 %s 状态码=%d", query, code)
		}
		if len(body.Items) != 1 {
			t.Fatalf("limit=1 必须恰好返回 1 条：%+v", body)
		}
		collected = append(collected, body.Items[0].AttentionId)
		if !body.HasMore {
			if body.NextCursor != nil {
				t.Fatalf("无下一页时不得返回 nextCursor：%+v", body)
			}
			break
		}
		if body.NextCursor == nil || page > 10 {
			t.Fatalf("hasMore=true 必须携带 nextCursor：%+v", body)
		}
		cursor = "&cursor=" + url.QueryEscape(*body.NextCursor)
	}
	if len(collected) != 5 || collected[0] == collected[1] {
		t.Fatalf("分页必须遍历全部批次且不重复：%v", collected)
	}

	// 参数校验。
	for _, invalid := range []string{"?from=" + now.Format(time.RFC3339Nano), "?status=bogus", "?limit=0", "?limit=101", "?cursor=-1"} {
		if _, code := getNotifications(invalid); code != http.StatusBadRequest {
			t.Fatalf("非法参数 %s 必须返回 400，实际 %d", invalid, code)
		}
	}
}

func TestAuditSnapshotRejectsFilterMismatch(t *testing.T) {
	db, err := persistence.Open(filepath.Join(t.TempDir(), "audit-filter-snapshot.db"))
	if err != nil {
		t.Fatalf("打开数据库：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("迁移数据库：%v", err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	if err := repository.NewAuditLogRepo(db).Insert(repository.AuditLogEntry{
		TS: now.Format(time.RFC3339Nano), Actor: "admin-a", Action: "user.update", EntityType: "user",
		EntityKey: "release-user", Repo: "release", Result: "rejected", SourceNode: "node-a",
	}); err != nil {
		t.Fatalf("写入审计：%v", err)
	}
	handlers := api.NewHandlers(api.Deps{AuditLogs: repository.NewAuditLogRepo(db), AuditObservability: repository.NewAuditObservabilityRepo(db), AuditSourceNode: "node-a", AuditAttentionKey: []byte("audit-observability-test-key")})
	server := httpserver.New("test", httpserver.WithHandlers(handlers), httpserver.WithMiddleware(func(c *gin.Context) {
		c.Set("auth.principal", &auth.Principal{UserID: 11, Username: "admin-a", Role: "admin", AuthSource: auth.AuthSourceWebJWT})
		c.Next()
	}))
	router := server.Handler(nil)
	from := now.Add(-time.Minute).Format(time.RFC3339Nano)
	to := now.Add(time.Minute).Format(time.RFC3339Nano)
	query := url.Values{"from": {from}, "to": {to}, "category": {"security_event"}, "result": {"failure"}, "actor": {"admin-a"}, "repository": {"release"}}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/observability/audit/summary?"+query.Encode(), nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("summary 状态码=%d，体=%s", recorder.Code, recorder.Body.String())
	}
	var summary api.AuditObservabilitySummary
	if err := json.Unmarshal(recorder.Body.Bytes(), &summary); err != nil {
		t.Fatalf("解析 summary：%v", err)
	}
	for name, value := range map[string]string{
		"category":   "management_change",
		"result":     "success",
		"actor":      "other-admin",
		"repository": "other-repository",
	} {
		changed := url.Values{}
		for key, values := range query {
			changed[key] = append([]string(nil), values...)
		}
		changed.Set(name, value)
		changed.Set("snapshot", summary.Snapshot)
		recorder = httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/observability/audit/events?"+changed.Encode(), nil))
		if recorder.Code != http.StatusConflict {
			t.Fatalf("变更 %s 后快照必须失效：status=%d body=%s", name, recorder.Code, recorder.Body.String())
		}
	}
}

func TestAuditAttentionKeepsSnapshotFilters(t *testing.T) {
	db, err := persistence.Open(filepath.Join(t.TempDir(), "audit-attention-filter.db"))
	if err != nil {
		t.Fatalf("打开数据库：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("迁移数据库：%v", err)
	}
	now := time.Now().UTC().Truncate(time.Minute)
	audits := repository.NewAuditLogRepo(db)
	for _, entry := range []repository.AuditLogEntry{
		{TS: now.Format(time.RFC3339Nano), Actor: "admin-a", Action: "asset.delete", EntityType: "asset", EntityKey: "selected", Repo: "release", Result: "rejected", SourceNode: "node-a", CorrelationID: "operation-filtered"},
		{TS: now.Add(time.Second).Format(time.RFC3339Nano), Actor: "admin-b", Action: "asset.delete", EntityType: "asset", EntityKey: "outside", Repo: "release", Result: "rejected", SourceNode: "node-a", CorrelationID: "operation-filtered"},
	} {
		if err := audits.Insert(entry); err != nil {
			t.Fatalf("写入审计：%v", err)
		}
	}
	handlers := api.NewHandlers(api.Deps{AuditLogs: audits, AuditObservability: repository.NewAuditObservabilityRepo(db), AuditSourceNode: "node-a", AuditAttentionKey: []byte("audit-observability-test-key")})
	server := httpserver.New("test", httpserver.WithHandlers(handlers), httpserver.WithMiddleware(func(c *gin.Context) {
		c.Set("auth.principal", &auth.Principal{UserID: 11, Username: "admin-a", Role: "admin", AuthSource: auth.AuthSourceWebJWT})
		c.Next()
	}))
	router := server.Handler(nil)
	query := url.Values{"from": {now.Add(-time.Minute).Format(time.RFC3339Nano)}, "to": {now.Add(time.Minute).Format(time.RFC3339Nano)}, "actor": {"admin-a"}}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/observability/audit/events?"+query.Encode(), nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("筛选事件状态码=%d，体=%s", recorder.Code, recorder.Body.String())
	}
	var page api.AuditEventPage
	if err := json.Unmarshal(recorder.Body.Bytes(), &page); err != nil {
		t.Fatalf("解析事件页：%v", err)
	}
	if len(page.Items) != 1 || page.Items[0].Attention == nil {
		t.Fatalf("筛选事件未得到唯一关注项：%+v", page)
	}
	attentionID := page.Items[0].Attention.AttentionId
	recorder = httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/observability/audit/attention/"+url.PathEscape(attentionID), nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("关注详情状态码=%d，体=%s", recorder.Code, recorder.Body.String())
	}
	var detail api.AuditAttentionDetail
	if err := json.Unmarshal(recorder.Body.Bytes(), &detail); err != nil {
		t.Fatalf("解析关注详情：%v", err)
	}
	if detail.TotalCount != 1 || len(detail.Items) != 1 || detail.Items[0].Actor.DisplayName != "admin-a" {
		t.Fatalf("关注详情扩大到了筛选外成员：%+v", detail)
	}
	body := []byte(`{"attentionId":"` + attentionID + `"}`)
	recorder = httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPut, "/api/v1/observability/audit/attention-acknowledgements", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("确认关注状态码=%d，体=%s", recorder.Code, recorder.Body.String())
	}
	var acknowledged api.AcknowledgeAuditAttentionResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &acknowledged); err != nil {
		t.Fatalf("解析确认结果：%v", err)
	}
	if acknowledged.TotalRiskEventCount != 1 {
		t.Fatalf("确认不应覆盖筛选外成员：%+v", acknowledged)
	}
}

func TestAuditAttentionQueueUsesSnapshotAndPaginatesGroups(t *testing.T) {
	db, err := persistence.Open(filepath.Join(t.TempDir(), "audit-attention-queue.db"))
	if err != nil {
		t.Fatalf("打开数据库：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("迁移数据库：%v", err)
	}

	now := time.Now().UTC().Truncate(time.Minute)
	audits := repository.NewAuditLogRepo(db)
	for index, entry := range []repository.AuditLogEntry{
		{TS: now.Add(-3 * time.Minute).Format(time.RFC3339Nano), Actor: "admin-a", Action: "asset.delete", EntityType: "asset", EntityKey: "one", Result: "rejected", SourceNode: "node-a"},
		{TS: now.Add(-2 * time.Minute).Format(time.RFC3339Nano), Actor: "admin-a", Action: "asset.delete", EntityType: "asset", EntityKey: "two", Result: "rejected", SourceNode: "node-a"},
		{TS: now.Add(-time.Minute).Format(time.RFC3339Nano), Actor: "admin-a", Action: "asset.delete", EntityType: "asset", EntityKey: "three", Result: "rejected", SourceNode: "node-a"},
	} {
		if err := audits.Insert(entry); err != nil {
			t.Fatalf("写入审计 %d：%v", index, err)
		}
	}

	handlers := api.NewHandlers(api.Deps{AuditLogs: audits, AuditObservability: repository.NewAuditObservabilityRepo(db), AuditSourceNode: "node-a", AuditAttentionKey: []byte("audit-attention-queue-test-key")})
	server := httpserver.New("test", httpserver.WithHandlers(handlers), httpserver.WithMiddleware(func(c *gin.Context) {
		c.Set("auth.principal", &auth.Principal{UserID: 11, Username: "admin-a", Role: "admin", AuthSource: auth.AuthSourceWebJWT})
		c.Next()
	}))
	router := server.Handler(nil)
	get := func(query url.Values) (api.AuditAttentionPage, *httptest.ResponseRecorder) {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/observability/audit/attentions?"+query.Encode(), nil))
		var page api.AuditAttentionPage
		if recorder.Code == http.StatusOK {
			if err := json.Unmarshal(recorder.Body.Bytes(), &page); err != nil {
				t.Fatalf("解析关注队列：%v", err)
			}
		}
		return page, recorder
	}
	query := url.Values{
		"from":  {now.Add(-10 * time.Minute).Format(time.RFC3339Nano)},
		"to":    {now.Add(time.Minute).Format(time.RFC3339Nano)},
		"limit": {"1"},
	}
	first, recorder := get(query)
	if recorder.Code != http.StatusOK {
		t.Fatalf("关注队列首屏状态码=%d，体=%s", recorder.Code, recorder.Body.String())
	}
	if first.TotalCount != 3 || len(first.Items) != 1 || first.Snapshot == "" || first.SnapshotAt.IsZero() || first.NextCursor == nil {
		t.Fatalf("关注队列首屏不符：%+v", first)
	}
	if err := audits.Insert(repository.AuditLogEntry{TS: now.Format(time.RFC3339Nano), Actor: "admin-a", Action: "asset.delete", EntityType: "asset", EntityKey: "after-snapshot", Result: "rejected", SourceNode: "node-a"}); err != nil {
		t.Fatalf("写入快照后审计：%v", err)
	}
	query.Set("snapshot", first.Snapshot)
	query.Set("cursor", *first.NextCursor)
	second, recorder := get(query)
	if recorder.Code != http.StatusOK {
		t.Fatalf("关注队列续页状态码=%d，体=%s", recorder.Code, recorder.Body.String())
	}
	if second.TotalCount != 3 || len(second.Items) != 1 || second.Items[0].AttentionId == first.Items[0].AttentionId {
		t.Fatalf("同快照续页不得引入新批次或重复首项：%+v", second)
	}
	query.Set("actor", "other-admin")
	if _, recorder := get(query); recorder.Code != http.StatusConflict {
		t.Fatalf("快照筛选变化必须失效：status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestAuditAttentionQueueRequiresAdmin(t *testing.T) {
	handlers := api.NewHandlers(api.Deps{})
	server := httpserver.New("test", httpserver.WithHandlers(handlers), httpserver.WithMiddleware(func(c *gin.Context) {
		c.Set("auth.principal", &auth.Principal{UserID: 12, Username: "member", Role: "user", AuthSource: auth.AuthSourceWebJWT})
		c.Next()
	}))
	recorder := httptest.NewRecorder()
	server.Handler(nil).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/observability/audit/attentions", nil))
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("非管理员访问关注队列必须返回 403：status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}
