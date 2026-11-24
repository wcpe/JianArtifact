-- 安全治理 风险调查案件：规则检测可建案，认领/结案/重开仅人工强认证入口；
-- 案件与事件关联只增不改（link 事实表），未决事件不因窗口消失。
CREATE TABLE security_case (
    case_id        TEXT PRIMARY KEY,
    title          TEXT NOT NULL,
    state          TEXT NOT NULL DEFAULT 'open' CHECK (state IN ('open','claimed','closed')),
    severity       TEXT NOT NULL DEFAULT 'medium' CHECK (severity IN ('low','medium','high','critical')),
    resolution     TEXT NOT NULL DEFAULT '',
    target_version INTEGER NOT NULL DEFAULT 1 CHECK (target_version >= 1),
    owner_user_id  INTEGER,
    opened_by      TEXT NOT NULL,
    reason         TEXT NOT NULL DEFAULT '',
    created_unix   INTEGER NOT NULL,
    updated_unix   INTEGER NOT NULL,
    closed_unix    INTEGER
);
CREATE INDEX idx_security_case_state ON security_case(state, updated_unix);

-- 案件↔事件关联：fact（已采信事件）| candidate（候选关联，待人工确认）。
CREATE TABLE security_case_link (
    case_id     TEXT NOT NULL REFERENCES security_case(case_id),
    event_id    TEXT NOT NULL,
    link_kind   TEXT NOT NULL CHECK (link_kind IN ('fact','candidate')),
    linked_by   TEXT NOT NULL,
    linked_unix INTEGER NOT NULL,
    PRIMARY KEY (case_id, event_id)
);
