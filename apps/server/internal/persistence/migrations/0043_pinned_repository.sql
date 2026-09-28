-- 0043：置顶仓库入库（用户级 + 全局兜底）
-- 引入版本：0.10.0（开发中）
-- 影响：新增 pinned_repository 表（用户级置顶，user_id IS NULL 表示全局置顶）+ 两个索引
-- 数据处理：无历史回填——既有置顶只存在用户浏览器的 localStorage（服务端不可见），无法迁移；预计耗时：建表 + 建索引，常量级
-- 回滚：不支持 down migration；需要恢复升级前备份
-- 相关：仓库列表 / 仪表盘状态面板的置顶，公开页展示全局置顶
--
-- 背景：置顶此前是**纯前端 localStorage 偏好**（键 jianartifact.pinnedRepos），换设备、
-- 换浏览器或清缓存即丢失，用户反馈「置顶总会掉」；且公开页（匿名浏览）完全感知不到置顶。
-- 改为服务端持久化后：登录用户读自己的置顶（跨设备一致），未登录回退全局置顶，
-- 公开仓库列表也带出全局置顶名，使**公开页同样能展示置顶**。
--
-- 口径：
--   - user_id IS NULL 的行 = **全局置顶**（由管理员维护），作为匿名与无个人置顶用户的兜底；
--     非 NULL 的行 = 某个登录用户的私有置顶。二者互不覆盖，读取时按主体选择。
--   - repository_id 级联随仓库删除（ON DELETE CASCADE），user_id 级联随用户删除，
--     不留悬空置顶；仓库重命名不影响置顶（按 ID 关联，名字在读取时实时解析）。
--   - 写入为**覆盖式**：某作用域一次 Set 即整体替换（事务内先删后插），
--     因此不会出现重复行，也不需要单独的去重逻辑。
CREATE TABLE pinned_repository (
  user_id       INTEGER REFERENCES user (id) ON DELETE CASCADE,  -- NULL = 全局置顶
  repository_id INTEGER NOT NULL REFERENCES repository (id) ON DELETE CASCADE,
  created_at    TEXT NOT NULL DEFAULT (datetime('now')),
  PRIMARY KEY (user_id, repository_id)
);

CREATE INDEX idx_pinned_repo_user ON pinned_repository (user_id);

-- SQLite 的主键唯一性**不覆盖 NULL**（NULL 互不相等），因此 (NULL, repository_id) 的重复
-- 主键不会冲突——全局置顶（user_id IS NULL）需要一个带 WHERE 的 partial unique index 兜底，
-- 保证同一仓库在全局作用域下至多出现一次。
CREATE UNIQUE INDEX idx_pinned_repo_global ON pinned_repository (repository_id) WHERE user_id IS NULL;
