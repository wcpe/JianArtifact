package repository

import (
	"path/filepath"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
)

func TestReplicationApplyLogObserveIsIdempotentAndPreservesError(t *testing.T) {
	db, err := persistence.Open(filepath.Join(t.TempDir(), "apply-log.db"))
	if err != nil {
		t.Fatalf("打开数据库：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("迁移：%v", err)
	}
	repo := NewReplicationApplyLogRepo(db)
	failed := ReplicationApplyLog{
		SourceNode:  "node-a",
		SourceSeq:   7,
		PeerURL:     "https://peer.example",
		EntityType:  "user",
		EntityKey:   "user:alice",
		Op:          "put",
		Result:      "failed",
		Detail:      "应用失败",
		LastError:   "旧错误证据",
		LastErrorAt: "2026-08-18T00:00:00Z",
		FirstSeenAt: "2026-08-18T00:00:00Z",
		LastSeenAt:  "2026-08-18T00:00:00Z",
	}
	if err := repo.Observe(failed); err != nil {
		t.Fatalf("首次观察：%v", err)
	}
	if err := repo.Observe(ReplicationApplyLog{
		SourceNode:  failed.SourceNode,
		SourceSeq:   failed.SourceSeq,
		PeerURL:     "https://relay.example",
		EntityType:  failed.EntityType,
		EntityKey:   failed.EntityKey,
		Op:          failed.Op,
		Result:      "applied",
		Detail:      "应用成功",
		FirstSeenAt: "2026-08-18T00:00:01Z",
		LastSeenAt:  "2026-08-18T00:00:01Z",
	}); err != nil {
		t.Fatalf("重复观察：%v", err)
	}
	got, err := repo.Get(failed.SourceNode, failed.SourceSeq)
	if err != nil {
		t.Fatalf("读取记录：%v", err)
	}
	if got.AttemptCount != 2 || got.Result != "applied" {
		t.Fatalf("幂等合并结果不符：%+v", got)
	}
	if got.PeerURL != "https://relay.example" {
		t.Fatalf("同一来源事件经不同 peer 重放时应保持单条记录并更新最近 peer：%+v", got)
	}
	if got.LastError != failed.LastError || got.LastErrorAt != failed.LastErrorAt {
		t.Fatalf("成功后必须保留错误证据：%+v", got)
	}
}
