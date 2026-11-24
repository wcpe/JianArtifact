-- 安全治理 安全通知：站内持久递增序列（7 天可重放）+ 站外投递登记。
-- 游标=递增 seq，绑定序列域即可分页回放；不依赖进程内广播。
CREATE TABLE security_notification (
    seq          INTEGER PRIMARY KEY AUTOINCREMENT,
    notice_id    TEXT NOT NULL UNIQUE,
    kind         TEXT NOT NULL,
    title        TEXT NOT NULL,
    payload      TEXT NOT NULL DEFAULT '{}',
    created_unix INTEGER NOT NULL
);
CREATE INDEX idx_security_notification_created ON security_notification(created_unix);

-- 本人确认状态（每管理员一行；ack 不改案件/处置/投递事实）。
CREATE TABLE security_notification_ack (
    seq        INTEGER NOT NULL REFERENCES security_notification(seq),
    user_id    INTEGER NOT NULL,
    ack_unix   INTEGER NOT NULL,
    PRIMARY KEY (seq, user_id)
);

-- 站外投递：渠道脱敏、持久重试、失败可见。
CREATE TABLE security_notification_delivery (
    delivery_id    TEXT PRIMARY KEY,
    seq            INTEGER NOT NULL REFERENCES security_notification(seq),
    channel        TEXT NOT NULL CHECK (channel IN ('smtp','qq')),
    target_masked  TEXT NOT NULL,
    state          TEXT NOT NULL CHECK (state IN ('pending','attempted','accepted','failed')),
    attempts       INTEGER NOT NULL DEFAULT 0,
    last_error     TEXT NOT NULL DEFAULT '',
    next_retry_unix INTEGER NOT NULL DEFAULT 0,
    created_unix   INTEGER NOT NULL,
    updated_unix   INTEGER NOT NULL
);
CREATE INDEX idx_security_notification_delivery_state ON security_notification_delivery(state, next_retry_unix);
