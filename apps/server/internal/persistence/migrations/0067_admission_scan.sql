-- FR-170 隔离静态检测：候选扫描任务的持久租约与有界结构化事实。
CREATE TABLE admission_scan_job (
    job_id TEXT PRIMARY KEY,
    operation_id TEXT NOT NULL REFERENCES private_admission_candidate(operation_id),
    manifest_version INTEGER NOT NULL,
    engine TEXT NOT NULL,
    engine_version TEXT NOT NULL,
    rules_version TEXT NOT NULL,
    parser_profile_version TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('queued','scanning','completed','failed')),
    lease_generation INTEGER NOT NULL DEFAULT 0,
    lease_until_unix INTEGER NOT NULL DEFAULT 0,
    worker_id TEXT NOT NULL DEFAULT '',
    attempts INTEGER NOT NULL DEFAULT 0,
    max_attempts INTEGER NOT NULL DEFAULT 3,
    next_run_at_unix INTEGER NOT NULL,
    verdict TEXT NOT NULL DEFAULT '' CHECK (verdict IN ('','approved','pending_review','rejected')),
    error_code TEXT NOT NULL DEFAULT '',
    created_unix INTEGER NOT NULL,
    updated_unix INTEGER NOT NULL,
    UNIQUE (operation_id, manifest_version),
    CHECK (lease_generation >= 0 AND attempts >= 0)
);
CREATE INDEX idx_admission_scan_job_runnable ON admission_scan_job(state, next_run_at_unix);
CREATE INDEX idx_admission_scan_job_lease ON admission_scan_job(state, lease_until_unix);

CREATE TABLE admission_scan_fact (
    job_id TEXT NOT NULL REFERENCES admission_scan_job(job_id),
    ordinal INTEGER NOT NULL,
    lease_generation INTEGER NOT NULL,
    object_digest TEXT NOT NULL,
    coverage_required INTEGER NOT NULL,
    coverage_completed INTEGER NOT NULL,
    coverage_missing TEXT NOT NULL DEFAULT '[]',
    integrity TEXT NOT NULL CHECK (integrity IN ('passed','failed','unknown')),
    verdict TEXT NOT NULL CHECK (verdict IN ('approved','pending_review','rejected')),
    findings TEXT NOT NULL DEFAULT '[]',
    worker_id TEXT NOT NULL,
    observed_at_unix INTEGER NOT NULL,
    PRIMARY KEY (job_id, ordinal),
    CHECK (coverage_required >= 0 AND coverage_completed >= 0)
);
