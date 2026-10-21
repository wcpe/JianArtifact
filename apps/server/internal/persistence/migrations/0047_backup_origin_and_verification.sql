-- 0047：备份包来源标记与恢复演练报告
-- 引入版本：未开窗（0.11.0 之后的下一窗口）
-- 影响：backup_package 新增 origin 列；新增 backup_verification 表及其 package_id 索引
-- 数据处理：无历史回填——origin 取列默认值 'manual'，与既有行的真实来源一致（0047 之前只有人工触发
--          的生成入口）；backup_verification 为空表。预计耗时：毫秒级（ADD COLUMN 带常量默认值时
--          SQLite 只改 schema、不重写表体，见下方说明；建表与建索引无既有数据可扫）
-- 回滚：不支持 down migration；需要恢复升级前备份
-- 相关：、docs/specs/0.12.0-auto-backup-and-drill.md
--
-- 背景： 要求周期化自动备份与恢复演练两件事。前者需要「这份包是人点的还是作业生成的」
-- 可分辨（手动包永不自动淘汰，见 domain/backup_retention.go），后者需要把演练报告落库留痕，
-- 而不是只回一个 ok / error 字符串。
--
-- origin 列为什么用 ALTER TABLE ADD COLUMN 而不是重建表（本目录 0020 / 0046 的范式）：
-- SQLite 对 ADD COLUMN 的限制只有三条——不能是 PRIMARY KEY / UNIQUE、不能有非常量默认值、
-- 且带 NOT NULL 时必须给常量默认值；**CHECK 约束不在禁止之列**，它会照常写进表 DDL 并在后续
-- INSERT / UPDATE 时生效。本目录已有同形先例：0033_replication_receipt_lineage.sql 的第一条
-- 语句就是 `ALTER TABLE asset_mutation ADD COLUMN origin TEXT NOT NULL DEFAULT 'local'
-- CHECK (origin IN ('local','received'))`，语义与本次完全同构。
-- 本机实测（modernc.org/sqlite v1.60.1，即 go.mod 锁定的驱动）确认两点：
--   ① ADD COLUMN 带 CHECK 不报错，sqlite_master.sql 里能看到该约束；
--   ② 写入 'bogus' 被拒（CHECK constraint failed: origin IN ('manual','auto')），省略该列取 'manual'。
-- 因此不重建 backup_package：0046 重建 acl 的原因是它要**放宽**既有 CHECK 并新增带 REFERENCES
-- 的列，而这两条恰恰是 ADD COLUMN 做不到的；本次只是追加一个带常量默认值的新列，
-- 重建表反而要额外承担「全表复制 + 外键悬空窗口 + 索引重建」三项风险（backup_verification
-- 随后就要指向本表，重建会让这条新外键在 DROP / RENAME 瞬间悬空）。
--
-- 为什么不给 backup_package 另建 (origin, status, created_at) 索引：保留策略唯一的候选集查询是
-- 「origin='auto' AND status='done' 按 created_at 倒序」（见 repository/backup_repo.go 的
-- ListRetentionCandidates）。既有 idx_backup_package_created 本身就是 (created_at DESC)，
-- 该查询的 ORDER BY 完全由索引序满足、无需临时排序，剩余的两个等值过滤在索引行走中顺带完成；
-- 备份包是「每天几份」量级的表，多加一条索引只会给每次生成添写放大，换不到可观测的收益。
--
-- 幂等性说明：迁移器把每个文件放在单个事务里执行（见 persistence/migrate.go），
-- schema 变更与 schema_migrations 记录同事务提交，不存在「列已加但版本未记」的半成品状态，
-- 因此这里按 0043 / 0044 / 0045 / 0046 的既有写法用裸 ALTER TABLE / CREATE TABLE / CREATE INDEX，
-- 不加 IF NOT EXISTS。

-- 来源：manual = 人工触发（CLI / Web / API），auto = backup-auto 作业生成。
-- 既有行全部是人工触发，默认值 'manual' 即为正确语义，无需回填 UPDATE。
ALTER TABLE backup_package ADD COLUMN origin TEXT NOT NULL DEFAULT 'manual'
  CHECK (origin IN ('manual', 'auto'));

-- 恢复演练报告：一次演练一行。逐项结论（包完整性、解包、SQLite 一致性、行数比对、blob 摘要）
-- 以 JSON 对象存 checks_json，而不是给每项单开一列——项数会随演练强度演进，
-- 加项不该每次都动 schema。
CREATE TABLE backup_verification (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    package_id    TEXT NOT NULL REFERENCES backup_package (package_id) ON DELETE CASCADE,
    level         TEXT NOT NULL CHECK (level IN ('shallow', 'deep')),
    ok            INTEGER NOT NULL CHECK (ok IN (0, 1)),
    started_at    TEXT NOT NULL,
    finished_at   TEXT NOT NULL,
    duration_ms   INTEGER NOT NULL DEFAULT 0 CHECK (duration_ms >= 0),
    checks_json   TEXT NOT NULL DEFAULT '{}',
    error_summary TEXT NOT NULL DEFAULT ''
);

-- 报告随包级联删除（包没了，针对它的报告没有意义），因此外键上的 CASCADE 需要一个
-- 以 package_id 为最左列的索引，否则每次删包都要全表扫 backup_verification。
-- 同一条索引也服务「按包查历史报告，最近优先」这条唯一的读路径。
CREATE INDEX idx_backup_verification_package ON backup_verification (package_id, started_at DESC);
