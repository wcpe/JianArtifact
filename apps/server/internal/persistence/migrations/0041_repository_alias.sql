-- 0041：仓库别名（一个仓库可被多个名字访问）
-- 引入版本：0.10.0（开发中）
-- 影响：新增 repository_alias 表（别名 -> 仓库），并建 repository_id 索引
-- 数据处理：无历史回填（存量仓库默认无别名）；预计耗时：建表 + 建索引，常量级
-- 回滚：不支持 down migration；需要恢复升级前备份
-- 相关：仓库别名与重命名（别名与主名共享命名空间）
--
-- 背景：仓库此前只能按 repository.name 单名访问，重命名会切断既有客户端与历史链接。
-- 引入别名表后，仓库可拥有多个等价入口名；重命名时自动把旧名登记为别名，
-- 使旧名仍可解析到同一仓库（归档一致、避免断链）。
--
-- 语义边界：
--   - 别名与仓库主名**共享命名空间、全局唯一**（alias 为主键即兜底）；服务层负责
--     拒绝「别名撞任何主名 / 撞其他别名 / 等于自身主名」，DB 主键约束是最后一道防线。
--   - repository_alias.repository_id 级联随仓库删除（ON DELETE CASCADE），不留悬空别名。
--   - 别名只是**访问入口**：解析后返回的仍是主名仓库，主名才是仓库的身份。
--   - 历史数据（audit_log、asset_download_minutes 中的 repo 名）不回填：旧名已成别名，
--     按旧名仍可解析到仓库，保留当时的名字更真实。
CREATE TABLE repository_alias (
  alias         TEXT PRIMARY KEY,
  repository_id INTEGER NOT NULL REFERENCES repository (id) ON DELETE CASCADE,
  created_at    TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX idx_repository_alias_repo ON repository_alias (repository_id);
