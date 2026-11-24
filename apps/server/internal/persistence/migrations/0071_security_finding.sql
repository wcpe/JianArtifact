-- 安全治理规则检测投影：security_event 按递增 sequence 依次消费，
-- 带版本规则的命中（finding）幂等登记 → 聚合到未决案件或自动建案。
-- 规则只做检测与通知；不自动封禁、不自动结案、不自动合并。
CREATE TABLE security_finding (
    finding_id   TEXT PRIMARY KEY,  -- 规则·版本·主体·窗口的确定性哈希（重算不再新发）
    rule_id      TEXT NOT NULL,
    rule_version INTEGER NOT NULL,
    severity     TEXT NOT NULL CHECK (severity IN ('low','medium','high','critical')),
    confidence   TEXT NOT NULL DEFAULT 'medium' CHECK (confidence IN ('low','medium','high')),
    subject_key  TEXT NOT NULL,     -- 安定錨点: user:<id> / cred:<id> / ip:<addr>
    window_from  INTEGER NOT NULL,
    window_to    INTEGER NOT NULL,
    summary      TEXT NOT NULL,     -- 规则条件·件数·目标说明（人类可读·已脱敏）
    case_id      TEXT REFERENCES security_case(case_id),
    created_unix INTEGER NOT NULL
);
CREATE INDEX idx_security_finding_subject ON security_finding(subject_key, created_unix);
CREATE INDEX idx_security_finding_case ON security_finding(case_id) WHERE case_id IS NOT NULL;

-- 命中的证据事件（按 sequence 反查 security_event）。
CREATE TABLE security_finding_event (
    finding_id     TEXT NOT NULL REFERENCES security_finding(finding_id),
    event_sequence INTEGER NOT NULL,
    PRIMARY KEY (finding_id, event_sequence)
);

-- 投影的依次消费水位（单 worker 前提，遗漏随下一批续推）。
CREATE TABLE security_finding_watermark (
    id            INTEGER PRIMARY KEY CHECK (id = 1),
    last_sequence INTEGER NOT NULL,
    updated_unix  INTEGER NOT NULL
);
