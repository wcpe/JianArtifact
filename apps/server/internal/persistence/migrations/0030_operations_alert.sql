-- 0030：运维告警去重持久化
-- 引入版本：0.8.0
-- 影响：新增 operations_alert 表及告警唯一约束
-- 数据处理：创建告警状态结构，无历史数据回填；预计耗时：毫秒级
-- 回滚：不支持 down migration；需要恢复升级前备份
-- 相关：FR-53 / ADR-0002
-- 同一 (code, source) 只存一行：首次观察到的时间保留，最近观察时间滚动更新；
-- 恢复（不再满足条件）时由读路径删除对应行。不参与复制，与 0028 同域。
CREATE TABLE operations_alert (
  code              TEXT NOT NULL,
  source            TEXT NOT NULL,
  severity          TEXT NOT NULL,
  first_observed_at TEXT NOT NULL,
  last_observed_at  TEXT NOT NULL,
  blocked_until     TEXT,
  PRIMARY KEY (code, source)
);
