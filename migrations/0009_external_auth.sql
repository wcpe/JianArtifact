-- 外部认证 provider（OIDC，P2 / FR-34 / ADR-0016）：给 users 表加外部身份绑定列。
-- 仅存非敏感的身份标识（provider 类别 + 外部稳定标识），绝不存任何外部凭据。
-- 既有本地用户这两列为 NULL；外部身份经映射绑定后填入，建立「外部身份 → 本地用户」关系。

-- 外部 IdP/目录类别（如 oidc）；本地账号为 NULL。
ALTER TABLE users ADD COLUMN external_idp TEXT;
-- 外部稳定标识（OIDC `sub`）；本地账号为 NULL。绝不存外部凭据。
ALTER TABLE users ADD COLUMN external_subject TEXT;

-- 按（外部 provider 类别, 外部标识）唯一定位本地用户，保证同一外部身份只绑定一个本地账号。
-- 仅对两列均非 NULL 的行建唯一约束（本地账号两列为 NULL 不参与，SQLite 唯一索引忽略 NULL 组合）。
CREATE UNIQUE INDEX idx_users_external_identity
    ON users (external_idp, external_subject)
    WHERE external_idp IS NOT NULL AND external_subject IS NOT NULL;
