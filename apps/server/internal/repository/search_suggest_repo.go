package repository

import (
	"context"

	"github.com/jmoiron/sqlx"
)

// 搜索建议的候选集查询。
//
// 候选集与搜索主链路共用**同一段 WHERE**（buildSearchWhereIndexed），因此三个口径对齐：
//   - 索引已就绪且存在可下推的包含词 → 额外前置 asset_path_fts 候选集条件收敛行集；包含词
//     自身的 path LIKE 条件仍然保留，因为 FTS 的大小写折叠范围是 LIKE 的超集，最终要靠后置
//     LIKE 复核才能与降级路径给出同一结果集；
//   - 索引未就绪（回填/重建中）或词太短（rune < 3）→ 整体退回既有 path LIKE 路径，
//     suggest 照常可用，只是没提速（规格 §3.3 的降级口径，本文件不再重复判定）；
//   - 排除词 / 扩展名 / 仓库范围谓词原样作为候选集上的后置条件。
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
// 排序键补到**全序**（`path ASC, id ASC`）而不是只按 path：唯一约束是 (repository_id, path)，
// 跨仓库的同名路径在 path 上完全并列，SQLite 对并列行的返回顺序不作保证；只按 path 截断时，
// 边界上的并列行仍可能在两次执行间互换，去重后的包名候选池跟着抖动。
//
// ctx 承载领域层施加的搜索总预算（与三条搜索语句共享同一个 deadline）。
//
// repoIDs 的**nil 与空切片含义不同**，这是本方法与其它搜索方法的唯一差别：
//   - nil 表示不限（管理员）：领域层的可读范围解析对管理员显式返回 nil（「搜全部」）；
//   - 非 nil 的空切片表示「一个可读仓库都没有」，直接返回空结果（fail-closed）。
//
// 为什么必须区分：SearchFilter 的仓库维度用「空切片 = 不限」表达——空切片时
// `repository_id IN (...)` 条件根本不出现，整条语句退化成全租户扫描。suggest 的调用方**总是**
// 先按可读范围收窄（收窄到空集 = 没有仓库可见，本就该给空候选），一旦某个调用点漏了
// 「收窄后为空就提前返回」这一步，空切片就会静默变成「扫遍所有租户」，把别人的包名当候选
// 返回——这是权限面的泄漏，不能指望调用方自觉。因此这里对空切片选择少给候选而非放宽范围。
func (r *AssetRepo) SuggestPaths(ctx context.Context, f SearchFilter, repoIDs []int64, limit int) ([]SuggestRow, error) {
	if limit <= 0 {
		return nil, nil
	}
	if repoIDs != nil && len(repoIDs) == 0 {
		return nil, nil
	}
	var rows []SuggestRow
	err := r.searchSnapshot(ctx, f, func(tx *sqlx.Tx, f SearchFilter) error {
		where, args := buildSearchWhereIndexed(f, repoIDs)
		return tx.SelectContext(ctx, &rows,
			`SELECT repository_id, path FROM asset WHERE `+where+` ORDER BY path ASC, id ASC LIMIT ?`,
			append(args, limit)...,
		)
	})
	return rows, err
}
