package repository

import (
	"path/filepath"
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
