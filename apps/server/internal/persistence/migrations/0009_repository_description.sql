-- 0009（FR-81）：仓库描述字段。管理后台可配置，详情页页头展示。
-- 默认空串（非 NULL），与行模型 string 字段对齐。
ALTER TABLE repository ADD COLUMN description TEXT NOT NULL DEFAULT '';
