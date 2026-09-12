-- 0028：当前节点业务聚合与主机分钟监控
-- 引入版本：0.8.0
-- 影响：新增协议指标、容量快照和主机监控表
-- 数据处理：创建观测数据结构，无历史数据回填；预计耗时：毫秒级
-- 回滚：不支持 down migration；需要恢复升级前备份
-- 相关：FR-53 / FR-120 / ADR-0002
CREATE TABLE protocol_metric_minute (
  bucket_start      TEXT PRIMARY KEY,
  request_count     INTEGER NOT NULL DEFAULT 0,
  download_count    INTEGER NOT NULL DEFAULT 0,
  failure_count     INTEGER NOT NULL DEFAULT 0,
  cache_hit_count   INTEGER NOT NULL DEFAULT 0,
  cache_miss_count  INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE capacity_snapshot_hour (
  bucket_start TEXT PRIMARY KEY,
  repository_count INTEGER NOT NULL,
  asset_count INTEGER NOT NULL,
  logical_bytes INTEGER NOT NULL
);

CREATE TABLE host_metric_minute (
  bucket_start TEXT PRIMARY KEY,
  host_state TEXT NOT NULL,
  host_error_code TEXT NOT NULL DEFAULT '',
  cpu_percent REAL,
  memory_total_bytes INTEGER,
  memory_available_bytes INTEGER,
  disk_available_bytes INTEGER,
  network_state TEXT NOT NULL,
  network_error_code TEXT NOT NULL DEFAULT '',
  network_receive_bytes_per_sec REAL,
  network_transmit_bytes_per_sec REAL,
  process_state TEXT NOT NULL,
  process_error_code TEXT NOT NULL DEFAULT '',
  process_rss_bytes INTEGER,
  process_cpu_percent REAL,
  goroutine_count INTEGER,
  open_file_descriptors INTEGER,
  readiness_state TEXT NOT NULL,
  readiness_error_code TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_host_metric_minute_bucket ON host_metric_minute (bucket_start DESC);
