package domain_test

import (
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// 本文件覆盖两项 proxy/group 调优的**设置读写口径**。
//
// 与 proxy_tuning_test.go 的分工：那边钉的是「setter 注入后运行时行为真的变了」，
// 这边钉的是「设置表里存下的秒数能不能被正确读回」——尤其是负缓存 TTL 最容易出错的一点：
// 必须区分「键缺失」与「值为 0」，否则「0 = 关闭」会被当成「未配置」而静默失效。

// TestProxyTuningSettingsRead 覆盖读取口径：缺省、写入回读、0 的语义、非法值。
func TestProxyTuningSettingsRead(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	svc := domain.NewSettingService(repository.NewSettingRepo(db))

	// 缺省：两个键都不存在。成员确认预算返回 0（调用方据此沿用默认），
	// 负缓存 TTL 必须 ok=false——这是「未配置」与「关闭」的分界。
	if got := svc.GroupMemberTimeoutSecs(); got != 0 {
		t.Errorf("缺省 GroupMemberTimeoutSecs = %d，期望 0", got)
	}
	if secs, ok := svc.NegativeCacheTTLSecs(); ok || secs != 0 {
		t.Errorf("缺省 NegativeCacheTTLSecs = (%d, %v)，期望 (0, false)", secs, ok)
	}

	// 写入合法值回读一致。
	eight, sixty := 8, 60
	if err := svc.UpdateSettings(domain.SettingsUpdate{
		GroupMemberTimeout: &eight,
		NegativeCacheTTL:   &sixty,
	}); err != nil {
		t.Fatalf("写入：%v", err)
	}
	if got := svc.GroupMemberTimeoutSecs(); got != 8 {
		t.Errorf("回读 GroupMemberTimeoutSecs = %d，期望 8", got)
	}
	if secs, ok := svc.NegativeCacheTTLSecs(); !ok || secs != 60 {
		t.Errorf("回读 NegativeCacheTTLSecs = (%d, %v)，期望 (60, true)", secs, ok)
	}

	// 0 必须能被写进去并**原样读回且 ok=true**：这是「关闭负缓存」唯一可靠的表达，
	// 若这里退化成 ok=false，界面上的关闭按钮就永远生效不了。
	zero := 0
	if err := svc.UpdateSettings(domain.SettingsUpdate{NegativeCacheTTL: &zero}); err != nil {
		t.Fatalf("写入 0：%v", err)
	}
	if secs, ok := svc.NegativeCacheTTLSecs(); !ok || secs != 0 {
		t.Errorf("写入 0 后回读 = (%d, %v)，期望 (0, true)：0 是合法的关闭值", secs, ok)
	}

	// 成员确认预算的 0 与负数都表示「未配置/非法」：本项无禁用语义，
	// 读到 0 就表示调用方应沿用环境变量/默认值，而不是把预算调成 0 秒。
	if err := svc.UpdateSettings(domain.SettingsUpdate{GroupMemberTimeout: &zero}); err != nil {
		t.Fatalf("写入 0：%v", err)
	}
	if got := svc.GroupMemberTimeoutSecs(); got != 0 {
		t.Errorf("写入 0 后 GroupMemberTimeoutSecs = %d，期望 0（调用方据此回退默认）", got)
	}
	negative := -5
	if err := svc.UpdateSettings(domain.SettingsUpdate{
		GroupMemberTimeout: &negative,
		NegativeCacheTTL:   &negative,
	}); err != nil {
		t.Fatalf("写入负数：%v", err)
	}
	if got := svc.GroupMemberTimeoutSecs(); got != 0 {
		t.Errorf("写入负数后 GroupMemberTimeoutSecs = %d，期望 0", got)
	}
	// 负缓存 TTL 的负数**不等于关闭**：必须报 ok=false 让调用方回退默认，
	// 否则一次 -600 的笔误就会静默关掉负缓存、放大上游压力。
	if secs, ok := svc.NegativeCacheTTLSecs(); ok || secs != 0 {
		t.Errorf("写入负数后 NegativeCacheTTLSecs = (%d, %v)，期望 (0, false)", secs, ok)
	}
}

// TestProxyTuningSettingsAreNodeLocal 钉住两项的**节点本地**分类。
//
// 为什么必须测：isNodeLocalSetting 未导出，写错（漏列一项）今天没有任何症状——
// 复制通道已退役，recordChange 用的是 NoopChangeRecorder。但分类的语义价值恰恰在于
// 「若复制能力回归，这些键不会被静默错复制」，属于最典型的一类埋雷。
// 这里用「写入后是否产生复制变更日志」作为分类的可观测代理。
func TestProxyTuningSettingsAreNodeLocal(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	svc := domain.NewSettingService(repository.NewSettingRepo(db))
	rec := &recordingRecorder{}
	svc.SetChangeRecorder(rec)

	groupSecs, negSecs := 8, 0
	if err := svc.UpdateSettings(domain.SettingsUpdate{
		GroupMemberTimeout: &groupSecs,
		NegativeCacheTTL:   &negSecs,
	}); err != nil {
		t.Fatalf("写入：%v", err)
	}
	if len(rec.records) != 0 {
		t.Fatalf("两项调优都是节点本地配置，不应记录复制变更，得 %d 条：%+v", len(rec.records), rec.records)
	}

	// 对照：业务键仍应记录，证明上面的「0 条」不是因为记录器没接上。
	timeout := 45
	if err := svc.UpdateSettings(domain.SettingsUpdate{UpstreamTimeout: &timeout}); err != nil {
		t.Fatalf("写入回源超时：%v", err)
	}
	if len(rec.records) != 1 {
		t.Fatalf("对照键 upstream_timeout 应记录 1 条变更，得 %d", len(rec.records))
	}
}
