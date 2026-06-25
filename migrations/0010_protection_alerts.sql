-- 防护告警表（FR-56，ADR-0017）：进程内阈值告警评估器产生的告警事件落本地 SQLite。
-- 告警是本机内部数据：只落本地、默认不外发、不向外部遥测 phone-home；不内置外发型通知。
-- 不存任何凭据 / 密钥；detail 仅记结构化上下文（维度 / 当前值 / 阈值 / 时间窗），禁含隐私。
-- 入库走异步有界 channel + 写任务批量落库（与审计 / 使用分析同款范式），失败仅 WARN 不阻塞主路径。

CREATE TABLE protection_alerts (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    ts            TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,  -- UTC 告警时间
    dimension     TEXT NOT NULL,                            -- 防护维度：rate_limit / ban / cc_challenge / waf / slowloris
    severity      TEXT NOT NULL,                            -- 严重度：warn | error
    observed_value INTEGER NOT NULL,                        -- 触发告警时的窗内观测计数
    threshold     INTEGER NOT NULL,                         -- 触发告警的阈值
    window_secs   INTEGER NOT NULL,                         -- 评估时间窗时长（秒）
    detail        TEXT                                      -- 结构化补充（中文文案），禁含凭据 / 隐私
);

-- 查询按时间倒序浏览，并支持按维度过滤；行数兜底裁剪按 id（即时间）删最旧。
CREATE INDEX idx_protection_alerts_ts ON protection_alerts (ts DESC);
CREATE INDEX idx_protection_alerts_dimension ON protection_alerts (dimension);
