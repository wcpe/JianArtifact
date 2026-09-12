-- 0007：全局设置表与内置匿名主体
-- 引入版本：0.6.0
-- 影响：新增 setting 表并新增 anonymous 用户
-- 数据处理：插入默认匿名访问设置和匿名主体；预计耗时：毫秒级
-- 回滚：不支持 down migration；需要恢复升级前备份
-- 相关：FR-66 / ADR-0002
-- 0007（FR-66）：全局设置表 + 内置匿名主体。
-- setting 为通用键值设置存储，当前仅承载匿名访问全局开关（默认开启）。
CREATE TABLE setting (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);

INSERT INTO setting (key, value) VALUES ('anonymous_access_enabled', 'true');

-- 内置 anonymous 用户：作为 ACL 主体承接匿名授权。
-- password_hash 为非法 argon2 哈希 '!'，口令验证永不通过；服务层另禁登录/删除/改密/停用。
INSERT INTO user (username, password_hash, role, status)
VALUES ('anonymous', '!', 'user', 'active');
