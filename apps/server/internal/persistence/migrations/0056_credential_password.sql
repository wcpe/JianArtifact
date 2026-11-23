-- 账号代次只代表当前 SQLite 内的状态；不给已有会话或机器凭据赋予新资格。
ALTER TABLE user ADD COLUMN account_epoch INTEGER NOT NULL DEFAULT 1
    CHECK (typeof(account_epoch) = 'integer' AND account_epoch > 0);

-- 与改密和安全事件同事务；用户 ID 是历史引用，不级联删除。
CREATE TABLE credential_password_receipt (
    operation_id TEXT PRIMARY KEY NOT NULL,
    event_id TEXT NOT NULL UNIQUE REFERENCES security_event(event_id),
    actor_kind TEXT NOT NULL CHECK (actor_kind IN ('user', 'local_operator')),
    actor_user_id INTEGER,
    target_user_id INTEGER NOT NULL CHECK (target_user_id > 0),
    CHECK ((actor_kind = 'user' AND actor_user_id IS NOT NULL AND actor_user_id > 0)
        OR (actor_kind = 'local_operator' AND actor_user_id IS NULL))
) STRICT, WITHOUT ROWID;
