-- 0042：主机网络补累计总量与逐网卡分钟样本
-- 引入版本：0.10.0（开发中）
-- 影响：host_metric_minute 新增 network_receive_bytes_total / network_transmit_bytes_total 列；
--       新增 host_network_interface_minute 表（逐网卡每分钟一行）与 (interface, bucket_start) 索引
-- 数据处理：两列可空，历史行保持 NULL（不回填、不伪造零）；新表无历史数据；预计耗时：加列 + 建表，常量级
-- 回滚：不支持 down migration；需要恢复升级前备份
-- 相关：主机监控网络总发送/总接收 + 网卡维度选择
--
-- 背景：既有网络指标只有速率（network_receive_bytes_per_sec / network_transmit_bytes_per_sec），
-- 且在采集层就把全部非回环网卡聚合成一个数，用户看不到「总发送 / 总接收」，也无法按网卡查看。
--
-- 口径：
--   - 总量 = 自网卡启动以来的累计字节数（网卡硬件/驱动计数器），由采样点原样落库，不做增量换算；
--     因此同一网卡跨样本单调不减，若网卡重置或重启计数会回落（由前端按差值展示或忽略）。
--   - host_metric_minute 的两列 = 该采样时刻**全部非回环网卡的聚合累计**，供「全部网卡」视图；
--   - host_network_interface_minute 逐网卡存同一时刻的累计与**按该网卡**算得的速率；
--     选定网卡后速率图与总量都取该网卡，避免聚合量与单网卡混读。
--   - 速率列可空：网卡刚出现（无同名网卡上一份样本）或计数回退时留 NULL，不伪造零。
ALTER TABLE host_metric_minute ADD COLUMN network_receive_bytes_total INTEGER DEFAULT NULL;
ALTER TABLE host_metric_minute ADD COLUMN network_transmit_bytes_total INTEGER DEFAULT NULL;

CREATE TABLE host_network_interface_minute (
  bucket_start           TEXT    NOT NULL,
  interface              TEXT    NOT NULL,
  state                  TEXT    NOT NULL,
  error_code             TEXT    NOT NULL DEFAULT '',
  receive_bytes_total    INTEGER,
  transmit_bytes_total   INTEGER,
  receive_bytes_per_sec  REAL,
  transmit_bytes_per_sec REAL,
  PRIMARY KEY (bucket_start, interface)
);
CREATE INDEX idx_host_net_iface_name ON host_network_interface_minute (interface, bucket_start);
