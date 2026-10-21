-- 0048：备份导入来源新增 local
-- 引入版本：未开窗（0.11.0 之后的下一窗口）
-- 影响：重建 backup_import 表以放宽 origin 的 CHECK 约束（新增取值 'local'）
-- 数据处理：既有行原样复制，import_id 与其余各列逐条不变；预计耗时：随 backup_import 行数增长
--          （重建需全表复制，该表是「每次导入一行」的量级，实际为毫秒级）
-- 回滚：不支持 down migration；需要恢复升级前备份
-- 相关：、docs/specs/0.12.0-backup-restore-entry.md
--
-- 背景：新增「用本机已有的备份包恢复」入口（POST /api/v1/backups/{id}/restore）后，
-- 导入记录的 origin 需要能表达"这份包来自本机"（'local'），以便在事件表里与另外三条
-- 从外部取包的通道（'url' / 'upload' / 'cli'）区分——它们最终走同一条编排，
-- 但用户来这一页要办的是不同的事。
--
-- 为什么必须重建表而不能 ALTER：0035 建表时把取值写死在表级 CHECK 里
-- （origin IN ('url','upload','cli')），而 SQLite 的 ALTER TABLE **不能放宽已有 CHECK**——
-- 这是 0046 重建 acl 的同一条理由（那里是要放宽 action 的 CHECK）。ADD COLUMN 也无济于事：
-- 本表已有该列，只是想改它的约束。（0047 给 backup_package 加 origin 之所以能用 ADD COLUMN，
-- 是因为那是**新增列**、CHECK 随之一起写进 DDL，情形不同。）
--
-- 实测教训（这条迁移的由来）：先只改了契约与 devmock，没动数据库，于是测试站上
-- POST .../restore 直接 500（内部错误）——CHECK constraint failed 被兜成 internal_error，
-- 日志里也看不出原因。契约、devmock、前端三处一致并不能保证真实数据库接受该取值；
-- 凡是"给某个受约束的列加新取值"，必须同时改表。
--
-- 范式：按本目录 0020 / 0046 的既有做法——建新表 → 复制数据 → 删旧表 → 改名 → 重建索引，
-- 并用 PRAGMA foreign_keys=OFF 包住重建段（DROP TABLE 的瞬间会让指向本表的外键悬空）。
-- backup_import 当前没有被任何表外键引用，重建后名字与列不变，引用方无感知。
--
-- 幂等性说明：迁移器把每个文件放在单个事务里执行（见 persistence/migrate.go），
-- schema 变更与 schema_migrations 记录同事务提交，不存在「表已重建但版本未记」的半成品状态，
-- 因此这里按 0043–0047 的既有写法用裸 CREATE TABLE / CREATE INDEX，不加 IF NOT EXISTS。

-- backup_import 重建：SQLite 不允许 ALTER TABLE 放宽 CHECK，故整表重建。
PRAGMA foreign_keys=OFF;

CREATE TABLE backup_import_new (
  import_id          TEXT PRIMARY KEY,
  -- 恢复来源通道。local = 用本机已登记的备份包恢复（POST /backups/{id}/restore）；
  -- 其余三条都要求包来自外部：url = 服务端按地址拉取，upload = 分片上传，
  -- cli = 运维在机器上直接指定归档路径。
  origin             TEXT NOT NULL CHECK (origin IN ('url', 'upload', 'cli', 'local')),
  status             TEXT NOT NULL CHECK (status IN ('queued', 'fetching', 'staging', 'pending_restart', 'done', 'failed')),
  source_url         TEXT NOT NULL DEFAULT '',
  operator           TEXT NOT NULL,
  overwrite          INTEGER NOT NULL DEFAULT 0 CHECK (overwrite IN (0, 1)),
  deep               INTEGER NOT NULL DEFAULT 0 CHECK (deep IN (0, 1)),
  total_bytes        INTEGER NOT NULL DEFAULT 0 CHECK (total_bytes >= 0),
  fetched_bytes      INTEGER NOT NULL DEFAULT 0 CHECK (fetched_bytes >= 0),
  blob_count         INTEGER NOT NULL DEFAULT 0 CHECK (blob_count >= 0),
  package_id         TEXT NOT NULL DEFAULT '',
  error_code         TEXT NOT NULL DEFAULT '',
  error_summary      TEXT NOT NULL DEFAULT '',
  created_at         TEXT NOT NULL,
  updated_at         TEXT NOT NULL,
  finished_at        TEXT,
  restore_pending_at TEXT
);

-- 既有行原样复制。列清单逐列写出（不用 SELECT *），这样将来加列时不会静默错位。
INSERT INTO backup_import_new (
  import_id, origin, status, source_url, operator, overwrite, deep,
  total_bytes, fetched_bytes, blob_count, package_id,
  error_code, error_summary, created_at, updated_at, finished_at, restore_pending_at
)
SELECT
  import_id, origin, status, source_url, operator, overwrite, deep,
  total_bytes, fetched_bytes, blob_count, package_id,
  error_code, error_summary, created_at, updated_at, finished_at, restore_pending_at
FROM backup_import;

DROP TABLE backup_import;
ALTER TABLE backup_import_new RENAME TO backup_import;

PRAGMA foreign_keys=ON;

-- 索引按 0035 原名与语义重建。
-- 列表按最近优先。
CREATE INDEX idx_backup_import_created ON backup_import (created_at DESC);
-- 运维按状态筛选（进行中 / 失败）。
CREATE INDEX idx_backup_import_status ON backup_import (status);
