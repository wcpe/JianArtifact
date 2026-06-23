//! JianArtifact 库 crate：地基能力的模块集合。
//!
//! 二进制入口（`src/main.rs`）只做启动编排，业务能力均下沉到本库的各模块，
//! 便于单元 / 集成测试直接复用。模块依赖方向单向无环（见 ARCHITECTURE）：
//! `api` → (`auth` / `meta` / `storage`) → `config`。
#![forbid(unsafe_code)]

pub mod api;
pub mod auth;
pub mod config;
pub mod meta;
pub mod storage;
