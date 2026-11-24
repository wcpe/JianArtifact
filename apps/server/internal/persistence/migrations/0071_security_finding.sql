-- FR-171 规则检测投影：security_event を递增 sequence で順次消費し、
-- バージョン付き規則の命中（finding）を冪等登録 → 未決案件への集約または自動建案。
-- 規則は検出と通知のみ；自動封禁・自動結案・自動マージは行わない。
CREATE TABLE security_finding (
    finding_id   TEXT PRIMARY KEY,  -- 規則・版・主体・窓の決定的ハッシュ（再計算で再発しない）
    rule_id      TEXT NOT NULL,
    rule_version INTEGER NOT NULL,
    severity     TEXT NOT NULL CHECK (severity IN ('low','medium','high','critical')),
    confidence   TEXT NOT NULL DEFAULT 'medium' CHECK (confidence IN ('low','medium','high')),
    subject_key  TEXT NOT NULL,     -- 安定錨点: user:<id> / cred:<id> / ip:<addr>
    window_from  INTEGER NOT NULL,
    window_to    INTEGER NOT NULL,
    summary      TEXT NOT NULL,     -- 規則条件・件数・対象の説明（人間可読・脱敏済み）
    case_id      TEXT REFERENCES security_case(case_id),
    created_unix INTEGER NOT NULL
);
CREATE INDEX idx_security_finding_subject ON security_finding(subject_key, created_unix);
CREATE INDEX idx_security_finding_case ON security_finding(case_id) WHERE case_id IS NOT NULL;

-- 命中の証拠イベント（sequence で security_event に逆引き）。
CREATE TABLE security_finding_event (
    finding_id     TEXT NOT NULL REFERENCES security_finding(finding_id),
    event_sequence INTEGER NOT NULL,
    PRIMARY KEY (finding_id, event_sequence)
);

-- 投影の順次消費水位（単一 worker 前提、取りこぼしは次バッチに持ち越し）。
CREATE TABLE security_finding_watermark (
    id            INTEGER PRIMARY KEY CHECK (id = 1),
    last_sequence INTEGER NOT NULL,
    updated_unix  INTEGER NOT NULL
);
