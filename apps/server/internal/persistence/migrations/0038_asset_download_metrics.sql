-- 0038：制品下载计量明细（按分钟预聚合）
-- 引入版本：0.10.0
-- 影响：新增 asset_download_minutes 表（分钟桶 × 仓库 × 制品 × 来源 IP × UA 归类）
-- 数据处理：无历史回填；预计耗时：建表 + 建索引，常量级
-- 回滚：不支持 down migration；需要恢复升级前备份
-- 相关：FR-142 / docs/specs/download-metrics.md
--
-- 背景：仪表盘「下载量」KPI 走 protocol_metric_minute（请求级、刻意不含路径与来源）；
-- per-制品计数与 IP/UA 聚合需要明细，但不得破坏既有链路的低开销与隐私约束——
-- 故独立建表并按分钟预聚合（同分钟同组合合并计数），采集端内存缓冲、周期 flush，
-- 下载路径不逐请求写 SQLite。
-- 安全边界：client_ip 明文仅供管理员查询；ua_family 为归类结果，原始 UA 串不得落库。
CREATE TABLE asset_download_minutes (
  bucket_start TEXT NOT NULL,
  repo TEXT NOT NULL,
  asset_path TEXT NOT NULL,
  client_ip TEXT NOT NULL,
  ua_family TEXT NOT NULL,
  download_count INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (bucket_start, repo, asset_path, client_ip, ua_family)
);

CREATE INDEX idx_asset_download_asset ON asset_download_minutes (repo, asset_path, bucket_start);
CREATE INDEX idx_asset_download_bucket ON asset_download_minutes (bucket_start);
