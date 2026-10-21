package repository

import (
	"context"
)

// 搜索建议的候选集查询。
//
// 候选集与搜索主链路共用**同一段 WHERE**（buildSearchWhereIndexed），因此三件事天然一致：
//   - 索引已就绪且存在可下推的包含词 → 走 asset_path_fts 候选集；
//   - 索引未就绪（回填/重建中）或词太短（rune < 3）→ 整体退回既有 path LIKE 路径，
//     suggest 照常可用，只是没提速（规格 §3.3 的降级口径，本文件不再重复判定）；
//   - 同一套排除词 / 扩展名 / 仓库范围谓词原样作为候选集上的后置条件。
//
// 抽出独立类型的理由：suggest 只需要 (repository_id, path) 两列——不取 size/hash 等元数据，
// 也不做 COUNT(*)。把「一次轻量查询」做成完整搜索的代价是每次键入都跑三条聚合语句，
// 与 suggest 的目的相悖（规格 §3.8「不带命中计数」）。

// SuggestRow 是包名候选的输入行：一条命中资产的所属仓库与仓库内路径。
type SuggestRow struct {
	RepositoryID int64  `db:"repository_id"`
	Path         string `db:"path"`
}

// SuggestPaths 从候选集中取前 limit 条 (repository_id, path)，供领域层抽取包名候选。
//
// **顺序必须是确定的**：候选顺序若取决于 FTS 内部回表顺序（rowid 序），同一输入在不同时刻会
// 给出不同的候选池，去重后的结果随之抖动（规格 §3.8「稳定序」）。这里固定 `ORDER BY path ASC`
// 截断，与 SearchAllByFilter 的聚合预算同一手法：水位由路径决定，不随执行计划变化。
//
// ctx 承载领域层施加的搜索总预算（与三条搜索语句共享同一个 deadline）；repoIDs 为空表示不限
// （管理员），非空时由调用方保证已按可读范围收窄。
func (r *AssetRepo) SuggestPaths(ctx context.Context, f SearchFilter, repoIDs []int64, limit int) ([]SuggestRow, error) {
	if limit <= 0 {
		return nil, nil
	}
	where, args := buildSearchWhereIndexed(f, repoIDs)
	var rows []SuggestRow
	err := r.db.SelectContext(ctx, &rows,
		`SELECT repository_id, path FROM asset WHERE `+where+` ORDER BY path ASC LIMIT ?`,
		append(args, limit)...,
	)
	return rows, err
}
