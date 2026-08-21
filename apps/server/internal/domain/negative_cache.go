package domain

import (
	"fmt"
	"sync"
	"time"
)

// negativeCacheTTL 是 404 负缓存默认 TTL：同路径确认不存在的制品在 TTL 内直接返回
// ErrNotFound，不再回源/探测；TTL 过期自动失效（新制品最长 60s 后可见，可接受权衡）。
const negativeCacheTTL = 60 * time.Second

// negativeCache 是 proxy/group 读路径的 404 负缓存（FR-111）：
// 记录 (repoID, path) 最近一次「确认不存在」的时间，TTL 内同路径读请求直接 404，
// 避免对同一缺失路径反复回源/探测（如 Gradle 反复尝试缺失子模块）。
//
// 只缓存「确认不存在」的 404（所有成员均确认、无成员被熔断/阻止/离线跳过）；
// 上游故障（5xx/超时/传输错误）与不可判定 404 一律不写，防止把上游故障误缓存成永久不存在。
// 内存态、并发安全；由 AssetService 持有，并经 SetChangeRecorder 注入 ReplicationService
// 共享（复制应用成功后失效对应键，见 replication.go）。
type negativeCache struct {
	mu  sync.Mutex
	ttl time.Duration
	m   map[string]time.Time // 键 = repoID\0path → 写入时间
}

// newNegativeCache 构造负缓存。
func newNegativeCache() *negativeCache {
	return &negativeCache{ttl: negativeCacheTTL, m: map[string]time.Time{}}
}

// setTTL 调整 TTL（测试与调优用；<=0 忽略）。
func (c *negativeCache) setTTL(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if d > 0 {
		c.ttl = d
	}
}

// hit 报告 (repoID, path) 是否处于负缓存有效期内；过期条目顺带清理。
func (c *negativeCache) hit(repoID int64, path string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := negativeKey(repoID, path)
	at, ok := c.m[key]
	if !ok {
		return false
	}
	if time.Since(at) >= c.ttl {
		delete(c.m, key)
		return false
	}
	return true
}

// add 记录一次「确认不存在」，写入当前时间。
func (c *negativeCache) add(repoID int64, path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[negativeKey(repoID, path)] = time.Now()
}

// remove 失效 (repoID, path) 的负缓存（写路径 Put/Delete/复制应用成功时调用）。
func (c *negativeCache) remove(repoID int64, path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, negativeKey(repoID, path))
}

// negativeKey 构造负缓存键：repoID\0path（repoID 与 path 均不含 \0，分隔无歧义）。
func negativeKey(repoID int64, path string) string {
	return fmt.Sprintf("%d\x00%s", repoID, path)
}
