-- FR-172 QQ 通知渠道：管理员 QQ openid 绑定表。
-- openid 为官方 QQ 平台主体标识；绑定由管理员建立（平台绑定流程待凭据接入后可扩展挑战确认）。
CREATE TABLE security_qq_binding (
    user_id INTEGER NOT NULL PRIMARY KEY,
    openid TEXT NOT NULL UNIQUE,
    bound_unix INTEGER NOT NULL
);
