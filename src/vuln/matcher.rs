//! 坐标级漏洞匹配（FR-71，ADR-0012）：把制品的生态坐标与本地已落库的公告受影响坐标比对，
//! 判断该制品版本是否落入某公告的受影响范围。
//!
//! **隐私红线**：本模块只在内存里对**本机已镜像**的公告数据做比对，输入仅为制品自身坐标与
//! 本地公告记录，**绝不发起任何网络请求、绝不把坐标外发到外部漏洞服务**（守 ADR-0012 / 数据不外发）。
//! 全部为无副作用纯函数，便于穷举测试（边界版本、范围开闭、显式版本列表等）。
//!
//! 版本范围语义按 OSV `affected` 约定：
//! - `ranges[].events` 是有序事件流，`introduced` 起含、`fixed` 止不含、`last_affected` 止含；
//!   某版本若 `>= introduced` 且尚未遇到使其"修复/越过末段"的事件，则落入受影响区间。
//! - `versions[]` 是显式受影响版本列表；命中其一即受影响。
//!
//! 二者满足其一即判定受影响。无范围也无显式版本时，保守判定为不受影响（避免误报全量版本）。

use serde::Deserialize;

/// 单条受影响坐标记录（来自本地 `vuln_advisory_affected`，`ranges` / `versions` 为原始 JSON 文本）。
///
/// 与上层 `meta` 的落库结构解耦：本结构只承载匹配判定所需字段，便于纯函数穷举测试。
#[derive(Debug, Clone)]
pub struct AffectedRecord {
    /// 受影响版本范围的原始 JSON 文本（OSV `affected[].ranges`），可空。
    pub ranges: Option<String>,
    /// 受影响具体版本列表的原始 JSON 文本（OSV `affected[].versions`），可空。
    pub versions: Option<String>,
}

/// OSV 受影响范围项。
#[derive(Debug, Deserialize)]
struct OsvRange {
    /// 范围类型（`SEMVER` / `ECOSYSTEM` / `GIT` 等）；`GIT` 类型不做版本号比较，跳过。
    #[serde(default, rename = "type")]
    kind: String,
    /// 有序事件流。
    #[serde(default)]
    events: Vec<OsvEvent>,
}

/// OSV 范围事件：起始 / 修复 / 末个受影响版本，三者互斥地承载一个版本边界。
#[derive(Debug, Deserialize)]
struct OsvEvent {
    /// 受影响起始版本（含）。
    #[serde(default)]
    introduced: Option<String>,
    /// 修复版本（不含，即 `< fixed` 才受影响）。
    #[serde(default)]
    fixed: Option<String>,
    /// 最后一个受影响版本（含，即 `<= last_affected` 才受影响）。
    #[serde(default)]
    last_affected: Option<String>,
}

/// 判定某制品坐标是否落入某条受影响记录的范围。
///
/// 先看显式 `versions` 列表（命中其一即受影响），再看 `ranges` 区间语义；满足其一即判受影响。
pub fn is_affected(version: &str, record: &AffectedRecord) -> bool {
    if version_in_explicit_list(version, record.versions.as_deref()) {
        return true;
    }
    version_in_ranges(version, record.ranges.as_deref())
}

/// 显式版本列表命中：`versions` 为 `["1.0","1.1"]` 形态，做字符串等值匹配。
fn version_in_explicit_list(version: &str, versions_json: Option<&str>) -> bool {
    let Some(text) = versions_json else {
        return false;
    };
    let Ok(list) = serde_json::from_str::<Vec<String>>(text) else {
        return false;
    };
    list.iter().any(|v| v == version)
}

/// 区间语义命中：解析 `ranges`，对每个范围按事件流判定该版本是否落入受影响区间。
fn version_in_ranges(version: &str, ranges_json: Option<&str>) -> bool {
    let Some(text) = ranges_json else {
        return false;
    };
    let Ok(ranges) = serde_json::from_str::<Vec<OsvRange>>(text) else {
        return false;
    };
    ranges.iter().any(|r| range_covers(version, r))
}

/// 判定单个范围是否覆盖该版本。
///
/// 按 OSV 约定遍历有序事件：维护"当前是否处于受影响开区间"。`introduced` 开启区间（起含），
/// `fixed` / `last_affected` 关闭区间；落在任一开启区间内即受影响。`GIT` 类型范围无可比版本号，跳过。
fn range_covers(version: &str, range: &OsvRange) -> bool {
    // GIT 范围以提交号定界，无法与版本号比较，保守跳过（不据此判受影响）
    if range.kind.eq_ignore_ascii_case("git") {
        return false;
    }
    let mut active_since: Option<&str> = None;
    for event in &range.events {
        if let Some(intro) = &event.introduced {
            // introduced 起含：开启一个自该版本起的受影响区间
            active_since = Some(intro);
            // 若当前版本恰 >= 起始且尚无后续封闭事件，先记着，遇封闭事件再判
            continue;
        }
        if let Some(fixed) = &event.fixed {
            // fixed 不含：仅当处于开启区间，且 introduced <= version < fixed 时受影响
            if let Some(since) = active_since {
                if compare_versions(version, since) != std::cmp::Ordering::Less
                    && compare_versions(version, fixed) == std::cmp::Ordering::Less
                {
                    return true;
                }
            }
            // 区间封闭
            active_since = None;
            continue;
        }
        if let Some(last) = &event.last_affected {
            // last_affected 含：introduced <= version <= last_affected 时受影响
            if let Some(since) = active_since {
                if compare_versions(version, since) != std::cmp::Ordering::Less
                    && compare_versions(version, last) != std::cmp::Ordering::Greater
                {
                    return true;
                }
            }
            active_since = None;
            continue;
        }
    }
    // 遍历结束仍处于开启区间（只有 introduced、无封闭事件）：自起始版本起一律受影响
    if let Some(since) = active_since {
        return compare_versions(version, since) != std::cmp::Ordering::Less;
    }
    false
}

/// 比较两个版本号（适配 Maven / npm 等常见点分版本）。
///
/// 规则：按 `.` 与 `-` 拆段逐段比较；纯数字段按数值比较，含非数字段按 ASCII 字典序比较；
/// 数字段恒小于非数字段（与 semver 预发布序一致，如 `1.0.0-rc < 1.0.0`）。段数不等时缺失段：
/// 若较短一方剩余被比较段均为"发布段"（数字），则较短者更大（`1.0 > 1.0-rc`，`1.0 == 1.0.0` 视末尾零）。
/// 这是覆盖绝大多数真实版本的保守可预期比较，不追求完整 semver 语义。
pub fn compare_versions(a: &str, b: &str) -> std::cmp::Ordering {
    use std::cmp::Ordering;
    let sa = split_version(a);
    let sb = split_version(b);
    let max = sa.len().max(sb.len());
    for i in 0..max {
        let pa = sa.get(i);
        let pb = sb.get(i);
        match (pa, pb) {
            (Some(x), Some(y)) => {
                let ord = compare_segment(x, y);
                if ord != Ordering::Equal {
                    return ord;
                }
            }
            // 一方缺段：缺失段视为"零发布段"。存在的若是预发布（非数字）则更小，否则相等续比。
            (Some(x), None) => {
                if !is_numeric(x) {
                    // a 多出一个预发布段（如 1.0.0-rc 对 1.0.0）→ a 更小
                    return Ordering::Less;
                }
                if parse_numeric(x) != 0 {
                    return Ordering::Greater;
                }
            }
            (None, Some(y)) => {
                if !is_numeric(y) {
                    return Ordering::Greater;
                }
                if parse_numeric(y) != 0 {
                    return Ordering::Less;
                }
            }
            (None, None) => unreachable!(),
        }
    }
    Ordering::Equal
}

/// 把版本串按 `.` 与 `-` 拆为段序列（空段忽略）。
fn split_version(v: &str) -> Vec<&str> {
    v.split(['.', '-']).filter(|s| !s.is_empty()).collect()
}

/// 比较单段：两段皆数字按数值比；否则数字 < 非数字，非数字间按 ASCII 字典序比。
fn compare_segment(a: &str, b: &str) -> std::cmp::Ordering {
    use std::cmp::Ordering;
    match (is_numeric(a), is_numeric(b)) {
        (true, true) => parse_numeric(a).cmp(&parse_numeric(b)),
        // 数字段恒小于非数字段（预发布序：1.0.0-1 < 1.0.0-alpha 之类的次序此处不细分）
        (true, false) => Ordering::Less,
        (false, true) => Ordering::Greater,
        (false, false) => a.cmp(b),
    }
}

/// 段是否为纯数字。
fn is_numeric(s: &str) -> bool {
    !s.is_empty() && s.bytes().all(|b| b.is_ascii_digit())
}

/// 解析数字段为 u64；溢出时回退为 0（极端长串不参与有效比较，保守处理）。
fn parse_numeric(s: &str) -> u64 {
    s.parse().unwrap_or(0)
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::cmp::Ordering;

    /// 便捷：构造一条仅含 ranges 的受影响记录。
    fn 带范围(ranges: &str) -> AffectedRecord {
        AffectedRecord {
            ranges: Some(ranges.to_string()),
            versions: None,
        }
    }

    /// 便捷：构造一条仅含显式版本列表的受影响记录。
    fn 带版本(versions: &str) -> AffectedRecord {
        AffectedRecord {
            ranges: None,
            versions: Some(versions.to_string()),
        }
    }

    #[test]
    fn 版本比较_数值段按数值非字典序() {
        // 9 < 10（数值），而非 '9' > '1' 的字典序
        assert_eq!(compare_versions("2.9.0", "2.10.0"), Ordering::Less);
        assert_eq!(compare_versions("1.0.0", "1.0.0"), Ordering::Equal);
        assert_eq!(compare_versions("1.2.0", "1.1.9"), Ordering::Greater);
    }

    #[test]
    fn 版本比较_末尾零段等价() {
        assert_eq!(compare_versions("1.0", "1.0.0"), Ordering::Equal);
        assert_eq!(compare_versions("1.0.0", "1.0"), Ordering::Equal);
    }

    #[test]
    fn 版本比较_预发布段小于正式版() {
        // 1.0.0-rc 应小于 1.0.0
        assert_eq!(compare_versions("1.0.0-rc", "1.0.0"), Ordering::Less);
        assert_eq!(compare_versions("1.0.0", "1.0.0-rc"), Ordering::Greater);
        // SNAPSHOT 亦视为预发布（小于正式）
        assert_eq!(compare_versions("2.0-SNAPSHOT", "2.0"), Ordering::Less);
    }

    #[test]
    fn 范围_introduced_fixed_左含右不含() {
        // 受影响：[2.0, 2.17.1)
        let r = 带范围(
            r#"[{"type":"ECOSYSTEM","events":[{"introduced":"2.0"},{"fixed":"2.17.1"}]}]"#,
        );
        // 边界：起始版本受影响（左含）
        assert!(is_affected("2.0", &r));
        assert!(is_affected("2.14.1", &r));
        // 修复版本不受影响（右不含）
        assert!(!is_affected("2.17.1", &r));
        // 修复之后不受影响
        assert!(!is_affected("2.18.0", &r));
        // 起始之前不受影响
        assert!(!is_affected("1.9", &r));
    }

    #[test]
    fn 范围_last_affected_右含() {
        // 受影响：[1.0, 1.5] 闭区间
        let r = 带范围(
            r#"[{"type":"ECOSYSTEM","events":[{"introduced":"1.0"},{"last_affected":"1.5"}]}]"#,
        );
        assert!(is_affected("1.0", &r));
        assert!(is_affected("1.5", &r)); // last_affected 含
        assert!(!is_affected("1.6", &r));
        assert!(!is_affected("0.9", &r));
    }

    #[test]
    fn 范围_仅introduced_自起始起全受影响() {
        // 只有 introduced，无 fixed：自 1.0 起一律受影响
        let r = 带范围(r#"[{"type":"ECOSYSTEM","events":[{"introduced":"1.0"}]}]"#);
        assert!(is_affected("1.0", &r));
        assert!(is_affected("99.0", &r));
        assert!(!is_affected("0.9", &r));
    }

    #[test]
    fn 范围_introduced_0_表示自始() {
        // introduced "0" 表示从最早版本起
        let r =
            带范围(r#"[{"type":"ECOSYSTEM","events":[{"introduced":"0"},{"fixed":"1.0.0"}]}]"#);
        assert!(is_affected("0.1", &r));
        assert!(is_affected("0.9.9", &r));
        assert!(!is_affected("1.0.0", &r));
    }

    #[test]
    fn 显式版本列表命中() {
        let r = 带版本(r#"["2.14.0","2.14.1"]"#);
        assert!(is_affected("2.14.0", &r));
        assert!(is_affected("2.14.1", &r));
        assert!(!is_affected("2.15.0", &r));
    }

    #[test]
    fn ranges_与_versions_并存满足其一即命中() {
        let r = AffectedRecord {
            ranges: Some(
                r#"[{"type":"ECOSYSTEM","events":[{"introduced":"2.0"},{"fixed":"2.5"}]}]"#
                    .to_string(),
            ),
            versions: Some(r#"["9.9.9"]"#.to_string()),
        };
        // 落入范围
        assert!(is_affected("2.3", &r));
        // 不在范围但在显式列表
        assert!(is_affected("9.9.9", &r));
        // 两者都不命中
        assert!(!is_affected("8.0.0", &r));
    }

    #[test]
    fn 多区间任一命中即受影响() {
        // 两个独立受影响区间：[1.0,1.2) 与 [2.0,2.2)
        let r = 带范围(
            r#"[{"type":"ECOSYSTEM","events":[
                {"introduced":"1.0"},{"fixed":"1.2"},
                {"introduced":"2.0"},{"fixed":"2.2"}
            ]}]"#,
        );
        assert!(is_affected("1.1", &r));
        assert!(!is_affected("1.5", &r)); // 落在两区间之间
        assert!(is_affected("2.1", &r));
        assert!(!is_affected("2.2", &r));
    }

    #[test]
    fn 无范围无版本保守判不受影响() {
        let r = AffectedRecord {
            ranges: None,
            versions: None,
        };
        assert!(!is_affected("1.0.0", &r));
        // 空数组同理
        assert!(!is_affected("1.0.0", &带范围("[]")));
        assert!(!is_affected("1.0.0", &带版本("[]")));
    }

    #[test]
    fn git_类型范围跳过不误判() {
        // GIT 范围以提交号定界，无可比版本号，不据此判受影响
        let r = 带范围(r#"[{"type":"GIT","events":[{"introduced":"0"},{"fixed":"abc123"}]}]"#);
        assert!(!is_affected("1.0.0", &r));
    }

    #[test]
    fn 坏_json_不命中不panic() {
        let r = AffectedRecord {
            ranges: Some("不是json".to_string()),
            versions: Some("也不是".to_string()),
        };
        assert!(!is_affected("1.0.0", &r));
    }
}
