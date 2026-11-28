package domain

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/repository"
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
const negativeCacheMaxEntries = 10000

// maxMavenSiblingPomReadBytes 是检测同坐标 POM <packaging> 时的最大读取字节数。
const maxMavenSiblingPomReadBytes = 64 << 10

type bypassNegativeCacheContextKey struct{}

// WithBypassNegativeCache 在请求上下文标记本次读请求跳过 404 负缓存（例如客户端携带
// Cache-Control: no-cache / Pragma: no-cache，如 Gradle --refresh-dependencies / mvn -U）。
func WithBypassNegativeCache(ctx context.Context) context.Context {
	return context.WithValue(ctx, bypassNegativeCacheContextKey{}, true)
}

func shouldBypassNegativeCache(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v, _ := ctx.Value(bypassNegativeCacheContextKey{}).(bool)
	return v
}

type negCacheEntry struct {
	writtenAt time.Time
	customTTL time.Duration
	hasCustom bool
}

// negativeCache 是 proxy/group 读路径的 404 负缓存：
// 记录 (repoID, path) 最近一次「确认不存在」的时间，TTL 内同路径读请求直接 404，
// 避免对同一缺失路径反复回源/探测（如 Gradle 反复尝试缺失子模块）。
//
// 只缓存「确认不存在」的 404（所有成员均确认、无成员被熔断/阻止/离线跳过）；
// 上游故障（5xx/超时/传输错误）与不可判定 404 一律不写，防止把上游故障误缓存成永久不存在。
// 条目数受 maxEntries 约束（见 negativeCacheMaxEntries）。
// 内存态、并发安全；由 AssetService 持有。
type negativeCache struct {
	mu         sync.Mutex
	ttl        time.Duration
	maxEntries int
	m          map[string]negCacheEntry // 键 = repoID\0path → 写入记录（支持仓库级自定义 TTL）
}

// newNegativeCache 构造负缓存。
func newNegativeCache() *negativeCache {
	return &negativeCache{ttl: negativeCacheTTL, maxEntries: negativeCacheMaxEntries, m: map[string]negCacheEntry{}}
}

// setTTL 调整全局默认 TTL（装配层注入配置、测试与调优用）。
//   - 负数**忽略**：保留「非法值不动现值」的防御；
//   - 0 **照写**：0 是「关闭全局负缓存」的显式表达。
func (c *negativeCache) setTTL(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if d < 0 {
		return
	}
	c.ttl = d
}

// setMaxEntries 调整条目数上限（测试与调优用；<=0 忽略）。
func (c *negativeCache) setMaxEntries(n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if n > 0 {
		c.maxEntries = n
	}
}

func (c *negativeCache) entryTTLLocked(e negCacheEntry) time.Duration {
	if e.hasCustom {
		return e.customTTL
	}
	return c.ttl
}

func (c *negativeCache) isExpiredLocked(e negCacheEntry, now time.Time) bool {
	ttl := c.entryTTLLocked(e)
	if ttl <= 0 {
		return true
	}
	return now.Sub(e.writtenAt) >= ttl
}

// hit 报告 (repoID, path) 是否处于负缓存有效期内；过期条目顺带清理。
func (c *negativeCache) hit(repoID int64, path string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := negativeKey(repoID, path)
	entry, ok := c.m[key]
	if !ok {
		return false
	}
	if c.isExpiredLocked(entry, time.Now()) {
		delete(c.m, key)
		return false
	}
	return true
}

// add 按全局默认 TTL 记录一次「确认不存在」，写入当前时间。
func (c *negativeCache) add(repoID int64, path string) {
	c.addEntry(repoID, path, 0, false)
}

// globalTTL 返回当前的全局负缓存 TTL（含 0 = 关闭态）。
// 单独暴露是为了避免调用方直接摸 c.mu 与 c.ttl（recordGroupNegativeCache 需要读它做口径比较）。
func (c *negativeCache) globalTTL() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ttl
}

// addWithCustomTTL 按仓库级覆盖 TTL 记录一次「确认不存在」。
// ttl<=0 表示该仓库关闭负缓存，不写表。
func (c *negativeCache) addWithCustomTTL(repoID int64, path string, ttl time.Duration) {
	c.addEntry(repoID, path, ttl, true)
}

// addEntry 写入一条负缓存。
//
// 目标是「不能关闭的东西永远关不掉」：全局 TTL=0 是运维的绝对关闭开关，
// 仓库级正数覆盖不得把它重新打开，否则全局关停在单个仓库上静默失效，
// 而运维看到的是「已经关了却还在返回缓存的 404」。反向（全局开启、仓库设 0）仍然有效。
func (c *negativeCache) addEntry(repoID int64, path string, customTTL time.Duration, hasCustom bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// 全局关闭态优先级最高，直接不写（含仓库级显式覆盖）。
	if c.ttl <= 0 {
		return
	}
	effectiveTTL := c.ttl
	if hasCustom {
		// 仓库级覆盖只允许更短或关闭，不允许超过全局上限，避免单仓把条目钉得比全局更久。
		if customTTL <= 0 {
			return
		}
		if customTTL < effectiveTTL {
			effectiveTTL = customTTL
		}
	}
	if effectiveTTL <= 0 {
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
	c.m[key] = negCacheEntry{
		writtenAt: time.Now(),
		customTTL: customTTL,
		hasCustom: hasCustom,
	}
}

// remove 失效 (repoID, path) 的负缓存（写路径 Put/Delete/回源缓存成功时调用）。
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
	now := time.Now()
	removed := 0
	for key, entry := range c.m {
		if c.isExpiredLocked(entry, now) {
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
	for key, entry := range c.m {
		if oldestKey == "" || entry.writtenAt.Before(oldestAt) || (entry.writtenAt.Equal(oldestAt) && key < oldestKey) {
			oldestKey, oldestAt = key, entry.writtenAt
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
	// TTL 是当前全局默认负缓存 TTL。
	TTL time.Duration
	// MaxEntries 是当前条目上限。
	MaxEntries int
	// Items 是过滤后按写入时间升序的前 Limit 条。
	Items []NegativeCacheEntry
}

// snapshot 返回负缓存快照：先清理过期条目，再按条件过滤并按写入时间升序截断。
func (c *negativeCache) snapshot(f NegativeCacheFilter) NegativeCacheSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.purgeExpiredLocked()
	out := NegativeCacheSnapshot{Total: len(c.m), TTL: c.ttl, MaxEntries: c.maxEntries}
	now := time.Now()
	for key, entry := range c.m {
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
		ttl := c.entryTTLLocked(entry)
		out.Items = append(out.Items, NegativeCacheEntry{
			RepositoryID: repoID,
			Path:         path,
			WrittenAt:    entry.writtenAt,
			Remaining:    ttl - now.Sub(entry.writtenAt),
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
func (c *negativeCache) purge(f NegativeCacheFilter) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	removed := 0
	for key, entry := range c.m {
		if c.isExpiredLocked(entry, now) {
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
func (s *AssetService) NegativeCacheEntriesCount() int { return s.negCache.count() }

// ParentGroupIDs 返回所有直接或间接包含 repoID 的父 group 仓库 ID。
// 导出的原因是级联失效的正确性依赖「别名成员也能建边」，
// 需要在集成测试里直接断言这条边存在，而不是靠间接的端到端行为猜测。
func (s *AssetService) ParentGroupIDs(repoID int64) []int64 { return s.parentGroupIDs(repoID) }

// NegativeCacheSnapshot 返回负缓存快照（仅管理员端点使用）。
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
// 指定 RepositoryID 时，同步级联失效所有包含该仓库的父级 group 负缓存，
// 防止「清了 proxy 负缓存但 maven-public 聚合仓仍返回过期 404」。
func (s *AssetService) PurgeNegativeCache(f NegativeCacheFilter) int {
	removed := s.negCache.purge(f)
	if f.RepositoryID != 0 {
		for _, gid := range s.parentGroupIDs(f.RepositoryID) {
			removed += s.negCache.purge(NegativeCacheFilter{
				RepositoryID: gid,
				ExactPath:    f.ExactPath,
			})
		}
	}
	return removed
}

// InvalidateRepoNegativeCache 清理指定仓库及其所有父级 group 的负缓存条目
// （在修改仓库上游/代理配置、重新上线或手动重测恢复后调用）。
func (s *AssetService) InvalidateRepoNegativeCache(repoID int64) int {
	if s == nil || repoID <= 0 {
		return 0
	}
	return s.PurgeNegativeCache(NegativeCacheFilter{RepositoryID: repoID})
}

// invalidatePathWithGroups 在成员仓库写入/回源缓存/删除制品后，同步失效自身及所有父级 group
// 上该路径的 404 负缓存，避免新上传或刚恢复的制品被 group 级负缓存继续遮蔽。
//
// 开销说明：parentGroupIDs 要读一遍仓库表并对每个 group 解析 config，属热路径上的固定成本，
// 而它挂在**每次 proxy 冷回源成功**与**每批发布**上（一次 500 依赖的构建就是 500 次）。
// 负缓存常态是空的（构建成功的那条路径根本不会被缓存），因此这里先做一次 O(1) 判空：
// 没有负缓存条目时，父子边一条也用不上，直接退化为一次 map 删除。
// 这让「没有毒缓存时零额外成本」与「有毒缓存时正确失效」同时成立。
func (s *AssetService) invalidatePathWithGroups(repoID int64, path string) {
	if s == nil || repoID <= 0 || path == "" {
		return
	}
	s.negCache.remove(repoID, path)
	if s.negCache.count() == 0 {
		return
	}
	for _, gid := range s.parentGroupIDs(repoID) {
		s.negCache.remove(gid, path)
	}
}

// invalidatePathsWithGroups 批量失效多条路径：父 group 边只解析一次，避免按条目数放大成 N 次全表扫描
// （批量资产操作的条目上限是 500，逐条调用会把一次删除变成 500 次仓库表遍历）。
func (s *AssetService) invalidatePathsWithGroups(repoID int64, paths []string) {
	if s == nil || repoID <= 0 || len(paths) == 0 {
		return
	}
	for _, path := range paths {
		if path != "" {
			s.negCache.remove(repoID, path)
		}
	}
	if s.negCache.count() == 0 {
		return
	}
	groups := s.parentGroupIDs(repoID)
	if len(groups) == 0 {
		return
	}
	for _, gid := range groups {
		for _, path := range paths {
			if path != "" {
				s.negCache.remove(gid, path)
			}
		}
	}
}

// logNegativeCacheCascadeFailure 记录父 group 反查失败。
// 级联失效是「修好之后还要立刻能读到」的关键一步，静默失败会让运维
// 在「已重新上传/已恢复上游却仍然 404」上白耗时间，因此必须留下可检索的痕迹。
func (s *AssetService) logNegativeCacheCascadeFailure(repoID int64, err error) {
	log.Printf("负缓存级联失效未能解析父 group，相关 group 条目可能残留 repositoryID=%d：%v", repoID, err)
}

// parentGroupIDs 查找所有直接或间接包含 targetRepoID 的 group 仓库 ID（带环路保护）。
func (s *AssetService) parentGroupIDs(targetRepoID int64) []int64 {
	if s == nil || s.repos == nil || targetRepoID <= 0 {
		return nil
	}
	all, err := s.repos.ListAllForGroupGraph()
	if err != nil || len(all) == 0 {
		if err != nil {
			// 级联失效依赖这张表；查询失败时静默跳过会让「清了缓存还是 404」无从排查。
			s.logNegativeCacheCascadeFailure(targetRepoID, err)
		}
		return nil
	}
	// byName 必须同时收主名与别名：group 成员允许写别名（validateConfig 用 GetByName 解析），
	// 只按主名建边会让用别名描述的成员静默丢掉父边——级联失效不触发，
	// 症状与本轮要修的毒缓存完全一样，且没有任何日志。
	//
	// 注意 List() 不填充 Aliases（它只 SELECT repository 表），必须单独批量取一次；
	// 用 ListAliasesByRepos 一次 IN 查询，避免逐仓 N+1。
	byName := make(map[string]*repository.Repository, len(all))
	ids := make([]int64, 0, len(all))
	for i := range all {
		byName[all[i].Name] = &all[i]
		ids = append(ids, all[i].ID)
	}
	if aliases, aliasErr := s.repos.ListAliasesByRepos(ids); aliasErr == nil {
		for i := range all {
			for _, alias := range aliases[all[i].ID] {
				if alias == "" {
					continue
				}
				// 主名优先：别名与主名冲突时保持已登记的条目（主名解析优先，与 GetByName 一致）。
				if _, exists := byName[alias]; !exists {
					byName[alias] = &all[i]
				}
			}
		}
	} else {
		// 别名查询失败不应让整条级联失效路径失效，但必须留痕：此时用别名描述的成员会丢边。
		s.logNegativeCacheCascadeFailure(0, aliasErr)
	}
	// memberID -> direct parent group IDs
	parents := make(map[int64][]int64)
	for i := range all {
		if all[i].Type != "group" {
			continue
		}
		cfg, decErr := all[i].DecodeConfig()
		if decErr != nil {
			continue
		}
		for _, memberName := range cfg.Members {
			if m, ok := byName[memberName]; ok {
				parents[m.ID] = append(parents[m.ID], all[i].ID)
			}
		}
	}
	visited := map[int64]bool{targetRepoID: true}
	queue := []int64{targetRepoID}
	var result []int64
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, gid := range parents[cur] {
			if visited[gid] {
				continue
			}
			visited[gid] = true
			result = append(result, gid)
			queue = append(queue, gid)
		}
	}
	return result
}

// recordProxyNegativeCache 在 proxy 上游明确返回 404 时按仓库级配置与 Maven 同坐标 POM 保护决定是否写入负缓存。
func (s *AssetService) recordProxyNegativeCache(repo *repository.Repository, cfg repository.RepositoryConfig, path string) {
	if s.shouldSkipMavenNegativeCache(repo, path) {
		return
	}
	if cfg.NegativeCacheTTLSeconds != nil {
		s.negCache.addWithCustomTTL(repo.ID, path, time.Duration(*cfg.NegativeCacheTTLSeconds)*time.Second)
		return
	}
	s.negCache.add(repo.ID, path)
}

// recordGroupNegativeCache 在 group 所有成员确认不存在时写入负缓存：
//   - 若命中 Maven 同坐标「有 pom 无 jar」保护，不写负缓存；
//   - 若任一在线 proxy 成员显式配置了 negativeCacheTtlSeconds（如 0 禁用或更短 TTL），
//     group 负缓存取其中的最小值（含 0 禁用），避免聚合仓负缓存覆盖成员级防毒配置。
func (s *AssetService) recordGroupNegativeCache(repo *repository.Repository, members []*repository.Repository, path string) {
	if s.shouldSkipMavenNegativeCache(repo, path) {
		return
	}
	hasCustom := false
	var minCustom time.Duration
	for _, member := range members {
		if member == nil || !member.Online || member.Type != "proxy" {
			continue
		}
		mcfg, err := member.DecodeConfig()
		if err != nil || mcfg.NegativeCacheTTLSeconds == nil {
			continue
		}
		d := time.Duration(*mcfg.NegativeCacheTTLSeconds) * time.Second
		if !hasCustom || d < minCustom {
			hasCustom = true
			minCustom = d
		}
	}
	if hasCustom {
		if minCustom <= 0 {
			return
		}
		// 若全局 TTL 更短（含 0 关闭），以更短者为准。
		globalTTL := s.negCache.globalTTL()
		if globalTTL <= 0 {
			return
		}
		if globalTTL < minCustom {
			minCustom = globalTTL
		}
		s.negCache.addWithCustomTTL(repo.ID, path, minCustom)
		return
	}
	s.negCache.add(repo.ID, path)
}

// shouldSkipMavenNegativeCache 防止「有 pom 无 jar」的负缓存中毒：
// 当 Maven 坐标的同目录 .pom 已缓存在本地（且 <packaging> 不是 pom）时，
// 该坐标对应的主 .jar 或运行时分类 .jar（排除 -sources.jar / -javadoc.jar 等可选附件）
// 遇到上游 404 大概率是瞬时 404 / 上游半发布 / 镜像毒缓存，绝不写入负缓存锁死构建。
//
// 查找范围必须超出本条仓库：客户端通常经 group 拉 POM，POM 会落在**最先服务它的那个成员**上。
// 若只看本 proxy 的本地缓存，当 POM 恰好在兄弟成员时本条 proxy 判定「无 POM 证据」并照写负缓存，
// 之后 group 读进入 phase 2 会在这个成员上被 negCache.hit 直接 404、根本不回源——
// 守护只对「POM 正好缓存在本条 proxy」的成员生效，等于没守住。
// 因此这里在自身未命中时再向所有父 group 找一次（父 group 的递归读会覆盖全部成员）。
func (s *AssetService) shouldSkipMavenNegativeCache(repo *repository.Repository, artPath string) bool {
	if repo == nil || repo.Format != "maven" {
		return false
	}
	pomPath, ok := mavenSiblingPomPath(artPath)
	if !ok {
		return false
	}
	if s.hasLocalMavenPom(repo, pomPath) {
		return true
	}
	// 只在自身未命中时向上找：命中即返回，常态（POM 与 jar 同仓）不会多付一次反查。
	for _, gid := range s.parentGroupIDs(repo.ID) {
		parent, err := s.repos.GetByID(gid)
		if err != nil || parent == nil {
			continue
		}
		if s.hasLocalMavenPom(parent, pomPath) {
			return true
		}
	}
	return false
}

// hasLocalMavenPom 报告 pomPath 是否已缓存在 repo（含其嵌套 group 成员）上，
// 且该 POM 声明的 packaging 意味着同坐标应有 jar。POM 无法解析时按「应有 jar」保守处理。
func (s *AssetService) hasLocalMavenPom(repo *repository.Repository, pomPath string) bool {
	_, rc, err := s.localGetRecursive(repo, pomPath, 0)
	if err != nil {
		return false
	}
	defer func() { _ = rc.Close() }()
	data, readErr := io.ReadAll(io.LimitReader(rc, maxMavenSiblingPomReadBytes))
	if readErr != nil || len(data) == 0 {
		return false
	}
	var pom struct {
		Packaging string `xml:"packaging"`
	}
	if xmlErr := xml.Unmarshal(data, &pom); xmlErr != nil {
		// POM 存在但无法解析 XML 时，出于保守起见不写负缓存。
		return true
	}
	// packaging=pom 的模块本就没有 jar，允许正常负缓存；其余（空/jar/bundle 等）应有 jar，跳过负缓存。
	return !strings.EqualFold(strings.TrimSpace(pom.Packaging), "pom")
}

// mavenSiblingPomPath 判断 artPath 是否为受「有 pom 无 jar」保护的 Maven JAR 路径，
// 并推导出同坐标的 .pom 路径（.../<artifactId>/<version>/<artifactId>-<version>.pom）。
func mavenSiblingPomPath(artPath string) (string, bool) {
	cleaned := strings.Trim(artPath, "/")
	lower := strings.ToLower(cleaned)
	if !strings.HasSuffix(lower, ".jar") {
		return "", false
	}
	// 排除 IDE/构建工具频繁探测的可选附件（它们天然可能不存在，应继续享受快速负缓存）。
	for _, optionalSuffix := range []string{
		"-sources.jar",
		"-javadoc.jar",
		"-test-sources.jar",
		"-tests.jar",
		"-groovydoc.jar",
		"-scaladoc.jar",
	} {
		if strings.HasSuffix(lower, optionalSuffix) {
			return "", false
		}
	}
	parts := strings.Split(cleaned, "/")
	if len(parts) < 4 {
		return "", false
	}
	artifactID := parts[len(parts)-3]
	version := parts[len(parts)-2]
	filename := parts[len(parts)-1]
	if artifactID == "" || version == "" || strings.HasSuffix(artifactID, ".gradle.plugin") {
		return "", false
	}
	dir := strings.Join(parts[:len(parts)-1], "/")

	// SNAPSHOT 走另一套命名：协议层已把 .../1.0-SNAPSHOT/foo-1.0-SNAPSHOT.jar
	// 改写成带时间戳的 foo-1.0-20260317.215705-3.jar，此时 artifactID-version 拼出的
	// "foo-1.0-SNAPSHOT" 与真实文件名对不上，若不单独处理整个守护对 SNAPSHOT 完全失效——
	// 而「pom 已发布、jar 还没传完」正是 SNAPSHOT 最典型的瞬时 404 场景。
	//
	// SNAPSHOT 目录下的同坐标 POM 与 jar 共享同一个前缀（都带同一时间戳），
	// 因此直接用文件名去 .jar 换 .pom 即可，无需也不该回退到字面 SNAPSHOT 名。
	if strings.HasSuffix(strings.ToUpper(version), "-SNAPSHOT") {
		base, ok := strings.CutSuffix(filename, ".jar")
		if !ok || base == "" {
			return "", false
		}
		// 可选附件（-sources/-javadoc 等）的时间戳形态形如 foo-1.0-20260317.215705-3-sources.jar，
		// 已在上面按后缀排除；这里剩余的均视为主构件或运行时 classifier。
		return dir + "/" + base + ".pom", true
	}

	prefix := artifactID + "-" + version
	// 匹配主 jar（<artifact>-<version>.jar）以及带运行时分类器的 jar（如 v12004-12004-mapped.jar / -universal.jar）。
	if filename != prefix+".jar" && !strings.HasPrefix(filename, prefix+"-") {
		return "", false
	}
	return dir + "/" + prefix + ".pom", true
}
