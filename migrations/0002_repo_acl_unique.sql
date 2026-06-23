-- 为每仓库 ACL 增加唯一约束：同一 (仓库, 用户, 权限) 不可重复授予。
-- 支撑 API 契约 POST /repositories/{id}/acl 的 409 语义（重复授权返回冲突）。
-- 同一用户对同一仓库仍可分别持有 read 与 write 两条（permission 不同，不冲突）。
CREATE UNIQUE INDEX idx_repo_acl_unique ON repo_acl (repo_id, user_id, permission);
