package domain

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// negativeCacheTTL 是 404 负缓存默认 TTL：同路径确认不存在的制品在 TTL 内直接返回
// ErrNotFound，不再回源/探测；TTL 过期自动失效（新制品最长 60s 后可见，可接受权衡）。
const negativeCacheTTL = 60 * time.Second

// negativeCacheMaxEntries 是 404 负缓存默认条目上限。
//
// 为什么必须有上限：过期条目只在同键 hit 时被顺带删除，一个 404 路径若之后再也
// 没被请求，其条目会一直残留到进程重启——高基数缺失路径（爬虫、错误配置的客户端）
// 下内存量与运行时间成正比。上限同时把管理端查询的遍历代价钉在有界范围内
// （见 NegativeCacheSnapshot：查询要持锁走一遍映射，无上限时会与所有读请求抢锁）。
//
// 淘汰只影响命中率、不影响正确性：负缓存本就是纯优化，丢掉条目最坏是多一次回源。
// 本值刻意不引环境变量（TTL 配置化属  口径，本 FR 只补上限）；容量调优需要
// 真实基数观测后再定，先给一个远高于典型缺失路径基数的常量。
const negativeCacheMaxEntries = 10000

// negativeCache 是 proxy/group 读路径的 404 负缓存：
// 记录 (repoID, path) 最近一次「确认不存在」的时间，TTL 内同路径读请求直接 404，
// 避免对同一缺失路径反复回源/探测（如 Gradle 反复尝试缺失子模块）。
//
// 只缓存「确认不存在」的 404（所有成员均确认、无成员被熔断/阻止/离线跳过）；
// 上游故障（5xx/超时/传输错误）与不可判定 404 一律不写，防止把上游故障误缓存成永久不存在。
// 条目数受 maxEntries 约束（见 negativeCacheMaxEntries）。
// 内存态、并发安全；由 AssetService 持有，并经 SetChangeRecorder 注入 ReplicationService
// 共享（复制应用成功后失效对应键，见 replication.go）。
type negativeCache struct {
	mu         sync.Mutex
	ttl        time.Duration
	maxEntries int
	m          map[string]time.Time // 键 = repoID\0path → 写入时间
}

// newNegativeCache 构造负缓存。
func newNegativeCache() *negativeCache {
	return &negativeCache{ttl: negativeCacheTTL, maxEntries: negativeCacheMaxEntries, m: map[string]time.Time{}}
}

// setTTL 调整 TTL（装配层注入配置、测试与调优用）。
//
// 两种边界必须分开处理，这是「0 = 关闭负缓存」的第一处改造点：
//   - 负数**忽略**：保留「非法值不动现值」的防御（配置层已回落默认，这里是第二道保险）。
//   - 0 **照写**：0 是「关闭负缓存」的显式表达（见 add / hit 的关闭态分支），
//     不能被当成「未配置」而忽略——忽略它会让配置层的 0 静默失效、负缓存照旧生效。
func (c *negativeCache) setTTL(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if d < 0 {
		return
	}
	c.ttl = d
}

// setMaxEntries 调整条目数上限（测试与调优用；<=0 忽略）。
//
// 有意不支持「0 = 关闭上限」：无上限正是本 FR 要修的缺陷（内存随时间线性增长、
// 管理端查询无界遍历），把它开成可配置的关闭项等于留一条回到旧缺陷的开关。
func (c *negativeCache) setMaxEntries(n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if n > 0 {
		c.maxEntries = n
	}
}

// hit 报告 (repoID, path) 是否处于负缓存有效期内；过期条目顺带清理。
//
// ttl <= 0 是关闭态：直接返回 false，语义显式，不依赖 time.Since(at) >= c.ttl
// 在 ttl==0 时恒为真这一巧合——后者今天「看起来能用」，但一旦日后有人调整比较方向
// 或加入时钟抽象，关闭态就会悄悄变成「永久命中」。
func (c *negativeCache) hit(repoID int64, path string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ttl <= 0 {
		return false
	}
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
//
// ttl <= 0 是关闭态：**直接返回、不写 map**。这是「0 = 关闭负缓存」的第二处改造点，
// 也是最容易漏掉的一半：hit 的过期判断是 time.Since(at) >= c.ttl，ttl==0 时恒为真，
// 于是命中永远失败——功能上「看起来已经关闭」。但若仍然写入，每次确认 404 都会往 m 里
// 塞一条键，而这条键只会在下次 hit 同一路径时才被删除；长期运行 + 大量不同缺失路径
// （Gradle 反复试错、扫描器、错误配置的客户端）会让 m 单调增长且永不回收，
// 是一个只在重启时才消失的内存增长缺陷。
//
// 满上限时先做一次全量过期清理，仍满则淘汰写入时间最早的一条。清理与淘汰都在
// 本方法已持有的锁内完成，因此只在真正满上限时付 O(n) 代价；未满上限的常态路径
// 与旧实现完全一致（一次 map 写入）。
func (c *negativeCache) add(repoID int64, path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// 关闭态不写任何条目：调用点（proxy/group 的 404 记账）全部保留，
	// 关闭语义由这里兜住——若改成在调用点散布 if enabled 判断，未来新增调用点会再次漏掉。
	if c.ttl <= 0 {
		return
	}
	key := negativeKey(repoID, path)
	// 同键刷新写入时间不占新额度：否则反复确认同一路径的 404 会白白淘汰他人。
	if _, exists := c.m[key]; !exists && c.maxEntries > 0 && len(c.m) >= c.maxEntries {
		c.purgeExpiredLocked()
		if len(c.m) >= c.maxEntries {
			c.evictOldestLocked()
		}
	}
	c.m[key] = time.Now()
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

// splitNegativeKey 把负缓存键拆回仓库 ID 与路径；键格式非法时 ok 为 false。
func splitNegativeKey(key string) (repoID int64, path string, ok bool) {
	idPart, rest, found := strings.Cut(key, "\x00")
	if !found {
		return 0, "", false
	}
	id, err := strconv.ParseInt(idPart, 10, 64)
	if err != nil {
		return 0, "", false
	}
	return id, rest, true
}

// purgeExpiredLocked 删除全部已过期条目，返回删除条数；调用方必须已持有 c.mu。
func (c *negativeCache) purgeExpiredLocked() int {
	removed := 0
	for key, at := range c.m {
		if time.Since(at) >= c.ttl {
			delete(c.m, key)
			removed++
		}
	}
	return removed
}

// evictOldestLocked 淘汰写入时间最早的一条（键序兜底，保证同一时刻写入时淘汰确定）；
// 调用方必须已持有 c.mu。
func (c *negativeCache) evictOldestLocked() {
	oldestKey := ""
	var oldestAt time.Time
	for key, at := range c.m {
		if oldestKey == "" || at.Before(oldestAt) || (at.Equal(oldestAt) && key < oldestKey) {
			oldestKey, oldestAt = key, at
		}
	}
	if oldestKey != "" {
		delete(c.m, oldestKey)
	}
}

// NegativeCacheEntry 是负缓存单条有效条目的只读视图。
// RepositoryID + Path 唯一确定条目，因此不需要额外 id。
type NegativeCacheEntry struct {
	// RepositoryID 是写入该条目的仓库 ID。
	RepositoryID int64
	// Repository 是仓库名；快照侧由仓库表补全，负缓存本身只存 ID。
	Repository string
	// Path 是制品路径（原样，已在响应层做控制字符转义）。
	Path string
	// WrittenAt 是该条目的写入时间。
	WrittenAt time.Time
	// Remaining 是剩余有效期。
	Remaining time.Duration
}

// NegativeCacheFilter 是负缓存查询与清理的过滤条件。
//
// 字段用途按操作区分（不混用）：
//   - RepositoryID：0 表示不限仓库；
//   - PathPrefix：仅快照查询使用，空串表示不限前缀；
//   - ExactPath：仅清理使用，非空表示只删这一条精确路径；
//   - Limit：仅快照查询使用，<=0 表示不限条数（调用方负责给默认值与上限）。
type NegativeCacheFilter struct {
	RepositoryID int64
	PathPrefix   string
	ExactPath    string
	Limit        int
}

// NegativeCacheSnapshot 是负缓存的一次性只读快照。
type NegativeCacheSnapshot struct {
	// Total 是**过滤前**的有效条目总数（清理过期后统计）。
	Total int
	// TTL 是当前负缓存 TTL。
	TTL time.Duration
	// MaxEntries 是当前条目上限。
	MaxEntries int
	// Items 是过滤后按写入时间升序的前 Limit 条。
	Items []NegativeCacheEntry
}

// snapshot 返回负缓存快照：先清理过期条目，再按条件过滤并按写入时间升序截断。
// 全程只读内存、持同一把锁完成一次遍历，不访问数据库与网络。
func (c *negativeCache) snapshot(f NegativeCacheFilter) NegativeCacheSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.purgeExpiredLocked()
	out := NegativeCacheSnapshot{Total: len(c.m), TTL: c.ttl, MaxEntries: c.maxEntries}
	now := time.Now()
	for key, at := range c.m {
		repoID, path, ok := splitNegativeKey(key)
		if !ok {
			continue
		}
		if f.RepositoryID != 0 && repoID != f.RepositoryID {
			continue
		}
		if f.PathPrefix != "" && !strings.HasPrefix(path, f.PathPrefix) {
			continue
		}
		out.Items = append(out.Items, NegativeCacheEntry{
			RepositoryID: repoID,
			Path:         path,
			WrittenAt:    at,
			Remaining:    c.ttl - now.Sub(at),
		})
	}
	sort.Slice(out.Items, func(i, j int) bool {
		if out.Items[i].WrittenAt.Equal(out.Items[j].WrittenAt) {
			return out.Items[i].Path < out.Items[j].Path
		}
		return out.Items[i].WrittenAt.Before(out.Items[j].WrittenAt)
	})
	if f.Limit > 0 && len(out.Items) > f.Limit {
		out.Items = out.Items[:f.Limit]
	}
	return out
}

// purge 删除匹配的条目并返回删除条数。
//
// 语义：RepositoryID 为 0 表示全清；只给 RepositoryID 表示按仓库清；
// RepositoryID + ExactPath 表示精确清一条。已过期但尚未被清理的条目不计数——
// 它们本就不再生效，把它们算进「删除数」会误导排障者以为动了仍在起作用的记忆。
func (c *negativeCache) purge(f NegativeCacheFilter) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	removed := 0
	for key, at := range c.m {
		if now.Sub(at) >= c.ttl {
			delete(c.m, key)
			continue
		}
		repoID, path, ok := splitNegativeKey(key)
		if !ok {
			delete(c.m, key)
			continue
		}
		if f.RepositoryID != 0 && repoID != f.RepositoryID {
			continue
		}
		if f.ExactPath != "" && path != f.ExactPath {
			continue
		}
		delete(c.m, key)
		removed++
	}
	return removed
}

// count 返回清理过期后的有效条目数。
func (c *negativeCache) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.purgeExpiredLocked()
	return len(c.m)
}

// SetNegativeCacheMaxEntries 调整 404 负缓存条目上限（测试与调优用；<=0 忽略）。
func (s *AssetService) SetNegativeCacheMaxEntries(n int) { s.negCache.setMaxEntries(n) }

// NegativeCacheEntriesCount 返回当前负缓存有效条目数（清理过期后统计）。
// 供装配层接进 /metrics 的 jianartifact_negative_cache_entries 仪表；
// 只读内存态，不查库、不访问网络。
func (s *AssetService) NegativeCacheEntriesCount() int { return s.negCache.count() }

// NegativeCacheSnapshot 返回负缓存快照（仅管理员端点使用）。
//
// 「只读内存」的边界：条目遍历与过期清理都在负缓存锁内完成、不查库；
// 仓库名是契约要求的字段（repositoryId + path 之外还要人能看懂的仓库名），
// 因此按命中到的仓库 ID 做**一次**批量查询补名，而不是逐条查库。
// 条目数已被 maxEntries 钉住，因此这次补名的 ID 数量同样有界。
func (s *AssetService) NegativeCacheSnapshot(f NegativeCacheFilter) (NegativeCacheSnapshot, error) {
	snap := s.negCache.snapshot(f)
	if len(snap.Items) == 0 {
		return snap, nil
	}
	ids := make([]int64, 0, len(snap.Items))
	seen := make(map[int64]struct{}, len(snap.Items))
	for _, it := range snap.Items {
		if _, ok := seen[it.RepositoryID]; ok {
			continue
		}
		seen[it.RepositoryID] = struct{}{}
		ids = append(ids, it.RepositoryID)
	}
	byID, err := s.repos.ListByIDs(ids)
	if err != nil {
		return snap, err
	}
	for i := range snap.Items {
		if repo, ok := byID[snap.Items[i].RepositoryID]; ok {
			snap.Items[i].Repository = repo.Name
		}
	}
	return snap, nil
}

// PurgeNegativeCache 清理负缓存并返回实际删除的**有效**条目数（仅管理员端点使用）。
// 纯内存操作，不查库、不访问网络。
func (s *AssetService) PurgeNegativeCache(f NegativeCacheFilter) int { return s.negCache.purge(f) }
