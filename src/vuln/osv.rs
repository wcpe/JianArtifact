//! OSV 公告 JSON 解析（FR-70，ADR-0012）。
//!
//! 把单条 OSV 格式的漏洞公告 JSON 解析为本地落库用的中间结构 [`ParsedAdvisory`]。
//! 解析为无副作用纯函数，便于喂样例 OSV JSON 穷举单测；下载与落库在别处。
//!
//! 仅取本批落库所需字段（id / 描述 / 时间 / 严重度 / 受影响坐标），OSV 其余字段忽略，
//! 不在此实现任何按制品坐标的匹配逻辑（属 FR-71）。

use serde::Deserialize;

/// 解析错误。
#[derive(Debug, thiserror::Error)]
pub enum OsvParseError {
    /// JSON 反序列化失败（格式非法或缺关键字段）。
    #[error("OSV JSON 解析失败: {0}")]
    Json(#[from] serde_json::Error),
}

/// 解析后的单条漏洞公告（落库前的中间表示，不含本机存储字段如 created_at）。
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ParsedAdvisory {
    /// 公告唯一标识（OSV 的 `id`）。
    pub id: String,
    /// 简要描述。
    pub summary: Option<String>,
    /// 详细描述。
    pub details: Option<String>,
    /// 严重度（取首个 severity 的 score，多为 CVSS 向量串）。
    pub severity: Option<String>,
    /// 上游最近修改时间（ISO8601）。
    pub modified: Option<String>,
    /// 发布时间（ISO8601）。
    pub published: Option<String>,
    /// 受影响的生态坐标（逐包展开）。
    pub affected: Vec<ParsedAffected>,
}

/// 解析后的单个受影响坐标。
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ParsedAffected {
    /// 生态（如 Maven / npm）。
    pub ecosystem: String,
    /// 包坐标名（如 `group:artifact`、npm 包名）。
    pub package: String,
    /// 受影响版本范围的原始 JSON 文本（保真存储，本批不解析其语义）。
    pub ranges: Option<String>,
    /// 受影响具体版本列表的原始 JSON 文本（保真存储）。
    pub versions: Option<String>,
}

/// OSV 公告的原始反序列化结构（仅声明本批所需字段，其余字段被忽略）。
#[derive(Debug, Deserialize)]
struct OsvAdvisory {
    /// 公告唯一标识。
    id: String,
    #[serde(default)]
    summary: Option<String>,
    #[serde(default)]
    details: Option<String>,
    #[serde(default)]
    modified: Option<String>,
    #[serde(default)]
    published: Option<String>,
    #[serde(default)]
    severity: Vec<OsvSeverity>,
    #[serde(default)]
    affected: Vec<OsvAffected>,
}

/// OSV 严重度项。
#[derive(Debug, Deserialize)]
struct OsvSeverity {
    /// 严重度评分（CVSS 向量串等）。
    #[serde(default)]
    score: Option<String>,
}

/// OSV 受影响项。
#[derive(Debug, Deserialize)]
struct OsvAffected {
    /// 受影响包坐标。
    #[serde(default)]
    package: Option<OsvPackage>,
    /// 受影响版本范围（事件序列），保真透传。
    #[serde(default)]
    ranges: Option<serde_json::Value>,
    /// 受影响具体版本列表，保真透传。
    #[serde(default)]
    versions: Option<serde_json::Value>,
}

/// OSV 包坐标。
#[derive(Debug, Deserialize)]
struct OsvPackage {
    /// 生态名（如 Maven / npm）。
    #[serde(default)]
    ecosystem: Option<String>,
    /// 包名（坐标）。
    #[serde(default)]
    name: Option<String>,
}

/// 解析单条 OSV 公告 JSON 文本为中间结构。
///
/// 缺少 `package.ecosystem` 或 `package.name` 的受影响项被跳过（无坐标无法落库为坐标行）；
/// 严重度取首个 `severity[].score`。其余 OSV 字段不在本批范围内，忽略。
pub fn parse_advisory(json: &str) -> Result<ParsedAdvisory, OsvParseError> {
    let raw: OsvAdvisory = serde_json::from_str(json)?;
    Ok(from_raw(raw))
}

/// 把原始反序列化结构归一为中间结构（纯函数，便于单测）。
fn from_raw(raw: OsvAdvisory) -> ParsedAdvisory {
    // 严重度取首个有 score 的项；OSV 多以 CVSS 向量串承载
    let severity = raw
        .severity
        .into_iter()
        .find_map(|s| s.score)
        .filter(|s| !s.is_empty());

    let affected = raw
        .affected
        .into_iter()
        .filter_map(|a| {
            // 无坐标的受影响项（缺生态或包名）无法形成坐标行，跳过
            let package = a.package?;
            let ecosystem = package.ecosystem?;
            let name = package.name?;
            if ecosystem.is_empty() || name.is_empty() {
                return None;
            }
            Some(ParsedAffected {
                ecosystem,
                package: name,
                // 保真存原始 JSON 文本，本批不解析其语义
                ranges: a.ranges.map(|v| v.to_string()),
                versions: a.versions.map(|v| v.to_string()),
            })
        })
        .collect();

    ParsedAdvisory {
        id: raw.id,
        summary: raw.summary.filter(|s| !s.is_empty()),
        details: raw.details.filter(|s| !s.is_empty()),
        severity,
        modified: raw.modified.filter(|s| !s.is_empty()),
        published: raw.published.filter(|s| !s.is_empty()),
        affected,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// 一条覆盖常见字段的 OSV 样例（Maven 生态，含 ranges 与 severity）。
    const SAMPLE_MAVEN: &str = r#"{
        "id": "GHSA-jfh8-c2jp-5v3q",
        "summary": "远程代码执行漏洞",
        "details": "受影响版本存在 RCE 风险。",
        "modified": "2023-11-08T04:00:00Z",
        "published": "2021-12-10T00:00:00Z",
        "severity": [
            { "type": "CVSS_V3", "score": "CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:C/C:H/I:H/A:H" }
        ],
        "affected": [
            {
                "package": { "ecosystem": "Maven", "name": "org.apache.logging.log4j:log4j-core" },
                "ranges": [
                    { "type": "ECOSYSTEM", "events": [ { "introduced": "2.0" }, { "fixed": "2.17.1" } ] }
                ],
                "versions": [ "2.14.0", "2.14.1" ]
            }
        ]
    }"#;

    #[test]
    fn 解析含完整字段的_maven_公告() {
        let adv = parse_advisory(SAMPLE_MAVEN).unwrap();
        assert_eq!(adv.id, "GHSA-jfh8-c2jp-5v3q");
        assert_eq!(adv.summary.as_deref(), Some("远程代码执行漏洞"));
        assert_eq!(adv.modified.as_deref(), Some("2023-11-08T04:00:00Z"));
        assert!(adv.severity.as_deref().unwrap().starts_with("CVSS:3.1"));
        assert_eq!(adv.affected.len(), 1);
        let aff = &adv.affected[0];
        assert_eq!(aff.ecosystem, "Maven");
        assert_eq!(aff.package, "org.apache.logging.log4j:log4j-core");
        // ranges / versions 保真为 JSON 文本
        assert!(aff.ranges.as_deref().unwrap().contains("2.17.1"));
        assert!(aff.versions.as_deref().unwrap().contains("2.14.0"));
    }

    #[test]
    fn 解析仅必填字段的最小公告() {
        // 仅有 id，无描述、无严重度、无受影响项
        let adv = parse_advisory(r#"{ "id": "OSV-2020-1" }"#).unwrap();
        assert_eq!(adv.id, "OSV-2020-1");
        assert!(adv.summary.is_none());
        assert!(adv.severity.is_none());
        assert!(adv.affected.is_empty());
    }

    #[test]
    fn 跳过缺坐标的受影响项() {
        // 一项有完整坐标，一项缺 name，一项缺 ecosystem，一项 package 整体缺失
        let json = r#"{
            "id": "OSV-X",
            "affected": [
                { "package": { "ecosystem": "npm", "name": "lodash" } },
                { "package": { "ecosystem": "npm" } },
                { "package": { "name": "孤包" } },
                { "ranges": [] }
            ]
        }"#;
        let adv = parse_advisory(json).unwrap();
        // 仅完整坐标项被保留
        assert_eq!(adv.affected.len(), 1);
        assert_eq!(adv.affected[0].ecosystem, "npm");
        assert_eq!(adv.affected[0].package, "lodash");
    }

    #[test]
    fn 多受影响坐标逐包展开() {
        let json = r#"{
            "id": "OSV-MULTI",
            "affected": [
                { "package": { "ecosystem": "npm", "name": "a" } },
                { "package": { "ecosystem": "npm", "name": "b" } }
            ]
        }"#;
        let adv = parse_advisory(json).unwrap();
        assert_eq!(adv.affected.len(), 2);
    }

    #[test]
    fn 非法_json_返回错误() {
        assert!(parse_advisory("不是 json").is_err());
        // 缺必填 id 也应失败
        assert!(parse_advisory(r#"{ "summary": "无 id" }"#).is_err());
    }

    #[test]
    fn 空串字段归一为_none() {
        let json = r#"{ "id": "OSV-EMPTY", "summary": "", "modified": "" }"#;
        let adv = parse_advisory(json).unwrap();
        assert!(adv.summary.is_none());
        assert!(adv.modified.is_none());
    }
}
