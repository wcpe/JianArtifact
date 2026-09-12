-- 0036：分片上传会话（Web 上传）记录（FR-137 第三通道）
-- 引入版本：0.8.0（开发中）
--
-- 与 URL 拉取导入（0035）并列的第三条导入通道：前端把 GB 级备份包按服务端约定的
-- 8 MiB 分片顺序上传，服务端落盘后组装成归档，再交 StartLocalImport 走导入状态机。
-- 会话有过期时间（TTL），到期未完成的磁盘目录与记录由 ReconcileExpired 清理。
-- 状态机：initialized → receiving → completed；中途取消置 aborted。
CREATE TABLE backup_upload (
  upload_id   TEXT PRIMARY KEY,
  file_name   TEXT NOT NULL,
  total_bytes INTEGER NOT NULL DEFAULT 0 CHECK (total_bytes >= 0),
  chunk_size  INTEGER NOT NULL DEFAULT 0 CHECK (chunk_size > 0),
  status      TEXT NOT NULL CHECK (status IN ('initialized', 'receiving', 'completed', 'aborted')),
  sha256      TEXT NOT NULL DEFAULT '',
  operator    TEXT NOT NULL,
  created_at  TEXT NOT NULL,
  updated_at  TEXT NOT NULL,
  expires_at  TEXT NOT NULL
);

-- 启动期按过期时间批量清理。
CREATE INDEX idx_backup_upload_expires ON backup_upload (expires_at);

-- 每个分片一行；同 (upload_id, chunk_index) 允许覆盖（续传/重传幂等）。
-- 外键级联删除：删除上传会话时其分片元数据一并清除。
CREATE TABLE backup_upload_chunk (
  upload_id   TEXT NOT NULL,
  chunk_index INTEGER NOT NULL,
  size        INTEGER NOT NULL DEFAULT 0 CHECK (size >= 0),
  sha256      TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (upload_id, chunk_index),
  FOREIGN KEY (upload_id) REFERENCES backup_upload(upload_id) ON DELETE CASCADE
);
