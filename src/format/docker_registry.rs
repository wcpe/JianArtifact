//! Docker Registry v2 存储编排（FR-16）：承载 blob 上传状态机、blob / manifest 存取，
//! 与 docker 协议绑定但与 HTTP 解耦——HTTP 层（`api/docker_routes.rs`）只做协议适配，
//! 实际存储复用通用 [`BlobStore`]（blob 本体）与 [`MetaStore`]（索引，元数据唯一真源）。
//!
//! 核心不变量（testing-and-quality §2.2/§2.5）：
//! - **blob / manifest 先落盘并校验 sha256（内容寻址即校验），再写元数据索引**；写索引失败回滚 blob。
//! - 上传分块经会话临时文件流式累积，**大镜像层不整体载入内存**；完成时校验客户端 digest。
//! - 同一 tag 覆盖（Docker 习惯，FR-61）；blob / manifest 按 digest 去重。

use std::collections::HashMap;
use std::path::PathBuf;
use std::sync::Mutex;

use tokio::io::{AsyncRead, AsyncWriteExt};
use uuid::Uuid;

use crate::meta::{MetaError, MetaStore, NewArtifact, RepositoryRecord};
use crate::storage::{BlobStore, StorageError};

use super::docker::{self, MEDIA_TYPE_MANIFEST_V2};

/// Docker 存储编排错误。
#[derive(Debug, thiserror::Error)]
pub enum DockerError {
    /// 资源不存在（blob / manifest / tag / 上传会话）。
    #[error("资源不存在")]
    NotFound,
    /// 上传会话不存在或已失效。
    #[error("上传会话不存在")]
    UnknownUpload,
    /// 客户端声明的 digest 与实际内容不符（BLOB_UPLOAD_INVALID / DIGEST_INVALID）。
    #[error("digest 与内容不匹配")]
    DigestMismatch,
    /// digest 格式非法（非 sha256:64hex）。
    #[error("digest 格式非法")]
    InvalidDigest,
    /// manifest 媒体类型不受支持。
    #[error("manifest 媒体类型不受支持")]
    UnsupportedMediaType,
    /// 上传体积超过配置上限（映射 413）。
    #[error("制品体积超过上限")]
    TooLarge,
    /// 存储层错误。
    #[error(transparent)]
    Storage(#[from] StorageError),
    /// 元数据层错误。
    #[error(transparent)]
    Meta(#[from] MetaError),
}

/// blob 上传会话：累积分块到临时文件，记录已写字节，PUT 完成时校验 digest 后落定。
struct UploadSession {
    /// 临时文件路径（位于会话临时目录）。
    tmp_path: PathBuf,
    /// 已累计写入字节数（用于 Range 头与超限判定）。
    written: u64,
}

/// blob 读取句柄：blob 文件流连同其大小与 digest。
#[derive(Debug)]
pub struct BlobHandle {
    /// blob 字节流（流式返回，不整体载入内存）。
    pub blob: tokio::fs::File,
    /// blob 字节数。
    pub size: i64,
    /// blob digest（`sha256:{hex}`）。
    pub digest: String,
}

/// manifest 读取结果：manifest 字节、媒体类型与 digest。
#[derive(Debug)]
pub struct ManifestHandle {
    /// manifest 原始字节（manifest 通常较小，按内容返回）。
    pub bytes: Vec<u8>,
    /// manifest 媒体类型（Content-Type）。
    pub media_type: String,
    /// manifest digest（`sha256:{hex}`）。
    pub digest: String,
}

/// 启动上传会话的产物：会话 id 供后续 PATCH / PUT 引用。
pub struct StartedUpload {
    /// 会话唯一 id（拼入 Location 头供客户端续传）。
    pub upload_id: String,
}

/// 分块追加结果：当前已累积字节数（用于 Range 响应）。
#[derive(Debug)]
pub struct AppendOutcome {
    /// 已累积字节数。
    pub written: u64,
}

/// Docker Registry v2 存储服务。
///
/// 泛型于 [`BlobStore`]，持有元数据存储与会话临时目录；会话状态在进程内存（重启即弃，
/// 符合 docker 上传会话的短生命周期语义）。`max_size` 为可选上传上限（超限 413）。
pub struct DockerRegistry<S: BlobStore> {
    /// blob 存储。
    store: S,
    /// 元数据存储。
    meta: MetaStore,
    /// 上传会话临时目录。
    session_dir: PathBuf,
    /// 进行中的上传会话：id → 会话状态。
    sessions: Mutex<HashMap<String, UploadSession>>,
    /// 单次上传字节上限（None 表示不限）。
    max_size: Option<u64>,
}

impl<S: BlobStore> DockerRegistry<S> {
    /// 构造服务并确保会话临时目录存在。
    pub async fn new(
        store: S,
        meta: MetaStore,
        session_dir: PathBuf,
        max_size: Option<u64>,
    ) -> Result<Self, StorageError> {
        tokio::fs::create_dir_all(&session_dir).await?;
        Ok(Self {
            store,
            meta,
            session_dir,
            sessions: Mutex::new(HashMap::new()),
            max_size,
        })
    }

    // ---------------- blob 上传状态机 ----------------

    /// 启动一次 blob 上传（POST .../blobs/uploads/）：建会话与空临时文件，返回会话 id。
    pub async fn start_upload(&self) -> Result<StartedUpload, DockerError> {
        let upload_id = Uuid::new_v4().to_string();
        let tmp_path = self.session_dir.join(&upload_id);
        // 建空临时文件占位（后续 PATCH 追加 / PUT 单体写入）
        tokio::fs::File::create(&tmp_path).await.map_err(StorageError::Io)?;
        self.sessions
            .lock()
            .expect("上传会话表锁未中毒")
            .insert(upload_id.clone(), UploadSession { tmp_path, written: 0 });
        Ok(StartedUpload { upload_id })
    }

    /// 向会话追加一段字节（PATCH，亦用于 PUT 携带末段）：流式写入临时文件，返回累计字节数。
    ///
    /// 超过 `max_size` 即报错（上层映射 413）并保留会话由上层取消；不整体载入内存。
    pub async fn append_upload<R>(
        &self,
        upload_id: &str,
        reader: R,
    ) -> Result<AppendOutcome, DockerError>
    where
        R: AsyncRead + Unpin + Send,
    {
        let (tmp_path, start_offset) = {
            let sessions = self.sessions.lock().expect("上传会话表锁未中毒");
            let session = sessions.get(upload_id).ok_or(DockerError::UnknownUpload)?;
            (session.tmp_path.clone(), session.written)
        };

        // 以追加模式打开临时文件，流式写入（锁外 IO）
        let appended = self.stream_append(&tmp_path, reader, start_offset).await?;
        let written = start_offset + appended;

        // 回写累计字节数
        let mut sessions = self.sessions.lock().expect("上传会话表锁未中毒");
        if let Some(session) = sessions.get_mut(upload_id) {
            session.written = written;
        }
        Ok(AppendOutcome { written })
    }

    /// 完成 blob 上传（PUT ...?digest=...）：把会话临时文件按内容寻址落定，校验 digest 后写索引。
    ///
    /// 次序：① 流式把临时文件喂 BlobStore（边读边算 sha256，落定即校验）；
    /// ② 比对客户端 digest；③ 写制品索引；写索引失败回滚 blob。返回落定的完整 digest。
    pub async fn finish_upload(
        &self,
        repo: &RepositoryRecord,
        image: &str,
        upload_id: &str,
        expected_digest: &str,
    ) -> Result<String, DockerError> {
        let expected_hex =
            docker::parse_digest(expected_digest).ok_or(DockerError::InvalidDigest)?;

        // 取出会话（移除登记，避免重复完成）
        let tmp_path = {
            let mut sessions = self.sessions.lock().expect("上传会话表锁未中毒");
            sessions
                .remove(upload_id)
                .ok_or(DockerError::UnknownUpload)?
                .tmp_path
        };

        // ① 流式落定为内容寻址 blob（BlobStore 边写边算 sha256）
        let file = tokio::fs::File::open(&tmp_path).await.map_err(StorageError::Io)?;
        let digests = self.store.put(file).await;
        // 无论成败都清理会话临时文件
        let _ = tokio::fs::remove_file(&tmp_path).await;
        let digests = digests?;

        // ② 校验客户端声明 digest 与实际内容一致；不一致即回滚刚落定的 blob（若无其他引用）
        if digests.sha256 != expected_hex {
            self.rollback_blob(&digests.sha256).await;
            return Err(DockerError::DigestMismatch);
        }

        // ③ 写 blob 索引（内部存储键 {image}/blobs/sha256:{hex}）
        let key = docker::blob_key(image, expected_digest);
        let write = self
            .meta
            .upsert_artifact(NewArtifact {
                repo_id: &repo.id,
                path: &key,
                size: digests.size as i64,
                sha256: &digests.sha256,
                sha1: &digests.sha1,
                md5: &digests.md5,
                sha512: &digests.sha512,
                content_type: Some("application/octet-stream"),
                cached: false,
            })
            .await;
        if let Err(e) = write {
            self.rollback_blob(&digests.sha256).await;
            return Err(e.into());
        }
        tracing::info!(仓库 = %repo.name, 镜像 = %image, digest = %expected_digest, "已落定 docker blob");
        Ok(docker::make_digest(&digests.sha256))
    }

    /// 取消上传会话：移除登记并清理临时文件（DELETE 上传 / digest 不符回滚时调用）。
    pub async fn cancel_upload(&self, upload_id: &str) {
        let tmp_path = {
            let mut sessions = self.sessions.lock().expect("上传会话表锁未中毒");
            sessions.remove(upload_id).map(|s| s.tmp_path)
        };
        if let Some(path) = tmp_path {
            let _ = tokio::fs::remove_file(&path).await;
        }
    }

    // ---------------- blob 读取 ----------------

    /// 查 blob 是否存在（HEAD），返回其大小（不存在返回 None）。
    pub async fn stat_blob(
        &self,
        repo: &RepositoryRecord,
        image: &str,
        digest: &str,
    ) -> Result<Option<i64>, DockerError> {
        if docker::parse_digest(digest).is_none() {
            return Err(DockerError::InvalidDigest);
        }
        let key = docker::blob_key(image, digest);
        let record = self.meta.get_artifact(&repo.id, &key).await?;
        Ok(record.map(|r| r.size))
    }

    /// 拉取 blob（GET）：返回文件流与大小（流式，不整体载入内存）。
    pub async fn get_blob(
        &self,
        repo: &RepositoryRecord,
        image: &str,
        digest: &str,
    ) -> Result<BlobHandle, DockerError> {
        if docker::parse_digest(digest).is_none() {
            return Err(DockerError::InvalidDigest);
        }
        let key = docker::blob_key(image, digest);
        let record = self
            .meta
            .get_artifact(&repo.id, &key)
            .await?
            .ok_or(DockerError::NotFound)?;
        let blob = self.store.get(&record.sha256).await?;
        Ok(BlobHandle {
            blob,
            size: record.size,
            digest: digest.to_string(),
        })
    }

    // ---------------- manifest 存取 ----------------

    /// 写入 manifest（PUT .../manifests/{reference}）：按 digest 内容寻址落 blob 并写两条索引——
    /// manifest 自身（按 digest）与 tag 指针（若 reference 为 tag）。返回 manifest digest。
    ///
    /// manifest 字节作为内容寻址 blob 存储（与普通 blob 共用 sha256 寻址，天然去重）。
    pub async fn put_manifest(
        &self,
        repo: &RepositoryRecord,
        image: &str,
        reference: &str,
        media_type: &str,
        bytes: Vec<u8>,
    ) -> Result<String, DockerError> {
        // 仅接受受支持的 manifest 媒体类型（schema2 / OCI image manifest）
        if !is_supported_manifest_media_type(media_type) {
            return Err(DockerError::UnsupportedMediaType);
        }
        if let Some(max) = self.max_size {
            if bytes.len() as u64 > max {
                return Err(DockerError::TooLarge);
            }
        }

        // ① 内容寻址落 blob（边写边算 sha256，落定即校验）
        let digests = self.store.put(std::io::Cursor::new(bytes)).await?;
        let digest = docker::make_digest(&digests.sha256);

        // 若 reference 是 digest，须与算得内容一致，杜绝错配
        if docker::is_digest_reference(reference) {
            match docker::parse_digest(reference) {
                Some(hex) if hex == digests.sha256 => {}
                Some(_) => {
                    self.rollback_blob(&digests.sha256).await;
                    return Err(DockerError::DigestMismatch);
                }
                None => {
                    self.rollback_blob(&digests.sha256).await;
                    return Err(DockerError::InvalidDigest);
                }
            }
        }

        // ② 写 manifest（按 digest）索引
        let manifest_key = docker::manifest_digest_key(image, &digest);
        if let Err(e) = self
            .write_manifest_index(repo, &manifest_key, &digests, media_type)
            .await
        {
            self.rollback_blob(&digests.sha256).await;
            return Err(e);
        }

        // ③ 若 reference 为 tag，再写 tag 指针索引（指向同一 sha256，覆盖旧 tag）
        if !docker::is_digest_reference(reference) {
            let tag_key = docker::tag_key(image, reference);
            self.write_manifest_index(repo, &tag_key, &digests, media_type)
                .await?;
        }

        tracing::info!(仓库 = %repo.name, 镜像 = %image, 引用 = %reference, digest = %digest, "已写入 docker manifest");
        Ok(digest)
    }

    /// 读取 manifest（GET/HEAD .../manifests/{reference}）：按 tag 或 digest 解析后返回字节、媒体类型与 digest。
    pub async fn get_manifest(
        &self,
        repo: &RepositoryRecord,
        image: &str,
        reference: &str,
    ) -> Result<ManifestHandle, DockerError> {
        // 据 reference 形态选键：digest → manifests/{digest}；否则 → tags/{tag}
        let key = if docker::is_digest_reference(reference) {
            if docker::parse_digest(reference).is_none() {
                return Err(DockerError::InvalidDigest);
            }
            docker::manifest_digest_key(image, reference)
        } else {
            docker::tag_key(image, reference)
        };

        let record = self
            .meta
            .get_artifact(&repo.id, &key)
            .await?
            .ok_or(DockerError::NotFound)?;

        // 读回 manifest 字节（manifest 较小，按内容返回以便设置 Docker-Content-Digest 头）
        use tokio::io::AsyncReadExt;
        let mut file = self.store.get(&record.sha256).await?;
        let mut bytes = Vec::with_capacity(record.size as usize);
        file.read_to_end(&mut bytes).await.map_err(StorageError::Io)?;

        let media_type = record
            .content_type
            .unwrap_or_else(|| MEDIA_TYPE_MANIFEST_V2.to_string());
        Ok(ManifestHandle {
            bytes,
            media_type,
            digest: docker::make_digest(&record.sha256),
        })
    }

    // ---------------- 内部工具 ----------------

    /// 写一条 manifest / tag 索引记录（共用同一 sha256，media_type 入 content_type）。
    async fn write_manifest_index(
        &self,
        repo: &RepositoryRecord,
        key: &str,
        digests: &crate::storage::BlobDigests,
        media_type: &str,
    ) -> Result<(), DockerError> {
        self.meta
            .upsert_artifact(NewArtifact {
                repo_id: &repo.id,
                path: key,
                size: digests.size as i64,
                sha256: &digests.sha256,
                sha1: &digests.sha1,
                md5: &digests.md5,
                sha512: &digests.sha512,
                content_type: Some(media_type),
                cached: false,
            })
            .await?;
        Ok(())
    }

    /// 回滚 / 清理 blob：仅当该 sha256 不再被任何索引引用时才删本体，避免误删共享 blob。
    async fn rollback_blob(&self, sha256: &str) {
        match self.meta.count_artifacts_by_sha256(sha256).await {
            Ok(0) => {
                if let Err(e) = self.store.delete(sha256).await {
                    tracing::warn!(sha256 = %sha256, 错误 = %e, "清理无引用 docker blob 失败");
                }
            }
            Ok(_) => {}
            Err(e) => {
                tracing::warn!(sha256 = %sha256, 错误 = %e, "查询 docker blob 引用计数失败，跳过清理");
            }
        }
    }

    /// 以追加模式把 `reader` 流式写入临时文件，超 `max_size` 即报错。返回本次写入字节数。
    async fn stream_append<R>(
        &self,
        tmp_path: &std::path::Path,
        mut reader: R,
        start_offset: u64,
    ) -> Result<u64, DockerError>
    where
        R: AsyncRead + Unpin + Send,
    {
        use tokio::io::AsyncReadExt;

        let mut file = tokio::fs::OpenOptions::new()
            .append(true)
            .open(tmp_path)
            .await
            .map_err(StorageError::Io)?;

        let mut buf = vec![0u8; 64 * 1024];
        let mut appended: u64 = 0;
        loop {
            let n = reader.read(&mut buf).await.map_err(StorageError::Io)?;
            if n == 0 {
                break;
            }
            // 超限即中止：保留已写部分由上层取消会话清理，不写半截 blob 索引
            if let Some(max) = self.max_size {
                if start_offset + appended + n as u64 > max {
                    return Err(DockerError::TooLarge);
                }
            }
            file.write_all(&buf[..n]).await.map_err(StorageError::Io)?;
            appended += n as u64;
        }
        file.flush().await.map_err(StorageError::Io)?;
        Ok(appended)
    }
}

/// 判断 manifest 媒体类型是否受支持（Docker schema2 与 OCI image manifest / index）。
fn is_supported_manifest_media_type(media_type: &str) -> bool {
    // 取分号前主类型，忽略可能的参数
    let main = media_type.split(';').next().unwrap_or("").trim();
    matches!(
        main,
        "application/vnd.docker.distribution.manifest.v2+json"
            | "application/vnd.oci.image.manifest.v1+json"
            | "application/vnd.docker.distribution.manifest.list.v2+json"
            | "application/vnd.oci.image.index.v1+json"
    )
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::meta::{NewRepository, RepoType, Visibility};
    use crate::storage::LocalFsStore;

    use digest::Digest;
    use tokio::io::AsyncReadExt;

    /// 算一段内容的 sha256 hex。
    fn sha256_hex(data: &[u8]) -> String {
        let mut h = sha2::Sha256::new();
        h.update(data);
        format!("{:x}", h.finalize())
    }

    /// 构造 (registry, meta, tempdir)。
    async fn 新建(
        max: Option<u64>,
    ) -> (DockerRegistry<LocalFsStore>, MetaStore, tempfile::TempDir) {
        let dir = tempfile::tempdir().unwrap();
        let meta = MetaStore::open_in_memory().await.unwrap();
        let store = LocalFsStore::new(dir.path().join("blobs")).await.unwrap();
        let reg = DockerRegistry::new(store, meta.clone(), dir.path().join("uploads"), max)
            .await
            .unwrap();
        (reg, meta, dir)
    }

    async fn 建_docker_仓库(meta: &MetaStore) -> RepositoryRecord {
        let id = meta
            .create_repository(NewRepository {
                name: "hub",
                format: "docker",
                r#type: RepoType::Hosted,
                visibility: Visibility::Public,
                upstream_url: None,
                upstream_auth_ref: None,
            })
            .await
            .unwrap();
        meta.get_repository_by_id(&id).await.unwrap().unwrap()
    }

    #[tokio::test]
    async fn blob_上传状态机_post_patch_put_可读回() {
        let (reg, meta, _d) = 新建(None).await;
        let repo = 建_docker_仓库(&meta).await;
        let content = b"docker-layer-bytes";
        let digest = docker::make_digest(&sha256_hex(content));

        // POST 启动
        let started = reg.start_upload().await.unwrap();
        // PATCH 分两段追加
        let a = reg.append_upload(&started.upload_id, &content[..9]).await.unwrap();
        assert_eq!(a.written, 9);
        let b = reg.append_upload(&started.upload_id, &content[9..]).await.unwrap();
        assert_eq!(b.written, content.len() as u64);
        // PUT 完成（校验 digest）
        let final_digest = reg
            .finish_upload(&repo, "app", &started.upload_id, &digest)
            .await
            .unwrap();
        assert_eq!(final_digest, digest);

        // HEAD / GET 可读回，内容一致
        assert_eq!(reg.stat_blob(&repo, "app", &digest).await.unwrap(), Some(content.len() as i64));
        let mut h = reg.get_blob(&repo, "app", &digest).await.unwrap();
        let mut buf = Vec::new();
        h.blob.read_to_end(&mut buf).await.unwrap();
        assert_eq!(buf, content);
        assert_eq!(h.size, content.len() as i64);
    }

    #[tokio::test]
    async fn blob_单体上传_post_put_可读回() {
        let (reg, meta, _d) = 新建(None).await;
        let repo = 建_docker_仓库(&meta).await;
        let content = b"single-shot-blob";
        let digest = docker::make_digest(&sha256_hex(content));

        let started = reg.start_upload().await.unwrap();
        // 单体：直接 append 全量再 finish（对应 POST 后 PUT 带 body）
        reg.append_upload(&started.upload_id, &content[..]).await.unwrap();
        let d = reg.finish_upload(&repo, "app", &started.upload_id, &digest).await.unwrap();
        assert_eq!(d, digest);
        assert_eq!(reg.stat_blob(&repo, "app", &digest).await.unwrap(), Some(content.len() as i64));
    }

    #[tokio::test]
    async fn put_digest_不匹配被拒且不留_blob() {
        let (reg, meta, _d) = 新建(None).await;
        let repo = 建_docker_仓库(&meta).await;
        let content = b"actual-content";
        // 故意给错 digest
        let wrong = docker::make_digest(&"0".repeat(64));

        let started = reg.start_upload().await.unwrap();
        reg.append_upload(&started.upload_id, &content[..]).await.unwrap();
        let err = reg
            .finish_upload(&repo, "app", &started.upload_id, &wrong)
            .await
            .unwrap_err();
        assert!(matches!(err, DockerError::DigestMismatch));
        // 不匹配不应留下错 digest 的索引
        let key = docker::blob_key("app", &wrong);
        assert!(meta.get_artifact(&repo.id, &key).await.unwrap().is_none());
    }

    #[tokio::test]
    async fn put_非法_digest_被拒() {
        let (reg, meta, _d) = 新建(None).await;
        let repo = 建_docker_仓库(&meta).await;
        let started = reg.start_upload().await.unwrap();
        reg.append_upload(&started.upload_id, &b"x"[..]).await.unwrap();
        let err = reg
            .finish_upload(&repo, "app", &started.upload_id, "not-a-digest")
            .await
            .unwrap_err();
        assert!(matches!(err, DockerError::InvalidDigest));
    }

    #[tokio::test]
    async fn 未知上传会话被拒() {
        let (reg, meta, _d) = 新建(None).await;
        let repo = 建_docker_仓库(&meta).await;
        let err = reg.append_upload("不存在", &b"x"[..]).await.unwrap_err();
        assert!(matches!(err, DockerError::UnknownUpload));
        let d = docker::make_digest(&"a".repeat(64));
        let err = reg.finish_upload(&repo, "app", "不存在", &d).await.unwrap_err();
        assert!(matches!(err, DockerError::UnknownUpload));
    }

    #[tokio::test]
    async fn manifest_按_tag_写入再按_tag_与_digest_读回() {
        let (reg, meta, _d) = 新建(None).await;
        let repo = 建_docker_仓库(&meta).await;
        let manifest = br#"{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.v2+json"}"#;
        let expected_digest = docker::make_digest(&sha256_hex(manifest));

        let digest = reg
            .put_manifest(&repo, "app", "1.0", MEDIA_TYPE_MANIFEST_V2, manifest.to_vec())
            .await
            .unwrap();
        assert_eq!(digest, expected_digest);

        // 按 tag 读回
        let by_tag = reg.get_manifest(&repo, "app", "1.0").await.unwrap();
        assert_eq!(by_tag.bytes, manifest);
        assert_eq!(by_tag.digest, expected_digest);
        assert_eq!(by_tag.media_type, MEDIA_TYPE_MANIFEST_V2);
        // 按 digest 读回
        let by_digest = reg.get_manifest(&repo, "app", &expected_digest).await.unwrap();
        assert_eq!(by_digest.bytes, manifest);
        assert_eq!(by_digest.digest, expected_digest);
    }

    #[tokio::test]
    async fn 同_tag_覆盖指向新_manifest() {
        let (reg, meta, _d) = 新建(None).await;
        let repo = 建_docker_仓库(&meta).await;
        let m1 = br#"{"schemaVersion":2,"v":1}"#;
        let m2 = br#"{"schemaVersion":2,"v":2,"more":"data"}"#;

        let d1 = reg.put_manifest(&repo, "app", "latest", MEDIA_TYPE_MANIFEST_V2, m1.to_vec()).await.unwrap();
        let d2 = reg.put_manifest(&repo, "app", "latest", MEDIA_TYPE_MANIFEST_V2, m2.to_vec()).await.unwrap();
        assert_ne!(d1, d2, "不同内容应有不同 digest");

        // 同 tag 覆盖：latest 现在指向 m2
        let now = reg.get_manifest(&repo, "app", "latest").await.unwrap();
        assert_eq!(now.bytes, m2);
        assert_eq!(now.digest, d2);
        // 旧 manifest 仍可按 digest 取得（按 digest 不可变）
        let old = reg.get_manifest(&repo, "app", &d1).await.unwrap();
        assert_eq!(old.bytes, m1);
    }

    #[tokio::test]
    async fn 不支持的_manifest_媒体类型被拒() {
        let (reg, meta, _d) = 新建(None).await;
        let repo = 建_docker_仓库(&meta).await;
        let err = reg
            .put_manifest(&repo, "app", "1.0", "text/plain", b"{}".to_vec())
            .await
            .unwrap_err();
        assert!(matches!(err, DockerError::UnsupportedMediaType));
    }

    #[tokio::test]
    async fn 读不存在的_blob_与_manifest_返回_notfound() {
        let (reg, meta, _d) = 新建(None).await;
        let repo = 建_docker_仓库(&meta).await;
        let d = docker::make_digest(&"b".repeat(64));
        assert!(matches!(
            reg.get_blob(&repo, "app", &d).await.unwrap_err(),
            DockerError::NotFound
        ));
        assert_eq!(reg.stat_blob(&repo, "app", &d).await.unwrap(), None);
        assert!(matches!(
            reg.get_manifest(&repo, "app", "no-such-tag").await.unwrap_err(),
            DockerError::NotFound
        ));
    }

    #[tokio::test]
    async fn 上传超限返回_too_large() {
        let (reg, meta, _d) = 新建(Some(4)).await;
        let _repo = 建_docker_仓库(&meta).await;
        let started = reg.start_upload().await.unwrap();
        // 上限 4 字节，写 10 字节应被拒
        let err = reg
            .append_upload(&started.upload_id, &b"0123456789"[..])
            .await
            .unwrap_err();
        assert!(matches!(err, DockerError::TooLarge));
        // 取消会话清理临时文件
        reg.cancel_upload(&started.upload_id).await;
    }

    #[tokio::test]
    async fn manifest_按_digest_引用与内容不符被拒() {
        let (reg, meta, _d) = 新建(None).await;
        let repo = 建_docker_仓库(&meta).await;
        let manifest = b"{}";
        // 用一个不匹配内容的 digest 作为 reference
        let wrong = docker::make_digest(&"c".repeat(64));
        let err = reg
            .put_manifest(&repo, "app", &wrong, MEDIA_TYPE_MANIFEST_V2, manifest.to_vec())
            .await
            .unwrap_err();
        assert!(matches!(err, DockerError::DigestMismatch));
    }
}
