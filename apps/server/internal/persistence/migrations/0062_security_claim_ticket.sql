-- 认领票：本机受控 CLI（或后续近期强认证管理员）签发的一次性受限凭据。
-- 绑定账号、账号代次、用途与期限（15 分钟）；token_hash 存校验值不落明文；
-- 一次认领与因子激活同一事务消费，过期/冲突须重新出具并使同用途旧票不可用。
CREATE TABLE security_claim_ticket (
    ticket_id TEXT PRIMARY KEY NOT NULL,
    user_id INTEGER REFERENCES user(id) ON DELETE CASCADE,
    purpose TEXT NOT NULL CHECK (purpose IN ('claim_install','claim_admin')),
    token_hash TEXT NOT NULL UNIQUE,
    account_epoch INTEGER NOT NULL CHECK (account_epoch >= 0),
    issued_by TEXT NOT NULL DEFAULT '',
    reason TEXT NOT NULL DEFAULT '',
    attempts_remaining INTEGER NOT NULL CHECK (attempts_remaining >= 0),
    expires_at INTEGER NOT NULL CHECK (expires_at > 0),
    state TEXT NOT NULL DEFAULT 'issued' CHECK (state IN ('issued','consumed','expired','cancelled','exhausted')),
    created_at INTEGER NOT NULL,
    consumed_at INTEGER
) STRICT, WITHOUT ROWID;
CREATE INDEX idx_security_claim_ticket_owner ON security_claim_ticket(user_id, purpose, state);
-- 同账号同用途至多一张生效票（claim_install 无账号，按用途单列约束）。
CREATE UNIQUE INDEX idx_claim_ticket_one_active_user ON security_claim_ticket(user_id, purpose)
    WHERE state = 'issued' AND user_id IS NOT NULL;
CREATE UNIQUE INDEX idx_claim_ticket_one_active_install ON security_claim_ticket(purpose)
    WHERE state = 'issued' AND user_id IS NULL;
