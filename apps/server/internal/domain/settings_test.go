package domain_test

import (
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// recordingRecorder 记录复制变更日志调用的测试替身（ChangeRecorder）。
type recordingRecorder struct {
	records []repository.Change
}

func (r *recordingRecorder) Record(entityType, entityKey, op string, data any) error {
	r.records = append(r.records, repository.Change{EntityType: entityType, EntityKey: entityKey, Op: op})
	return nil
}

// TestSettingsDynamicConfig 基础配置动态化（FR-89）：缺省回退、写后读一致、非法值归零。
func TestSettingsDynamicConfig(t *testing.T) {
	db := newTestDB(t)
	settings := repository.NewSettingRepo(db)
	svc := domain.NewSettingService(settings)

	// 缺省：未配置返回空 / 0。
	if got := svc.PublicURL(); got != "" {
		t.Errorf("缺省 PublicURL = %q，期望空串", got)
	}
	if got := svc.SyncIntervalSecs(); got != 0 {
		t.Errorf("缺省 SyncIntervalSecs = %d，期望 0", got)
	}
	if got := svc.UpstreamTimeoutSecs(); got != 0 {
		t.Errorf("缺省 UpstreamTimeoutSecs = %d，期望 0", got)
	}

	// 写后读一致。
	if err := svc.SetPublicURL("https://repo.wcpe.top"); err != nil {
		t.Fatalf("SetPublicURL：%v", err)
	}
	if got := svc.PublicURL(); got != "https://repo.wcpe.top" {
		t.Errorf("PublicURL = %q，期望 https://repo.wcpe.top", got)
	}
	if err := svc.SetSyncInterval(10); err != nil {
		t.Fatalf("SetSyncInterval：%v", err)
	}
	if got := svc.SyncIntervalSecs(); got != 10 {
		t.Errorf("SyncIntervalSecs = %d，期望 10", got)
	}
	if err := svc.SetUpstreamTimeout(60); err != nil {
		t.Fatalf("SetUpstreamTimeout：%v", err)
	}
	if got := svc.UpstreamTimeoutSecs(); got != 60 {
		t.Errorf("UpstreamTimeoutSecs = %d，期望 60", got)
	}

	// 清空对外 URL（空串 = 未配置）。
	if err := svc.SetPublicURL(""); err != nil {
		t.Fatalf("SetPublicURL(\"\")：%v", err)
	}
	if got := svc.PublicURL(); got != "" {
		t.Errorf("清空后 PublicURL = %q，期望空串", got)
	}

	// 非法值读取归零（直接写 setting 表模拟脏数据）。
	if err := settings.Set(domain.SettingKeyReplSyncInterval, "abc"); err != nil {
		t.Fatalf("写脏数据：%v", err)
	}
	if got := svc.SyncIntervalSecs(); got != 0 {
		t.Errorf("非法 SyncIntervalSecs = %d，期望 0", got)
	}
	if err := settings.Set(domain.SettingKeyReplSyncInterval, "0"); err != nil {
		t.Fatalf("写脏数据：%v", err)
	}
	if got := svc.SyncIntervalSecs(); got != 0 {
		t.Errorf("越界 SyncIntervalSecs = %d，期望 0", got)
	}
}

// TestSettingsWriteRecordsChange 设置写入记录复制变更日志（FR-83 链路）。
func TestSettingsWriteRecordsChange(t *testing.T) {
	db := newTestDB(t)
	settings := repository.NewSettingRepo(db)
	svc := domain.NewSettingService(settings)
	rec := &recordingRecorder{}
	svc.SetChangeRecorder(rec)

	if err := svc.SetUpstreamTimeout(45); err != nil {
		t.Fatalf("SetUpstreamTimeout：%v", err)
	}
	if len(rec.records) != 1 {
		t.Fatalf("应记录 1 条变更日志，得 %d", len(rec.records))
	}
	if rec.records[0].EntityType != domain.EntitySetting || rec.records[0].Op != domain.OpPut {
		t.Errorf("变更日志字段不符：%+v", rec.records[0])
	}
}

// TestSettingsClusterKeyNotReplicated 集群配置键（repl:*）绝不记录复制变更日志，
// 避免同步间隔等节点本地配置被复制到对端造成混乱（用户约束）。
func TestSettingsClusterKeyNotReplicated(t *testing.T) {
	db := newTestDB(t)
	settings := repository.NewSettingRepo(db)
	svc := domain.NewSettingService(settings)
	rec := &recordingRecorder{}
	svc.SetChangeRecorder(rec)

	// SetSyncInterval 写 repl:sync_interval（集群键）→ 不应记录变更日志。
	if err := svc.SetSyncInterval(10); err != nil {
		t.Fatalf("SetSyncInterval：%v", err)
	}
	if len(rec.records) != 0 {
		t.Fatalf("集群键 repl:sync_interval 不应记录复制变更，得 %d 条：%+v", len(rec.records), rec.records)
	}

	// 对照：业务键 SetUpstreamTimeout 仍应记录。
	if err := svc.SetUpstreamTimeout(45); err != nil {
		t.Fatalf("SetUpstreamTimeout：%v", err)
	}
	if len(rec.records) != 1 {
		t.Fatalf("业务键应记录 1 条变更，得 %d", len(rec.records))
	}
}
