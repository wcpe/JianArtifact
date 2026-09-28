package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// 审计事件流按仓库筛选必须按「主名 ∪ 别名」全集处理（ADR-0028）：仓库有别名（或重命名后
// 旧名自动转别名）时，按当前主名筛仍要能查到记在别名 / 旧名下的历史事件，否则会静默漏检。
func TestAuditEventsRepositoryFilterExpandsAliases(t *testing.T) {
	db := openAPITestDB(t)
	users := repository.NewUserRepo(db)
	if _, err := users.Create("admin", "hash", "admin"); err != nil {
		t.Fatalf("创建 admin：%v", err)
	}
	settings := domain.NewSettingService(repository.NewSettingRepo(db))
	repoSvc := domain.NewRepositoryService(
		repository.NewRepoRepo(db),
		repository.NewAclRepo(db),
		repository.NewAssetRepo(db),
		settings,
		users,
	)
	// 主名 evt-main、别名 evt-old。
	if _, err := repoSvc.Create("evt-main", "raw", "hosted", "public", "", repository.RepositoryConfig{}, "evt-old"); err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	auditLogs := repository.NewAuditLogRepo(db)
	ts := time.Now().UTC().Format(time.RFC3339Nano)
	// 各写一条：一条记在主名下、一条记在别名（旧名）下。
	for _, name := range []string{"evt-main", "evt-old"} {
		if err := auditLogs.Insert(repository.AuditLogEntry{
			TS: ts, Actor: "admin", Action: "asset.put", EntityType: "asset",
			EntityKey: name + "/a.bin", Repo: name, Result: "ok",
		}); err != nil {
			t.Fatalf("写入 %s 的审计：%v", name, err)
		}
	}

	handler := NewHandlers(Deps{
		Repos:              repoSvc,
		Settings:           settings,
		AuditObservability: repository.NewAuditObservabilityRepo(db),
		AuditSourceNode:    "node-a",
	})
	from := time.Now().UTC().Add(-time.Hour)
	to := time.Now().UTC().Add(time.Minute)
	name := "evt-main"

	rec := serveAuditEventsList(handler, ListAuditObservabilityEventsParams{From: &from, To: &to, Repository: &name})
	if rec.Code != http.StatusOK {
		t.Fatalf("审计事件流状态码 = %d（体：%s）", rec.Code, rec.Body.String())
	}
	var page AuditEventPage
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("解析事件流：%v", err)
	}
	// 按主名筛：主名与别名下的两条都应命中（只按主名等值匹配时只有 1 条）。
	// 注：事件流的 totalCount 恒为 -1（服务端不执行精确 COUNT，见契约），故断言 items 条数。
	if len(page.Items) != 2 {
		t.Fatalf("按主名筛应命中主名 + 别名两条事件，得 %d（total=%d）", len(page.Items), page.TotalCount)
	}

	// 按别名筛同样应命中两条（集合对称）。
	alias := "evt-old"
	rec = serveAuditEventsList(handler, ListAuditObservabilityEventsParams{From: &from, To: &to, Repository: &alias})
	if rec.Code != http.StatusOK {
		t.Fatalf("按别名筛状态码 = %d（体：%s）", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("解析事件流：%v", err)
	}
	if len(page.Items) != 2 {
		t.Fatalf("按别名筛应命中主名 + 别名两条事件，得 %d", len(page.Items))
	}

	// 未知名退回字面量：筛不到任何记录（与既有行为一致，不报错）。
	ghost := "evt-ghost"
	rec = serveAuditEventsList(handler, ListAuditObservabilityEventsParams{From: &from, To: &to, Repository: &ghost})
	if rec.Code != http.StatusOK {
		t.Fatalf("未知名筛选状态码 = %d（体：%s）", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("解析事件流：%v", err)
	}
	if len(page.Items) != 0 {
		t.Fatalf("未知名筛选应无命中，得 %d", len(page.Items))
	}
}

// serveAuditEventsList 以管理员身份调用 GET /observability/audit/events。
func serveAuditEventsList(handler *Handlers, params ListAuditObservabilityEventsParams) *httptest.ResponseRecorder {
	router := gin.New()
	router.GET("/events", func(c *gin.Context) {
		c.Set("auth.principal", &auth.Principal{Role: "admin"})
		handler.ListAuditObservabilityEvents(c, params)
	})
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/events", nil))
	return rec
}
