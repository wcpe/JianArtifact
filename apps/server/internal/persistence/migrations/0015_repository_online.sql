-- 0015：仓库 online/offline 状态
-- 引入版本：0.7.1
-- 影响：新增 repository.online 字段
-- 数据处理：已有仓库默认设置为在线，不回填业务状态；预计耗时：毫秒级
-- 回滚：不支持 down migration；需要恢复升级前备份
-- 相关：FR-113 / ADR-0023
-- 0015：仓库 online/offline 状态（默认在线）。FR-113。
-- online 是节点本地运维状态：SetOnline 不写复制变更日志，且 RepoChangeData 不含
-- online 字段（M-2 三硬约束），保证对端复制应用不覆盖本地运维状态。
ALTER TABLE repository ADD COLUMN online INTEGER NOT NULL DEFAULT 1;
