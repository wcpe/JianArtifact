package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/config"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// TestOpenServicesSurvivesPersistedFreeze 是该缺陷的**端到端**回归：
// 从「磁盘上已有 frozen:true」出发，走真实 openServices 装配（含冻结回读与
// FailInterruptedRunning 启动自愈），断言服务能装起来、任务被回收、且冻结仍然生效。
//
// 为什么必须有这条而不能只靠 domain 层用例：领域层用例是手动
// svc.SetBusinessWriteGate(freeze) 注入内存冻结态，绕过了 openServices 里
// 「回读文件 → 装配 9 个服务 → 启动自愈」的真实顺序。而死锁恰恰是**顺序**造成的，
// 只测领域层等于把最该守的那段路径留空——将来谁调整 wiring.go 的装配顺序，
// 领域用例仍会绿，死锁却复发。本用例覆盖的正是那段。
func TestOpenServicesSurvivesPersistedFreeze(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(config.EnvDataDir, dir)
	t.Setenv(config.EnvJWTSecret, "freeze-startup-deadlock-test-key-32b!!")

	// 准备一个 running 的迁移任务：模拟上个进程崩溃时的遗留状态。
	// 先正常装配一次（此时无冻结文件，自愈放行），建任务并 Start 到 running。
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("加载配置：%v", err)
	}
	first, err := openServices(cfg, withFreezePersistence())
	if err != nil {
		t.Fatalf("首次装配：%v", err)
	}
	mig := domain.NewMigrationService(repository.NewMigrationTaskRepo(first.db), nil)
	task, err := mig.Create(domain.MigrationCreateInput{
		SourceType:   repository.MigrationSourceOfflineDir,
		SourceConfig: map[string]any{"path": "/x"},
	})
	if err != nil {
		t.Fatalf("建任务：%v", err)
	}
	if _, err := mig.Start(task.ID, nil); err != nil {
		t.Fatalf("启动任务：%v", err)
	}
	if err := first.db.Close(); err != nil {
		t.Fatalf("关闭首次装配：%v", err)
	}

	// 写入 frozen:true 的冻结状态文件——这就是修复前触发死锁的那份磁盘状态。
	freezePath := filepath.Join(dir, "write-freeze.json")
	until := time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
	raw := `{"frozen":true,"frozenAt":"` + time.Now().UTC().Format(time.RFC3339Nano) +
		`","until":"` + until + `","reason":"端到端死锁回归"}`
	if err := os.WriteFile(freezePath, []byte(raw), 0o600); err != nil {
		t.Fatalf("写冻结文件：%v", err)
	}

	// 关键断言：带着 frozen:true 重新装配必须成功。
	// 修复前这里会返回「回收中断的迁移任务：写入已冻结」，正是线上无限重启的形态。
	second, err := openServices(cfg, withFreezePersistence())
	if err != nil {
		t.Fatalf("冻结状态下的启动装配不应失败，实际：%v", err)
	}
	defer func() { _ = second.db.Close() }()

	// 断言 1：启动自愈确实执行了——遗留 running 任务被标成 failed。
	got, err := domain.NewMigrationService(repository.NewMigrationTaskRepo(second.db), nil).
		Get(task.ID)
	if err != nil {
		t.Fatalf("读取任务：%v", err)
	}
	if got.Status != repository.MigrationStatusFailed {
		t.Errorf("遗留任务状态 = %q，期望 failed（启动自愈未执行）", got.Status)
	}

	// 断言 2：冻结语义没有被削弱——业务写仍必须被拒。
	if err := second.freeze.RequireBusinessWrite(); err == nil {
		t.Error("装配后冻结应仍然生效，但 RequireBusinessWrite 放行了")
	}
	userMig := domain.NewMigrationService(repository.NewMigrationTaskRepo(second.db), nil)
	userMig.SetBusinessWriteGate(second.freeze)
	if _, err := userMig.Start(task.ID, nil); err == nil {
		t.Error("冻结期间用户发起的迁移写应被拒绝")
	}
}
