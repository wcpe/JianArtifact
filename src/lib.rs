//! JianArtifact 库 crate：地基能力的模块集合。
//!
//! 二进制入口（`src/main.rs`）只做启动编排，业务能力均下沉到本库的各模块，
//! 便于单元 / 集成测试直接复用。模块依赖方向单向无环（见 ARCHITECTURE）：
//! `api` → (`auth` / `authz` / `repo` / `format`) → (`proxy` / `storage` / `meta`) → `config`，
//! 其中 `format` 可依赖 `storage` / `meta` / `proxy`。`auth` 承载认证（口令 / JWT / API Token /
//! Basic / 登录防护），`authz` 承载鉴权（仓库读写判定纯函数），`repo` 承载仓库领域模型与生命周期，
//! `format` 承载统一格式 trait 与通用制品机理（写入 / 读取 / 删除），`proxy` 承载上游代理与单飞缓存，
//! `vuln` 承载漏洞库离线镜像（定期下载公开漏洞数据落本地库，依赖 `meta` / `config`，坐标不外发）。
#![forbid(unsafe_code)]

pub mod api;
pub mod auth;
pub mod authz;
pub mod config;
pub mod format;
pub mod meta;
pub mod metrics_keys;
pub mod migrate;
pub mod proxy;
pub mod repo;
pub mod storage;
pub mod update;
pub mod vuln;
pub mod web;
