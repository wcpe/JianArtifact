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
//
//  4. **索引正确性不能只靠约定**：completed_at 一旦非空就不再回填，此后任何绕过仓储的写入
//     （运维脚本、手工 SQL、未来的迁移）都会留下**永久静默**的漂移——状态端点照旧报就绪，
//     用户只看到「某些制品搜不到」。因此本文件另外提供 MeasureDrift 这个集合并集比对原语，
//     由状态端点、回填完成与启动自检三处调用，让漂移至少可被观察到。它把两个方向分开报
//     （漏索引 / 孤儿索引行），并有一个已知盲区（path 就地 UPDATE），详见该函数的注释。

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
//
// 与索引真实条目数的比对不是这里的事，见 MeasureDrift。
func (r *SearchIndexStateRepo) CountIndexed(lastID int64) (int64, error) {
	var n int64
	err := r.db.Get(&n, `SELECT COUNT(*) FROM asset WHERE id <= ?`, lastID)
	return n, err
}

// SearchIndexDrift 是「索引行数 vs 资产行数」的对比快照。
//
// 两个差异方向**分开报**，不再合成一个差额：漏 k 行、孤儿 k 行时合成差额恰好为 0，而两者的
// 危险程度与处置方式完全不同——漏索引会让制品搜不到（必须重建），孤儿行只浪费索引空间
// （可择机重建）。任何按单个差额做的告警都会在「删过仓库的实例」上永久为真，把真信号埋掉。
type SearchIndexDrift struct {
	// IndexRows 是索引里真实存在的文档数（asset_path_fts_docsize 影子表的行数）。
	IndexRows int64 `db:"index_rows"`
	// AssetRows 是 asset 真源当前的行数。
	AssetRows int64 `db:"asset_rows"`
	// MissingRows 是**漏索引**的行数：asset 里有、索引里没有的 rowid 个数。这是危险方向。
	MissingRows int64 `db:"missing_rows"`
	// OrphanRows 是**孤儿索引行**的行数：索引里有、asset 里已不存在的 rowid 个数。
	// 仓库删除经 ON DELETE CASCADE 带走 asset 行时天然产生，只浪费空间、不丢失结果。
	OrphanRows int64 `db:"orphan_rows"`
}

// HasMissing 报告是否存在漏索引（资产行在、索引条目缺失）。
func (d SearchIndexDrift) HasMissing() bool { return d.MissingRows > 0 }

// MeasureDrift 取出当前的漂移快照，供状态端点与启动/回填完成的自检使用。
//
// **已知盲区：本函数只比对行数与 rowid 集合，"drift 为 0" 不等于"索引内容正确"。**
// 若某条 asset 行的 path 被就地 UPDATE（`UPDATE asset SET path = ?`），external content 表
// 不会重建该行的索引（见 search_index_repo.go 文件头第 1 条）：索引里仍是旧文本，行数与
// rowid 集合却完全一致，因此这里报 0 漂移。结果是「按新名字搜不到、按旧名字能搜到」这种
// 最危险的静默错结果，而本指标对它完全无感。现任写入路径不会产生这种 UPDATE（冲突键就是
// path，改名走「插新行、删旧行」，rowid 随之改变），但运维脚本或手工 SQL 可以——那种情况下
// 只能靠全量重建（POST /api/v1/search/index/rebuild）恢复，不能指望本指标报警。要真正覆盖
// 这一盲区，得逐行比对索引文本与 asset.path，成本等同一次全表重建，不适合放在观测点。
//
// **为什么数 asset_path_fts_docsize，而不是其它两张表**（在本仓库的驱动
// modernc.org/sqlite v1.60.1 上逐条实测，改之前请重跑对照）：
//
//  1. `SELECT COUNT(*) FROM asset_path_fts` —— **不能用**。external content 表上它数的是
//     **内容表**（asset）的行数：实测 3 条 asset、索引为空时仍返回 3，索引被 delete-all 清空后
//     依旧报全量，完全量不出索引条目。
//  2. `SELECT COUNT(*) FROM asset_path_fts_data` —— **也不能用**。这张影子表存的是索引 b-tree
//     的**段数据 blob**，与文档数无关：实测空索引已有 2 行（结构段），写入 2 条文档后变 3 行，
//     对同一 rowid 重复插入后涨到 4 行，delete-all 之后仍为 2 行。把它当行数会得到一个恒定非零
//     且随写入次数漂移的数字。
//  3. `SELECT COUNT(*) FROM asset_path_fts_docsize` —— **正确**。FTS5 为每个已索引文档在这张
//     影子表里留一行 (id, sz)：实测空索引 0 行、写入 2 条文档 2 行、对同一 rowid 重复插入仍是
//     2 行（与幂等写入一致）、delete-all 之后回到 0 行、内容行被删而索引行残留时仍保留该行
//     （正是我们要看见的孤儿）。它的 id 就是 asset.id，因此可以按 rowid 直接与 asset 做差集。
//
// 四个计数放在**同一条语句**里取，保证四份数字属于同一时刻（分别发查询会跨事务，在高写入
// 负载下能配出并不存在的漂移）。成本：两次窄表全扫 + 两次主键探针；比纯 COUNT(*) 略高，仍与
// 表大小线性相关，因此只放在状态端点与启动/回填完成这两个低频观测点上，不进搜索热路径。
func (r *SearchIndexStateRepo) MeasureDrift() (SearchIndexDrift, error) {
	var d SearchIndexDrift
	err := r.db.Get(&d, `SELECT
		(SELECT COUNT(*) FROM asset_path_fts_docsize) AS index_rows,
		(SELECT COUNT(*) FROM asset) AS asset_rows,
		(SELECT COUNT(*) FROM asset_path_fts_docsize AS d
			WHERE NOT EXISTS (SELECT 1 FROM asset AS a WHERE a.id = d.id)) AS orphan_rows,
		(SELECT COUNT(*) FROM asset AS a
			WHERE NOT EXISTS (SELECT 1 FROM asset_path_fts_docsize AS d WHERE d.id = a.id)) AS missing_rows`)
	if err != nil {
		return SearchIndexDrift{}, err
	}
	return d, nil
}
