package domain

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/archive"
)

// 包内容与当前系统的差异对比。
//
// # 性能是这个功能的第一约束
//
// 真实实例可能有几十万制品、20GB 数据。因此这里有三条硬规矩：
//
//  1. **只在 SQL 层做差集**。把两侧数据全搬到 Go 里再比，内存与耗时都不可接受。
//  2. **摘要与明细分离**。默认只返回各分类的**计数**（聚合查询，代价与数据量同阶但常量极小）；
//     用户点开某一类才分页拉明细（代价 = 页大小）。
//  3. **解包只做一次**。解出包内 db 快照的代价与包体积同阶（20GB 是分钟级）。
//     若每次翻页都解一遍，界面会被自己卡死。因此快照解出后**缓存在临时目录**，
//     带 TTL 与"同时最多 N 个"的上限。
//
// # 对比的粒度（必须如实告知用户）
//
// 比较的是**元数据**：仓库名、制品路径、大小、blob 哈希。
// 不比对文件内容——逐字节验证 20GB 需要读两份完整数据，且已有"深度校验"承担那个职责。
// 界面上必须写明这一点，否则"对比无差异"会被误读成"字节也完全一致"。
//
// # 缓存上限为什么是 2
//
// 每个缓存的快照约等于包内 db 的大小。按 20GB 数据估算，两个快照已接近磁盘压力上限；
// 再多可能在"用户只是想对比一下"的场景里把盘写满。超出时淘汰最旧的。

const (
	// diffSnapshotTTL 是包内快照的缓存存活时间。超过即清理，避免长期占盘。
	diffSnapshotTTL = 10 * time.Minute
	// maxDiffSnapshots 是同时缓存的快照数上限（详见文件头）。
	maxDiffSnapshots = 2
	// maxDiffDetailPageSize 是单次明细请求的硬上限，防止一次拉回几十万条。
	maxDiffDetailPageSize = 200
	// defaultDiffDetailPageSize 是默认页大小。
	defaultDiffDetailPageSize = 50
)

// DiffKind 是差异分类。
type DiffKind string

const (
	// DiffOnlyInPackage 包里有、本机没有 —— 恢复后会新增。
	DiffOnlyInPackage DiffKind = "onlyInPackage"
	// DiffOnlyLocal 本机有、包里没有 —— 恢复后会消失。
	DiffOnlyLocal DiffKind = "onlyLocal"
	// DiffChanged 两边都有但内容（哈希/大小）不同 —— 恢复后会被替换。
	DiffChanged DiffKind = "changed"
	// DiffSame 完全一致 —— 恢复后不变。
	DiffSame DiffKind = "same"
)

// DiffEntity 是可对比的实体类型。
type DiffEntity string

const (
	DiffEntityRepositories DiffEntity = "repositories"
	DiffEntityAssets       DiffEntity = "assets"
	DiffEntityUsers        DiffEntity = "users"
)

// DiffCounts 是一类实体的四分类计数。
type DiffCounts struct {
	OnlyInPackage int `json:"onlyInPackage"`
	OnlyLocal     int `json:"onlyLocal"`
	Changed       int `json:"changed"`
	Same          int `json:"same"`
}

// BackupDiffSummary 是对比的摘要（不含明细，因此很小）。
type BackupDiffSummary struct {
	PackageID    string     `json:"packageId"`
	Repositories DiffCounts `json:"repositories"`
	Assets       DiffCounts `json:"assets"`
	Users        DiffCounts `json:"users"`
	// MetadataOnly 恒为 true：提醒调用方本次只比元数据。
	MetadataOnly bool `json:"metadataOnly"`
}

// DiffDetailItem 是一条差异明细。
type DiffDetailItem struct {
	// Key 是该项的稳定标识（仓库名 / 制品路径 / 用户名）。
	Key string `json:"key"`
	// Detail 是供人判断的补充说明（如大小变化、哈希前缀）。
	Detail string `json:"detail"`
}

// DiffDetailPage 是一页明细。
type DiffDetailPage struct {
	Items []DiffDetailItem `json:"items"`
	Total int              `json:"total"`
	// Truncated 表示因页大小上限而未返回全部（Total 仍给真实总数）。
	Truncated bool `json:"truncated"`
}

// diffSnapshotCache 缓存"已解出的包内数据库快照"。
//
// 只缓存路径与时间，不缓存数据本身：真正的查询仍走 SQLite。
type diffSnapshotCache struct {
	mu      sync.Mutex
	entries map[string]diffCacheEntry
}

type diffCacheEntry struct {
	dbPath  string // 解出的 db 路径（位于系统临时目录）
	dir     string // 包含 dbPath 的临时目录，清理时整目录删
	usedAt  time.Time
	fileKey string // 包体 mtime+size，包被替换时据此失效
}

var globalDiffCache = &diffSnapshotCache{entries: map[string]diffCacheEntry{}}

// snapshotFor 返回包内快照的 db 路径，必要时解包。
//
// 缓存命中条件：同一包 + 文件指纹未变 + 未超 TTL。
// 未命中时解包（与包体积同阶的代价），并把结果放进缓存。
func (s *BackupService) snapshotFor(packageID string) (string, error) {
	pkgPath := s.PackagePath(packageID)
	info, err := os.Stat(pkgPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", ErrBackupFileMissing
		}
		return "", fmt.Errorf("检查备份包：%w", err)
	}
	fileKey := fmt.Sprintf("%d:%d", info.ModTime().UnixNano(), info.Size())

	globalDiffCache.mu.Lock()
	entry, ok := globalDiffCache.entries[packageID]
	globalDiffCache.mu.Unlock()
	if ok && entry.fileKey == fileKey && time.Since(entry.usedAt) < diffSnapshotTTL {
		// 命中：刷新使用时间（LRU 语义），并确认文件还在（临时目录可能被外部清理）。
		if _, statErr := os.Stat(entry.dbPath); statErr == nil {
			globalDiffCache.mu.Lock()
			entry.usedAt = time.Now()
			globalDiffCache.entries[packageID] = entry
			globalDiffCache.mu.Unlock()
			return entry.dbPath, nil
		}
	}

	// 未命中：解包。先清理过期与超限的缓存，避免边解边把盘塞满。
	globalDiffCache.evict()

	reader, _, openErr := archive.Open(pkgPath)
	if openErr != nil {
		return "", fmt.Errorf("打开备份包：%w", openErr)
	}
	tmpDir, mkErr := os.MkdirTemp("", "jianartifact-diff-")
	if mkErr != nil {
		return "", fmt.Errorf("创建临时目录：%w", mkErr)
	}
	dbPath := filepath.Join(tmpDir, archive.DBName)
	// 只解 db：blob 与索引对元数据对比没有帮助。
	if extractErr := extractDBOnly(reader, dbPath); extractErr != nil {
		_ = os.RemoveAll(tmpDir)
		return "", fmt.Errorf("解出数据库快照：%w", extractErr)
	}

	globalDiffCache.mu.Lock()
	globalDiffCache.entries[packageID] = diffCacheEntry{
		dbPath:  dbPath,
		dir:     tmpDir,
		usedAt:  time.Now(),
		fileKey: fileKey,
	}
	globalDiffCache.mu.Unlock()
	return dbPath, nil
}

// evict 清理过期条目，并在超限时淘汰最旧的。
func (c *diffSnapshotCache) evict() {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	for id, e := range c.entries {
		if now.Sub(e.usedAt) >= diffSnapshotTTL {
			_ = os.RemoveAll(e.dir)
			delete(c.entries, id)
		}
	}
	// 仍超限：按使用时间淘汰最旧的，直到不超。
	for len(c.entries) >= maxDiffSnapshots {
		oldestID := ""
		var oldest time.Time
		for id, e := range c.entries {
			if oldestID == "" || e.usedAt.Before(oldest) {
				oldestID, oldest = id, e.usedAt
			}
		}
		if oldestID == "" {
			break
		}
		e := c.entries[oldestID]
		_ = os.RemoveAll(e.dir)
		delete(c.entries, oldestID)
	}
}

// DiffSummary 计算包内容与当前系统的差异摘要。
//
// 只做聚合查询：返回的是四类计数，不搬任何明细。因此代价与"扫两遍索引"同阶，
// 与制品数量线性但常数极小——这是设计上刻意的（见文件头的性能约束）。
func (s *BackupService) DiffSummary(packageID string) (BackupDiffSummary, error) {
	out := BackupDiffSummary{PackageID: packageID, MetadataOnly: true}
	if _, err := s.Get(packageID); err != nil {
		return out, err
	}
	err := s.withPackageAttached(packageID, func() error {
		var err error
		if out.Repositories, err = s.readDiffCounts(DiffEntityRepositories); err != nil {
			return err
		}
		if out.Assets, err = s.readDiffCounts(DiffEntityAssets); err != nil {
			return err
		}
		if out.Users, err = s.readDiffCounts(DiffEntityUsers); err != nil {
			return err
		}
		return nil
	})
	return out, err
}

// readDiffCounts 执行某实体的四分类聚合。
func (s *BackupService) readDiffCounts(entity DiffEntity) (DiffCounts, error) {
	var c DiffCounts
	spec, ok := diffSpecs[entity]
	if !ok {
		return c, fmt.Errorf("%w：未知的对比实体 %q", ErrValidation, entity)
	}
	if err := s.db.QueryRow(spec.countSQL).Scan(
		&c.OnlyInPackage, &c.OnlyLocal, &c.Changed, &c.Same); err != nil {
		return c, fmt.Errorf("对比 %s：%w", entity, err)
	}
	return c, nil
}

// DiffDetail 拉取某一类差异的明细（分页）。
//
// 单页上限 maxDiffDetailPageSize：几十万制品时，界面一次只可能看几十条，
// 拉全量既浪费带宽也让浏览器卡顿。Total 仍给真实总数，供界面显示"还有 N 条"。
func (s *BackupService) DiffDetail(packageID string, entity DiffEntity, kind DiffKind, page, pageSize int) (DiffDetailPage, error) {
	out := DiffDetailPage{Items: []DiffDetailItem{}}
	if _, err := s.Get(packageID); err != nil {
		return out, err
	}
	spec, ok := diffSpecs[entity]
	if !ok {
		return out, fmt.Errorf("%w：未知的对比实体 %q", ErrValidation, entity)
	}
	detailSQL, ok := spec.detailSQL[kind]
	if !ok {
		return out, fmt.Errorf("%w：未知的差异分类 %q", ErrValidation, kind)
	}

	if pageSize <= 0 {
		pageSize = defaultDiffDetailPageSize
	}
	if pageSize > maxDiffDetailPageSize {
		pageSize = maxDiffDetailPageSize
	}
	if page < 1 {
		page = 1
	}

	err := s.withPackageAttached(packageID, func() error {
		// 总数与明细来自同一份口径（同一 ATTACH 下的两张表），避免两处漂移。
		counts, err := s.readDiffCounts(entity)
		if err != nil {
			return err
		}
		out.Total = counts.forKind(kind)
		if out.Total == 0 {
			return nil
		}
		rows, err := s.db.Query(detailSQL, pageSize, (page-1)*pageSize)
		if err != nil {
			return fmt.Errorf("读取明细：%w", err)
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var item DiffDetailItem
			if scanErr := rows.Scan(&item.Key, &item.Detail); scanErr == nil {
				out.Items = append(out.Items, item)
			}
		}
		out.Truncated = page*pageSize < out.Total
		return nil
	})
	return out, err
}

// forKind 按分类取对应计数。
func (c DiffCounts) forKind(kind DiffKind) int {
	switch kind {
	case DiffOnlyInPackage:
		return c.OnlyInPackage
	case DiffOnlyLocal:
		return c.OnlyLocal
	case DiffChanged:
		return c.Changed
	case DiffSame:
		return c.Same
	}
	return 0
}

// diffSpec 描述一类实体的对比：如何取计数、如何取明细。
//
// 所有 SQL 都在 ATTACH 之后执行：主库是本地活动库，pkg 是包内快照。
// 差集因此能**在数据库内完成**，不需要把两侧数据搬到 Go 里比——那是性能红线。
type diffSpec struct {
	// countSQL 返回一行四列：onlyInPackage, onlyLocal, changed, same。
	countSQL string
	// detailSQL 按分类返回 (key, detail) 两列，带 LIMIT ? OFFSET ?。
	// 四个分类各一条，用 {KIND} 占位以复用公共结构。
	detailSQL map[DiffKind]string
}

// diffSpecs 是各实体的对比定义。
//
// 对比口径（必须与界面的说明一致）：只有**元数据**——
// 仓库比 name/format/type/visibility，制品比 (仓库名, 路径, blob 哈希, 大小)，
// 账号比 username/role/status。不比对文件内容。
var diffSpecs = map[DiffEntity]diffSpec{
	DiffEntityRepositories: {
		// 与资产同理：必须两侧都覆盖，否则"本机独有"统计不到。
		countSQL: `
WITH p AS (SELECT name, format, type, visibility FROM pkg.repository),
     l AS (SELECT name, format, type, visibility FROM repository),
     j AS (
       SELECT p.name AS p_name, l.name AS l_name, p.format AS p_fmt, p.type AS p_type,
              p.visibility AS p_vis, l.format AS l_fmt, l.type AS l_type, l.visibility AS l_vis
       FROM p LEFT JOIN l ON l.name = p.name
       UNION ALL
       SELECT NULL, l.name, NULL, NULL, NULL, l.format, l.type, l.visibility
       FROM l
       WHERE NOT EXISTS (SELECT 1 FROM p WHERE p.name = l.name)
     )
SELECT
  COALESCE(SUM(CASE WHEN l_name IS NULL THEN 1 ELSE 0 END), 0),
  COALESCE(SUM(CASE WHEN p_name IS NULL THEN 1 ELSE 0 END), 0),
  COALESCE(SUM(CASE WHEN p_name IS NOT NULL AND l_name IS NOT NULL AND
        (p_fmt <> l_fmt OR p_type <> l_type OR p_vis <> l_vis) THEN 1 ELSE 0 END), 0),
  COALESCE(SUM(CASE WHEN p_name IS NOT NULL AND l_name IS NOT NULL AND
        p_fmt = l_fmt AND p_type = l_type AND p_vis = l_vis THEN 1 ELSE 0 END), 0)
FROM j`,
		detailSQL: map[DiffKind]string{
			DiffOnlyInPackage: `
SELECT p.name, p.format || ' / ' || p.type || ' / ' || p.visibility
FROM (SELECT name, format, type, visibility FROM pkg.repository) p
LEFT JOIN (SELECT name FROM repository) l ON l.name = p.name
WHERE l.name IS NULL ORDER BY p.name LIMIT ? OFFSET ?`,
			DiffOnlyLocal: `
SELECT p.name, p.format || ' / ' || p.type || ' / ' || p.visibility
FROM (SELECT name, format, type, visibility FROM repository) p
LEFT JOIN (SELECT name FROM pkg.repository) l ON l.name = p.name
WHERE l.name IS NULL ORDER BY p.name LIMIT ? OFFSET ?`,
			DiffChanged: `
SELECT p.name, '本机 ' || l.format || '/' || l.type || '/' || l.visibility
  || ' → 包内 ' || p.format || '/' || p.type || '/' || p.visibility
FROM (SELECT name, format, type, visibility FROM pkg.repository) p
JOIN (SELECT name, format, type, visibility FROM repository) l ON l.name = p.name
WHERE l.format <> p.format OR l.type <> p.type OR l.visibility <> p.visibility
ORDER BY p.name LIMIT ? OFFSET ?`,
			DiffSame: `
SELECT p.name, p.format || ' / ' || p.type || ' / ' || p.visibility
FROM (SELECT name, format, type, visibility FROM pkg.repository) p
JOIN (SELECT name, format, type, visibility FROM repository) l ON l.name = p.name
WHERE l.format = p.format AND l.type = p.type AND l.visibility = p.visibility
ORDER BY p.name LIMIT ? OFFSET ?`,
		},
	},
	DiffEntityAssets: {
		// 用 UNION ALL 模拟 FULL OUTER JOIN（SQLite 无原生 full join）。
		//
		// 早先只从包侧 LEFT JOIN，于是"本机有、包里没有"的制品**永远统计不到**：
		// 包侧没有这一行，join 结果里也就没有它。测试直接抓到了这个缺陷。
		countSQL: `
WITH p AS (SELECT a.path, a.blob_hash, a.size, r.name AS repo FROM pkg.asset a
           JOIN pkg.repository r ON r.id = a.repository_id),
     l AS (SELECT a.path, a.blob_hash, a.size, r.name AS repo FROM asset a
           JOIN repository r ON r.id = a.repository_id),
     j AS (
       SELECT p.path AS p_path, l.path AS l_path,
              p.blob_hash AS p_hash, l.blob_hash AS l_hash,
              p.size AS p_size, l.size AS l_size
       FROM p LEFT JOIN l ON l.repo = p.repo AND l.path = p.path
       UNION ALL
       SELECT NULL, l.path, NULL, l.blob_hash, NULL, l.size
       FROM l
       WHERE NOT EXISTS (SELECT 1 FROM p WHERE p.repo = l.repo AND p.path = l.path)
     )
SELECT
  COALESCE(SUM(CASE WHEN l_path IS NULL THEN 1 ELSE 0 END), 0),
  COALESCE(SUM(CASE WHEN p_path IS NULL THEN 1 ELSE 0 END), 0),
  COALESCE(SUM(CASE WHEN p_path IS NOT NULL AND l_path IS NOT NULL AND
        (p_hash <> l_hash OR p_size <> l_size) THEN 1 ELSE 0 END), 0),
  COALESCE(SUM(CASE WHEN p_path IS NOT NULL AND l_path IS NOT NULL AND
        p_hash = l_hash AND p_size = l_size THEN 1 ELSE 0 END), 0)
FROM j`,
		detailSQL: map[DiffKind]string{
			DiffOnlyInPackage: `
SELECT p.repo || '/' || p.path, p.size || ' 字节'
FROM (SELECT a.path, a.size, r.name AS repo FROM pkg.asset a
      JOIN pkg.repository r ON r.id = a.repository_id) p
LEFT JOIN (SELECT a.path, r.name AS repo FROM asset a
           JOIN repository r ON r.id = a.repository_id) l
  ON l.repo = p.repo AND l.path = p.path
WHERE l.path IS NULL ORDER BY p.repo, p.path LIMIT ? OFFSET ?`,
			DiffOnlyLocal: `
SELECT p.repo || '/' || p.path, p.size || ' 字节'
FROM (SELECT a.path, a.size, r.name AS repo FROM asset a
      JOIN repository r ON r.id = a.repository_id) p
LEFT JOIN (SELECT a.path, r.name AS repo FROM pkg.asset a
           JOIN pkg.repository r ON r.id = a.repository_id) l
  ON l.repo = p.repo AND l.path = p.path
WHERE l.path IS NULL ORDER BY p.repo, p.path LIMIT ? OFFSET ?`,
			DiffChanged: `
SELECT p.repo || '/' || p.path,
       '大小 ' || l.size || ' → ' || p.size || '；哈希 ' || substr(l.blob_hash,1,8) || ' → ' || substr(p.blob_hash,1,8)
FROM (SELECT a.path, a.blob_hash, a.size, r.name AS repo FROM pkg.asset a
      JOIN pkg.repository r ON r.id = a.repository_id) p
JOIN (SELECT a.path, a.blob_hash, a.size, r.name AS repo FROM asset a
      JOIN repository r ON r.id = a.repository_id) l
  ON l.repo = p.repo AND l.path = p.path
WHERE l.blob_hash <> p.blob_hash OR l.size <> p.size
ORDER BY p.repo, p.path LIMIT ? OFFSET ?`,
			DiffSame: `
SELECT p.repo || '/' || p.path, p.size || ' 字节'
FROM (SELECT a.path, a.blob_hash, a.size, r.name AS repo FROM pkg.asset a
      JOIN pkg.repository r ON r.id = a.repository_id) p
JOIN (SELECT a.path, a.blob_hash, a.size, r.name AS repo FROM asset a
      JOIN repository r ON r.id = a.repository_id) l
  ON l.repo = p.repo AND l.path = p.path
WHERE l.blob_hash = p.blob_hash AND l.size = p.size
ORDER BY p.repo, p.path LIMIT ? OFFSET ?`,
		},
	},
	DiffEntityUsers: {
		countSQL: `
WITH p AS (SELECT username, role, status FROM pkg.user),
     l AS (SELECT username, role, status FROM user),
     j AS (
       SELECT p.username AS p_user, l.username AS l_user,
              p.role AS p_role, p.status AS p_status,
              l.role AS l_role, l.status AS l_status
       FROM p LEFT JOIN l ON l.username = p.username
       UNION ALL
       SELECT NULL, l.username, NULL, NULL, l.role, l.status
       FROM l
       WHERE NOT EXISTS (SELECT 1 FROM p WHERE p.username = l.username)
     )
SELECT
  COALESCE(SUM(CASE WHEN l_user IS NULL THEN 1 ELSE 0 END), 0),
  COALESCE(SUM(CASE WHEN p_user IS NULL THEN 1 ELSE 0 END), 0),
  COALESCE(SUM(CASE WHEN p_user IS NOT NULL AND l_user IS NOT NULL AND
        (p_role <> l_role OR p_status <> l_status) THEN 1 ELSE 0 END), 0),
  COALESCE(SUM(CASE WHEN p_user IS NOT NULL AND l_user IS NOT NULL AND
        p_role = l_role AND p_status = l_status THEN 1 ELSE 0 END), 0)
FROM j`,
		detailSQL: map[DiffKind]string{
			DiffOnlyInPackage: `
SELECT p.username, p.role || ' / ' || p.status
FROM (SELECT username, role, status FROM pkg.user) p
LEFT JOIN (SELECT username FROM user) l ON l.username = p.username
WHERE l.username IS NULL ORDER BY p.username LIMIT ? OFFSET ?`,
			DiffOnlyLocal: `
SELECT p.username, p.role || ' / ' || p.status
FROM (SELECT username, role, status FROM user) p
LEFT JOIN (SELECT username FROM pkg.user) l ON l.username = p.username
WHERE l.username IS NULL ORDER BY p.username LIMIT ? OFFSET ?`,
			DiffChanged: `
SELECT p.username, '本机 ' || l.role || '/' || l.status || ' → 包内 ' || p.role || '/' || p.status
FROM (SELECT username, role, status FROM pkg.user) p
JOIN (SELECT username, role, status FROM user) l ON l.username = p.username
WHERE l.role <> p.role OR l.status <> p.status
ORDER BY p.username LIMIT ? OFFSET ?`,
			DiffSame: `
SELECT p.username, p.role || ' / ' || p.status
FROM (SELECT username, role, status FROM pkg.user) p
JOIN (SELECT username, role, status FROM user) l ON l.username = p.username
WHERE l.role = p.role AND l.status = p.status
ORDER BY p.username LIMIT ? OFFSET ?`,
		},
	},
}

// withPackageAttached 在本地库连接上临时挂载包内快照，执行 fn 后卸载。
//
// 为什么用 ATTACH 而不是两侧各查一遍再在 Go 里比：后者要把全量数据搬进内存，
// 几十万制品时既慢又占内存；而 ATTACH 后差集完全在 SQLite 内完成，
// 数据始终留在磁盘页缓存里。
//
// 连接池上限为 1（见 persistence.buildDSN 的 SetMaxOpenConns），
// 因此挂载/卸载不会在并发请求间互相干扰；这里仍用互斥锁把整个 fn 包起来，
// 防止同一连接上两个 goroutine 交错 ATTACH。
func (s *BackupService) withPackageAttached(packageID string, fn func() error) error {
	snapPath, err := s.snapshotFor(packageID)
	if err != nil {
		return err
	}
	s.diffMu.Lock()
	defer s.diffMu.Unlock()

	if _, err := s.db.Exec(`ATTACH DATABASE ? AS pkg`, snapPath); err != nil {
		return fmt.Errorf("挂载包内快照：%w", err)
	}
	defer func() { _, _ = s.db.Exec(`DETACH DATABASE pkg`) }()
	return fn()
}
