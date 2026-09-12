-- 0009：仓库描述字段
-- 引入版本：0.6.0
-- 影响：新增 repository.description 字段
-- 数据处理：已有记录使用空串默认值，不回填业务数据；预计耗时：毫秒级
-- 回滚：不支持 down migration；需要恢复升级前备份
-- 相关：FR-81 / ADR-0002
-- 0009（FR-81）：仓库描述字段。管理后台可配置，详情页页头展示。
-- 默认空串（非 NULL），与行模型 string 字段对齐。
ALTER TABLE repository ADD COLUMN description TEXT NOT NULL DEFAULT '';
