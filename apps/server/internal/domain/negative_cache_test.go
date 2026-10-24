package domain

import (
	"testing"
	"time"
)

// 本文件覆盖 domain 侧的「0 = 关闭负缓存」语义。
//
// 为什么用内部测试（package domain）而非 domain_test：关闭语义的关键证据是
// 「map 不再增长」，而 m 是不导出字段——只从 hit/add 的外部行为看，关闭态与
// 「写进去但立刻过期」完全无法区分，而这恰恰是本 FR 要修的那个缺陷。
//
// 关闭态由 setTTL(0) 表达（装配层注入 JIAN_NEGATIVE_CACHE_TTL=0 的结果）。

// TestNegativeCacheClosedDoesNotWrite 钉住「关闭态不写 map」：
// 关闭后 add 不留下任何条目，hit 恒为 false。
func TestNegativeCacheClosedDoesNotWrite(t *testing.T) {
	c := newNegativeCache()
	c.setTTL(0)

	c.add(1, "com/example/app-1.0.0.jar")

	if got := len(c.m); got != 0 {
		t.Fatalf("关闭态下 add 不得写入条目，实际条目数 %d", got)
	}
	if c.hit(1, "com/example/app-1.0.0.jar") {
		t.Fatalf("关闭态下 hit 必须返回 false")
	}
}

// TestNegativeCacheClosedMapDoesNotGrow 钉住「关闭态内存不增长」：
// 连续记录多条不同的缺失路径后，map 仍为空。
//
// 这正是「看起来已关闭但内存泄漏」的那个场景：若 add 仍写入，每条键只会在下次
// hit 同一路径时才被删除，长期运行 + 大量不同缺失路径会让 map 单调增长且永不回收。
func TestNegativeCacheClosedMapDoesNotGrow(t *testing.T) {
	c := newNegativeCache()
	c.setTTL(0)

	for i := 0; i < 100; i++ {
		c.add(int64(i), "missing/path-"+string(rune('a'+i%26))+".jar")
	}
	if got := len(c.m); got != 0 {
		t.Fatalf("关闭态下连续 add 后条目数必须为 0，实际 %d", got)
	}
	// 同一路径重复 add 同样不得写入。
	for i := 0; i < 3; i++ {
		c.add(7, "same.jar")
	}
	if got := len(c.m); got != 0 {
		t.Fatalf("关闭态下重复 add 同一路径后条目数必须为 0，实际 %d", got)
	}
}

// TestNegativeCacheSetTTLNegativeIgnored 钉住负数语义：
// 负数不动现值（默认 60s 与已设的自定义值都保持），只有 0 才是关闭。
//
// 负数被忽略是第二道保险——配置层已把负数回落默认 60s，若这里把负数当关闭，
// 一次「-600」的手误就会静默关掉负缓存、放大上游压力。
func TestNegativeCacheSetTTLNegativeIgnored(t *testing.T) {
	c := newNegativeCache()
	if c.ttl != negativeCacheTTL {
		t.Fatalf("构造后 TTL = %s，期望默认 %s", c.ttl, negativeCacheTTL)
	}
	c.setTTL(-1)
	if c.ttl != negativeCacheTTL {
		t.Fatalf("setTTL(-1) 不得改变默认 TTL，实际 %s", c.ttl)
	}

	c.setTTL(5 * time.Second)
	c.setTTL(-1)
	if c.ttl != 5*time.Second {
		t.Fatalf("setTTL(-1) 不得改变已设的自定义 TTL，实际 %s", c.ttl)
	}
}

// TestNegativeCacheExpiryStillWorks 钉住过期行为不回归：
// 正数 TTL 下 hit 由 true 变 false（关闭态的早返回没有影响正常路径）。
func TestNegativeCacheExpiryStillWorks(t *testing.T) {
	c := newNegativeCache()
	c.setTTL(time.Millisecond)
	c.add(1, "expiring.jar")
	if !c.hit(1, "expiring.jar") {
		t.Fatalf("刚写入的条目应命中")
	}
	time.Sleep(5 * time.Millisecond)
	if c.hit(1, "expiring.jar") {
		t.Fatalf("TTL 过期后不应再命中")
	}
	if got := len(c.m); got != 0 {
		t.Fatalf("过期条目应在 hit 时被清理，实际残留 %d 条", got)
	}
}

// TestNegativeCacheClosedKeepsMaxEntries 钉住「TTL 关闭」与「容量上限」互不干扰
// （容量上限 maxEntries 与 TTL 是两件事，不得互相覆盖）：
// 关闭 TTL 不改变上限；设置上限也不重新打开负缓存。
func TestNegativeCacheClosedKeepsMaxEntries(t *testing.T) {
	c := newNegativeCache()
	if c.maxEntries != negativeCacheMaxEntries {
		t.Fatalf("构造后上限 = %d，期望默认 %d", c.maxEntries, negativeCacheMaxEntries)
	}

	// 关闭 TTL：上限保持不变。
	c.setTTL(0)
	if c.maxEntries != negativeCacheMaxEntries {
		t.Fatalf("关闭 TTL 不得改变条目上限，实际 %d", c.maxEntries)
	}

	// 设置上限：仍然处于关闭态，add 依旧不写。
	c.setMaxEntries(5)
	if c.ttl != 0 {
		t.Fatalf("设置条目上限不得改变 TTL，实际 %s", c.ttl)
	}
	c.add(1, "still-closed.jar")
	if got := len(c.m); got != 0 {
		t.Fatalf("关闭态下设置上限后 add 仍不得写入，实际 %d 条", got)
	}

	// 快照如实反映两者：TTL=0（关闭）与上限 5 并存。
	snap := c.snapshot(NegativeCacheFilter{})
	if snap.TTL != 0 {
		t.Fatalf("快照 TTL = %s，期望 0（关闭）", snap.TTL)
	}
	if snap.MaxEntries != 5 {
		t.Fatalf("快照 MaxEntries = %d，期望 5", snap.MaxEntries)
	}
	if snap.Total != 0 {
		t.Fatalf("关闭态下快照条目数应为 0，实际 %d", snap.Total)
	}
}

// TestNegativeCacheReopenAfterClose 钉住关闭后可恢复：
// 再次 setTTL 正数值时负缓存重新工作（配置纠正后无需重启进程）。
func TestNegativeCacheReopenAfterClose(t *testing.T) {
	c := newNegativeCache()
	c.setTTL(0)
	c.add(1, "a.jar")
	if got := len(c.m); got != 0 {
		t.Fatalf("关闭态下不应写入，实际 %d 条", got)
	}

	c.setTTL(time.Hour)
	c.add(1, "a.jar")
	if !c.hit(1, "a.jar") {
		t.Fatalf("恢复正数 TTL 后应重新命中")
	}
	if got := len(c.m); got != 1 {
		t.Fatalf("恢复后应写入 1 条，实际 %d 条", got)
	}
}
