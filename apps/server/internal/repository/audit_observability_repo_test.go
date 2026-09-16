package repository

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
)

func TestAuditObservabilityRepoPagesWithinSnapshot(t *testing.T) {
	db, err := persistence.Open(filepath.Join(t.TempDir(), "observability-page.db"))
	if err != nil {
		t.Fatalf("打开数据库：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("迁移数据库：%v", err)
	}

	audits := NewAuditLogRepo(db)
	now := time.Now().UTC().Truncate(time.Second)
	for index := 0; index < 3; index++ {
		if err := audits.Insert(AuditLogEntry{
			TS:         now.Add(time.Duration(index) * time.Second).Format(time.RFC3339Nano),
			Actor:      "admin",
			Action:     "repository.delete",
			EntityType: "repository",
			EntityKey:  "release",
			Repo:       "release",
			Result:     "ok",
			SourceNode: "node-a",
		}); err != nil {
			t.Fatalf("写入审计：%v", err)
		}
	}
	if err := audits.Insert(AuditLogEntry{
		TS:         now.Add(4 * time.Second).Format(time.RFC3339Nano),
		Actor:      "other-admin",
		Action:     "user.update",
		EntityType: "user",
		EntityKey:  "release-user",
		Repo:       "other-release",
		Result:     "rejected",
		SourceNode: "node-a",
	}); err != nil {
		t.Fatalf("写入筛选审计：%v", err)
	}

	repo := NewAuditObservabilityRepo(db)
	filter := ObservabilityFilter{
		From:             now.Add(-time.Second).Format(time.RFC3339Nano),
		To:               now.Add(time.Minute).Format(time.RFC3339Nano),
		AuditMaxID:       2,
		ReplicationMaxID: 0,
		SourceNode:       "node-a",
	}
	page, total, next, err := repo.ListEventPage(filter, 0, 1)
	if err != nil {
		t.Fatalf("读取第一页：%v", err)
	}
	if total != 2 || len(page) != 1 || next == nil || page[0].SourceEventID != 2 {
		t.Fatalf("第一页未受快照边界约束：total=%d page=%+v next=%v", total, page, next)
	}
	page, total, next, err = repo.ListEventPage(filter, *next, 1)
	if err != nil {
		t.Fatalf("读取第二页：%v", err)
	}
	if total != 2 || len(page) != 1 || next != nil || page[0].SourceEventID != 1 {
		t.Fatalf("第二页不稳定：total=%d page=%+v next=%v", total, page, next)
	}
	filtered := filter
	filtered.AuditMaxID = 4
	filtered.Categories = []string{"security_event"}
	filtered.Results = []string{"failure"}
	filtered.Actor = "other-admin"
	filtered.Repository = "other-release"
	page, total, next, err = repo.ListEventPage(filtered, 0, 1)
	if err != nil {
		t.Fatalf("读取筛选页：%v", err)
	}
	if total != 1 || len(page) != 1 || next != nil || page[0].SourceEventID != 4 {
		t.Fatalf("数据库分页未同时应用筛选与快照：total=%d page=%+v next=%v", total, page, next)
	}
	limited, tooLarge, err := repo.ListEventsLimited(filter, 1)
	if err != nil {
		t.Fatalf("读取受限聚合事件：%v", err)
	}
	if !tooLarge || len(limited) != 1 {
		t.Fatalf("聚合读取必须在上限处拒绝全量加载：tooLarge=%t events=%+v", tooLarge, limited)
	}

	event, err := repo.EventByID("audit", 2, "node-a")
	if err != nil {
		t.Fatalf("按标识读取事件：%v", err)
	}
	if event.SourceEventID != 2 || event.Action != "repository.delete" {
		t.Fatalf("按标识读取了错误事件：%+v", event)
	}
}

func TestAcknowledgeAndAuditRollsBackTogether(t *testing.T) {
	db, err := persistence.Open(filepath.Join(t.TempDir(), "observability-ack.db"))
	if err != nil {
		t.Fatalf("打开数据库：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("迁移数据库：%v", err)
	}
	if _, err := db.Exec(`CREATE TRIGGER reject_attention_audit BEFORE INSERT ON audit_log
		WHEN NEW.action = 'audit.attention_acknowledge'
		BEGIN SELECT RAISE(ABORT, '拒绝确认审计'); END`); err != nil {
		t.Fatalf("创建失败注入：%v", err)
	}

	repo := NewAuditObservabilityRepo(db)
	userID := int64(1)
	_, _, err = repo.AcknowledgeAndAudit(
		[]ObservabilityEvent{{Source: "audit", SourceEventID: 99}},
		&userID,
		"admin",
		"web",
		time.Now().UTC().Format(time.RFC3339Nano),
		AuditLogEntry{TS: time.Now().UTC().Format(time.RFC3339Nano), Action: "audit.attention_acknowledge"},
	)
	if err == nil {
		t.Fatal("确认审计失败时应整体回滚")
	}
	var acknowledgements int
	if err := db.Get(&acknowledgements, `SELECT COUNT(*) FROM audit_attention_ack`); err != nil {
		t.Fatalf("查询确认行：%v", err)
	}
	if acknowledgements != 0 {
		t.Fatalf("审计插入失败后不得遗留确认行：%d", acknowledgements)
	}
}

// TestAuditAggregateReadUsesNarrowProjection 覆盖线上 bug 的根因：
// 聚合读取以前用全量投影 + 5000 上限，而线上 24h 窗口早就过万级事件量
// （实测：上线第 3 天 24h 已有 5900 条），默认审计总览因此直接 409。
// 现在聚合走窄投影（不读 detail / body_preview / user_agent），上限提高一个数量级；
// 详情路径继续用全量投影，两条路径的取舍必须各自成立。
func TestAuditAggregateReadUsesNarrowProjection(t *testing.T) {
	db, err := persistence.Open(filepath.Join(t.TempDir(), "observability-aggregate.db"))
	if err != nil {
		t.Fatalf("打开数据库：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("迁移数据库：%v", err)
	}

	const seeded = 6000
	now := time.Now().UTC()
	tx, err := db.Beginx()
	if err != nil {
		t.Fatalf("开启事务：%v", err)
	}
	stmt, err := tx.Prepare(`INSERT INTO audit_log
		(ts, actor, action, entity_type, entity_key, repo, detail, result, ip, source_node,
		 status_code, duration_ms, user_agent, body_preview)
		VALUES (?, 'admin', 'asset.put', 'asset', ?, 'release', ?, 'ok', '203.0.113.7', '',
		 200, 12, 'agent/1.0', ?)`)
	if err != nil {
		t.Fatalf("准备插入语句：%v", err)
	}
	for index := 0; index < seeded; index++ {
		at := now.Add(-time.Duration(index) * time.Second).Format(time.RFC3339Nano)
		if _, err := stmt.Exec(at, fmt.Sprintf("release/pkg-%d.jar", index), strings.Repeat("x", 1024), strings.Repeat("y", 1024)); err != nil {
			t.Fatalf("插入第 %d 条审计事件：%v", index, err)
		}
	}
	if err := stmt.Close(); err != nil {
		t.Fatalf("关闭插入语句：%v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("提交事务：%v", err)
	}

	repo := NewAuditObservabilityRepo(db)
	filter := ObservabilityFilter{
		From:       now.Add(-24 * time.Hour).Format(time.RFC3339Nano),
		To:         now.Add(time.Minute).Format(time.RFC3339Nano),
		AuditMaxID: seeded,
	}

	// 护栏语义未被削弱：超过传入上限仍判定超限。
	if _, tooLarge, err := repo.ListAggregateEventsLimited(filter, 5000); err != nil || !tooLarge {
		t.Fatalf("聚合读取超过上限时应判定超限：tooLarge=%v err=%v", tooLarge, err)
	}
	// 提高后的上限下取全量：默认 24h 视图可用。
	events, tooLarge, err := repo.ListAggregateEventsLimited(filter, 50000)
	if err != nil {
		t.Fatalf("聚合读取：%v", err)
	}
	if tooLarge || len(events) != seeded {
		t.Fatalf("聚合读取应取到 %d 条且不超限：got=%d tooLarge=%v", seeded, len(events), tooLarge)
	}
	for _, event := range events {
		if event.Detail != "" || event.BodyPreview != "" || event.UserAgent != "" {
			t.Fatalf("窄投影不得读取大字段：detail=%d body=%d userAgent=%d", len(event.Detail), len(event.BodyPreview), len(event.UserAgent))
		}
	}
	// 详情路径（全量投影）仍能拿到大字段。
	full, _, err := repo.ListEventsLimited(filter, 10)
	if err != nil {
		t.Fatalf("详情读取：%v", err)
	}
	if len(full) != 10 || full[0].BodyPreview == "" || full[0].UserAgent == "" {
		t.Fatalf("详情读取必须保留大字段：count=%d body=%q userAgent=%q", len(full), full[0].BodyPreview, full[0].UserAgent)
	}
}
