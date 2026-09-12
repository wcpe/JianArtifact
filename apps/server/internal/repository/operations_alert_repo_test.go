package repository

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
)

// TestOperationsAlertRepoDedupsByCodeAndSource 验证：同一 (code, source) 只存一行，
// 首次发现时间保留、最近观察时间滚动；恢复后清除。
func TestOperationsAlertRepoDedupsByCodeAndSource(t *testing.T) {
	db, err := persistence.Open(filepath.Join(t.TempDir(), "operations-alert.db"))
	if err != nil {
		t.Fatalf("打开数据库：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("迁移数据库：%v", err)
	}
	repo := NewOperationsAlertRepo(db)

	firstAt := FormatMetricTime(time.Date(2026, 8, 30, 6, 0, 0, 0, time.UTC))
	secondAt := FormatMetricTime(time.Date(2026, 8, 30, 6, 5, 0, 0, time.UTC))
	blockedUntil := FormatMetricTime(time.Date(2026, 8, 30, 7, 0, 0, 0, time.UTC))

	// 同一上游首次出现：插入一行。
	if err := repo.UpsertActive([]OperationsAlertRow{
		{Code: "upstream_auto_blocked", Source: "repository:maven-central", Severity: "warning", FirstObservedAt: firstAt, LastObservedAt: firstAt, BlockedUntil: &blockedUntil},
	}); err != nil {
		t.Fatalf("首次写入告警：%v", err)
	}
	// 再次观察到同一上游：应合并而非新增，first 保留、last 滚动。
	if err := repo.UpsertActive([]OperationsAlertRow{
		{Code: "upstream_auto_blocked", Source: "repository:maven-central", Severity: "warning", FirstObservedAt: secondAt, LastObservedAt: secondAt, BlockedUntil: &blockedUntil},
	}); err != nil {
		t.Fatalf("再次写入告警：%v", err)
	}
	rows, err := repo.List()
	if err != nil {
		t.Fatalf("读取告警：%v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("去重失败：同 (code,source) 应只存一行，实际 %d 行", len(rows))
	}
	if rows[0].FirstObservedAt != firstAt {
		t.Fatalf("first_observed_at 被覆盖：%s，期望保留 %s", rows[0].FirstObservedAt, firstAt)
	}
	if rows[0].LastObservedAt != secondAt {
		t.Fatalf("last_observed_at 未滚动：%s，期望 %s", rows[0].LastObservedAt, secondAt)
	}
	if rows[0].BlockedUntil == nil || *rows[0].BlockedUntil != blockedUntil {
		t.Fatalf("blocked_until 未持久化：%v", rows[0].BlockedUntil)
	}

	// 健康恢复：不再活跃的告警应被清除。
	if err := repo.RemoveRecovered(map[string]bool{}); err != nil {
		t.Fatalf("清理恢复告警：%v", err)
	}
	rows, err = repo.List()
	if err != nil {
		t.Fatalf("读取清理后告警：%v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("恢复后告警未清除：%+v", rows)
	}
}

// TestOperationsAlertRepoKeepsActiveKeys 验证 RemoveRecovered 只清理不活跃项。
func TestOperationsAlertRepoKeepsActiveKeys(t *testing.T) {
	db, err := persistence.Open(filepath.Join(t.TempDir(), "operations-alert-keep.db"))
	if err != nil {
		t.Fatalf("打开数据库：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("迁移数据库：%v", err)
	}
	repo := NewOperationsAlertRepo(db)
	at := FormatMetricTime(time.Now().UTC())
	if err := repo.UpsertActive([]OperationsAlertRow{
		{Code: "upstream_auto_blocked", Source: "repository:maven-central", Severity: "warning", FirstObservedAt: at, LastObservedAt: at},
		{Code: "sync_failed", Source: "replication", Severity: "warning", FirstObservedAt: at, LastObservedAt: at},
	}); err != nil {
		t.Fatalf("写入告警：%v", err)
	}
	if err := repo.RemoveRecovered(map[string]bool{"upstream_auto_blocked\x00repository:maven-central": true}); err != nil {
		t.Fatalf("清理恢复告警：%v", err)
	}
	rows, err := repo.List()
	if err != nil {
		t.Fatalf("读取告警：%v", err)
	}
	if len(rows) != 1 || rows[0].Code != "upstream_auto_blocked" {
		t.Fatalf("保留活跃告警失败：%+v", rows)
	}
}
