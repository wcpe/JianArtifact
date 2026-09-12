-- 0037：审计事件的 HTTP 上下文与账号邮箱
-- 引入版本：0.8.0
-- 影响：audit_log 新增请求方法/路由/状态码/耗时/令牌预览/脱敏请求体/操作者邮箱列；users 新增邮箱列
-- 数据处理：已有行使用空值/零值默认值，无历史回填；预计耗时：随 audit_log 行数增长（仅加列）
-- 回滚：不支持 down migration；需要恢复升级前备份
-- 相关：FR-118 / ADR-0002
--
-- 背景：管理端审计中心需要按「请求方法 / 路由 / 状态码 / 耗时 / 客户端 IP / 认证方式」检索，
-- 并在详情中展示请求 ID、User-Agent、令牌预览与脱敏请求体。audit_log 已含 ip / user_agent /
-- request_id / token_id / token_name / auth_source，本迁移补齐剩余 HTTP 上下文与操作者邮箱快照。
-- 安全边界：token_preview 与 body_preview 只能是掩码/截断后的内容，不得写入凭据原文。
ALTER TABLE audit_log ADD COLUMN http_method TEXT NOT NULL DEFAULT '';
ALTER TABLE audit_log ADD COLUMN http_path TEXT NOT NULL DEFAULT '';
ALTER TABLE audit_log ADD COLUMN status_code INTEGER NOT NULL DEFAULT 0;
ALTER TABLE audit_log ADD COLUMN duration_ms INTEGER NOT NULL DEFAULT 0;
ALTER TABLE audit_log ADD COLUMN token_preview TEXT NOT NULL DEFAULT '';
ALTER TABLE audit_log ADD COLUMN body_preview TEXT NOT NULL DEFAULT '';
ALTER TABLE audit_log ADD COLUMN actor_email TEXT NOT NULL DEFAULT '';

CREATE INDEX idx_audit_log_method ON audit_log (http_method, ts DESC);
CREATE INDEX idx_audit_log_status ON audit_log (status_code, ts DESC);
CREATE INDEX idx_audit_log_actor_email ON audit_log (actor_email, ts DESC);

-- 账号邮箱：审计检索维度，并作为事件记录时的身份快照来源。
ALTER TABLE user ADD COLUMN email TEXT NOT NULL DEFAULT '';
CREATE INDEX idx_user_email ON user (email);
