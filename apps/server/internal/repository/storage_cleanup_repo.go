package repository

import (
	"fmt"

	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
)

// 本文件承载 FR-41 存储治理清理作业专用的元数据读写。
//
// 与 AssetMutationRepo / AssetRepo 分成不同文件与类型：清理作业的查询条件
// （终态状态 + 时间窗裁剪、按 updated_at 选最旧资产）与业务读写路径完全不同，
// 独立类型便于把「只读终态、绝不碰在途」这条安全约束收敛到一处。

// StorageCleanupRepo 是清理作业的元数据入口。
type StorageCleanupRepo struct{ db *persistence.DB }

// NewStorageCleanupRepo 构造清理作业仓储。
func NewStorageCleanupRepo(db *persistence.DB) *StorageCleanupRepo {
	return &StorageCleanupRepo{db: db}
}

// StoragePruneCounts 是一轮元数据裁剪的计数结果（供作业日志）。
type StoragePruneCounts struct {
	Mutations  int // asset_mutation 终态行
	Items      int // asset_mutation_item 明细行
	Quarantine int // blob_quarantine 终态行
}

// pruneCandidateClause 选出「可安全裁剪的终态操作」。
//
// 两个条件都不可放宽：
//   - status 只认 asset_mutation 的终态（completed / rolled_back）；prepared / staged /
//     committing / rolling_back 表示操作尚未结束或仍需启动恢复推进，绝不裁剪。
//   - NOT EXISTS 要求该操作下没有任何非终态的隔离记录。孤立看终态行是不够的：
//     一个 completed 操作完全可能仍挂着 pending_gc 的隔离记录（物理回收失败留下的滞留件），
//     此时删除父行会连带删除这条唯一记录，让隔离文件变成「有字节、无元数据」的永久垃圾，
//     且再也无法被恢复或回收。因此宁可保留元数据，也不裁剪。
const pruneCandidateClause = `SELECT id FROM asset_mutation
	WHERE status IN ('completed','rolled_back')
	  AND updated_at < ?
	  AND NOT EXISTS (
	    SELECT 1 FROM blob_quarantine q
	    WHERE q.operation_id = asset_mutation.id AND q.status NOT IN ('deleted','restored')
	  )`

// PruneTerminalMutations 删除早于 cutoff 的终态操作行及其明细与隔离终态记录。
// cutoff 与 updated_at 同为 UTC 的 "2006-01-02 15:04:05" 文本，字典序即时间序。
// 三条语句使用同一个候选集合：子行只随终态父行一起消失，候选集合在事务内保持稳定。
func (r *StorageCleanupRepo) PruneTerminalMutations(cutoff string) (StoragePruneCounts, error) {
	var counts StoragePruneCounts
	tx, err := r.db.Beginx()
	if err != nil {
		return counts, err
	}
	defer func() { _ = tx.Rollback() }()
	statements := []struct {
		query  string
		target *int
	}{
		{`DELETE FROM blob_quarantine WHERE operation_id IN (` + pruneCandidateClause + `)`, &counts.Quarantine},
		{`DELETE FROM asset_mutation_item WHERE operation_id IN (` + pruneCandidateClause + `)`, &counts.Items},
		{`DELETE FROM asset_mutation WHERE id IN (` + pruneCandidateClause + `)`, &counts.Mutations},
	}
	for _, statement := range statements {
		result, err := tx.Exec(statement.query, cutoff)
		if err != nil {
			return StoragePruneCounts{}, fmt.Errorf("裁剪操作元数据：%w", err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return StoragePruneCounts{}, err
		}
		*statement.target = int(affected)
	}
	if err := tx.Commit(); err != nil {
		return StoragePruneCounts{}, err
	}
	return counts, nil
}

// ListProxyRepositories 返回全部 proxy 类型仓库（含 config 列），供代理缓存保留判定。
func (r *StorageCleanupRepo) ListProxyRepositories() ([]Repository, error) {
	var repos []Repository
	err := r.db.Select(&repos, `SELECT id, name, format, type, visibility, description, config, online, created_at
		FROM repository WHERE type='proxy' ORDER BY id`)
	return repos, err
}

// ListStaleAssets 返回仓库内 updated_at 早于 cutoff 的资产，最旧的优先，最多 limit 条。
// 供代理缓存保留按时间淘汰；limit <= 0 时不查询（调用方以每轮上限收敛删除量）。
func (r *StorageCleanupRepo) ListStaleAssets(repositoryID int64, cutoff string, limit int) ([]Asset, error) {
	if limit <= 0 {
		return nil, nil
	}
	var assets []Asset
	err := r.db.Select(&assets, `SELECT id, repository_id, path, blob_hash, size, content_type, sha1, md5, created_at, updated_at
		FROM asset WHERE repository_id=? AND updated_at < ? ORDER BY updated_at, path LIMIT ?`, repositoryID, cutoff, limit)
	return assets, err
}
