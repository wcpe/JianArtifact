-- 安全治理 案件既読水位（按管理员保存，同一版本幂等，不改案件状态）；
-- 安全治理 投递行人工再試行是 UPDATE 既有行，不需新表。
CREATE TABLE security_case_read (
    case_id      TEXT NOT NULL REFERENCES security_case(case_id),
    user_id      INTEGER NOT NULL,
    seen_version INTEGER NOT NULL,
    read_unix    INTEGER NOT NULL,
    PRIMARY KEY (case_id, user_id)
);
