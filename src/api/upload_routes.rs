//! 通用制品上传端点（FR-73）：Web 控制台统一上传入口，`POST /api/v1/repositories/{id}/upload`。
//!
//! 把 multipart/form-data 上传适配为既有 hosted 直传：据目标仓库格式从表单取坐标字段，
//! 拼出仓库内路径后委托 [`crate::format::ArtifactService::put_hosted`] 落 blob + 写索引。
//! 仅支持 Maven / npm / Raw 三格式且仅 hosted 仓库（proxy 由 `put_hosted` 内置拒绝为 400）。
//!
//! handler 保持薄：写授权复用 `repo_access::load_writable_repo`；路径拼装委托各格式纯函数；
//! 不在此重造存储 / 校验和 / 事务，也不写各格式协议业务。

use axum::{
    extract::{Multipart, Path, State},
    http::StatusCode,
    response::{IntoResponse, Response},
};

use crate::format::{ArtifactCoordinates, Format, MavenFormat, NpmFormat};
use crate::meta::{ArtifactRecord, RepositoryRecord};

use super::repo_access::load_writable_repo;
use super::{ApiError, AppState, Identity};

/// 上传文件字段名（multipart 中承载制品字节的字段）。
const FILE_FIELD: &str = "file";

/// 通用上传支持的格式名。
const FORMAT_MAVEN: &str = "maven";
const FORMAT_NPM: &str = "npm";
const FORMAT_RAW: &str = "raw";

/// 上传端点：写授权后据仓库格式拼坐标，流式落 blob 并写索引。
///
/// 成功覆盖返回 200、新建返回 201（与既有 PUT 直传语义一致）。
pub async fn upload_artifact(
    State(state): State<AppState>,
    identity: Identity,
    Path(repo_id): Path<String>,
    multipart: Multipart,
) -> Result<Response, ApiError> {
    // 写授权：无读 404（隐藏存在性）、有读无写 403
    let repo = load_writable_repo(&state, &identity, &repo_id).await?;

    // 读取 multipart 各字段到内存（受上传上限约束，超限 413）
    let fields = read_fields(multipart, state.config.limits.max_artifact_size).await?;

    // 取上传文件字段（含文件名与字节）
    let file = fields
        .iter()
        .find(|f| f.name == FILE_FIELD && f.filename.is_some())
        .ok_or_else(|| ApiError::BadRequest("缺少上传文件字段 file".to_string()))?;
    let file_name = file.filename.clone().unwrap_or_default();

    // 据仓库格式拼仓库内路径：Maven 缺坐标时回退 jar 内嵌 pom（FR-123）、快照铸造时间戳唯一版本（FR-122）
    let path = build_upload_path(&state, &repo, &fields, &file_name, &file.bytes).await?;

    // 经格式处理器归一化坐标（拒目录穿越 / 空路径）
    let format = state
        .formats
        .get(&repo.format)
        .ok_or_else(|| ApiError::BadRequest("仓库格式未实现".to_string()))?;
    let coords: ArtifactCoordinates = format.parse_path(&path)?;

    // 流式落 blob + 写索引（proxy 仓库由 put_hosted 内置拒绝为 400）
    let outcome = state
        .artifacts
        .put_hosted(
            &repo,
            format,
            &coords,
            &file.bytes[..],
            state.config.limits.max_artifact_size,
        )
        .await?;

    // Maven：服务端上传无客户端逐文件 PUT 的 sidecar，故为主构件补齐四校验和 sidecar，
    // 使产出制品与 mvn deploy 一致、可被官方客户端独立 GET 校验和并校验（FR-69）。
    if repo.format == FORMAT_MAVEN && !MavenFormat::is_sidecar(&coords.path) {
        // 可选用户上传 pom（FR-123，pom 三级兜底「用户上传」层）：先落库，使后续兜底视其已存在而不覆盖。
        store_user_pom_if_present(&state, &repo, format, &coords.path, &fields).await?;
        write_maven_checksum_sidecars(&state, &repo, format, &outcome.record).await?;
        // 写入后维护服务端权威派生文件（FR-121/122，ADR-0037）：pom 三级兜底（持有主构件字节，可提取 jar
        // 内嵌 pom，否则生成最小 pom）+ 重生成 artifact 级 maven-metadata.xml；快照构件另生成快照级 metadata。
        super::maven_publish::maintain_after_maven_write(
            &state,
            &repo,
            format,
            &coords.path,
            Some(&file.bytes),
        )
        .await?;
    }

    tracing::info!(
        仓库 = %repo.name,
        格式 = %repo.format,
        路径 = %coords.path,
        覆盖 = outcome.overwritten,
        "Web 上传制品成功"
    );

    let status = if outcome.overwritten {
        StatusCode::OK
    } else {
        StatusCode::CREATED
    };
    Ok(status.into_response())
}

/// 据仓库格式与表单字段拼出制品在仓库内的存储路径。
///
/// - Maven：坐标 `group_id` / `artifact_id` / `version` 可留空，缺失时回退 jar 内嵌 pom 自动识别（FR-123）；
///   快照主构件铸造唯一时间戳路径（FR-122）。
/// - npm：表单 `name` / `version` + 上传文件名 → `{name}/-/{文件名}`（不解包 .tgz）。
/// - Raw：表单 `path` 即仓库内路径。
/// - 其余格式：不支持经通用上传发布（400）。
async fn build_upload_path(
    state: &AppState,
    repo: &RepositoryRecord,
    fields: &[UploadField],
    file_name: &str,
    main_bytes: &[u8],
) -> Result<String, ApiError> {
    match repo.format.as_str() {
        FORMAT_MAVEN => {
            let (group_id, artifact_id, version) = resolve_maven_coords(fields, main_bytes)?;
            let base = MavenFormat::artifact_path(&group_id, &artifact_id, &version, file_name);
            // 快照主构件（非 sidecar / 非 pom 自身）→ 铸造唯一时间戳版本（FR-122）
            if MavenFormat::is_snapshot_version(&version)
                && !file_name.ends_with(".pom")
                && !MavenFormat::is_sidecar(&base)
            {
                return super::maven_publish::mint_snapshot_path(
                    state,
                    repo,
                    &group_id,
                    &artifact_id,
                    &version,
                    file_name,
                )
                .await;
            }
            Ok(base)
        }
        FORMAT_NPM => {
            let name = required_text(fields, "name")?;
            // version 在 Web 上传中校验存在（用于人工核对），路径以 name + 文件名定位 tarball
            let _version = required_text(fields, "version")?;
            Ok(NpmFormat::tarball_path(&name, file_name))
        }
        FORMAT_RAW => {
            let path = required_text(fields, "path")?;
            Ok(path)
        }
        other => Err(ApiError::BadRequest(format!(
            "格式 {other} 不支持经通用上传端点发布（仅 maven / npm / raw）"
        ))),
    }
}

/// 解析 Maven 坐标（FR-123）：表单 `group_id` / `artifact_id` / `version` 为主，缺失项回退 jar 内嵌 pom；
/// 仍无法补齐（无表单且 jar 无内嵌 pom）则 400，提示填写坐标或上传含内嵌 pom 的 jar。
fn resolve_maven_coords(
    fields: &[UploadField],
    main_bytes: &[u8],
) -> Result<(String, String, String), ApiError> {
    let mut group_id = optional_text(fields, "group_id");
    let mut artifact_id = optional_text(fields, "artifact_id");
    let mut version = optional_text(fields, "version");

    // 任一坐标缺失 → 尝试从 jar 内嵌 pom 提取补齐（复用 FR-120）
    if group_id.is_none() || artifact_id.is_none() || version.is_none() {
        if let Some(gav) = MavenFormat::parse_gav_from_jar(main_bytes) {
            group_id = group_id.or(Some(gav.group_id));
            artifact_id = artifact_id.or(Some(gav.artifact_id));
            version = version.or(Some(gav.version));
        }
    }

    match (group_id, artifact_id, version) {
        (Some(g), Some(a), Some(v)) => Ok((g, a, v)),
        _ => Err(ApiError::BadRequest(
            "缺少 Maven 坐标且无法从 jar 内嵌 pom 识别：请填写 groupId / artifactId / version，或上传含内嵌 pom 的 jar".to_string(),
        )),
    }
}

/// 可选用户上传 pom（FR-123）：表单含 `pom` 文件字段时，按主构件同名落 `.pom` + 四校验和 sidecar。
///
/// 落在主构件旁同基名的 `.pom`（release `{a}-{v}.pom`；快照 `{a}-{base}-{ts}-{bn}.pom`）；先于
/// 服务端 pom 兜底写入，使 [`maven_publish::maintain_after_maven_write`] 视其已存在而不覆盖（client-priority）。
async fn store_user_pom_if_present(
    state: &AppState,
    repo: &RepositoryRecord,
    format: &dyn Format,
    main_path: &str,
    fields: &[UploadField],
) -> Result<(), ApiError> {
    // 主构件本身是 pom 则无需另存
    if main_path.ends_with(".pom") {
        return Ok(());
    }
    let Some(pom) = fields
        .iter()
        .find(|f| f.name == "pom" && f.filename.is_some())
    else {
        return Ok(());
    };
    let pom_path = derive_pom_path(main_path);
    let coords = format.parse_path(&pom_path)?;
    let outcome = state
        .artifacts
        .put_hosted(
            repo,
            format,
            &coords,
            &pom.bytes[..],
            state.config.limits.max_artifact_size,
        )
        .await?;
    write_maven_checksum_sidecars(state, repo, format, &outcome.record).await
}

/// 由主构件路径推导同基名的 pom 路径（换扩展名为 `.pom`）。
fn derive_pom_path(main_path: &str) -> String {
    match main_path.rsplit_once('.') {
        Some((base, _)) => format!("{base}.pom"),
        None => format!("{main_path}.pom"),
    }
}

/// 为 Maven 主构件补齐四校验和 sidecar（`.sha1` / `.md5` / `.sha256` / `.sha512`）。
///
/// 服务端 Web 上传没有客户端逐文件 PUT 的 sidecar，故据主构件已算好的四摘要各落一份小文件，
/// 内容为对应摘要的小写十六进制——使 mvn 等官方客户端下载时可独立取回校验和并比对。
/// sidecar 经 `put_hosted` 落为独立制品（其覆盖策略放行 sidecar 更新），与 mvn deploy 产物同构。
async fn write_maven_checksum_sidecars(
    state: &AppState,
    repo: &RepositoryRecord,
    format: &dyn Format,
    record: &ArtifactRecord,
) -> Result<(), ApiError> {
    let digests = [
        ("sha1", record.sha1.as_str()),
        ("md5", record.md5.as_str()),
        ("sha256", record.sha256.as_str()),
        ("sha512", record.sha512.as_str()),
    ];
    for (ext, digest) in digests {
        let path = format!("{}.{}", record.path, ext);
        let coords = format.parse_path(&path)?;
        state
            .artifacts
            .put_hosted(repo, format, &coords, digest.as_bytes(), None)
            .await?;
    }
    Ok(())
}

/// 取某文本字段的值；缺失 / 为空一律 400（必填字段不可缺）。
fn required_text(fields: &[UploadField], name: &str) -> Result<String, ApiError> {
    optional_text(fields, name)
        .ok_or_else(|| ApiError::BadRequest(format!("缺少必填表单字段 {name}")))
}

/// 取某文本字段的值；缺失 / 为空返回 None（可选字段）。
fn optional_text(fields: &[UploadField], name: &str) -> Option<String> {
    fields
        .iter()
        .find(|f| f.name == name && f.filename.is_none())
        .map(|f| String::from_utf8_lossy(&f.bytes).trim().to_string())
        .filter(|v| !v.is_empty())
}

/// multipart 中读出的单个字段（文本字段或文件字段）。
struct UploadField {
    /// 字段名。
    name: String,
    /// 文件名（文件字段有，文本字段为 None）。
    filename: Option<String>,
    /// 字段字节内容。
    bytes: Vec<u8>,
}

/// 逐字段读取 multipart 上传体到内存，累计受上传上限约束（超限 413）。
///
/// 与 PyPI 上传同款策略：缓冲单次上传总字节，超过 `max` 即拒绝并返回 413，不继续读入。
async fn read_fields(
    mut multipart: Multipart,
    max: Option<u64>,
) -> Result<Vec<UploadField>, ApiError> {
    let mut fields = Vec::new();
    let mut total: u64 = 0;
    loop {
        let field = match multipart.next_field().await {
            Ok(Some(f)) => f,
            Ok(None) => break,
            Err(_) => return Err(ApiError::BadRequest("multipart 解析失败".to_string())),
        };
        let name = field.name().unwrap_or("").to_string();
        let filename = field.file_name().map(str::to_string);
        let bytes = field
            .bytes()
            .await
            .map_err(|_| ApiError::BadRequest("读取 multipart 字段失败".to_string()))?;
        total = total.saturating_add(bytes.len() as u64);
        if let Some(limit) = max {
            if total > limit {
                return Err(ApiError::PayloadTooLarge);
            }
        }
        fields.push(UploadField {
            name,
            filename,
            bytes: bytes.to_vec(),
        });
    }
    Ok(fields)
}
