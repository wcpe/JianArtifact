package repository

import (
	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
)

// 本文件提供 asset_path_fts 的**回填状态**读写（search_index_state 单行表，见迁移 0049）
// 与后台分批回填所需的批次原语。索引行本身的维护原语在 search_index_repo.go，
// 两者一起构成 「后台异步回填 + 可重建」的持久层。
//
// 三条必须守住的口径：
//
//  1. **水位推进与索引写入必须同事务**：WriteBatch 把「写本批索引」与「把 last_id 推进到本批
//     最后一条 id」封在同一个事务里。否则会出现「水位已到 N、索引只写到 N-k」的窗口，
//     而续跑逻辑只看水位，那 k 行会永久漏索引（且没有任何自愈路径，只能全量重建）。
//
//  2. **批次必须短**：SQLite 只有一个写者（WAL 的「多读 + 单写」），一个写事务提交前，
//     其余写事务全部在锁外排队。回填事务一旦长，在线写入就会被拖住。所以事务粒度是
//     「一批」（规格 §3.2 定为 2000 行），批与批之间由调用方让出。
//     实测一批 2000 行的 FTS 写事务约 20ms（本机），远小于 DSN 里 busy_timeout(5000)
//     的等待预算，因此批间让出对在线写入的干扰在毫秒级。
//
//  3. **清空索引只能用 'delete-all'**：DELETE FROM / rebuild 的实测差异见
//     search_index_repo.go 的 searchIndexClearAll 注释，重置（Reset）复用那一个入口。

// SearchIndexState 是 search_index_state 的读模型：回填水位与完成时刻。
//
// 表里没有 done 列——CompletedAt 为 NULL 即表示回填未完成（规格 §3.1 刻意不设两个字段
// 表达同一状态，避免它们漂移）。
type SearchIndexState struct {
	LastID      int64   `db:"last_id"`
	TotalRows   int64   `db:"total_rows"`
	CompletedAt *string `db:"completed_at"`
	UpdatedAt   string  `db:"updated_at"`
}

// Completed 报告回填是否已完成。
func (s SearchIndexState) Completed() bool { return s.CompletedAt != nil }

// SearchIndexRow 是回填批次里的一行：ID 即索引 rowid，Path 即索引内容。
// 只取这两列——回填只需要它们，绝不把整行元数据搬进内存（百万级时会放大成内存问题）。
type SearchIndexRow struct {
	ID   int64  `db:"id"`
	Path string `db:"path"`
}

// SearchIndexStateRepo 读写索引回填状态与回填批次。
type SearchIndexStateRepo struct{ db *persistence.DB }

// NewSearchIndexStateRepo 构造 SearchIndexStateRepo。
func NewSearchIndexStateRepo(db *persistence.DB) *SearchIndexStateRepo {
	return &SearchIndexStateRepo{db: db}
}

// Load 读回填状态。迁移 0049 已写入初始行，因此不存在「无记录」分支。
func (r *SearchIndexStateRepo) Load() (SearchIndexState, error) {
	var st SearchIndexState
	err := r.db.Get(&st,
		`SELECT last_id, total_rows, completed_at, updated_at FROM search_index_state WHERE id = 1`)
	if err != nil {
		return SearchIndexState{}, err
	}
	return st, nil
}

// SnapshotBounds 取本次回填的上界（asset 最大 id）与总行数快照。
//
// 上界**只取一次**：此后新写入的行由在线同步负责，回填不追——追会让回填永不收敛。
// MAX(id) 与 COUNT(*) 放在同一条语句里，保证两个数字属于同一时刻；又因为 upper 就是最大 id，
// 「id <= upper 的行数」等于全表行数，所以 COUNT(*) 就是规格要求的 total_rows 快照。
func (r *SearchIndexStateRepo) SnapshotBounds() (upper int64, total int64, err error) {
	err = r.db.QueryRow(`SELECT COALESCE(MAX(id), 0), COUNT(*) FROM asset`).Scan(&upper, &total)
	return upper, total, err
}

// SaveBounds 记录本次回填的总行数快照，供状态端点展示进度（不参与正确性判定）。
func (r *SearchIndexStateRepo) SaveBounds(total int64) error {
	_, err := r.db.Exec(
		`UPDATE search_index_state SET total_rows = ?, updated_at = datetime('now') WHERE id = 1`, total)
	return err
}

// FetchBatch 取 (after, upper] 区间内按 id 升序的一批待回填行。
// limit 由调用方给定（规格 §3.2 为 2000）；返回空切片表示区间内已无待回填行。
func (r *SearchIndexStateRepo) FetchBatch(after, upper int64, limit int) ([]SearchIndexRow, error) {
	var rows []SearchIndexRow
	err := r.db.Select(&rows,
		`SELECT id, path FROM asset WHERE id > ? AND id <= ? ORDER BY id LIMIT ?`, after, upper, limit)
	if err != nil {
		return nil, err
	}
	return rows, nil
}

// WriteBatch 在一个短事务里「写本批索引 + 把水位推进到本批最后一条 id」，提交后由调用方让出。
//
// 写入复用 searchIndexInsert：对同一 rowid 重复插入是幂等无害的（实测不产生第二条命中），
// 这正是回填与在线写入交叠时必须的语义——同一行可能被覆盖写后再被回填补一次。
func (r *SearchIndexStateRepo) WriteBatch(rows []SearchIndexRow, lastID int64) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := r.db.Beginx()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, row := range rows {
		if err := searchIndexInsert(tx, row.ID, row.Path); err != nil {
			return err
		}
	}
	// 水位与本批索引同事务提交：任何一步失败都整体回滚，不会出现「水位越过未写的行」。
	if _, err := tx.Exec(
		`UPDATE search_index_state SET last_id = ?, updated_at = datetime('now') WHERE id = 1`, lastID); err != nil {
		return err
	}
	return tx.Commit()
}

// MarkCompleted 把回填标记为完成（completed_at 由 NULL 变为当前时刻）。
func (r *SearchIndexStateRepo) MarkCompleted() error {
	_, err := r.db.Exec(
		`UPDATE search_index_state SET completed_at = datetime('now'), updated_at = datetime('now') WHERE id = 1`)
	return err
}

// Reset 清空全文索引并把回填水位归零（completed_at 也回到 NULL），供全量重建使用。
//
// 一个事务内完成「清空索引 + 重置水位」：两者若分开提交，中间窗口会让并发读者看到
// 「水位很高但索引为空」（搜索会静默漏结果），而单事务下外部要么看到重建前、要么看到重建后。
// 清空用 'delete-all' 而不是 DELETE FROM 的理由见 searchIndexClearAll 的注释。
func (r *SearchIndexStateRepo) Reset() error {
	tx, err := r.db.Beginx()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := searchIndexClearAll(tx); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE search_index_state
		SET last_id = 0, total_rows = 0, completed_at = NULL, updated_at = datetime('now')
		WHERE id = 1`); err != nil {
		return err
	}
	return tx.Commit()
}

// CountIndexed 数出水位以内（id <= lastID）的 asset 行数，作为状态端点 `indexed` 的口径。
//
// 为什么不数索引行数：asset_path_fts 是 external content 表，`SELECT COUNT(*) FROM asset_path_fts`
// 实测返回的是**内容表**的行数（索引被清空后仍报全量），根本不是索引条目数；而读 FTS 影子表
// 又会把「在线同步新写入的行」与「仓库级联删除留下的孤儿行」一起算进来，与 total（同一张
// asset 表上的快照）不可比。按水位回真源计数与 total 同源同口径，能直接看出「还剩多少没回填」。
func (r *SearchIndexStateRepo) CountIndexed(lastID int64) (int64, error) {
	var n int64
	err := r.db.Get(&n, `SELECT COUNT(*) FROM asset WHERE id <= ?`, lastID)
	return n, err
}
