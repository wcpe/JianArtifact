-- 0011（复制同步历史）：每次同步记录一条日志（含进行中/成功/失败与统计）。
-- 供 web「集群」页可视化展示同步记录与进度（FR-88 延伸）。
-- success 用 NULL 表示进行中（开始写一条 running，结束后更新 success/finished_at/统计）。
CREATE TABLE repl_sync_log (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  peer_url      TEXT    NOT NULL,             -- 对端基址
  started_at    TEXT    NOT NULL,             -- 开始时间（RFC3339Nano）
  finished_at   TEXT,                         -- 结束时间；NULL=进行中
  success       INTEGER,                      -- NULL=进行中 / 1=成功 / 0=失败
  from_seq      INTEGER NOT NULL,             -- 起始水位
  to_seq        INTEGER NOT NULL,             -- 结束水位（失败时为已推进水位）
  changes       INTEGER NOT NULL,             -- 拉取变更条数
  applied       INTEGER NOT NULL,             -- 成功应用条数
  failed        INTEGER NOT NULL,             -- 应用失败条数
  blobs         INTEGER NOT NULL,             -- 补拉 blob 数
  entity_counts TEXT    NOT NULL DEFAULT '{}', -- 变更实体构成 JSON：{"asset":5,"repository":2,"user":1}
  error_text    TEXT    NOT NULL DEFAULT ''   -- 失败摘要（成功为空）
);
CREATE INDEX idx_repl_sync_log_started ON repl_sync_log(started_at DESC);
