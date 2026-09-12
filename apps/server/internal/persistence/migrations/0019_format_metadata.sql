-- 0019：格式协议元数据
-- 引入版本：0.8.0
-- 影响：新增 format_metadata 表及格式查询索引
-- 数据处理：创建格式元数据结构，无历史数据回填；预计耗时：毫秒级
-- 回滚：不支持 down migration；需要恢复升级前备份
-- 相关：FR-27 / FR-29 / ADR-0002
-- 0019：格式协议元数据（PyPI Simple / NuGet V3）。
-- 文件字节仍由 asset/blob 保存；本表只保存生成索引所需的稳定元数据。
CREATE TABLE format_metadata (
  id                INTEGER PRIMARY KEY AUTOINCREMENT,
  repository_id     INTEGER NOT NULL REFERENCES repository(id) ON DELETE CASCADE,
  format            TEXT NOT NULL,
  name_normalized   TEXT NOT NULL,
  name_display      TEXT NOT NULL DEFAULT '',
  version           TEXT NOT NULL DEFAULT '',
  version_normalized TEXT NOT NULL DEFAULT '',
  filename          TEXT NOT NULL,
  asset_path        TEXT NOT NULL,
  sha256            TEXT NOT NULL,
  size              INTEGER NOT NULL DEFAULT 0,
  requires_python   TEXT NOT NULL DEFAULT '',
  yanked            TEXT NOT NULL DEFAULT '',
  metadata_json     TEXT NOT NULL DEFAULT '{}',
  source_kind       TEXT NOT NULL DEFAULT 'hosted',
  source_member     TEXT NOT NULL DEFAULT '',
  created_at        TEXT NOT NULL DEFAULT (datetime('now')),
  updated_at        TEXT NOT NULL DEFAULT (datetime('now')),
  UNIQUE(repository_id, format, name_normalized, filename),
  UNIQUE(repository_id, format, name_normalized, version_normalized, filename)
);
CREATE INDEX idx_format_metadata_project ON format_metadata(repository_id, format, name_normalized);
CREATE INDEX idx_format_metadata_version ON format_metadata(repository_id, format, name_normalized, version_normalized);
