-- 强认证因子、挑战与恢复码。绑定账号代次（account_epoch），库外安全时代由恢复闸另行核对。
-- 挑战：短时、单用途、单效消费、限尝试；token_hash 存高熵不透明串的校验值，不回读明文。
CREATE TABLE security_auth_challenge (
    challenge_id TEXT PRIMARY KEY NOT NULL,
    user_id INTEGER NOT NULL REFERENCES user(id) ON DELETE CASCADE,
    account_epoch INTEGER NOT NULL CHECK (account_epoch > 0),
    purpose TEXT NOT NULL CHECK (purpose IN ('login_mfa','factor_enroll','factor_recover','step_up')),
    token_hash TEXT NOT NULL UNIQUE,
    attempts_remaining INTEGER NOT NULL CHECK (attempts_remaining >= 0),
    expires_at INTEGER NOT NULL CHECK (expires_at > 0),
    state TEXT NOT NULL DEFAULT 'issued' CHECK (state IN ('issued','consumed','expired','cancelled','exhausted')),
    created_at INTEGER NOT NULL,
    consumed_at INTEGER,
    auth_source TEXT NOT NULL DEFAULT '',
    session_binding TEXT NOT NULL DEFAULT ''
) STRICT, WITHOUT ROWID;
CREATE INDEX idx_security_auth_challenge_owner ON security_auth_challenge(user_id, purpose, state);

-- TOTP 因子：种子密文按 key_version 的实例专用密钥带认证加密；
-- last_accepted_step 条件更新保证同一时间步至多一次成功（含登记验证）。
CREATE TABLE security_totp_factor (
    factor_id TEXT PRIMARY KEY NOT NULL,
    user_id INTEGER NOT NULL REFERENCES user(id) ON DELETE CASCADE,
    account_epoch INTEGER NOT NULL CHECK (account_epoch > 0),
    state TEXT NOT NULL CHECK (state IN ('enrollment_pending','active','retired')),
    secret_ciphertext TEXT NOT NULL,
    key_version INTEGER NOT NULL CHECK (key_version > 0),
    last_accepted_step INTEGER NOT NULL DEFAULT -1,
    created_at INTEGER NOT NULL,
    expires_at INTEGER,
    activated_at INTEGER,
    retired_at INTEGER,
    challenge_id TEXT
) STRICT, WITHOUT ROWID;
CREATE UNIQUE INDEX idx_totp_factor_one_active ON security_totp_factor(user_id) WHERE state='active';
CREATE UNIQUE INDEX idx_totp_factor_one_pending ON security_totp_factor(user_id) WHERE state='enrollment_pending';

-- 恢复码：非秘密行 ID、独立不可逆校验值、批次覆盖旧批；单事务消费。
CREATE TABLE security_recovery_code (
    code_id TEXT PRIMARY KEY NOT NULL,
    user_id INTEGER NOT NULL REFERENCES user(id) ON DELETE CASCADE,
    factor_id TEXT NOT NULL REFERENCES security_totp_factor(factor_id) ON DELETE CASCADE,
    batch INTEGER NOT NULL CHECK (batch > 0),
    code_hash TEXT NOT NULL UNIQUE,
    created_at INTEGER NOT NULL,
    consumed_at INTEGER
) STRICT, WITHOUT ROWID;
CREATE INDEX idx_security_recovery_code_owner ON security_recovery_code(user_id, batch, consumed_at);
