package repository

import (
	"database/sql"
	"errors"

	"github.com/jmoiron/sqlx"
)

// 本文件维护 asset.path 的 FTS5 派生索引（asset_path_fts，见迁移 0049 与 ADR-0030）。
//
// 为什么要单独一个入口：asset_path_fts 是 **external content** 表（content='asset'、
// content_rowid='id'），rowid 严格等于 asset.id。这样一张表的维护规则与普通表不同，且有几条
// 只能靠实测发现的坑（下述每条都在本仓库的驱动 modernc.org/sqlite 上实测确认过）：
//
//  1. **改 path 的 UPDATE 不会重建索引**：直接 `UPDATE asset SET path=?` 之后，新文本查不到、
//     旧文本仍能命中。因此 path 变化绝不能靠 UPDATE 维护索引，必须「删旧行 + 插新行」——
//     这正是 ADR-0030 要求把维护封在本文件里的原因。（本仓库的写入路径里 path 从不原地变更：
//     冲突键就是 path，重命名走的是「插新行、删旧行」两条，rowid 随之改变。）
//
//  2. **同一个 rowid 只能「在」或「不在」索引里，删除必须先确保它在**：这是最容易踩的坑。
//     在**内容行存在、但索引里没有该 rowid** 时（迁移刚完成、历史行尚未回填，这恰恰是常态），
//     FTS5 处理移除会报 267 `database disk image is malformed`——无论用专用命令
//     `INSERT INTO asset_path_fts(asset_path_fts, rowid, path) VALUES('delete', ?, ?)`
//     还是 `DELETE FROM asset_path_fts WHERE rowid = ?`（两者等价，都会踩）；
//     而报错之后同一连接上针对该表的后续操作也会被污染。反过来，**内容行与索引行都不在**时
//     移除是安全空操作。因此 searchIndexDelete 先做一次同 rowid 同 path 的插入（对已入索引
//     的行是幂等无害的：实测同一 rowid 重复插入不会产生重复命中），再执行移除，
//     使「删除一个从未入索引的历史行」不会失败。
//
//  3. **移除必须在 asset 行仍在时执行**：external content 表要靠内容行才知道该 rowid 有哪些
//     词要移除；内容行先消失虽然不报错，但也清不掉词，索引里会留下不再对应该 path 的残留条目。
//     所以删除资产的顺序固定为「先清索引、再删 asset 行」，换路径固定为「先插新行（建索引）、
//     再删旧行（清索引）」。
//
//  4. **索引行与 asset 行一一对应，孤儿行不产生错结果，但会打断「读列值」的查询**：
//     生产查询形态是 `id IN (SELECT rowid FROM asset_path_fts WHERE asset_path_fts MATCH ?)`，
//     只读 rowid、回 asset 表取真值，孤儿行在回表时自然消失（实测 MATCH 命中不受影响）。
//     但**读 FTS 表列值**的查询（例如 `SELECT ... FROM asset_path_fts WHERE path LIKE ...`）
//     需要回内容表取列值，一旦存在孤儿行就报 267 `fts5: missing row N from content table`。
//     结论：别对 asset_path_fts 的列直接做过滤，一律按 rowid 回表。
//
// **仓库删除留下的孤儿索引行（本批不处理，已知遗漏）**：asset 行也会经 repository 的
// ON DELETE CASCADE 整仓消失，这条路径不在本批的写入点里，因此会留下孤儿索引行。按上面第 4 条，
// 它们不会伪造命中、也不会让结果重复，代价是索引只增不减（体积偏大、索引行数不再等于 asset
// 行数），并且会让「读 FTS 列值」的探查式查询报 267。本批**刻意不新增触发器**（触发器会把
// 「先删后插」这类易错逻辑固化进只增不改的迁移），清理手段统一走后续批次的全量重建
// （清空索引 → 水位归零 → 重灌）或整仓清理。
//
// **对可观测性的影响：这些孤儿行可以忽略，它们只占空间、不影响任何查询结果。**
// 具体口径（避免后来者把它当成故障去追）：
//   - MeasureDrift 的 OrphanRows 在任何「删过仓库」的实例上都会**永久非零**——这是预期状态，
//     不是告警条件。真正需要关注的是 MissingRows（漏索引，会让制品搜不到），它必须为 0。
//   - 因此不要把 OrphanRows 接进告警，也不要拿「IndexRows == AssetRows」当健康判据：
//     那个等式在有孤儿行的实例上永远不成立。要真正收敛，只能等一次全量重建
//     （重建会顺带丢弃孤儿条目，见 searchIndexClearAll 第 3 条对 'rebuild' 的实测说明）。
//   - 空间代价的量级：一条孤儿索引行只对应一个已不存在的 path 的词元，随「删过的仓库里的
//     资产总数」线性增长；与整个索引相比通常可忽略，重建一次即清零。
//
// 一句话：孤儿行 = 只影响空间，可忽略；漏索引 = 影响正确性，必须重建。
//
// sqlx.Ext 同时兼容 *sqlx.DB 与 *sqlx.Tx：在线写入用调用方事务（与 asset 写入同一事务，
// 保证「元数据已提交、索引未写」这种窗口不存在），后台回填用自身事务。

// searchIndexInsert 把某条资产的当前 path 写入全文索引。
//
// 对已入索引的同一 rowid 重复插入是幂等无害的：实测同一 rowid 重复写入不会产生第二条命中，
// 这正是后台回填与在线写入交叠时需要的语义（同一行可能被写两次）。
func searchIndexInsert(ext sqlx.Ext, rowid int64, path string) error {
	_, err := ext.Exec(`INSERT INTO asset_path_fts(rowid, path) VALUES (?, ?)`, rowid, path)
	return err
}

// searchIndexDelete 从全文索引移除某 rowid 的条目；对从未入索引的行也安全。
//
// path 必须是该资产行**当前**的 path（且调用时 asset 行仍在库中）：external content 表要靠
// 内容行确定要移除哪些词。实现上先插一次再移除——未入索引的行因此变成「刚入索引又移除」，
// 避免 FTS5 在「内容行在、索引行不在」时报 267（见文件头第 2 条）。
func searchIndexDelete(ext sqlx.Ext, rowid int64, path string) error {
	if err := searchIndexInsert(ext, rowid, path); err != nil {
		return err
	}
	_, err := ext.Exec(`DELETE FROM asset_path_fts WHERE rowid = ?`, rowid)
	return err
}

// searchIndexClearAll 清空整张全文索引（不触碰 asset 真源），供管理员触发的全量重建使用。
//
// 为什么只能用专用命令 'delete-all'：三条候选语句的语义**不一样**，且都在本仓库的驱动
// modernc.org/sqlite v1.60.1 上实测过（结论如下，改动前请重跑对照）：
//
//  1. `INSERT INTO asset_path_fts(asset_path_fts) VALUES('delete-all')` —— **正确形态**。
//     它直接丢弃 FTS5 的全部索引条目，全程不读内容表，因此对三种病态输入都安全：
//     索引本就为空、内容表为空、索引里存在孤儿行（asset 行已被仓库级联删除）。
//     实测 docsize 2 → 0，且清空后重新写入与 MATCH 检索均正常。
//
//  2. `DELETE FROM asset_path_fts` —— **不能用**。没有孤儿行时它看起来等价于清空（实测
//     docsize 2 → 0），但一旦存在孤儿索引行（而孤儿恰恰是「重建」要修的典型状态），语句会在
//     孤儿行处**提前停住**：Exec 返回 nil 错误，却只删掉了孤儿之前的部分条目，孤儿自身与
//     其后条目全部保留（实测：3 条索引行、其中 rowid 2 为孤儿，执行后 docsize 仍为 1、
//     `MATCH "beta"` 仍命中 rowid 2）。「报成功但只清一半」是最坏的失败形态——重建会静默地
//     在旧索引上叠加，且后续 `DELETE ... WHERE rowid = ?` 还会因该表状态被弄乱而报 267
//     database disk image is malformed（实测），同连接上针对该表的后续操作都被牵连。
//
//  3. `INSERT INTO asset_path_fts(asset_path_fts) VALUES('rebuild')` —— **语义相反**：它按内容表
//     **重建**索引，是「灌满」而不是「清空」（实测：索引清空后 rebuild 会把内容表全部行重新写入，
//     顺带丢弃孤儿条目）。它确实能一步得到一致索引，但代价是在**单个事务**里重扫全表：SQLite
//     只有一个写者（WAL 的「多读 + 单写」），百万级 asset 上会长时间占住写锁、把全部在线写入
//     堵在忙等待里，正是迁移 0049 与规格 §3.2 明确拒绝的形态。因此重建走「delete-all 清空
//     （瞬时）+ 水位归零 + 分批回填」，靠回填的分批让出保证在线可用。
func searchIndexClearAll(ext sqlx.Ext) error {
	_, err := ext.Exec(`INSERT INTO asset_path_fts(asset_path_fts) VALUES('delete-all')`)
	return err
}

// assetRowID 按 (repository_id, path) 查资产行 id，供写入路径判断「新增还是覆盖写」，
// 以及删除 / 路径变更前取旧行 id（索引维护需要 rowid）。行不存在时返回 (0, false, nil)。
func assetRowID(ext sqlx.Ext, repoID int64, path string) (int64, bool, error) {
	var id int64
	err := ext.QueryRowx(`SELECT id FROM asset WHERE repository_id = ? AND path = ?`, repoID, path).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return id, true, nil
}
