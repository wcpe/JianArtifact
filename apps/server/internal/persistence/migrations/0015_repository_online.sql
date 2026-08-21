-- 0015：仓库 online/offline 状态（默认在线）。FR-113。
-- online 是节点本地运维状态：SetOnline 不写复制变更日志，且 RepoChangeData 不含
-- online 字段（M-2 三硬约束），保证对端复制应用不覆盖本地运维状态。
ALTER TABLE repository ADD COLUMN online INTEGER NOT NULL DEFAULT 1;
