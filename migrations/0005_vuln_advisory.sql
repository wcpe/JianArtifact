-- 漏洞库离线镜像落库结构（FR-70，ADR-0012）。
-- 把公开漏洞数据（如 OSV）镜像到本机并解析入库；本批仅做镜像/落库，
-- 不做按制品坐标匹配/标记（FR-71，后续批次）。
-- 约定：时间用 TEXT（ISO8601），布尔用 INTEGER（0/1），与既有表保持一致。

-- 漏洞公告表：一条 OSV 公告一行。来源为公开离线镜像，坐标不外发。
CREATE TABLE vuln_advisories (
    id          TEXT PRIMARY KEY,          -- 公告唯一标识（如 OSV 的 GHSA-xxxx / CVE-xxxx）
    source      TEXT NOT NULL,             -- 数据来源标识（如 osv）
    summary     TEXT,                      -- 简要描述
    details     TEXT,                      -- 详细描述
    severity    TEXT,                      -- 严重度（如 CVSS 向量串或等级；无则为空）
    modified    TEXT,                      -- 公告上游最近修改时间（ISO8601，用于增量判定）
    published   TEXT,                      -- 公告发布时间（ISO8601）
    created_at  TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP  -- 本机落库时间
);

-- 公告受影响坐标表：一条公告可影响多个生态包，逐行展开存储其坐标与版本范围。
-- 本批仅忠实落库这些坐标，供后续 FR-71 本地匹配使用；本批不实现匹配逻辑。
CREATE TABLE vuln_advisory_affected (
    id           TEXT PRIMARY KEY,
    advisory_id  TEXT NOT NULL,            -- 所属公告
    ecosystem    TEXT NOT NULL,            -- 生态（如 Maven / npm）
    package      TEXT NOT NULL,            -- 包坐标名（如 group:artifact、npm 包名）
    ranges       TEXT,                     -- 受影响版本范围（原始 JSON 文本，保真存储）
    versions     TEXT,                     -- 受影响具体版本列表（原始 JSON 文本，保真存储）
    FOREIGN KEY (advisory_id) REFERENCES vuln_advisories (id) ON DELETE CASCADE
);

-- 镜像刷新状态表：记录每个数据源每个生态最近一次成功刷新的状态，支持幂等刷新与运维观察。
CREATE TABLE vuln_mirror_state (
    source          TEXT NOT NULL,         -- 数据来源标识（如 osv）
    ecosystem       TEXT NOT NULL,         -- 镜像的生态
    last_refreshed  TEXT,                  -- 最近一次成功刷新时间（ISO8601）
    advisory_count  INTEGER NOT NULL DEFAULT 0,  -- 最近一次刷新落库的公告条数
    PRIMARY KEY (source, ecosystem)
);

-- 索引：按生态 + 包查询受影响公告（供后续坐标级匹配定位）。
CREATE INDEX idx_vuln_affected_eco_pkg ON vuln_advisory_affected (ecosystem, package);
