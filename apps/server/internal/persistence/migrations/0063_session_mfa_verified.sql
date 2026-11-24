-- 会话级近期强认证水位：登录兑现或 step_up 确认写入 mfa_verified_at；
-- 敏感操作按 5 分钟窗口重查该水位而非静态 JWT 声明。
ALTER TABLE credential_session ADD COLUMN mfa_verified_at INTEGER;
CREATE INDEX idx_credential_session_mfa ON credential_session(user_id, mfa_verified_at);
