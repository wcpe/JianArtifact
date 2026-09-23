-- 0039：主机监控补磁盘总量/已用、内存已用、进程运行时长
-- 引入版本：0.10.0（开发中）
-- 影响：host_metric_minute 新增 memory_used_bytes、disk_total_bytes、disk_used_bytes、process_uptime_seconds 列
-- 数据处理：四列均可空，历史行保持 NULL（由监控组状态解释，不伪造零），不回填；预计耗时：毫秒级
-- 回滚：不支持 down migration；需要恢复升级前备份
-- 相关：主机监控补容量与运行时长指标
--
-- 口径：已用量由采样点按 total − available 计算后落库，前端只展示不复算；
-- 磁盘总量与可用量取自数据目录所在卷的同一次采集调用；
-- 进程运行时长仅统计当前进程（不做系统进程枚举）。
ALTER TABLE host_metric_minute ADD COLUMN memory_used_bytes INTEGER DEFAULT NULL;
ALTER TABLE host_metric_minute ADD COLUMN disk_total_bytes INTEGER DEFAULT NULL;
ALTER TABLE host_metric_minute ADD COLUMN disk_used_bytes INTEGER DEFAULT NULL;
ALTER TABLE host_metric_minute ADD COLUMN process_uptime_seconds INTEGER DEFAULT NULL;
