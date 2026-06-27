//! 开源许可清单（FR-102，ADR-0025）。
//!
//! 本产品把众多 Rust crate 与前端 npm 包打进单一二进制分发，多数开源许可要求分发时附带归因
//! （作者）与许可证信息。本模块持有**构建期扫描生成、编译期嵌入**的许可清单，运行时只读、
//! 绝不外发（守 ADR-0009 数据不外发基调）。
//!
//! 设计要点：
//! - **静态嵌入资源、不碰 DB / 网络**：清单经 `include_str!` 在编译期嵌入（产物由
//!   `scripts/gen-licenses.mjs` 生成），运行时惰性解析为 DTO；本模块不依赖 `meta` / `config`，
//!   定位同 `monitor`（仅做数据组装、不碰元数据真源）。
//! - **优雅降级**：本地开发未跑生成脚本时，嵌入的是占位 JSON（`generated=false`、空清单）；
//!   解析失败同样降级为空清单 + `generated=false`，绝不 panic、不阻断启动。
//! - **纯解析可测**：把「JSON 文本 → 清单」的解析与降级抽为无副作用纯函数（`parse`），
//!   便于穷举单测；惰性缓存交 `embedded()`。

use std::sync::OnceLock;

use serde::{Deserialize, Serialize};

/// 编译期嵌入的许可清单 JSON（由 `scripts/gen-licenses.mjs` 生成、覆盖占位）。
///
/// 干净检出 / 本地未生成时为占位（`generated=false`、空 entries），保证 `include_str!` 恒可编译。
const EMBEDDED_JSON: &str = include_str!("data.generated.json");

/// 依赖类别：运行时依赖 / 开发依赖。
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum Kind {
    /// 运行时依赖（随产物分发，归因义务最重）。
    Runtime,
    /// 开发依赖（仅构建 / 测试期使用）。
    Dev,
}

/// 依赖来源生态：Rust crate / 前端 npm 包。
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum Source {
    /// Rust crate（cargo-about / cargo metadata 扫描）。
    Rust,
    /// 前端 npm 包（pnpm licenses 扫描）。
    Frontend,
}

/// 单条依赖的许可归因。
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct LicenseEntry {
    /// 包名。
    pub name: String,
    /// 版本号。
    pub version: String,
    /// 许可证（SPDX 表达式，可能为多许可如 `MIT OR Apache-2.0`）。
    pub license: String,
    /// 作者 / 版权方（可能为空字符串）。
    pub author: String,
    /// 运行时 / 开发依赖。
    pub kind: Kind,
    /// 来源生态（rust / frontend）。
    pub source: Source,
}

/// 清单汇总（供前端统计卡）。
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct LicenseSummary {
    /// 依赖总数。
    pub total: usize,
    /// 运行时依赖数。
    pub runtime: usize,
    /// 开发依赖数。
    pub dev: usize,
    /// 许可证种类数（去重后的 license 表达式个数）。
    pub licenses: usize,
}

/// 对外许可清单 DTO。
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct LicenseManifest {
    /// 是否已由构建期脚本生成；`false` 表示占位 / 未生成，前端显空态。
    pub generated: bool,
    /// 逐条依赖归因。
    pub entries: Vec<LicenseEntry>,
    /// 汇总统计。
    pub summary: LicenseSummary,
}

impl LicenseManifest {
    /// 空清单（未生成 / 降级）：`generated=false`、无条目、汇总全零。
    fn empty() -> Self {
        LicenseManifest {
            generated: false,
            entries: Vec::new(),
            summary: LicenseSummary {
                total: 0,
                runtime: 0,
                dev: 0,
                licenses: 0,
            },
        }
    }
}

/// 纯解析：把许可清单 JSON 文本解析为 DTO；解析失败降级为空清单（不 panic）。
///
/// 无副作用，便于穷举单测（合法 / 非法 / 占位三类输入）。
pub fn parse(json: &str) -> LicenseManifest {
    match serde_json::from_str::<LicenseManifest>(json) {
        Ok(manifest) => manifest,
        Err(err) => {
            // 嵌入数据损坏属构建期问题；运行时降级为空清单并记 WARN，不阻断服务
            tracing::warn!(错误 = %err, "开源许可清单解析失败，降级为空清单");
            LicenseManifest::empty()
        }
    }
}

/// 取编译期嵌入的许可清单（惰性解析一次后缓存）。
pub fn embedded() -> &'static LicenseManifest {
    static MANIFEST: OnceLock<LicenseManifest> = OnceLock::new();
    MANIFEST.get_or_init(|| parse(EMBEDDED_JSON))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn 解析合法清单返回条目与汇总() {
        let json = r#"{
            "generated": true,
            "entries": [
                {"name":"serde","version":"1.0.0","license":"MIT OR Apache-2.0","author":"dtolnay","kind":"runtime","source":"rust"},
                {"name":"vitest","version":"3.0.0","license":"MIT","author":"Anthony Fu","kind":"dev","source":"frontend"}
            ],
            "summary": {"total":2,"runtime":1,"dev":1,"licenses":2}
        }"#;
        let m = parse(json);
        assert!(m.generated);
        assert_eq!(m.entries.len(), 2);
        assert_eq!(m.entries[0].name, "serde");
        assert_eq!(m.entries[0].kind, Kind::Runtime);
        assert_eq!(m.entries[0].source, Source::Rust);
        assert_eq!(m.entries[1].kind, Kind::Dev);
        assert_eq!(m.entries[1].source, Source::Frontend);
        assert_eq!(m.summary.total, 2);
        assert_eq!(m.summary.licenses, 2);
    }

    #[test]
    fn 解析非法_json_降级为空清单() {
        let m = parse("{ this is not json");
        assert!(!m.generated);
        assert!(m.entries.is_empty());
        assert_eq!(m.summary.total, 0);
    }

    #[test]
    fn 解析占位清单为未生成空态() {
        // 与仓库内提交的占位 data.generated.json 一致
        let json = r#"{"generated":false,"entries":[],"summary":{"total":0,"runtime":0,"dev":0,"licenses":0}}"#;
        let m = parse(json);
        assert!(!m.generated);
        assert!(m.entries.is_empty());
    }

    #[test]
    fn 嵌入清单可被解析不_panic() {
        // 嵌入的占位 / 真实清单都应能解析（不 panic）；占位时 generated=false
        let m = embedded();
        // 结构自洽：summary.total 与 entries 数对齐（生成脚本保证；占位时均为 0）
        assert_eq!(m.summary.total, m.entries.len());
    }
}
