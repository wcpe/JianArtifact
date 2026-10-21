-- 0049：资产路径的 FTS5 全文索引表与索引回填状态表
-- 引入版本：未开窗（0.11.0 之后的下一窗口）
-- 影响：新增虚拟表 asset_path_fts（FTS5 external content，索引 asset.path）与单行状态表 search_index_state
-- 数据处理：**不在迁移内回填**——只建结构并写入一行初始状态（last_id=0、total_rows=0、completed_at 为 NULL）；
--          历史行由启动后的后台分批回填（见 FR-149 后续批次），未回填期间搜索继续走既有 LIKE 路径；预计耗时：毫秒级
-- 回滚：不支持 down migration；需要恢复升级前备份
-- 相关：FR-149、docs/specs/search-index-and-performance.md §3.1、docs/adr/0030-fts5-search-index.md
--
-- 背景（为什么需要这张表）：全局搜索把每个关键词译成 `path LIKE '%词%'`（asset_repo.go 的
-- buildSearchWhere），前导通配让 asset 上的所有 B-Tree 索引都无从定位——0002 的
-- UNIQUE(repository_id, path)、0006 的 idx_asset_repo_path、0045 的 idx_asset_repo_updated
-- 只服务等值与前缀条件。于是每次搜索对百万级 asset 至少做两三次全表扫描，且随库增长线性恶化。
-- trigram 分词器是唯一同时满足「走索引」与「任意子串」的选择（版本号 `1.17.12` 这类关键词在
-- unicode61 下会被切碎并命中无关路径），取舍与备选方案的完整论证见 ADR-0030，此处只承接结论。
--
-- 形态口径（由 ADR-0030 决策）：
--   - external content（content='asset'、content_rowid='id'），rowid 严格等于 asset.id，
--     索引不复制路径文本。asset 仍是唯一真源，本表是纯派生索引：内容以 rowid 回 asset 取真值。
--   - tokenize='trigram' 固定，且**不开启 case_sensitive**：既有 LIKE 在 SQLite 默认设置下
--     只对 ASCII 折叠大小写，trigram 分词器默认同样仅折叠 ASCII，二者天然对齐；
--     一旦开启 case_sensitive，`Foo` 就查不到 `foo`，与旧实现分叉。
--   - 索引由**应用层在 asset 写入的同一事务内显式维护**（repository/search_index_repo.go），
--     不使用触发器：asset 的写入点可穷举，而 external content 表上改 path 的 UPDATE 不会重建
--     索引，正确维护必须「先删后插」成对执行，把这套易错操作封进一个可测试的入口比固化进
--     只增不改的迁移里的触发器更可控。
--
-- 为什么不在迁移里回填（关键）：迁移器把每个文件放在**单个事务**里执行（persistence/migrate.go），
-- 只前向、无 down。把百万级回填塞进迁移，升级会变成一次长时间持写锁的不可用事务——本库只有
-- 单个连接（db.go 的 SetMaxOpenConns(1)），持锁期间所有写入排队——而且中途失败无法断点恢复，
-- 只能整体重来。因此本迁移只建结构；回填交给启动后的后台分批任务（按 last_id 水位推进、
-- 任意时刻可中断续跑）。回填完成前搜索整体走既有 LIKE 路径：正确但尚未提速，这是刻意取舍。
--
-- 关于状态表：索引状态**不复用 schema_migrations**。「结构已建」与「数据已灌」生命周期不同，
-- 混用会让「升级完成」与「搜索已提速」不可区分。单行语义用 CHECK (id = 1) 钉住，
-- 避免出现第二行让水位语义分裂；后续若新增其它索引，再另起表或扩展主键，不在此处预留多行形态。
-- completed_at 为 NULL 即表示回填未完成（不另设 done 列，避免两个字段表达同一状态而漂移）。
--
-- 幂等性说明：迁移器把建表与 schema_migrations 记录同一事务提交，不存在「表已建但版本未记」
-- 的半成品状态，故按 0043 / 0044 的既有写法用裸 CREATE，不加 IF NOT EXISTS。
CREATE VIRTUAL TABLE asset_path_fts USING fts5(
    path,
    content='asset',
    content_rowid='id',
    tokenize='trigram'
);

-- search_index_state：asset_path 索引的回填水位（单行，id 恒为 1）。
--   last_id      —— 已回填到的最大 asset.id（水位）；0 表示尚未开始。
--   total_rows   —— 回填开始时取的 asset 行数快照，仅用于进度展示与一致性校验，不参与正确性判定。
--   completed_at —— 回填完成时刻（UTC "YYYY-MM-DD HH:MM:SS"）；NULL 表示未完成。
--   updated_at   —— 本行最后一次变更时刻，供排查「回填是否还在推进」。
CREATE TABLE search_index_state (
    id           INTEGER PRIMARY KEY CHECK (id = 1),
    last_id      INTEGER NOT NULL DEFAULT 0 CHECK (last_id >= 0),
    total_rows   INTEGER NOT NULL DEFAULT 0 CHECK (total_rows >= 0),
    completed_at TEXT,
    updated_at   TEXT NOT NULL DEFAULT (datetime('now'))
);

-- 初始行：水位归零、完成时刻留空，等后台回填推进。写入初始行而不是让读取方处理「无记录」，
-- 可以让状态读取与水位推进都只有一条正常路径（无 null 分支）。
INSERT INTO search_index_state (id, last_id, total_rows) VALUES (1, 0, 0);
