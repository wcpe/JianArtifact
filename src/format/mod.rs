//! 格式处理层（FR-14~17）：定义统一的 [`Format`] trait 与按格式名注册的 [`FormatRegistry`]，
//! 各格式处理器只负责自身协议，经 trait 多态接入——**严禁用 if-else / switch 按格式名堆逻辑**。
//!
//! 本批先把通用机理与 trait 做扎实，并以 Raw 作为首个 trait 实现端到端验证机理；
//! Maven / npm / Docker 等其余格式在后续批次按同一 trait 接入，不在本模块堆叠。
//!
//! 依赖方向（ARCHITECTURE §2）：`api` → `format` → (`storage` / `meta` / `proxy`)，单向无环。

use crate::meta::ArtifactRecord;

mod browse;
mod cargo;
pub mod docker;
pub mod docker_registry;
mod go_mod;
mod maven;
mod npm;
mod nuget;
mod pypi;
mod raw;
pub mod service;

pub use browse::{collapse_directory_entries, DirEntry, DirEntryKind};
pub use cargo::{CargoError, CargoFormat, CargoPublishRequest};
pub use docker::DockerFormat;
pub use docker_registry::{DockerError, DockerRegistry};
pub use go_mod::{GoError, GoFormat, GoRequest, VersionFile};
pub use maven::{Gav, MavenFormat, MavenVersions, SnapshotBuild, SnapshotBuilds};
pub use npm::{NpmError, NpmFormat, PublishRequest};
pub use nuget::{NuGetError, NuGetFormat, PackageIdentity};
pub use pypi::{
    MultipartField, PypiError, PypiFormat, UploadRequest, PACKAGES_PREFIX as PYPI_PACKAGES_PREFIX,
    PEP691_CONTENT_TYPE as PYPI_PEP691_CONTENT_TYPE, SIMPLE_SEGMENT as PYPI_SIMPLE_SEGMENT,
};
pub use raw::RawFormat;
pub use service::{ArtifactKind, ArtifactService, ServiceError};

// VulnCoordinate / Format::vuln_coordinate 在本模块内定义并对外公开，供 api 层做坐标级漏洞匹配。

/// 制品在仓库内的定位坐标：由格式把请求路径解析而来，或用于反解为存储路径。
///
/// 当前四格式均以"仓库内相对路径"作为制品键，故坐标即归一化后的路径；
/// 该结构保留为格式扩展点（如 Maven 可在内部据路径解出 GAV，但对外键仍是路径）。
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ArtifactCoordinates {
    /// 仓库内制品路径（已归一化：去除首尾多余斜杠、不含 `..`）。
    pub path: String,
}

/// 路径解析错误：请求路径不合法（越权穿越 / 为空等）。
#[derive(Debug, thiserror::Error, PartialEq, Eq)]
pub enum PathError {
    /// 路径为空或仅由分隔符构成。
    #[error("制品路径不能为空")]
    Empty,
    /// 路径含非法分段（`.` / `..`），可能用于目录穿越。
    #[error("制品路径含非法分段")]
    Traversal,
}

/// 制品的生态坐标三元组（FR-71）：由格式从制品路径反解，用于本地漏洞库坐标级匹配。
///
/// `ecosystem` 与 OSV `package.ecosystem` 对齐（如 `Maven` / `npm`），`package` 为该生态的包坐标名
/// （Maven 为 `group:artifact`，npm 为包名），`version` 为版本号。无标准坐标的格式（如 Raw / Docker）
/// 不产出本坐标（`vuln_coordinate` 返回 None），其制品不参与坐标级漏洞匹配。
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct VulnCoordinate {
    /// 生态名（与 OSV `package.ecosystem` 对齐）。
    pub ecosystem: String,
    /// 包坐标名（Maven `group:artifact`、npm 包名）。
    pub package: String,
    /// 版本号。
    pub version: String,
}

/// 使用方式片段（FR-68）：详情页按格式生成的获取与接入示例。
#[derive(Debug, Clone, PartialEq, Eq, serde::Serialize)]
pub struct UsageSnippet {
    /// 片段标题（如"下载"/"依赖坐标"）。
    pub title: String,
    /// 片段语言 / 类型（如 `bash` / `xml`），供前端高亮。
    pub language: String,
    /// 片段正文。
    pub content: String,
}

/// 统一格式 trait：每种格式声明自身的路径映射、覆盖策略、使用片段与内容类型推断。
///
/// 通用制品机理（[`ArtifactService`]）只依赖本 trait，不感知具体格式；新增格式 = 新增实现 +
/// 注册，符合开闭原则，杜绝按格式名分支的可变逻辑堆叠。
pub trait Format: Send + Sync {
    /// 格式名（小写，与仓库 `format` 字段、注册表键一致）。
    fn name(&self) -> &'static str;

    /// 把请求中的"仓库内路径"解析并归一化为制品坐标。
    ///
    /// 须拒绝目录穿越（含 `..` / `.` 分段）与空路径，杜绝越权读写仓库存储区之外。
    fn parse_path(&self, raw_path: &str) -> Result<ArtifactCoordinates, PathError>;

    /// 覆盖策略：在已存在同坐标制品 `existing` 时，是否允许本次上传覆盖。
    ///
    /// 由各格式按原生语义实现（Raw 允许覆盖；Maven release 不可覆盖等留各自实现）。
    fn can_overwrite(&self, existing: &ArtifactRecord) -> bool;

    /// 据制品坐标推断内容类型（Content-Type）；无法判断时返回 None。
    fn content_type(&self, coords: &ArtifactCoordinates) -> Option<String>;

    /// 生成使用方式片段：据对外基础地址、仓库名与制品路径产出获取 / 接入示例。
    fn usage_snippets(
        &self,
        public_base_url: &str,
        repo_name: &str,
        coords: &ArtifactCoordinates,
    ) -> Vec<UsageSnippet>;

    /// 从制品坐标反解生态坐标三元组，供本地漏洞库坐标级匹配（FR-71）。
    ///
    /// 默认返回 None——无标准生态坐标的格式（Raw / Docker 等）不参与坐标级匹配。
    /// 有坐标的格式（Maven / npm）覆写本方法，从仓库内路径反解 `(ecosystem, package, version)`。
    fn vuln_coordinate(&self, _coords: &ArtifactCoordinates) -> Option<VulnCoordinate> {
        None
    }
}

/// 按格式名注册的格式注册表：通用机理据仓库 `format` 字段查得对应处理器。
///
/// 用静态分发的实现集合 + 名称匹配查找，不在业务路径上按格式名写 if-else。
#[derive(Default)]
pub struct FormatRegistry {
    /// 已注册的格式处理器集合。
    formats: Vec<Box<dyn Format>>,
}

impl FormatRegistry {
    /// 构造空注册表。
    pub fn new() -> Self {
        Self {
            formats: Vec::new(),
        }
    }

    /// 注册一个格式处理器（按其 `name()` 索引）。
    pub fn register(&mut self, format: Box<dyn Format>) {
        self.formats.push(format);
    }

    /// 构造含当前已实现格式（Raw、Maven、npm、Docker、Go、Cargo、PyPI、NuGet）的注册表。
    ///
    /// 其余格式由各自批次实现后在此注册，本批不提前占位未实现格式。
    pub fn with_builtin() -> Self {
        let mut registry = Self::new();
        registry.register(Box::new(RawFormat));
        registry.register(Box::new(MavenFormat));
        registry.register(Box::new(NpmFormat));
        registry.register(Box::new(DockerFormat));
        registry.register(Box::new(GoFormat));
        registry.register(Box::new(CargoFormat));
        registry.register(Box::new(PypiFormat));
        registry.register(Box::new(NuGetFormat));
        registry
    }

    /// 按格式名查处理器；未注册时返回 None。
    pub fn get(&self, name: &str) -> Option<&dyn Format> {
        self.formats
            .iter()
            .find(|f| f.name() == name)
            .map(|f| f.as_ref())
    }
}

/// 归一化仓库内路径并拒绝穿越：去除空段，禁止 `.` / `..`。
///
/// 各格式的 `parse_path` 可复用本函数作为基础校验，保证存储键始终落在仓库存储区内。
pub(crate) fn normalize_repo_path(raw_path: &str) -> Result<String, PathError> {
    let mut segments = Vec::new();
    for seg in raw_path.split('/') {
        if seg.is_empty() {
            // 跳过空段（来自首尾或连续斜杠），不计入路径
            continue;
        }
        if seg == "." || seg == ".." {
            // 任何当前 / 上级目录分段都视为穿越企图，直接拒绝
            return Err(PathError::Traversal);
        }
        segments.push(seg);
    }
    if segments.is_empty() {
        return Err(PathError::Empty);
    }
    Ok(segments.join("/"))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn 归一化去除多余斜杠() {
        assert_eq!(normalize_repo_path("a/b/c.txt").unwrap(), "a/b/c.txt");
        assert_eq!(normalize_repo_path("/a//b/").unwrap(), "a/b");
    }

    #[test]
    fn 归一化拒绝空路径与穿越() {
        assert_eq!(normalize_repo_path(""), Err(PathError::Empty));
        assert_eq!(normalize_repo_path("///"), Err(PathError::Empty));
        assert_eq!(normalize_repo_path("a/../b"), Err(PathError::Traversal));
        assert_eq!(normalize_repo_path("./a"), Err(PathError::Traversal));
        assert_eq!(normalize_repo_path("a/.."), Err(PathError::Traversal));
    }

    #[test]
    fn 注册表按名查得且未知返回none() {
        let registry = FormatRegistry::with_builtin();
        assert!(registry.get("raw").is_some());
        assert_eq!(registry.get("raw").unwrap().name(), "raw");
        // Maven、npm、Docker 已实现并注册，应查得
        assert!(registry.get("maven").is_some());
        assert_eq!(registry.get("maven").unwrap().name(), "maven");
        assert!(registry.get("npm").is_some());
        assert_eq!(registry.get("npm").unwrap().name(), "npm");
        assert!(registry.get("docker").is_some());
        assert_eq!(registry.get("docker").unwrap().name(), "docker");
        assert!(registry.get("go").is_some());
        assert_eq!(registry.get("go").unwrap().name(), "go");
        // Cargo 已实现并注册，应查得
        assert!(registry.get("cargo").is_some());
        assert_eq!(registry.get("cargo").unwrap().name(), "cargo");
        assert!(registry.get("pypi").is_some());
        assert_eq!(registry.get("pypi").unwrap().name(), "pypi");
        assert!(registry.get("nuget").is_some());
        assert_eq!(registry.get("nuget").unwrap().name(), "nuget");
        assert!(registry.get("不存在").is_none());
    }
}
