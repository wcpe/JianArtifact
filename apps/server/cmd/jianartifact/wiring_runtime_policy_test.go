package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/config"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
	"github.com/wcpe/jianartifact/apps/server/internal/scheduler"
)

// 本文件覆盖运行期策略 13 项的**装配级**链路：播种口径、作业名映射、以及改间隔后
// 调度器状态是否真的换了值。这三件事都只有在 cmd 包才看得到（它同时看得见
// domain 的读取函数、config 的默认值与真实的 Scheduler）。
//
// 播种写错会有两种典型症状，且都不报错：
//  ① 表里已有界面值却被环境变量覆盖回去（用户改了像没改）；
//  ② 显式 0（禁用）被当成"未配置"跳过，重启后作业自己又开了。

// TestSeedRuntimePolicySettings 覆盖播种的三条口径：
// 空表用环境变量值写入、已有值一律不动、0 照写。
func TestSeedRuntimePolicySettings(t *testing.T) {
	t.Parallel()
	db := openSeedTestDB(t)
	settings := repository.NewSettingRepo(db)
	svc := domain.NewSettingService(settings)

	cfg := &config.Config{
		BlobGCInterval:              24 * time.Hour,
		StorageCleanupInterval:      6 * time.Hour,
		BackupOrphanCleanupInterval: 12 * time.Hour,
		TrashGCInterval:             0, // 显式禁用：必须被当成合法初值写进去
		UpstreamProbeInterval:       90 * time.Second,
		BackupOrphanGrace:           3 * time.Hour,
		StorageMetadataRetention:    14 * 24 * time.Hour,
		StorageTempMaxAge:           6 * time.Hour,
		TrashRetentionDays:          10,
		TrashMaxBytes:               1 << 30,
		AutoBlockBaseSeconds:        45 * time.Second,
		SearchTimeout:               8 * time.Second,
		CapacityCacheTTL:            15 * time.Second,
	}
	if err := seedRuntimePolicySettings(settings, cfg); err != nil {
		t.Fatalf("播种：%v", err)
	}

	// 一、空表：环境变量解析结果作为初值写入，单位换算必须正确
	// （小时 → 小时、天 → 天，两处都容易写成"按秒存"而差 3600 / 86400 倍）。
	if secs, ok := svc.BlobGCIntervalSecs(); !ok || secs != 86400 {
		t.Errorf("blob_gc_interval 应播种为 (86400, true)，实际 (%d, %v)", secs, ok)
	}
	if secs, ok := svc.StorageCleanupIntervalSecs(); !ok || secs != 21600 {
		t.Errorf("storage_cleanup_interval 应播种为 (21600, true)，实际 (%d, %v)", secs, ok)
	}
	if secs, ok := svc.TrashGCIntervalSecs(); !ok || secs != 0 {
		t.Errorf("trash_gc_interval 应播种为 (0, true)（显式禁用），实际 (%d, %v)", secs, ok)
	}
	if secs, ok := svc.UpstreamProbeIntervalSecs(); !ok || secs != 90 {
		t.Errorf("upstream_probe_interval 应播种为 (90, true)，实际 (%d, %v)", secs, ok)
	}
	if hours := svc.BackupOrphanGraceHours(); hours != 3 {
		t.Errorf("backup_orphan_grace_hours 应播种为 3，实际 %d", hours)
	}
	if days, ok := svc.StorageMetadataRetentionDays(); !ok || days != 14 {
		t.Errorf("storage_metadata_retention_days 应播种为 (14, true)，实际 (%d, %v)", days, ok)
	}
	if hours, ok := svc.StorageTempMaxAgeHours(); !ok || hours != 6 {
		t.Errorf("storage_temp_max_age_hours 应播种为 (6, true)，实际 (%d, %v)", hours, ok)
	}
	if days, ok := svc.TrashRetentionDays(); !ok || days != 10 {
		t.Errorf("trash_retention_days 应播种为 (10, true)，实际 (%d, %v)", days, ok)
	}
	if n, ok := svc.TrashMaxBytes(); !ok || n != 1<<30 {
		t.Errorf("trash_max_bytes 应播种为 (2^30, true)，实际 (%d, %v)", n, ok)
	}
	if secs := svc.AutoBlockBaseSeconds(); secs != 45 {
		t.Errorf("auto_block_base_seconds 应播种为 45，实际 %d", secs)
	}
	if secs := svc.SearchTimeoutSeconds(); secs != 8 {
		t.Errorf("search_timeout_seconds 应播种为 8，实际 %d", secs)
	}
	if secs, ok := svc.CapacityCacheTTLSeconds(); !ok || secs != 15 {
		t.Errorf("capacity_cache_ttl_seconds 应播种为 (15, true)，实际 (%d, %v)", secs, ok)
	}

	// 二、模拟运维在界面改过值，然后重跑播种：一律不得被环境变量覆盖。
	// 若这里失败，症状是"我在界面上改成 1 小时了，重启又变回 6 小时"。
	if err := svc.UpdateSettings(domain.SettingsUpdate{
		BlobGCInterval:               intPtr(3600),
		TrashGCInterval:              intPtr(1800),
		BackupOrphanGraceHours:       intPtr(9),
		StorageMetadataRetentionDays: intPtr(30),
		TrashMaxBytes:                int64Ptr(4 << 30),
	}); err != nil {
		t.Fatalf("界面改值：%v", err)
	}
	if err := seedRuntimePolicySettings(settings, cfg); err != nil {
		t.Fatalf("重复播种：%v", err)
	}
	if secs, ok := svc.BlobGCIntervalSecs(); !ok || secs != 3600 {
		t.Errorf("已有界面值不得被环境变量覆盖：blob_gc_interval 期望 (3600, true)，实际 (%d, %v)", secs, ok)
	}
	if secs, ok := svc.TrashGCIntervalSecs(); !ok || secs != 1800 {
		t.Errorf("已有界面值不得被环境变量覆盖：trash_gc_interval 期望 (1800, true)，实际 (%d, %v)", secs, ok)
	}
	if hours := svc.BackupOrphanGraceHours(); hours != 9 {
		t.Errorf("已有界面值不得被环境变量覆盖：宽限期期望 9，实际 %d", hours)
	}
	if days, ok := svc.StorageMetadataRetentionDays(); !ok || days != 30 {
		t.Errorf("已有界面值不得被环境变量覆盖：元数据保留期望 (30, true)，实际 (%d, %v)", days, ok)
	}
	if n, ok := svc.TrashMaxBytes(); !ok || n != 4<<30 {
		t.Errorf("已有界面值不得被环境变量覆盖：回收站上限期望 (2^32, true)，实际 (%d, %v)", n, ok)
	}
	// 未改过的项保持首次播种值。
	if secs := svc.SearchTimeoutSeconds(); secs != 8 {
		t.Errorf("未改过的项应保持首次播种值 8，实际 %d", secs)
	}
}

// TestRuntimePolicyJobNamesMatchRegisteredJobs 钉住「设置键 → 调度器作业名」映射。
//
// 为什么必须单独测：名字写错**不会报错**——Scheduler.SetInterval 对未知作业名只返回 false
// （与 Register 的静默跳过同口径）。因此"看起来对"是靠不住的，只能逐个和注册处对齐。
// 这里用真实的 registerXxxJob 把作业注册进一个空调度器，再逐个比对映射表里的名字。
func TestRuntimePolicyJobNamesMatchRegisteredJobs(t *testing.T) {
	t.Parallel()
	sched := scheduler.New()
	hour := time.Hour
	registerBlobGCJob(sched, hour, func() (int, error) { return 0, nil })
	registerStorageCleanupJob(sched, hour, func() (domain.StorageCleanupResult, error) {
		return domain.StorageCleanupResult{}, nil
	})
	registerBackupOrphanCleanupJob(sched, hour, func() (domain.BackupOrphanCleanupResult, error) {
		return domain.BackupOrphanCleanupResult{}, nil
	})
	registerTrashGCJob(sched, hour, func() time.Duration { return 24 * time.Hour },
		func(time.Duration) (domain.TrashPurgeResult, error) { return domain.TrashPurgeResult{}, nil })
	registerUpstreamProbeJob(sched, hour, func(ctx context.Context) (domain.UpstreamProbeResult, error) {
		return domain.UpstreamProbeResult{}, nil
	})

	registered := map[string]bool{}
	for _, st := range sched.Status() {
		registered[st.Name] = true
	}
	if len(registered) != len(runtimePolicyJobNames) {
		t.Fatalf("注册的作业数 %d 与映射表条数 %d 不一致：%v",
			len(registered), len(runtimePolicyJobNames), registered)
	}
	for key, name := range runtimePolicyJobNames {
		if !registered[name] {
			t.Errorf("映射表把 %s 指向作业 %q，但该名字没有注册（实际已注册：%v）", key, name, registered)
		}
	}
}

// TestApplyRuntimePolicyChangeSetsJobInterval 验证「改作业间隔 → 调度器 Status 反映新值」。
//
// 这是规格里"热生效"的核心断言：不重启进程，SetInterval 之后 Status 里的 interval
// 必须是新值。顺带覆盖两个边界：未注册的作业名不 panic、d<=0 被拒（禁用只能靠不注册表达）。
func TestApplyRuntimePolicyChangeSetsJobInterval(t *testing.T) {
	t.Parallel()
	sched := scheduler.New()
	registerBlobGCJob(sched, time.Hour, func() (int, error) { return 0, nil })
	registerStorageCleanupJob(sched, time.Hour, func() (domain.StorageCleanupResult, error) {
		return domain.StorageCleanupResult{}, nil
	})

	svc := &appServices{scheduler: sched}

	// 前置：两个作业的初始间隔都是 1 小时。
	for _, st := range sched.Status() {
		if st.Interval != time.Hour {
			t.Fatalf("作业 %s 初始间隔应为 1h，实际 %s", st.Name, st.Interval)
		}
	}

	// 改成 60 秒：状态必须立刻反映新值（下一轮才按新值起算）。
	svc.applyRuntimePolicyChange(domain.SettingKeyBlobGCInterval, 60*time.Second)
	if got := jobInterval(t, sched, "blob-gc"); got != 60*time.Second {
		t.Fatalf("blob-gc 间隔应为 60s，实际 %s：SetInterval 可能没接线", got)
	}
	// 另一个作业不受影响。
	if got := jobInterval(t, sched, "storage-cleanup"); got != time.Hour {
		t.Fatalf("未提交的 storage-cleanup 间隔不应变化：期望 1h，实际 %s", got)
	}

	// 未注册的键（这里用 search_timeout 之外的陌生键）不得 panic，也不得改动任何作业。
	svc.applyRuntimePolicyChange("no_such_setting_key", time.Minute)
	if got := jobInterval(t, sched, "blob-gc"); got != 60*time.Second {
		t.Fatalf("未知键不得改动作业间隔：期望 60s，实际 %s", got)
	}

	// d <= 0 被 SetInterval 拒（运行期把间隔改成 0 会让 loop 忙等），状态保持原值。
	svc.applyRuntimePolicyChange(domain.SettingKeyBlobGCInterval, 0)
	if got := jobInterval(t, sched, "blob-gc"); got != 60*time.Second {
		t.Fatalf("d=0 不应生效（禁用只能靠不注册表达）：期望 60s，实际 %s", got)
	}
}

// TestEffectivePolicyDurationZeroSemantics 覆盖「生效值」读取的三条口径。
//
// 这是本次改动最容易做错的一处：ok=true 且值是 0 表示**显式禁用**，必须返回 0；
// 只有 ok=false（键缺失 / 值非法）才回退环境变量解析结果。
// 若把 0 也当"未配置"回退，运维在界面上关掉的清理作业会在下次重启时自己回来。
func TestEffectivePolicyDurationZeroSemantics(t *testing.T) {
	t.Parallel()
	fallback := 24 * time.Hour

	// 显式 0（禁用）必须原样返回 0，不得回退。
	if got := effectivePolicyDuration(func() (int, bool) { return 0, true }, fallback); got != 0 {
		t.Errorf("ok=true 且值为 0 应返回 0（禁用），实际 %s", got)
	}
	// 键缺失才回退。
	if got := effectivePolicyDuration(func() (int, bool) { return 0, false }, fallback); got != fallback {
		t.Errorf("ok=false 应回退 %s，实际 %s", fallback, got)
	}
	if got := effectivePolicyDuration(func() (int, bool) { return 3600, true }, fallback); got != time.Hour {
		t.Errorf("ok=true 且值为 3600 应返回 1h，实际 %s", got)
	}

	// 0 无意义的两项：0 与缺失都回退。
	if got := effectivePositiveDuration(func() int { return 0 }, 10*time.Second); got != 10*time.Second {
		t.Errorf("0 秒应回退 10s（本项无禁用语义），实际 %s", got)
	}
	if got := effectivePositiveDuration(func() int { return 7 }, 10*time.Second); got != 7*time.Second {
		t.Errorf("7 秒应返回 7s，实际 %s", got)
	}
	if got := effectivePositiveHours(func() int { return 0 }, 3*time.Hour); got != 3*time.Hour {
		t.Errorf("0 小时应回退 3h（零宽限不允许），实际 %s", got)
	}
}

// TestWiringRuntimePolicyHotApplies 是**装配级**验证：走真实的 appServices.handlers()，
// 经 HTTP 保存一项作业间隔，再断言真实调度器的 Status 反映新值。
//
// 为什么必须单独一条：上面的 applyRuntimePolicyChange 用例是直接调方法，
// 它证明不了 wiring 里真的把这个回调传给了 api.Deps（漏传时回调为 nil，
// PutSettings 静默跳过，用例仍然全绿）。这里把最后一段链路也钉住。
func TestWiringRuntimePolicyHotApplies(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(config.EnvDataDir, dir)
	t.Setenv(config.EnvJWTSecret, "wiring-runtime-policy-secret-32b")
	t.Setenv(config.EnvEnabledFormats, "raw")
	// 环境变量给一个明显不同于后续保存值的初值，确保断言的是新值而不是初值。
	t.Setenv(config.EnvBlobGCInterval, "86400")
	t.Setenv(config.EnvSearchTimeoutSeconds, "10")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("加载配置：%v", err)
	}
	svc, err := openServices(cfg)
	if err != nil {
		t.Fatalf("装配服务：%v", err)
	}
	t.Cleanup(func() { _ = svc.db.Close() })

	// 前置：播种后作业以环境变量初值注册。
	if got := jobInterval(t, svc.scheduler, "blob-gc"); got != 24*time.Hour {
		t.Fatalf("启动后 blob-gc 间隔应为 24h（环境变量初值），实际 %s", got)
	}

	h := svc.handlers("test", nil)
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("auth.principal", &auth.Principal{Role: "admin", Username: "admin", UserID: 1})
		c.Next()
	})
	router.PUT("/api/v1/settings", h.PutSettings)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/v1/settings", strings.NewReader(`{"blobGcInterval":1800}`))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("保存作业间隔应 200，得 %d：%s", rec.Code, rec.Body.String())
	}

	// 关键断言：真实调度器的间隔变成了 1800s，不需要重启。
	if got := jobInterval(t, svc.scheduler, "blob-gc"); got != 30*time.Minute {
		t.Fatalf("保存后 blob-gc 间隔应为 30m，实际 %s：wiring 的 OnRuntimePolicyChange 可能没接上", got)
	}
}

// TestJobEnabled 覆盖「间隔改成 0 = 禁用」的每轮判断。
//
// 这条语义不能靠 Scheduler.SetInterval 实现（它拒绝 d<=0，因为 0 会让 loop 忙等），
// 因此由作业自己的每轮检查接管。若这里写反（把 0 当成"启用"），界面上关掉一个
// 持续占 IO 的清理作业会完全无效——而界面还显示"已关闭"。
func TestJobEnabled(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		get  func() (int, bool)
		want bool
	}{
		{"显式 0 = 禁用", func() (int, bool) { return 0, true }, false},
		{"正数 = 启用", func() (int, bool) { return 86400, true }, true},
		// 键缺失/值非法时按启用处理：此时运行时用的是环境变量/默认值（即作业确实在跑），
		// 若当成禁用就会让一个正在跑的作业被静默关掉。
		{"键缺失按启用处理", func() (int, bool) { return 0, false }, true},
	}
	for _, tc := range cases {
		if got := jobEnabled(tc.get); got != tc.want {
			t.Errorf("%s：jobEnabled = %v，期望 %v", tc.name, got, tc.want)
		}
	}
}

// TestScheduleJobRespectsZeroInterval 端到端验证「把间隔改成 0 后，作业下一轮不再干活」。
//
// 关键点：Scheduler.SetInterval 拒绝 d<=0（0 会让 loop 进 time.NewTimer(0) 忙等），
// 所以"运行期禁用"完全依赖作业自己的每轮检查（jobEnabled）。这里按 wiring.go 里
// 完全相同的结构把两者拼起来：外层是 jobEnabled 检查，内层是真正的清理函数。
//
// 判据是内层清理函数的调用计数：禁用前必须增长，改成 0 之后必须停止增长。
// 用 40ms 的短间隔而不是等真实间隔，既不慢也不依赖 wall clock。
func TestScheduleJobRespectsZeroInterval(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// intervalSecs 用原子量模拟 setting 表里那一格：0 = 禁用。
	var intervalSecs atomic.Int64
	intervalSecs.Store(1) // 启用
	var calls atomic.Int64

	sched := scheduler.New()
	// 与 wiring.go 的 blob-gc 注册同构：间隔取配置值，作业体内的第一件事是 jobEnabled 检查。
	registerBlobGCJob(sched, 40*time.Millisecond, func() (int, error) {
		if !jobEnabled(func() (int, bool) {
			v := int(intervalSecs.Load())
			return v, true
		}) {
			return 0, nil
		}
		calls.Add(1)
		return 0, nil
	})
	sched.Start(ctx)

	// 一、启用状态下作业应当被反复触发。
	waitFor(t, 2*time.Second, func() bool { return calls.Load() > 0 }, "启用状态下作业应被触发")
	waitFor(t, 2*time.Second, func() bool { return calls.Load() > 2 }, "启用状态下作业应持续触发")

	// 二、改成 0（界面上的"禁用"）：下一轮起不再执行。
	intervalSecs.Store(0)
	// 等过几个 tick，让在途那一轮跑完、后续轮次按新配置判定。
	time.Sleep(200 * time.Millisecond)
	afterDisable := calls.Load()
	time.Sleep(200 * time.Millisecond)
	if got := calls.Load(); got != afterDisable {
		t.Fatalf("间隔改成 0 后作业仍在执行：调用计数从 %d 涨到 %d：jobEnabled 检查可能没接在作业体里", afterDisable, got)
	}

	// 三、再改回正数：下一轮恢复执行（不需要重启进程）。
	intervalSecs.Store(3600)
	waitFor(t, 2*time.Second, func() bool { return calls.Load() > afterDisable },
		"间隔改回正数后作业应恢复执行")
}

// waitFor 轮询等待条件成立，超时则失败。
func waitFor(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal(msg)
}

// jobInterval 读取某个作业当前在调度器里的间隔。
func jobInterval(t *testing.T, sched *scheduler.Scheduler, name string) time.Duration {
	t.Helper()
	for _, st := range sched.Status() {
		if st.Name == name {
			return st.Interval
		}
	}
	t.Fatalf("调度器里没有作业 %q", name)
	return 0
}

func intPtr(v int) *int       { return &v }
func int64Ptr(v int64) *int64 { return &v }
