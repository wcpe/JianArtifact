package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/config"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// 本文件覆盖两项 proxy/group 调优的**播种与启动生效**链路。
//
// 要点：环境变量的角色只是「首次播种的初值」，不是唯一真源。播种写错会有两种典型症状，
// 且都不报错——① 表里已有界面值却被环境变量覆盖回默认（用户改了像没改）；
// ② 表里没有键就不写，导致设置页永远显示"未配置"、运行时恒用编译期默认。

// openSeedTestDB 打开一个已迁移的临时库。
func openSeedTestDB(t *testing.T) *persistence.DB {
	t.Helper()
	db, err := persistence.Open(filepath.Join(t.TempDir(), "seed.db"))
	if err != nil {
		t.Fatalf("打开数据库：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("迁移：%v", err)
	}
	return db
}

// TestSeedProxyTuningSettings 覆盖播种：空表用环境变量值、已有值一律不动、0 照写。
func TestSeedProxyTuningSettings(t *testing.T) {
	t.Parallel()
	db := openSeedTestDB(t)
	settings := repository.NewSettingRepo(db)
	svc := domain.NewSettingService(settings)

	// 一、空表：环境变量值作为初值写入。负缓存 TTL 显式取 0（关闭），
	// 它必须被当作合法初值写进去，而不是"非空才写"判据下的漏网值。
	cfg := &config.Config{
		GroupMemberTimeout: 8 * time.Second,
		NegativeCacheTTL:   0,
	}
	if err := seedProxyTuningSettings(settings, cfg); err != nil {
		t.Fatalf("播种：%v", err)
	}
	if got := svc.GroupMemberTimeoutSecs(); got != 8 {
		t.Fatalf("group_member_timeout 应播种为 8，实际 %d", got)
	}
	if secs, ok := svc.NegativeCacheTTLSecs(); !ok || secs != 0 {
		t.Fatalf("negative_cache_ttl 应播种为 (0, true)（0 = 关闭），实际 (%d, %v)", secs, ok)
	}

	// 二、表里已有值（相当于运维在界面改过）：重跑播种不得覆盖。
	five, sixty := 5, 60
	if err := svc.UpdateSettings(domain.SettingsUpdate{
		GroupMemberTimeout: &five,
		NegativeCacheTTL:   &sixty,
	}); err != nil {
		t.Fatalf("界面改值：%v", err)
	}
	if err := seedProxyTuningSettings(settings, cfg); err != nil {
		t.Fatalf("重复播种：%v", err)
	}
	if got := svc.GroupMemberTimeoutSecs(); got != 5 {
		t.Fatalf("已有界面值不得被环境变量覆盖：期望 5，实际 %d", got)
	}
	if secs, ok := svc.NegativeCacheTTLSecs(); !ok || secs != 60 {
		t.Fatalf("已有界面值不得被环境变量覆盖：期望 (60, true)，实际 (%d, %v)", secs, ok)
	}

	// 三、两项逐键独立播种：只删掉其中一项，再播种时只有它被补回环境变量值。
	// （备份三项是"整组判断"，两项调优刻意不是——它们互不相干，理由见函数注释。）
	if err := settings.Set(domain.SettingKeyNegativeCacheTTL, "0"); err != nil {
		t.Fatalf("改负缓存 TTL：%v", err)
	}
	if err := seedProxyTuningSettings(settings, cfg); err != nil {
		t.Fatalf("再播种：%v", err)
	}
	if got := svc.GroupMemberTimeoutSecs(); got != 5 {
		t.Fatalf("另一项已存在的值不应受影响：期望 5，实际 %d", got)
	}
}

// TestOpenServicesAppliesProxyTuningSettings 是装配级验证：设置表里的**生效值**必须
// 真的注入 AssetService，而不是只落在表里、让运行时继续用环境变量/默认值。
//
// 为什么必须跑两次装配：只用「环境变量给 0」验证是不够的——装配层在播种之前就已经
// 用 cfg.NegativeCacheTTL 注入过一次，所以即便漏掉 applyProxyTuningSettings，
// 结果也是 0，测试会假绿（实测确认过）。真正要钉住的语义是「**环境变量只是初值，
// 此后以 setting 表为准**」：因此第二轮刻意让环境变量与 setting 表给出不同的值，
// 只有 setting 表的值能解释最终结果。
func TestOpenServicesAppliesProxyTuningSettings(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(config.EnvDataDir, dir)
	t.Setenv(config.EnvJWTSecret, "wiring-tuning-test-secret-key-32bytes")
	t.Setenv(config.EnvEnabledFormats, "raw")
	// 环境变量：首轮作为初值播种，此后不应再是真源。
	t.Setenv(config.EnvNegativeCacheTTL, "60")
	t.Setenv(config.EnvGroupMemberTimeout, "8")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("加载配置：%v", err)
	}

	// 第一轮：空表 → 环境变量值作为初值写入。
	first, err := openServices(cfg)
	if err != nil {
		t.Fatalf("首次装配：%v", err)
	}
	if got := first.settingSvc.GroupMemberTimeoutSecs(); got != 8 {
		t.Fatalf("首轮 group_member_timeout 应播种为 8，实际 %d", got)
	}
	if secs, ok := first.settingSvc.NegativeCacheTTLSecs(); !ok || secs != 60 {
		t.Fatalf("首轮 negative_cache_ttl 应播种为 (60, true)，实际 (%d, %v)", secs, ok)
	}
	if snap, err := first.assetSvc.NegativeCacheSnapshot(domain.NegativeCacheFilter{}); err != nil {
		t.Fatalf("取负缓存快照：%v", err)
	} else if snap.TTL != 60*time.Second {
		t.Fatalf("首轮负缓存 TTL 应为 60s（环境变量初值），实际 %s", snap.TTL)
	}

	// 模拟运维在界面改成「关闭负缓存 + 5s 成员预算」，然后重启。
	zero, five := 0, 5
	if err := first.settingSvc.UpdateSettings(domain.SettingsUpdate{
		GroupMemberTimeout: &five,
		NegativeCacheTTL:   &zero,
	}); err != nil {
		t.Fatalf("界面改值：%v", err)
	}
	if err := first.db.Close(); err != nil {
		t.Fatalf("关闭数据库：%v", err)
	}

	// 第二轮：环境变量仍是 60/8，但 setting 表已有界面值 → 必须以后者为准。
	second, err := openServices(cfg)
	if err != nil {
		t.Fatalf("二次装配：%v", err)
	}
	t.Cleanup(func() { _ = second.db.Close() })

	if got := second.settingSvc.GroupMemberTimeoutSecs(); got != 5 {
		t.Fatalf("重启后不得被环境变量覆盖：group_member_timeout 期望 5，实际 %d", got)
	}
	if secs, ok := second.settingSvc.NegativeCacheTTLSecs(); !ok || secs != 0 {
		t.Fatalf("重启后不得被环境变量覆盖：negative_cache_ttl 期望 (0, true)，实际 (%d, %v)", secs, ok)
	}
	// 关键断言：运行时状态也必须跟着 setting 表，而不是环境变量。
	// 漏掉 applyProxyTuningSettings 时这里会是 60s（env 注入值）。
	snap, err := second.assetSvc.NegativeCacheSnapshot(domain.NegativeCacheFilter{})
	if err != nil {
		t.Fatalf("取负缓存快照：%v", err)
	}
	if snap.TTL != 0 {
		t.Fatalf("负缓存 TTL 应按 setting 表注入为 0（关闭），实际 %s：装配层可能漏掉 applyProxyTuningSettings", snap.TTL)
	}
}

// TestWiringSettingsCallbackHotApplies 验证**生产装配**里的回调接线（而不是测试自建的镜像）。
//
// 为什么单列一条：internal/api 的端到端用例为了让回调可控，是在测试里自己把
// OnNegativeCacheTTLChange 接成 assets.SetNegativeCacheTTL；那能证明 API 层会触发回调，
// 但证明不了 wiring 里接的确实是同一个 setter（接成别的字段、或根本忘了传，测试都还是绿的）。
// 这里直接走 appServices.handlers() 构造出的真实 handler，把最后一段链路也钉住。
func TestWiringSettingsCallbackHotApplies(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(config.EnvDataDir, dir)
	t.Setenv(config.EnvJWTSecret, "wiring-tuning-callback-secret-32bytes")
	t.Setenv(config.EnvEnabledFormats, "raw")
	// 初值 60s（默认），随后的保存要把它改成 0（关闭）。
	t.Setenv(config.EnvNegativeCacheTTL, "60")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("加载配置：%v", err)
	}
	svc, err := openServices(cfg)
	if err != nil {
		t.Fatalf("装配服务：%v", err)
	}
	t.Cleanup(func() { _ = svc.db.Close() })

	// 前置：启动后运行时 TTL 是 60s。
	if snap, err := svc.assetSvc.NegativeCacheSnapshot(domain.NegativeCacheFilter{}); err != nil {
		t.Fatalf("取负缓存快照：%v", err)
	} else if snap.TTL != 60*time.Second {
		t.Fatalf("启动后负缓存 TTL 应为 60s，实际 %s", snap.TTL)
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
	req := httptest.NewRequest(http.MethodPut, "/api/v1/settings", strings.NewReader(`{"negativeCacheTTL":0}`))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("保存负缓存 TTL 应 200，得 %d：%s", rec.Code, rec.Body.String())
	}

	// 关键断言：真实 wiring 的回调把运行时 TTL 改成了 0，不需要重启。
	snap, err := svc.assetSvc.NegativeCacheSnapshot(domain.NegativeCacheFilter{})
	if err != nil {
		t.Fatalf("取负缓存快照：%v", err)
	}
	if snap.TTL != 0 {
		t.Fatalf("保存后负缓存 TTL 应为 0（关闭），实际 %s：wiring 的 OnNegativeCacheTTLChange 可能没接上", snap.TTL)
	}
}
