-- 通用扁平时序表（FR-105，ADR-0027）：一行 = 一个指标键在一个时刻的标量取值。
CREATE TABLE metric_samples (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    metric_key TEXT    NOT NULL,
    ts         INTEGER NOT NULL,   -- Unix 毫秒，UTC
    value      REAL    NOT NULL
);
CREATE INDEX idx_metric_samples_key_ts ON metric_samples (metric_key, ts);
