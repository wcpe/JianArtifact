//! Maven 写入后派生文件维护（FR-121/122，ADR-0037）：服务端权威 maven-metadata.xml + pom 三级兜底 + 快照时间戳。
//!
//! 在 Maven 主版本文件写入 hosted 仓库后调用：按 client-priority 补缺 pom（仅 Web 上传持有字节时），
//! 并按 SQLite 索引重生成 artifact 级 `maven-metadata.xml`（FR-121）与快照级 `maven-metadata.xml`
//! （FR-122）及四校验和 sidecar。handler 保持薄、不在路由层写该业务；纯协议逻辑（路径 / 聚合 /
//! 生成）下沉 `MavenFormat`，本模块只做编排。

use crate::format::{Format, Gav, MavenFormat};
use crate::meta::RepositoryRecord;

use super::{ApiError, AppState};

/// 取当前 Unix 秒（UTC）；用于铸造快照时间戳唯一版本（FR-122）。
fn now_epoch_secs() -> u64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_secs())
        .unwrap_or(0)
}

/// 为 Web 上传的 Maven 快照主构件铸造唯一时间戳存储路径（FR-122）。
///
/// 形如 `{group路径}/{a}/{base}-SNAPSHOT/{a}-{base}-{yyyyMMdd.HHmmss}-{buildNumber}.{ext}`：
/// 时间戳取真实 now，构建号据该快照目录现有最大构建号 +1。供 upload 路由在 `put_hosted` 前改写落库路径。
pub async fn mint_snapshot_path(
    state: &AppState,
    repo: &RepositoryRecord,
    group_id: &str,
    artifact_id: &str,
    snapshot_version: &str,
    file_name: &str,
) -> Result<String, ApiError> {
    let ext = file_name.rsplit('.').next().unwrap_or("jar");
    let dir_prefix = format!(
        "{}{}/",
        MavenFormat::artifact_prefix(group_id, artifact_id),
        snapshot_version.trim()
    );
    let records = state
        .meta
        .list_artifacts_under_prefix(&repo.id, &dir_prefix)
        .await?;
    let builds =
        MavenFormat::collect_snapshot_builds(&records, group_id, artifact_id, snapshot_version);
    let build_number = builds.next_build_number();
    let timestamp = MavenFormat::epoch_to_snapshot_timestamp(now_epoch_secs());
    Ok(MavenFormat::snapshot_artifact_path(
        group_id,
        artifact_id,
        snapshot_version,
        ext,
        &timestamp,
        build_number,
    ))
}

/// Maven 写入后维护派生文件（FR-121/122）。
///
/// - `written_path`：本次写入的制品仓库内路径。
/// - `artifact_bytes`：Web 上传时为主构件字节（用于 jar 内嵌 pom 提取）；`mvn deploy` 路径为
///   `None`（client-priority，pom 由客户端上传，服务端不兜底）。
///
/// 仅对「能反解 GAV、非 sidecar、非 maven-metadata.xml」的主版本文件触发；其余直接返回。
/// 快照时间戳构件额外重生成快照级 metadata。
pub async fn maintain_after_maven_write(
    state: &AppState,
    repo: &RepositoryRecord,
    format: &dyn Format,
    written_path: &str,
    artifact_bytes: Option<&[u8]>,
) -> Result<(), ApiError> {
    let Some((gav, file_name)) = resolve_main_artifact(written_path) else {
        return Ok(());
    };

    // 是否为快照时间戳构件（FR-122）：解析出 (timestamp, buildNumber, ext) 则是
    let snapshot_build = if MavenFormat::is_snapshot_version(&gav.version) {
        MavenFormat::parse_snapshot_build(
            file_name,
            &gav.artifact_id,
            MavenFormat::snapshot_base(&gav.version),
        )
    } else {
        None
    };

    // ① pom 兜底：仅 Web 上传持有字节、且写入的不是 pom 自身时补缺（client-priority）
    if let Some(bytes) = artifact_bytes {
        if !file_name.ends_with(".pom") {
            ensure_pom(
                state,
                repo,
                format,
                &gav,
                file_name,
                bytes,
                snapshot_build.as_ref(),
            )
            .await?;
        }
    }

    // ② 快照级 maven-metadata.xml（FR-122）：仅快照时间戳构件触发，按目录内时间戳构建聚合
    if snapshot_build.is_some() {
        regenerate_snapshot_metadata(state, repo, format, &gav).await?;
    }

    // ③ artifact 级 maven-metadata.xml（FR-121）：release 与 snapshot 两类都做（snapshot 作为一个版本列入）
    regenerate_metadata(state, repo, format, &gav).await?;
    Ok(())
}

/// 判定本次写入是否为「需维护派生文件」的 Maven 主版本文件，并反解其 GAV。
///
/// 排除 `maven-metadata.xml`（派生物自身）与校验和 / 签名 sidecar，避免重复触发与自递归。
fn resolve_main_artifact(path: &str) -> Option<(Gav, &str)> {
    let file_name = path.rsplit('/').next().unwrap_or(path);
    if file_name == "maven-metadata.xml" {
        return None;
    }
    if MavenFormat::is_sidecar(path) {
        return None;
    }
    let gav = MavenFormat::gav_from_path(path)?;
    Some((gav, file_name))
}

/// pom 三级兜底（FR-121/122）：pom 已存在则 client-priority 不覆盖；否则 jar 内嵌 → 最小 pom，附 sidecar。
///
/// 快照构件的 pom 落在同一时间戳唯一名（`{a}-{base}-{ts}-{bn}.pom`）下；release 落 `{a}-{v}.pom`。
async fn ensure_pom(
    state: &AppState,
    repo: &RepositoryRecord,
    format: &dyn Format,
    gav: &Gav,
    main_file_name: &str,
    artifact_bytes: &[u8],
    snapshot_build: Option<&(String, u32, String)>,
) -> Result<(), ApiError> {
    let pom_path = match snapshot_build {
        Some((timestamp, build_number, _ext)) => MavenFormat::snapshot_artifact_path(
            &gav.group_id,
            &gav.artifact_id,
            &gav.version,
            "pom",
            timestamp,
            *build_number,
        ),
        None => MavenFormat::pom_path(&gav.group_id, &gav.artifact_id, &gav.version),
    };
    // client-priority：已存在同名 pom（客户端上传或此前已生成）→ 不覆盖
    if state
        .meta
        .get_artifact(&repo.id, &pom_path)
        .await?
        .is_some()
    {
        return Ok(());
    }
    // 第二级 jar 内嵌 pom 原样提取；取不到则第三级按 GAV 生成最小 pom（pom 的 <version> 仍为 SNAPSHOT 版本）
    let pom_bytes = MavenFormat::extract_embedded_pom(artifact_bytes).unwrap_or_else(|| {
        let packaging = MavenFormat::derive_packaging(main_file_name);
        MavenFormat::build_minimal_pom(&gav.group_id, &gav.artifact_id, &gav.version, packaging)
    });
    write_derived(state, repo, format, &pom_path, &pom_bytes).await
}

/// 重生成 artifact 级 maven-metadata.xml（FR-121）：按前缀列举 → 纯函数聚合 → 落盘 + sidecar。
async fn regenerate_metadata(
    state: &AppState,
    repo: &RepositoryRecord,
    format: &dyn Format,
    gav: &Gav,
) -> Result<(), ApiError> {
    let prefix = MavenFormat::artifact_prefix(&gav.group_id, &gav.artifact_id);
    let records = state
        .meta
        .list_artifacts_under_prefix(&repo.id, &prefix)
        .await?;
    let versions = MavenFormat::collect_versions(&records, &gav.group_id, &gav.artifact_id);
    if versions.versions.is_empty() {
        // 无任何版本（理论上不至于，主文件刚写入）→ 不生成空 metadata
        return Ok(());
    }
    let metadata_path = MavenFormat::artifact_metadata_path(&gav.group_id, &gav.artifact_id);
    let bytes = MavenFormat::build_artifact_metadata(&gav.group_id, &gav.artifact_id, &versions);
    write_derived(state, repo, format, &metadata_path, &bytes).await
}

/// 重生成快照级 maven-metadata.xml（FR-122）：按快照目录列举时间戳构建 → 聚合 → 落盘 + sidecar。
async fn regenerate_snapshot_metadata(
    state: &AppState,
    repo: &RepositoryRecord,
    format: &dyn Format,
    gav: &Gav,
) -> Result<(), ApiError> {
    let dir_prefix = format!(
        "{}{}/",
        MavenFormat::artifact_prefix(&gav.group_id, &gav.artifact_id),
        gav.version
    );
    let records = state
        .meta
        .list_artifacts_under_prefix(&repo.id, &dir_prefix)
        .await?;
    let builds = MavenFormat::collect_snapshot_builds(
        &records,
        &gav.group_id,
        &gav.artifact_id,
        &gav.version,
    );
    if builds.is_empty() {
        return Ok(());
    }
    let path = MavenFormat::snapshot_metadata_path(&gav.group_id, &gav.artifact_id, &gav.version);
    let bytes = MavenFormat::build_snapshot_metadata(
        &gav.group_id,
        &gav.artifact_id,
        &gav.version,
        &builds,
    );
    write_derived(state, repo, format, &path, &bytes).await
}

/// 落一个服务端派生文件（pom / metadata）+ 其四校验和 sidecar（与 Web 上传补 sidecar 同款机理）。
///
/// 经 `put_hosted` 写入（blob 先落盘校验再写索引、失败回滚），不经路由层、不再触发本维护逻辑（无递归）。
async fn write_derived(
    state: &AppState,
    repo: &RepositoryRecord,
    format: &dyn Format,
    path: &str,
    bytes: &[u8],
) -> Result<(), ApiError> {
    let coords = format.parse_path(path)?;
    // 派生文件为服务端生成、体积小且受信，不施加用户上传上限
    let outcome = state
        .artifacts
        .put_hosted(repo, format, &coords, bytes, None)
        .await?;
    let digests = [
        ("sha1", outcome.record.sha1.as_str()),
        ("md5", outcome.record.md5.as_str()),
        ("sha256", outcome.record.sha256.as_str()),
        ("sha512", outcome.record.sha512.as_str()),
    ];
    for (ext, digest) in digests {
        let sidecar_path = format!("{path}.{ext}");
        let sidecar_coords = format.parse_path(&sidecar_path)?;
        state
            .artifacts
            .put_hosted(repo, format, &sidecar_coords, digest.as_bytes(), None)
            .await?;
    }
    Ok(())
}
