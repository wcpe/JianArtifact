-- 库内账号凭据锚；不代表库外恢复时代。不回填旧 Token 或会话。
ALTER TABLE user ADD COLUMN credential_revision INTEGER NOT NULL DEFAULT 0 CHECK (credential_revision >= 0);
ALTER TABLE api_token ADD COLUMN account_epoch INTEGER CHECK (account_epoch > 0);
ALTER TABLE api_token ADD COLUMN expires_at INTEGER CHECK (expires_at > 0);

CREATE TABLE credential_revision_clock (
    singleton INTEGER PRIMARY KEY CHECK (singleton=1),
    revision INTEGER NOT NULL CHECK (revision >= 0)
) STRICT;
INSERT INTO credential_revision_clock VALUES (1,0);

-- 既有改密事务推进代次时也同序推进签发水位。新建或重新绑定不能接纳早于它的外部流程。
CREATE TRIGGER credential_revision_after_change AFTER UPDATE OF account_epoch, external_subject, auth_source ON user
WHEN OLD.account_epoch <> NEW.account_epoch OR OLD.external_subject <> NEW.external_subject OR OLD.auth_source <> NEW.auth_source
BEGIN
    UPDATE credential_revision_clock SET revision=revision+1 WHERE singleton=1;
    UPDATE user SET credential_revision=(SELECT revision FROM credential_revision_clock WHERE singleton=1) WHERE id=NEW.id;
END;
CREATE TRIGGER credential_revision_after_insert AFTER INSERT ON user
BEGIN
    UPDATE credential_revision_clock SET revision=revision+1 WHERE singleton=1;
    UPDATE user SET credential_revision=(SELECT revision FROM credential_revision_clock WHERE singleton=1) WHERE id=NEW.id;
END;

CREATE TABLE credential_session (
    session_id TEXT PRIMARY KEY NOT NULL,
    jti TEXT NOT NULL UNIQUE,
    user_id INTEGER NOT NULL REFERENCES user(id) ON DELETE CASCADE,
    account_epoch INTEGER NOT NULL CHECK (account_epoch > 0),
    expires_at INTEGER NOT NULL CHECK (expires_at > 0),
    revoked_at INTEGER
) STRICT, WITHOUT ROWID;
CREATE INDEX idx_credential_session_user ON credential_session(user_id);

CREATE TABLE credential_account_access_receipt (
    operation_id TEXT PRIMARY KEY NOT NULL,
    event_id TEXT NOT NULL UNIQUE REFERENCES security_event(event_id),
    actor_kind TEXT NOT NULL CHECK (actor_kind IN ('user','local_operator')),
    actor_user_id INTEGER,
    target_user_id INTEGER NOT NULL CHECK (target_user_id > 0),
    CHECK ((actor_kind='user' AND actor_user_id IS NOT NULL AND actor_user_id>0) OR (actor_kind='local_operator' AND actor_user_id IS NULL))
) STRICT, WITHOUT ROWID;
