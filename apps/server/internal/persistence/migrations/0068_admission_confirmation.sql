-- FR-170 裁决确认令牌：单次消费登记（绑定关系由令牌签名承载，表仅保证不可重放）。
CREATE TABLE admission_confirmation_token (
    token_id TEXT PRIMARY KEY,
    used_unix INTEGER NOT NULL
);
