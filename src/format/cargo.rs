//! Cargo 稀疏索引格式（FR-26，hosted + proxy）：按 Cargo sparse registry 协议接入。
//!
//! 作为统一 [`Format`] trait 的实现，复用通用制品机理（[`super::ArtifactService`]）的
//! 存储 / 代理 / 四校验和机理，本模块只负责 Cargo 自身协议：稀疏索引文件（每版本一行 JSON）
//! 的生成 / 合并、按包名长度分目录的索引路径映射、`config.json` 生成、publish 二进制体解析、
//! 以及 yank / unyank 标记翻转。不在此重造存储 / 代理 / 校验和。
//!
//! 存储约定（仓库内路径）：
//! - 索引文件存于 `index/{index_path}`（如 `index/se/rd/serde`），随发布追加更新（可覆盖）。
//! - `.crate` blob 存于 `crates/{name}/{name}-{vers}.crate`，已发布不可覆盖。
//! - `config.json` 不落存储，按请求动态生成（依赖对外基址，避免与配置双真源）。

use serde_json::{json, Map, Value};

use crate::meta::ArtifactRecord;

use super::{normalize_repo_path, ArtifactCoordinates, Format, PathError, UsageSnippet};

/// 索引文件在仓库内的存储前缀目录。
const INDEX_PREFIX: &str = "index";
/// `.crate` 本体在仓库内的存储前缀目录。
const CRATES_PREFIX: &str = "crates";
/// 索引 / config.json 的内容类型。
const JSON_CONTENT_TYPE: &str = "application/json";
/// `.crate` 本体的内容类型。
const CRATE_CONTENT_TYPE: &str = "application/octet-stream";

/// Cargo 格式处理器：按存储路径区分索引文件与 `.crate` 本体，覆盖策略据此判定。
pub struct CargoFormat;

/// Cargo 协议错误：publish 体不合法 / 版本已存在 / 版本不存在等。
#[derive(Debug, thiserror::Error, PartialEq, Eq)]
pub enum CargoError {
    /// publish 请求体结构不合法（长度前缀越界 / JSON 解析失败 / 缺字段等）。
    #[error("cargo 发布请求体不合法: {0}")]
    InvalidBody(String),
    /// 该版本已发布，按 Cargo 语义不可覆盖（FR-61）。
    #[error("版本 {0} 已发布，不可覆盖")]
    VersionExists(String),
    /// yank / unyank 时目标版本在索引中不存在。
    #[error("版本 {0} 不存在")]
    VersionNotFound(String),
}

/// 从 Cargo publish 二进制体解析出的单次发布内容。
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct CargoPublishRequest {
    /// 包名（crate name）。
    pub name: String,
    /// 本次发布的版本号。
    pub vers: String,
    /// publish metadata JSON（含 deps / features 等，用于生成索引行）。
    pub metadata: Value,
    /// `.crate` 原始字节。
    pub crate_bytes: Vec<u8>,
}

impl CargoFormat {
    /// 据包名计算稀疏索引文件相对路径（不含 `index/` 前缀），按 Cargo 规范分目录：
    /// 1 字符 → `1/{name}`；2 字符 → `2/{name}`；3 字符 → `3/{name[0]}/{name}`；
    /// ≥4 字符 → `{name[0..2]}/{name[2..4]}/{name}`。包名按 Cargo 规范统一小写比较。
    pub fn index_path(name: &str) -> String {
        // Cargo 索引以小写包名分目录（包名本身大小写不敏感地映射到同一索引路径）
        let lower = name.to_ascii_lowercase();
        let chars: Vec<char> = lower.chars().collect();
        match chars.len() {
            0 => lower,
            1 => format!("1/{lower}"),
            2 => format!("2/{lower}"),
            3 => format!("3/{}/{}", chars[0], lower),
            _ => {
                let a: String = chars[0..2].iter().collect();
                let b: String = chars[2..4].iter().collect();
                format!("{a}/{b}/{lower}")
            }
        }
    }

    /// 索引文件在仓库内的完整存储路径（含 `index/` 前缀）。
    pub fn index_storage_path(name: &str) -> String {
        format!("{INDEX_PREFIX}/{}", Self::index_path(name))
    }

    /// `.crate` 本体在仓库内的存储路径：`crates/{name}/{name}-{vers}.crate`。
    pub fn crate_storage_path(name: &str, vers: &str) -> String {
        format!("{CRATES_PREFIX}/{name}/{name}-{vers}.crate")
    }

    /// 解析 Cargo publish 二进制体：`[4 字节 LE json 长度][metadata JSON][4 字节 LE crate 长度][.crate]`。
    ///
    /// 不修改 `.crate` 字节内容（原样落盘、摘要由存储层算）。
    pub fn parse_publish(body: &[u8]) -> Result<CargoPublishRequest, CargoError> {
        // ① 读 metadata JSON 长度前缀（4 字节小端）
        let (json_len, rest) = read_u32_le_prefixed(body)
            .ok_or_else(|| CargoError::InvalidBody("metadata 长度前缀越界".to_string()))?;
        let metadata: Value = serde_json::from_slice(json_len)
            .map_err(|e| CargoError::InvalidBody(format!("metadata JSON 解析失败: {e}")))?;

        // ② 读 .crate 字节长度前缀（4 字节小端）
        let (crate_bytes, _tail) = read_u32_le_prefixed(rest)
            .ok_or_else(|| CargoError::InvalidBody(".crate 长度前缀越界".to_string()))?;

        // ③ 从 metadata 取必需的 name / vers
        let name = metadata
            .get("name")
            .and_then(Value::as_str)
            .ok_or_else(|| CargoError::InvalidBody("metadata 缺少 name".to_string()))?
            .to_string();
        let vers = metadata
            .get("vers")
            .and_then(Value::as_str)
            .ok_or_else(|| CargoError::InvalidBody("metadata 缺少 vers".to_string()))?
            .to_string();

        Ok(CargoPublishRequest {
            name,
            vers,
            metadata,
            crate_bytes: crate_bytes.to_vec(),
        })
    }

    /// 据 publish metadata 与 `.crate` 的 sha256 生成一行索引 JSON（紧凑、无换行）。
    ///
    /// 字段对齐 Cargo 索引：`name`/`vers`/`deps`/`cksum`/`features`/`yanked=false`，
    /// 并按需透传 `links`/`features2`/`rust_version`。deps 从 metadata 的 `deps` 数组转换
    /// （metadata 用 `version_req`，索引用 `req`；metadata 用 `explicit_name_in_toml`，索引重命名规则相应处理）。
    pub fn index_line(req: &CargoPublishRequest, cksum: &str) -> Result<String, CargoError> {
        let meta = req
            .metadata
            .as_object()
            .ok_or_else(|| CargoError::InvalidBody("metadata 不是对象".to_string()))?;

        let deps = meta
            .get("deps")
            .and_then(Value::as_array)
            .map(|arr| arr.iter().map(convert_dep).collect::<Vec<_>>())
            .unwrap_or_default();

        let features = meta
            .get("features")
            .cloned()
            .unwrap_or_else(|| Value::Object(Map::new()));

        let mut line = Map::new();
        line.insert("name".to_string(), Value::String(req.name.clone()));
        line.insert("vers".to_string(), Value::String(req.vers.clone()));
        line.insert("deps".to_string(), Value::Array(deps));
        line.insert("cksum".to_string(), Value::String(cksum.to_string()));
        line.insert("features".to_string(), features);
        line.insert("yanked".to_string(), Value::Bool(false));

        // 透传可选字段（存在才写，保持与 Cargo 索引一致）
        for key in ["links", "rust_version"] {
            if let Some(v) = meta.get(key) {
                if !v.is_null() {
                    line.insert(key.to_string(), v.clone());
                }
            }
        }

        serde_json::to_string(&Value::Object(line))
            .map_err(|e| CargoError::InvalidBody(format!("索引行序列化失败: {e}")))
    }

    /// 把新版本索引行合并进既有索引文件内容（每行一个版本）。
    ///
    /// `existing` 为既有索引文件字节（首次发布传空切片）；同 `vers` 已存在返回
    /// [`CargoError::VersionExists`]（Cargo 已发布不可覆盖，FR-61）。返回合并后的索引字节
    /// （行间以 `\n` 分隔、末尾带换行）。
    pub fn merge_index(existing: &[u8], new_line: &str, vers: &str) -> Result<Vec<u8>, CargoError> {
        let mut lines = parse_index_lines(existing)?;
        // 已发布不可覆盖：扫描既有行的 vers
        for (_, parsed) in &lines {
            if parsed.get("vers").and_then(Value::as_str) == Some(vers) {
                return Err(CargoError::VersionExists(vers.to_string()));
            }
        }
        lines.push((
            new_line.to_string(),
            serde_json::from_str(new_line).unwrap(),
        ));
        Ok(render_index_lines(&lines))
    }

    /// 翻转指定版本索引行的 `yanked` 字段；版本不存在返回 [`CargoError::VersionNotFound`]。
    ///
    /// 返回更新后的索引文件字节。
    pub fn set_yanked(existing: &[u8], vers: &str, yanked: bool) -> Result<Vec<u8>, CargoError> {
        let mut lines = parse_index_lines(existing)?;
        let mut found = false;
        for (text, parsed) in &mut lines {
            if parsed.get("vers").and_then(Value::as_str) == Some(vers) {
                if let Some(obj) = parsed.as_object_mut() {
                    obj.insert("yanked".to_string(), Value::Bool(yanked));
                }
                // 重序列化该行以反映翻转后的 yanked
                *text = serde_json::to_string(parsed)
                    .map_err(|e| CargoError::InvalidBody(format!("索引行序列化失败: {e}")))?;
                found = true;
            }
        }
        if !found {
            return Err(CargoError::VersionNotFound(vers.to_string()));
        }
        Ok(render_index_lines(&lines))
    }

    /// 生成 registry `config.json` 字节：把下载与 API 都指回本仓库。
    ///
    /// `dl` 指向本仓库的下载 API，`api` 指向本仓库根（用于 publish / yank）。
    pub fn config_json(public_base_url: &str, repo_name: &str) -> Vec<u8> {
        let base = public_base_url.trim_end_matches('/');
        let doc = json!({
            "dl": format!("{base}/{repo_name}/api/v1/crates"),
            "api": format!("{base}/{repo_name}"),
        });
        // config.json 由我们生成、结构固定，序列化不会失败
        serde_json::to_vec(&doc).expect("config.json 序列化")
    }
}

/// 把 publish metadata 的一条 dep 转换为索引格式的 dep。
///
/// metadata：`{name, version_req, features, optional, default_features, target, kind, registry,
/// explicit_name_in_toml}`；索引：`{name, req, features, optional, default_features, target,
/// kind, registry, package}`（重命名包用 `package` 记原名、`name` 记 toml 中的别名）。
fn convert_dep(dep: &Value) -> Value {
    let Some(obj) = dep.as_object() else {
        return dep.clone();
    };
    let mut out = Map::new();

    // 处理重命名：explicit_name_in_toml 存在则 name=别名、package=原 name
    let orig_name = obj.get("name").and_then(Value::as_str).unwrap_or_default();
    match obj.get("explicit_name_in_toml").and_then(Value::as_str) {
        Some(alias) if !alias.is_empty() => {
            out.insert("name".to_string(), Value::String(alias.to_string()));
            out.insert("package".to_string(), Value::String(orig_name.to_string()));
        }
        _ => {
            out.insert("name".to_string(), Value::String(orig_name.to_string()));
        }
    }

    // version_req → req
    if let Some(req) = obj.get("version_req") {
        out.insert("req".to_string(), req.clone());
    }
    // 直接透传的同名字段
    for key in [
        "features",
        "optional",
        "default_features",
        "target",
        "kind",
        "registry",
    ] {
        if let Some(v) = obj.get(key) {
            out.insert(key.to_string(), v.clone());
        }
    }
    Value::Object(out)
}

/// 解析 4 字节小端长度前缀及其后随数据；返回 (数据切片, 余下切片)，越界返回 None。
fn read_u32_le_prefixed(buf: &[u8]) -> Option<(&[u8], &[u8])> {
    if buf.len() < 4 {
        return None;
    }
    let len = u32::from_le_bytes([buf[0], buf[1], buf[2], buf[3]]) as usize;
    let rest = &buf[4..];
    if rest.len() < len {
        return None;
    }
    Some((&rest[..len], &rest[len..]))
}

/// 解析索引文件字节为 (原始行文本, 已解析 JSON) 列表；忽略空行，非法行返回错误。
fn parse_index_lines(existing: &[u8]) -> Result<Vec<(String, Value)>, CargoError> {
    let text = std::str::from_utf8(existing)
        .map_err(|_| CargoError::InvalidBody("索引文件非 UTF-8".to_string()))?;
    let mut out = Vec::new();
    for line in text.lines() {
        let trimmed = line.trim();
        if trimmed.is_empty() {
            continue;
        }
        let parsed: Value = serde_json::from_str(trimmed)
            .map_err(|e| CargoError::InvalidBody(format!("既有索引行解析失败: {e}")))?;
        out.push((trimmed.to_string(), parsed));
    }
    Ok(out)
}

/// 把索引行列表渲染为字节（行间 `\n`、末尾带换行）。
fn render_index_lines(lines: &[(String, Value)]) -> Vec<u8> {
    let mut out = String::new();
    for (text, _) in lines {
        out.push_str(text);
        out.push('\n');
    }
    out.into_bytes()
}

impl Format for CargoFormat {
    fn name(&self) -> &'static str {
        "cargo"
    }

    fn parse_path(&self, raw_path: &str) -> Result<ArtifactCoordinates, PathError> {
        // Cargo 以归一化后的仓库内路径作为制品键（索引 index/...、本体 crates/...）
        let path = normalize_repo_path(raw_path)?;
        Ok(ArtifactCoordinates { path })
    }

    fn can_overwrite(&self, existing: &ArtifactRecord) -> bool {
        // 索引文件（index/ 前缀）随发布追加更新 → 可覆盖；
        // .crate 本体（crates/ 前缀）已发布不可覆盖。
        existing.path.starts_with(&format!("{INDEX_PREFIX}/"))
    }

    fn content_type(&self, coords: &ArtifactCoordinates) -> Option<String> {
        // .crate 本体按 octet-stream；索引文件按 json
        if coords.path.starts_with(&format!("{CRATES_PREFIX}/")) {
            Some(CRATE_CONTENT_TYPE.to_string())
        } else {
            Some(JSON_CONTENT_TYPE.to_string())
        }
    }

    fn usage_snippets(
        &self,
        public_base_url: &str,
        repo_name: &str,
        coords: &ArtifactCoordinates,
    ) -> Vec<UsageSnippet> {
        let base = public_base_url.trim_end_matches('/');
        // 从存储路径反推包名：本体路径形如 crates/{name}/{name}-{vers}.crate
        let package = coords
            .path
            .strip_prefix(&format!("{CRATES_PREFIX}/"))
            .and_then(|rest| rest.split('/').next())
            .map(str::to_string)
            .unwrap_or_else(|| repo_name.to_string());
        let index_url = format!("sparse+{base}/{repo_name}/");
        vec![
            UsageSnippet {
                title: "添加依赖".to_string(),
                language: "bash".to_string(),
                content: format!("cargo add {package} --registry {repo_name}"),
            },
            UsageSnippet {
                title: "仓库接入（.cargo/config.toml）".to_string(),
                language: "toml".to_string(),
                // 仅示意接入位，凭据用 cargo login 占位，不写真实 Token
                content: format!(
                    "[registries.{repo_name}]\nindex = \"{index_url}\"\n# 鉴权：cargo login --registry {repo_name} <你的-API-Token>"
                ),
            },
        ]
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// 构造一条最小制品记录，仅用于覆盖策略 / 内容类型判定。
    fn 记录(path: &str) -> ArtifactRecord {
        ArtifactRecord {
            id: "id".to_string(),
            repo_id: "r".to_string(),
            path: path.to_string(),
            size: 1,
            sha256: "s".to_string(),
            sha1: "s".to_string(),
            md5: "s".to_string(),
            sha512: "s".to_string(),
            content_type: None,
            cached: 0,
            created_at: "now".to_string(),
        }
    }

    /// 构造一份 Cargo publish 二进制体：长度前缀 + metadata + 长度前缀 + crate 字节。
    fn 发布体(metadata: &Value, crate_bytes: &[u8]) -> Vec<u8> {
        let json = serde_json::to_vec(metadata).unwrap();
        let mut body = Vec::new();
        body.extend_from_slice(&(json.len() as u32).to_le_bytes());
        body.extend_from_slice(&json);
        body.extend_from_slice(&(crate_bytes.len() as u32).to_le_bytes());
        body.extend_from_slice(crate_bytes);
        body
    }

    #[test]
    fn 名称为_cargo() {
        assert_eq!(CargoFormat.name(), "cargo");
    }

    #[test]
    fn 索引路径按包名长度分目录() {
        assert_eq!(CargoFormat::index_path("a"), "1/a");
        assert_eq!(CargoFormat::index_path("ab"), "2/ab");
        assert_eq!(CargoFormat::index_path("abc"), "3/a/abc");
        assert_eq!(CargoFormat::index_path("serde"), "se/rd/serde");
        assert_eq!(CargoFormat::index_path("tokio"), "to/ki/tokio");
        // 大写包名归一化为小写索引路径
        assert_eq!(CargoFormat::index_path("MyCrate"), "my/cr/mycrate");
    }

    #[test]
    fn crate_存储路径含版本() {
        assert_eq!(
            CargoFormat::crate_storage_path("serde", "1.0.0"),
            "crates/serde/serde-1.0.0.crate"
        );
    }

    #[test]
    fn 解析发布体提取_name_vers_与_crate() {
        let meta = json!({ "name": "serde", "vers": "1.0.0", "deps": [], "features": {} });
        let body = 发布体(&meta, b"CRATE-BYTES");
        let req = CargoFormat::parse_publish(&body).unwrap();
        assert_eq!(req.name, "serde");
        assert_eq!(req.vers, "1.0.0");
        assert_eq!(req.crate_bytes, b"CRATE-BYTES");
    }

    #[test]
    fn 解析发布体长度前缀越界报错() {
        // 声明 json 长度 100 但实际无那么多字节
        let mut body = Vec::new();
        body.extend_from_slice(&100u32.to_le_bytes());
        body.extend_from_slice(b"short");
        assert!(matches!(
            CargoFormat::parse_publish(&body),
            Err(CargoError::InvalidBody(_))
        ));
    }

    #[test]
    fn 解析发布体缺字段报错() {
        let meta = json!({ "vers": "1.0.0" }); // 缺 name
        let body = 发布体(&meta, b"x");
        assert!(matches!(
            CargoFormat::parse_publish(&body),
            Err(CargoError::InvalidBody(_))
        ));
    }

    #[test]
    fn 索引行含必需字段且_cksum_用_sha256() {
        let meta = json!({
            "name": "serde", "vers": "1.0.0",
            "deps": [], "features": { "default": [] }
        });
        let body = 发布体(&meta, b"x");
        let req = CargoFormat::parse_publish(&body).unwrap();
        let line = CargoFormat::index_line(&req, "abc123sha256").unwrap();
        let v: Value = serde_json::from_str(&line).unwrap();
        assert_eq!(v["name"], "serde");
        assert_eq!(v["vers"], "1.0.0");
        assert_eq!(v["cksum"], "abc123sha256");
        assert_eq!(v["yanked"], false);
        assert!(v["deps"].is_array());
        assert!(v["features"].is_object());
    }

    #[test]
    fn 索引行转换_deps_version_req_为_req() {
        let meta = json!({
            "name": "mycrate", "vers": "0.1.0", "features": {},
            "deps": [{
                "name": "serde",
                "version_req": "^1.0",
                "features": ["derive"],
                "optional": false,
                "default_features": true,
                "target": null,
                "kind": "normal"
            }]
        });
        let body = 发布体(&meta, b"x");
        let req = CargoFormat::parse_publish(&body).unwrap();
        let line = CargoFormat::index_line(&req, "ck").unwrap();
        let v: Value = serde_json::from_str(&line).unwrap();
        let dep = &v["deps"][0];
        assert_eq!(dep["name"], "serde");
        // version_req 转为 req
        assert_eq!(dep["req"], "^1.0");
        assert!(dep.get("version_req").is_none());
        assert_eq!(dep["kind"], "normal");
    }

    #[test]
    fn 索引行重命名依赖用_package_记原名() {
        let meta = json!({
            "name": "mycrate", "vers": "0.1.0", "features": {},
            "deps": [{
                "name": "serde",
                "version_req": "^1.0",
                "explicit_name_in_toml": "serde_alias"
            }]
        });
        let body = 发布体(&meta, b"x");
        let req = CargoFormat::parse_publish(&body).unwrap();
        let line = CargoFormat::index_line(&req, "ck").unwrap();
        let v: Value = serde_json::from_str(&line).unwrap();
        let dep = &v["deps"][0];
        // name 用 toml 别名，package 记原 crate 名
        assert_eq!(dep["name"], "serde_alias");
        assert_eq!(dep["package"], "serde");
    }

    #[test]
    fn 合并首次发布生成单行索引() {
        let line = r#"{"name":"a","vers":"1.0.0","cksum":"c","yanked":false}"#;
        let merged = CargoFormat::merge_index(b"", line, "1.0.0").unwrap();
        let text = String::from_utf8(merged).unwrap();
        assert_eq!(text, format!("{line}\n"));
    }

    #[test]
    fn 合并追加新版本到既有索引() {
        let l1 = r#"{"name":"a","vers":"1.0.0","cksum":"c1","yanked":false}"#;
        let idx = CargoFormat::merge_index(b"", l1, "1.0.0").unwrap();
        let l2 = r#"{"name":"a","vers":"2.0.0","cksum":"c2","yanked":false}"#;
        let idx2 = CargoFormat::merge_index(&idx, l2, "2.0.0").unwrap();
        let text = String::from_utf8(idx2).unwrap();
        assert!(text.contains("1.0.0"));
        assert!(text.contains("2.0.0"));
        // 两行
        assert_eq!(text.lines().count(), 2);
    }

    #[test]
    fn 合并同版本返回_versionexists() {
        let l1 = r#"{"name":"a","vers":"1.0.0","cksum":"c1","yanked":false}"#;
        let idx = CargoFormat::merge_index(b"", l1, "1.0.0").unwrap();
        let l2 = r#"{"name":"a","vers":"1.0.0","cksum":"c2","yanked":false}"#;
        let err = CargoFormat::merge_index(&idx, l2, "1.0.0").unwrap_err();
        assert_eq!(err, CargoError::VersionExists("1.0.0".to_string()));
    }

    #[test]
    fn yank_翻转指定版本标记() {
        let l1 = r#"{"name":"a","vers":"1.0.0","cksum":"c1","yanked":false}"#;
        let l2 = r#"{"name":"a","vers":"2.0.0","cksum":"c2","yanked":false}"#;
        let mut idx = CargoFormat::merge_index(b"", l1, "1.0.0").unwrap();
        idx = CargoFormat::merge_index(&idx, l2, "2.0.0").unwrap();

        // yank 1.0.0
        let yanked = CargoFormat::set_yanked(&idx, "1.0.0", true).unwrap();
        let text = String::from_utf8(yanked.clone()).unwrap();
        for line in text.lines() {
            let v: Value = serde_json::from_str(line).unwrap();
            if v["vers"] == "1.0.0" {
                assert_eq!(v["yanked"], true);
            } else {
                assert_eq!(v["yanked"], false);
            }
        }

        // unyank 1.0.0 还原
        let unyanked = CargoFormat::set_yanked(&yanked, "1.0.0", false).unwrap();
        let text = String::from_utf8(unyanked).unwrap();
        for line in text.lines() {
            let v: Value = serde_json::from_str(line).unwrap();
            assert_eq!(v["yanked"], false);
        }
    }

    #[test]
    fn yank_版本不存在报错() {
        let l1 = r#"{"name":"a","vers":"1.0.0","cksum":"c1","yanked":false}"#;
        let idx = CargoFormat::merge_index(b"", l1, "1.0.0").unwrap();
        let err = CargoFormat::set_yanked(&idx, "9.9.9", true).unwrap_err();
        assert_eq!(err, CargoError::VersionNotFound("9.9.9".to_string()));
    }

    #[test]
    fn config_json_指回本仓库() {
        let bytes = CargoFormat::config_json("http://localhost:8080/", "crates-hosted");
        let v: Value = serde_json::from_slice(&bytes).unwrap();
        assert_eq!(v["dl"], "http://localhost:8080/crates-hosted/api/v1/crates");
        assert_eq!(v["api"], "http://localhost:8080/crates-hosted");
    }

    #[test]
    fn 覆盖策略_索引可更新_crate不可覆盖() {
        // 索引文件（index/ 前缀）可更新
        assert!(CargoFormat.can_overwrite(&记录("index/se/rd/serde")));
        // .crate 本体（crates/ 前缀）不可覆盖
        assert!(!CargoFormat.can_overwrite(&记录("crates/serde/serde-1.0.0.crate")));
    }

    #[test]
    fn 内容类型按_crate_与索引区分() {
        let ct = |p: &str| {
            CargoFormat.content_type(&ArtifactCoordinates {
                path: p.to_string(),
            })
        };
        assert_eq!(ct("index/se/rd/serde").as_deref(), Some("application/json"));
        assert_eq!(
            ct("crates/serde/serde-1.0.0.crate").as_deref(),
            Some("application/octet-stream")
        );
    }

    #[test]
    fn 解析路径归一化且拒穿越() {
        assert_eq!(
            CargoFormat.parse_path("/index//se/rd/serde").unwrap().path,
            "index/se/rd/serde"
        );
        assert_eq!(
            CargoFormat.parse_path("crates/../etc"),
            Err(PathError::Traversal)
        );
    }

    #[test]
    fn 使用片段含_cargo_add_与接入_且无真实凭据() {
        let coords = ArtifactCoordinates {
            path: "crates/serde/serde-1.0.0.crate".to_string(),
        };
        let snippets =
            CargoFormat.usage_snippets("http://localhost:8080/", "crates-hosted", &coords);
        assert_eq!(snippets.len(), 2);
        assert!(snippets[0].content.contains("cargo add serde"));
        // 接入片段含 sparse+ 索引地址与 cargo login 占位
        assert!(snippets[1]
            .content
            .contains("sparse+http://localhost:8080/crates-hosted/"));
        assert!(snippets[1].content.contains("cargo login"));
        // 不含真实 Token 字面（仅占位）
        assert!(!snippets[1].content.contains("Bearer "));
    }
}
