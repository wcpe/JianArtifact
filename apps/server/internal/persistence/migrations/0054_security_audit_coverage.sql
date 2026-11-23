-- 审计采集检查点只保存最后一批回执；窗口缺口独立于事件正文，不存来源或错误原文。
CREATE TABLE security_audit_checkpoint (
    singleton         INTEGER PRIMARY KEY CHECK (singleton = 1),
    worker_id         TEXT NOT NULL,
    last_batch        INTEGER NOT NULL CHECK (last_batch >= 0),
    batch_digest      BLOB,
    heartbeat_at      INTEGER NOT NULL,
    covered_from      INTEGER NOT NULL,
    covered_through   INTEGER NOT NULL CHECK (covered_from <= covered_through AND covered_through <= heartbeat_at),
    event_watermark   INTEGER NOT NULL CHECK (event_watermark >= 0),
    clean_stop        INTEGER NOT NULL CHECK (clean_stop IN (0, 1)),
    coverage          TEXT NOT NULL CHECK (coverage IN ('pending', 'complete', 'gapped', 'unknown')),
    CHECK (coverage <> 'complete' OR covered_from < covered_through)
) STRICT;

-- 已激活的租约不能在检查点被接管后重放启动、重新成为写者。
CREATE TABLE security_audit_worker (
    worker_id TEXT PRIMARY KEY
) STRICT, WITHOUT ROWID;

CREATE TABLE security_audit_gap (
    window_start      INTEGER NOT NULL,
    window_end        INTEGER NOT NULL CHECK (window_end > window_start),
    reason            TEXT NOT NULL CHECK (reason IN ('global_rate', 'source_rate', 'source_table_full', 'queue_full',
                       'database_unavailable', 'invalid_fact', 'counter_overflow', 'unclean_restart', 'restart')),
    coverage          TEXT NOT NULL CHECK (coverage IN ('gapped', 'unknown')),
    observed_count    INTEGER,
    suppressed_count  INTEGER,
    CHECK ((coverage = 'unknown' AND observed_count IS NULL AND suppressed_count IS NULL) OR
           (coverage = 'gapped' AND observed_count IS NOT NULL AND suppressed_count IS NOT NULL AND
            observed_count >= suppressed_count AND suppressed_count > 0)),
    PRIMARY KEY (window_end, window_start, reason)
) STRICT, WITHOUT ROWID;

CREATE INDEX security_audit_gap_recent ON security_audit_gap(window_start DESC, coverage DESC, window_end DESC, reason);
