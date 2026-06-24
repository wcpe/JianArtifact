-- 使用分析采集表（FR-57，ADR-0009）：访问 / 下载统计落本地 SQLite，本机内部数据。
-- 数据默认不外发、不向外部遥测 phone-home；任何外部导出默认关闭（本批不做导出）。
-- 设计为聚合计数为主、可选明细为辅，明细量级由后台裁剪兜底，避免撑爆 SQLite。

-- 聚合计数表：按（仓库 + 制品路径 + 动作）累加，访问 / 下载各一行计数。
-- 采集走 UPSERT 累加（INSERT ... ON CONFLICT DO UPDATE count = count + 1），并发下计数准确。
-- repo_path 为空串表示仓库级（非具体制品）的聚合；action 取 access | download。
CREATE TABLE usage_stats (
    repo_name   TEXT NOT NULL,                            -- 目标仓库名
    repo_path   TEXT NOT NULL,                            -- 制品仓库内路径（仓库级聚合时为空串）
    action      TEXT NOT NULL,                            -- 动作枚举：access | download
    count       INTEGER NOT NULL DEFAULT 0,               -- 累计次数
    last_at     TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,  -- 最近一次发生时间（UTC）
    PRIMARY KEY (repo_name, repo_path, action)
);

-- 面板查询常按动作排序取热门制品 / 仓库用量，建动作 + 计数索引以加速聚合扫描。
CREATE INDEX idx_usage_stats_action_count ON usage_stats (action, count DESC);

-- 可选明细表：逐条访问 / 下载事件，仅在配置开启明细时写入，量级由后台裁剪兜底。
-- 不记凭据 / 隐私：actor 只记用户名或 anonymous；source_ip 可空。
CREATE TABLE usage_events (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    ts          TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,  -- 事件时间（UTC）
    repo_name   TEXT NOT NULL,                            -- 目标仓库名
    repo_path   TEXT NOT NULL,                            -- 制品仓库内路径
    action      TEXT NOT NULL,                            -- access | download
    actor       TEXT NOT NULL,                            -- 用户名或 anonymous，不记凭据
    source_ip   TEXT                                      -- 来源 IP（可空）
);

-- 明细按时间倒序浏览，行数兜底裁剪按 id（即时间）删最旧。
CREATE INDEX idx_usage_events_ts ON usage_events (ts DESC);
