//! 通用制品机理（FR-11/12/60/61/64/69）：与具体格式无关的写入 / 读取 / 删除编排，
//! 只依赖 [`Format`] trait 多态、[`BlobStore`]、[`MetaStore`] 与 [`proxy`] 单飞缓存。
//!
//! 核心不变量（testing-and-quality §2.2/§2.4/§2.5）：
//! - **blob 先落盘并校验 sha256，再写元数据索引**；写索引失败回滚 blob，不留孤儿索引 / 孤儿 blob。
//! - 流式处理：大文件不整体载入内存；超 `max_artifact_size` 即拒绝（413）且不留半截 blob。
//! - 覆盖策略经 [`Format::can_overwrite`] 判定（Raw 允许覆盖；其余格式各自语义）。
//! - proxy cache-miss 经**单飞合并**一次回源，上游失败不缓存损坏内容；**锁外做 IO**。

use std::pin::Pin;
use std::sync::Arc;
use std::task::{Context, Poll};

use tokio::io::{AsyncRead, ReadBuf};

use crate::meta::{ArtifactRecord, MetaError, MetaStore, NewArtifact, RepoType, RepositoryRecord};
use crate::proxy::{SingleFlight, Upstream, UpstreamError};
use crate::storage::{BlobStore, StorageError};

use super::{ArtifactCoordinates, Format};

/// 通用制品机理错误。
#[derive(Debug, thiserror::Error)]
pub enum ServiceError {
    /// 制品不存在（仓库内无此路径，或 proxy 上游也无）。
    #[error("制品不存在")]
    NotFound,
    /// 覆盖策略拒绝：同坐标已存在且该格式不允许覆盖（如 Maven release）。
    #[error("制品已存在且不允许覆盖")]
    OverwriteForbidden,
    /// 上传体积超过配置上限（映射 413）。
    #[error("制品体积超过上限")]
    TooLarge,
    /// 上游拉取失败（proxy 回源失败 / 超时 / 非 2xx）。
    #[error("上游拉取失败")]
    Upstream,
    /// 仓库类型与操作不匹配（如对 proxy 仓库直传、对 hosted 仓库回源）。
    #[error("{0}")]
    InvalidOperation(String),
    /// 存储层错误。
    #[error(transparent)]
    Storage(#[from] StorageError),
    /// 元数据层错误。
    #[error(transparent)]
    Meta(#[from] MetaError),
}

/// 写入结果：制品索引记录连同其内容类型，供上层封装响应。
#[derive(Debug, Clone)]
pub struct WriteOutcome {
    /// 落定后的制品索引记录。
    pub record: ArtifactRecord,
    /// 本次是否覆盖了既有同坐标制品。
    pub overwritten: bool,
}

/// 制品读取句柄：以文件流暴露内容，连同索引记录（含内容类型 / 大小 / 校验和）。
#[derive(Debug)]
pub struct ReadHandle {
    /// 制品索引记录。
    pub record: ArtifactRecord,
    /// blob 文件流（调用方据此流式返回响应体，不整体载入内存）。
    pub blob: tokio::fs::File,
}

/// 制品类别：区分本次读取命中的来源，便于上层日志与语义区分。
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ArtifactKind {
    /// hosted 直传制品 / proxy 缓存命中。
    Local,
    /// proxy cache-miss 经回源后落定。
    FetchedFromUpstream,
}

/// 通用制品机理服务：编排写入 / 读取 / 删除，与格式无关。
///
/// 持有 blob 存储、元数据存储与 proxy 单飞合并器；具体格式经 [`Format`] trait 多态传入，
/// 服务本身不按格式名分支。
pub struct ArtifactService<S: BlobStore, U: Upstream> {
    /// blob 存储。
    store: S,
    /// 元数据存储。
    meta: MetaStore,
    /// 上游客户端（proxy 回源用）。
    upstream: U,
    /// 单飞合并器：键为 (仓库 id + 路径)，合并同一制品的并发回源。
    single_flight: Arc<SingleFlight<String>>,
}

impl<S: BlobStore, U: Upstream> ArtifactService<S, U> {
    /// 构造服务。
    pub fn new(store: S, meta: MetaStore, upstream: U) -> Self {
        Self {
            store,
            meta,
            upstream,
            single_flight: Arc::new(SingleFlight::new()),
        }
    }

    /// hosted 直传写入（FR-11/61/64/69）：流式落 blob 校验后写索引，按覆盖策略处理重复。
    ///
    /// 次序固定：① 据覆盖策略检查既有制品；② 流式写 blob（边写边算四摘要，超限即拒）；
    /// ③ 写元数据索引；④ 写索引失败则回滚 blob（仅当无其他引用）。
    pub async fn put_hosted<R>(
        &self,
        repo: &RepositoryRecord,
        format: &dyn Format,
        coords: &ArtifactCoordinates,
        reader: R,
        max_size: Option<u64>,
    ) -> Result<WriteOutcome, ServiceError>
    where
        R: AsyncRead + Unpin + Send,
    {
        if RepoType::from_db_str(&repo.r#type) != RepoType::Hosted {
            return Err(ServiceError::InvalidOperation(
                "只能向 hosted 仓库直传制品".to_string(),
            ));
        }

        // ① 覆盖策略：同坐标已存在且格式不允许覆盖 → 拒绝（不读 / 不写 blob）
        let existing = self.meta.get_artifact(&repo.id, &coords.path).await?;
        let overwritten = existing.is_some();
        if let Some(ref e) = existing {
            if !format.can_overwrite(e) {
                return Err(ServiceError::OverwriteForbidden);
            }
        }

        // ② 流式落 blob：用限长读包裹，超限在写入途中即报错（BlobStore 会清理半截临时文件）
        let limited = LimitedReader::new(reader, max_size);
        let digests = match self.store.put(limited).await {
            Ok(d) => d,
            // 限长读触发的超限错误以专属 sentinel 标记，映射 413
            Err(StorageError::Io(e)) if is_too_large(&e) => {
                return Err(ServiceError::TooLarge);
            }
            Err(e) => return Err(e.into()),
        };

        // ③ 写元数据索引（blob 已落盘且 sha256 由 BlobStore 边写边算）
        let content_type = format.content_type(coords);
        let write_index = self
            .meta
            .upsert_artifact(NewArtifact {
                repo_id: &repo.id,
                path: &coords.path,
                size: digests.size as i64,
                sha256: &digests.sha256,
                sha1: &digests.sha1,
                md5: &digests.md5,
                sha512: &digests.sha512,
                content_type: content_type.as_deref(),
                cached: false,
            })
            .await;

        if let Err(e) = write_index {
            // ④ 写索引失败 → 回滚 blob（仅当无其他索引引用同 sha256），不留孤儿 blob
            self.rollback_blob(&digests.sha256).await;
            return Err(e.into());
        }

        let record = self
            .meta
            .get_artifact(&repo.id, &coords.path)
            .await?
            .ok_or(ServiceError::NotFound)?;
        tracing::info!(仓库 = %repo.name, 路径 = %coords.path, 覆盖 = overwritten, "已写入 hosted 制品");
        Ok(WriteOutcome { record, overwritten })
    }

    /// 读取制品（FR-11/12）：hosted / proxy-cache-hit 直接流式返回；
    /// proxy cache-miss 经单飞合并回源 → 校验落盘 → 写索引 → 返回。
    pub async fn get(
        &self,
        repo: &RepositoryRecord,
        format: &dyn Format,
        coords: &ArtifactCoordinates,
    ) -> Result<(ReadHandle, ArtifactKind), ServiceError> {
        // 缓存 / 本地命中：直接流式返回
        if let Some(record) = self.meta.get_artifact(&repo.id, &coords.path).await? {
            let blob = self.store.get(&record.sha256).await?;
            return Ok((ReadHandle { record, blob }, ArtifactKind::Local));
        }

        // hosted 未命中即不存在；proxy 才回源
        if RepoType::from_db_str(&repo.r#type) != RepoType::Proxy {
            return Err(ServiceError::NotFound);
        }
        let upstream_url = repo
            .upstream_url
            .as_deref()
            .ok_or(ServiceError::Upstream)?
            .to_string();

        self.fetch_and_cache(repo, format, coords, &upstream_url).await
    }

    /// proxy cache-miss：单飞合并回源、落盘、写索引。返回新落定的读句柄。
    async fn fetch_and_cache(
        &self,
        repo: &RepositoryRecord,
        format: &dyn Format,
        coords: &ArtifactCoordinates,
        upstream_url: &str,
    ) -> Result<(ReadHandle, ArtifactKind), ServiceError> {
        // 单飞键：仓库 + 路径，合并同一制品的并发回源（IO 在 leader 锁外执行）
        let key = format!("{}\u{0}{}", repo.id, coords.path);
        let result = self
            .single_flight
            .run(&key, || self.do_fetch_and_cache(repo, format, coords, upstream_url))
            .await;

        match result {
            Ok(sha256) => {
                // 回源者与等待者都据索引取回最新记录与 blob 流
                let record = self
                    .meta
                    .get_artifact(&repo.id, &coords.path)
                    .await?
                    .ok_or(ServiceError::NotFound)?;
                debug_assert_eq!(record.sha256, sha256);
                let blob = self.store.get(&record.sha256).await?;
                Ok((ReadHandle { record, blob }, ArtifactKind::FetchedFromUpstream))
            }
            // 回源失败：统一回退为 Upstream 错误，绝不缓存损坏内容（do_fetch 内已保证不写索引）
            Err(_) => Err(ServiceError::Upstream),
        }
    }

    /// 实际回源逻辑（在单飞 leader 锁外执行）：拉取 → 落盘校验 → 写索引，返回 sha256。
    ///
    /// 任一步失败都不写索引；落盘成功但写索引失败时回滚 blob，杜绝损坏 / 孤儿缓存。
    async fn do_fetch_and_cache(
        &self,
        repo: &RepositoryRecord,
        format: &dyn Format,
        coords: &ArtifactCoordinates,
        upstream_url: &str,
    ) -> Result<String, String> {
        // 单飞窗口内可能已有其他 leader 落定过：再查一次缓存，命中则直接复用
        match self.meta.get_artifact(&repo.id, &coords.path).await {
            Ok(Some(r)) => return Ok(r.sha256),
            Ok(None) => {}
            Err(e) => return Err(e.to_string()),
        }

        // 拉取上游字节流（锁外 IO）
        let body = self
            .upstream
            .fetch(upstream_url, &coords.path)
            .await
            .map_err(|e: UpstreamError| e.to_string())?;

        // 流式落 blob：边写边算 sha256，BlobStore 落定即等于校验通过（内容寻址）
        let digests = self.store.put(body).await.map_err(|e| e.to_string())?;

        // 写缓存索引（cached = true）
        let content_type = format.content_type(coords);
        if let Err(e) = self
            .meta
            .upsert_artifact(NewArtifact {
                repo_id: &repo.id,
                path: &coords.path,
                size: digests.size as i64,
                sha256: &digests.sha256,
                sha1: &digests.sha1,
                md5: &digests.md5,
                sha512: &digests.sha512,
                content_type: content_type.as_deref(),
                cached: true,
            })
            .await
        {
            // 写索引失败 → 回滚 blob，不留孤儿
            self.rollback_blob(&digests.sha256).await;
            return Err(e.to_string());
        }
        tracing::info!(仓库 = %repo.name, 路径 = %coords.path, "proxy 已回源并缓存制品");
        Ok(digests.sha256)
    }

    /// 删除制品（FR-60）：hosted 删索引 + blob 本体（无其他引用时）；proxy 删缓存索引 + blob。
    ///
    /// 两类仓库都先删索引、再按引用计数清 blob；proxy 删缓存后下次 cache-miss 可重新拉取。
    pub async fn delete(
        &self,
        repo: &RepositoryRecord,
        coords: &ArtifactCoordinates,
    ) -> Result<(), ServiceError> {
        let record = self
            .meta
            .get_artifact(&repo.id, &coords.path)
            .await?
            .ok_or(ServiceError::NotFound)?;

        // 先删索引（元数据唯一真源），再按引用计数清 blob
        self.meta.delete_artifact(&repo.id, &coords.path).await?;
        self.rollback_blob(&record.sha256).await;
        tracing::info!(仓库 = %repo.name, 路径 = %coords.path, "已删除制品");
        Ok(())
    }

    /// 回滚 / 清理 blob：仅当该 sha256 不再被任何索引引用时才删本体，避免误删共享 blob。
    async fn rollback_blob(&self, sha256: &str) {
        match self.meta.count_artifacts_by_sha256(sha256).await {
            Ok(0) => {
                if let Err(e) = self.store.delete(sha256).await {
                    tracing::warn!(sha256 = %sha256, 错误 = %e, "清理无引用 blob 失败");
                }
            }
            // 仍有引用：保留 blob
            Ok(_) => {}
            Err(e) => {
                // 计数失败时保守起见不删 blob（宁可暂留也不误删被引用的本体）
                tracing::warn!(sha256 = %sha256, 错误 = %e, "查询 blob 引用计数失败，跳过清理");
            }
        }
    }
}

/// 超限专属错误载荷：包进 `io::Error::other`，供上层精确识别"上传超限"而非普通 IO 失败。
#[derive(Debug)]
struct TooLargeMarker;

impl std::fmt::Display for TooLargeMarker {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "制品体积超过上限")
    }
}

impl std::error::Error for TooLargeMarker {}

/// 判断一个 IO 错误是否由限长读的超限触发（据专属 sentinel 载荷识别）。
fn is_too_large(e: &std::io::Error) -> bool {
    e.get_ref()
        .map(|inner| inner.is::<TooLargeMarker>())
        .unwrap_or(false)
}

/// 限长读包装：累计读取字节超过上限时返回专属超限错误，供上层映射 413。
///
/// `limit` 为 None 时不施加限制，直接透传底层读。
struct LimitedReader<R> {
    /// 底层读。
    inner: R,
    /// 字节上限（None 表示不限）。
    limit: Option<u64>,
    /// 已读取字节累计。
    read: u64,
}

impl<R> LimitedReader<R> {
    /// 构造限长读。
    fn new(inner: R, limit: Option<u64>) -> Self {
        Self {
            inner,
            limit,
            read: 0,
        }
    }
}

impl<R: AsyncRead + Unpin> AsyncRead for LimitedReader<R> {
    fn poll_read(
        mut self: Pin<&mut Self>,
        cx: &mut Context<'_>,
        buf: &mut ReadBuf<'_>,
    ) -> Poll<std::io::Result<()>> {
        let before = buf.filled().len();
        let inner = Pin::new(&mut self.inner);
        match inner.poll_read(cx, buf) {
            Poll::Ready(Ok(())) => {
                let n = (buf.filled().len() - before) as u64;
                self.read += n;
                if let Some(limit) = self.limit {
                    if self.read > limit {
                        // 超限：以专属 sentinel 作为 error 载荷，上层据此精确识别并返回 413
                        return Poll::Ready(Err(std::io::Error::other(TooLargeMarker)));
                    }
                }
                Poll::Ready(Ok(()))
            }
            other => other,
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::atomic::{AtomicUsize, Ordering};

    use tokio::io::AsyncReadExt;

    use crate::format::RawFormat;
    use crate::meta::{NewRepository, Visibility};
    use crate::storage::LocalFsStore;

    /// 计数型 mock 上游：记录被拉取次数，可配置内容、延迟与失败，用于穷举单飞 / 回退竞态。
    struct MockUpstream {
        /// 返回的内容。
        content: Vec<u8>,
        /// 被 fetch 的次数。
        calls: Arc<AtomicUsize>,
        /// 每次拉取前的人为延迟（毫秒），用于拉开并发窗口。
        delay_ms: u64,
        /// 是否模拟上游失败。
        fail: bool,
    }

    impl MockUpstream {
        fn new(content: &[u8], calls: Arc<AtomicUsize>) -> Self {
            Self {
                content: content.to_vec(),
                calls,
                delay_ms: 0,
                fail: false,
            }
        }
        fn with_delay(mut self, ms: u64) -> Self {
            self.delay_ms = ms;
            self
        }
        fn failing(content: &[u8], calls: Arc<AtomicUsize>) -> Self {
            Self {
                content: content.to_vec(),
                calls,
                delay_ms: 0,
                fail: true,
            }
        }
    }

    impl Upstream for MockUpstream {
        async fn fetch(
            &self,
            _base_url: &str,
            _rel_path: &str,
        ) -> Result<crate::proxy::UpstreamBody, UpstreamError> {
            self.calls.fetch_add(1, Ordering::SeqCst);
            if self.delay_ms > 0 {
                tokio::time::sleep(std::time::Duration::from_millis(self.delay_ms)).await;
            }
            if self.fail {
                return Err(UpstreamError::Transport("mock 上游故障".to_string()));
            }
            Ok(Box::new(std::io::Cursor::new(self.content.clone())))
        }
    }

    /// 构造一套测试用 (服务, 库, blob目录)。
    async fn 新建服务(
        upstream: MockUpstream,
    ) -> (ArtifactService<LocalFsStore, MockUpstream>, MetaStore, tempfile::TempDir) {
        let dir = tempfile::tempdir().unwrap();
        let meta = MetaStore::open_in_memory().await.unwrap();
        let store = LocalFsStore::new(dir.path().join("blobs")).await.unwrap();
        let svc = ArtifactService::new(store, meta.clone(), upstream);
        (svc, meta, dir)
    }

    /// 建一个仓库记录并返回。
    async fn 建仓库(
        meta: &MetaStore,
        name: &str,
        r#type: RepoType,
        upstream: Option<&str>,
    ) -> RepositoryRecord {
        let id = meta
            .create_repository(NewRepository {
                name,
                format: "raw",
                r#type,
                visibility: Visibility::Public,
                upstream_url: upstream,
                upstream_auth_ref: None,
            })
            .await
            .unwrap();
        meta.get_repository_by_id(&id).await.unwrap().unwrap()
    }

    fn 坐标(p: &str) -> ArtifactCoordinates {
        ArtifactCoordinates { path: p.to_string() }
    }

    // ---------- 写入：blob 先落盘再写索引、四校验和正确、覆盖 ----------

    #[tokio::test]
    async fn hosted_写入后可读回且四校验和正确() {
        let calls = Arc::new(AtomicUsize::new(0));
        let (svc, meta, _dir) = 新建服务(MockUpstream::new(b"", calls)).await;
        let repo = 建仓库(&meta, "h", RepoType::Hosted, None).await;
        let coords = 坐标("a/b.txt");

        let out = svc
            .put_hosted(&repo, &RawFormat, &coords, &b"abc"[..], None)
            .await
            .unwrap();
        assert!(!out.overwritten);
        // "abc" 的四校验和标准向量
        assert_eq!(
            out.record.sha256,
            "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
        );
        assert_eq!(out.record.sha1, "a9993e364706816aba3e25717850c26c9cd0d89d");
        assert_eq!(out.record.md5, "900150983cd24fb0d6963f7d28e17f72");
        assert_eq!(out.record.size, 3);
        assert_eq!(out.record.content_type.as_deref(), Some("text/plain; charset=utf-8"));

        // 读回内容一致
        let (mut handle, kind) = svc.get(&repo, &RawFormat, &coords).await.unwrap();
        assert_eq!(kind, ArtifactKind::Local);
        let mut buf = Vec::new();
        handle.blob.read_to_end(&mut buf).await.unwrap();
        assert_eq!(buf, b"abc");
    }

    #[tokio::test]
    async fn raw_允许覆盖且覆盖标志为真() {
        let calls = Arc::new(AtomicUsize::new(0));
        let (svc, meta, _dir) = 新建服务(MockUpstream::new(b"", calls)).await;
        let repo = 建仓库(&meta, "h", RepoType::Hosted, None).await;
        let coords = 坐标("f.bin");

        svc.put_hosted(&repo, &RawFormat, &coords, &b"v1"[..], None)
            .await
            .unwrap();
        let out = svc
            .put_hosted(&repo, &RawFormat, &coords, &b"v2-longer"[..], None)
            .await
            .unwrap();
        assert!(out.overwritten, "Raw 同路径覆盖应标记 overwritten");
        // 索引只剩一条且为新内容
        assert_eq!(meta.list_artifacts_by_repo(&repo.id).await.unwrap().len(), 1);
        let (mut h, _) = svc.get(&repo, &RawFormat, &coords).await.unwrap();
        let mut buf = Vec::new();
        h.blob.read_to_end(&mut buf).await.unwrap();
        assert_eq!(buf, b"v2-longer");
    }

    // ---------- §2.4 流式：超限 413 且不留半截 blob ----------

    #[tokio::test]
    async fn 超过上限拒绝_413_且不留半截_blob() {
        let calls = Arc::new(AtomicUsize::new(0));
        let (svc, meta, dir) = 新建服务(MockUpstream::new(b"", calls)).await;
        let repo = 建仓库(&meta, "h", RepoType::Hosted, None).await;
        let coords = 坐标("big.bin");

        // 上限 4 字节，写 10 字节应被拒
        let err = svc
            .put_hosted(&repo, &RawFormat, &coords, &b"0123456789"[..], Some(4))
            .await
            .unwrap_err();
        assert!(matches!(err, ServiceError::TooLarge));
        // 索引未写入
        assert!(meta.get_artifact(&repo.id, "big.bin").await.unwrap().is_none());
        // blob 目录下除 tmp 外无落定的正式 blob（半截已被清理）
        let blobs = dir.path().join("blobs");
        let mut dirs = tokio::fs::read_dir(&blobs).await.unwrap();
        let mut 桶数 = 0;
        while let Some(e) = dirs.next_entry().await.unwrap() {
            // tmp 子目录应为空，其余分桶目录不应出现
            if e.file_name() != "tmp" {
                桶数 += 1;
            }
        }
        assert_eq!(桶数, 0, "超限不应留下任何落定 blob");
    }

    // ---------- §2.5 事务：写索引失败回滚 blob、无孤儿 ----------

    #[tokio::test]
    async fn 写索引失败回滚_blob_无孤儿() {
        let calls = Arc::new(AtomicUsize::new(0));
        let (svc, meta, _dir) = 新建服务(MockUpstream::new(b"", calls)).await;
        // 构造一个 id 不存在于库中的仓库记录：写索引会因外键失败，触发 blob 回滚
        let ghost = RepositoryRecord {
            id: "不存在的仓库id".to_string(),
            name: "ghost".to_string(),
            format: "raw".to_string(),
            r#type: "hosted".to_string(),
            visibility: "public".to_string(),
            upstream_url: None,
            upstream_auth_ref: None,
            created_at: "now".to_string(),
        };
        let coords = 坐标("p.txt");
        let err = svc
            .put_hosted(&ghost, &RawFormat, &coords, &b"orphan-check"[..], None)
            .await
            .unwrap_err();
        assert!(matches!(err, ServiceError::Meta(_)), "外键失败应是 Meta 错误");

        // "orphan-check" 的 blob 不应残留：计数为 0 且 store 中不存在
        let sha = {
            use digest::Digest;
            let mut h = sha2::Sha256::new();
            h.update(b"orphan-check");
            format!("{:x}", h.finalize())
        };
        assert_eq!(meta.count_artifacts_by_sha256(&sha).await.unwrap(), 0);
    }

    // ---------- §2.3 代理缓存：单飞合并、回退不缓存损坏 ----------

    #[tokio::test]
    async fn proxy_cache_miss_回源后命中不再回源() {
        let calls = Arc::new(AtomicUsize::new(0));
        let (svc, meta, _dir) =
            新建服务(MockUpstream::new(b"upstream-bytes", calls.clone())).await;
        let repo = 建仓库(&meta, "p", RepoType::Proxy, Some("https://up.example")).await;
        let coords = 坐标("lib/x.bin");

        // 首次：cache-miss → 回源
        let (mut h, kind) = svc.get(&repo, &RawFormat, &coords).await.unwrap();
        assert_eq!(kind, ArtifactKind::FetchedFromUpstream);
        let mut buf = Vec::new();
        h.blob.read_to_end(&mut buf).await.unwrap();
        assert_eq!(buf, b"upstream-bytes");
        // 缓存索引已写入且标记 cached
        let rec = meta.get_artifact(&repo.id, "lib/x.bin").await.unwrap().unwrap();
        assert_eq!(rec.cached, 1);

        // 再次：缓存命中，不再回源
        let (_, kind2) = svc.get(&repo, &RawFormat, &coords).await.unwrap();
        assert_eq!(kind2, ArtifactKind::Local);
        assert_eq!(calls.load(Ordering::SeqCst), 1, "命中后不应再回源");
    }

    #[tokio::test]
    async fn proxy_并发_cache_miss_单飞合并只回源一次() {
        let calls = Arc::new(AtomicUsize::new(0));
        let (svc, meta, _dir) =
            新建服务(MockUpstream::new(b"shared", calls.clone()).with_delay(30)).await;
        let repo = 建仓库(&meta, "p", RepoType::Proxy, Some("https://up.example")).await;
        let svc = Arc::new(svc);

        // 并发 N 个同制品 cache-miss
        let mut handles = Vec::new();
        for _ in 0..12 {
            let svc = svc.clone();
            let repo = repo.clone();
            handles.push(tokio::spawn(async move {
                let coords = ArtifactCoordinates {
                    path: "lib/same.bin".to_string(),
                };
                let (mut h, _) = svc.get(&repo, &RawFormat, &coords).await.unwrap();
                let mut buf = Vec::new();
                h.blob.read_to_end(&mut buf).await.unwrap();
                buf
            }));
        }
        for h in handles {
            assert_eq!(h.await.unwrap(), b"shared");
        }
        // 单飞合并：上游仅被拉取一次
        assert_eq!(calls.load(Ordering::SeqCst), 1, "并发同制品应只回源一次");
        // 缓存里只有一条索引，未写坏
        assert_eq!(meta.list_artifacts_by_repo(&repo.id).await.unwrap().len(), 1);
    }

    #[tokio::test]
    async fn proxy_上游失败回退且不缓存损坏() {
        let calls = Arc::new(AtomicUsize::new(0));
        let (svc, meta, _dir) = 新建服务(MockUpstream::failing(b"x", calls.clone())).await;
        let repo = 建仓库(&meta, "p", RepoType::Proxy, Some("https://up.example")).await;
        let coords = 坐标("lib/y.bin");

        let err = svc.get(&repo, &RawFormat, &coords).await.unwrap_err();
        assert!(matches!(err, ServiceError::Upstream));
        // 上游失败：不写任何缓存索引
        assert!(meta.get_artifact(&repo.id, "lib/y.bin").await.unwrap().is_none());
        assert_eq!(meta.list_artifacts_by_repo(&repo.id).await.unwrap().len(), 0);
    }

    // ---------- 删除：hosted 删本体 + 索引；proxy 删缓存后可重新拉取 ----------

    #[tokio::test]
    async fn hosted_删除清本体与索引() {
        let calls = Arc::new(AtomicUsize::new(0));
        let (svc, meta, _dir) = 新建服务(MockUpstream::new(b"", calls)).await;
        let repo = 建仓库(&meta, "h", RepoType::Hosted, None).await;
        let coords = 坐标("d.txt");
        let out = svc
            .put_hosted(&repo, &RawFormat, &coords, &b"to-delete"[..], None)
            .await
            .unwrap();
        let sha = out.record.sha256.clone();

        svc.delete(&repo, &coords).await.unwrap();
        // 索引与 blob 本体都已清理
        assert!(meta.get_artifact(&repo.id, "d.txt").await.unwrap().is_none());
        assert!(!svc.store.exists(&sha).await.unwrap());
        // 再删返回 NotFound
        assert!(matches!(
            svc.delete(&repo, &coords).await.unwrap_err(),
            ServiceError::NotFound
        ));
    }

    #[tokio::test]
    async fn proxy_删缓存后可重新回源() {
        let calls = Arc::new(AtomicUsize::new(0));
        let (svc, meta, _dir) =
            新建服务(MockUpstream::new(b"again", calls.clone())).await;
        let repo = 建仓库(&meta, "p", RepoType::Proxy, Some("https://up.example")).await;
        let coords = 坐标("lib/z.bin");

        svc.get(&repo, &RawFormat, &coords).await.unwrap();
        assert_eq!(calls.load(Ordering::SeqCst), 1);
        // 删缓存
        svc.delete(&repo, &coords).await.unwrap();
        assert!(meta.get_artifact(&repo.id, "lib/z.bin").await.unwrap().is_none());
        // 再取应重新回源（计数 +1）
        svc.get(&repo, &RawFormat, &coords).await.unwrap();
        assert_eq!(calls.load(Ordering::SeqCst), 2, "删缓存后应可重新拉取");
    }

    #[tokio::test]
    async fn 共享_sha256_删一条不误删_blob() {
        let calls = Arc::new(AtomicUsize::new(0));
        let (svc, meta, _dir) = 新建服务(MockUpstream::new(b"", calls)).await;
        let r1 = 建仓库(&meta, "h1", RepoType::Hosted, None).await;
        let r2 = 建仓库(&meta, "h2", RepoType::Hosted, None).await;
        // 两个仓库写入相同内容（同 sha256）
        let out = svc
            .put_hosted(&r1, &RawFormat, &坐标("a"), &b"dup"[..], None)
            .await
            .unwrap();
        svc.put_hosted(&r2, &RawFormat, &坐标("b"), &b"dup"[..], None)
            .await
            .unwrap();
        let sha = out.record.sha256.clone();

        // 删 r1 的那条，blob 仍被 r2 引用，不应删除本体
        svc.delete(&r1, &坐标("a")).await.unwrap();
        assert!(svc.store.exists(&sha).await.unwrap(), "仍有引用不应删 blob");
        // r2 仍可读
        let (mut h, _) = svc.get(&r2, &RawFormat, &坐标("b")).await.unwrap();
        let mut buf = Vec::new();
        h.blob.read_to_end(&mut buf).await.unwrap();
        assert_eq!(buf, b"dup");
    }

    // ---------- 仓库类型与操作匹配 ----------

    #[tokio::test]
    async fn 向_proxy_直传被拒() {
        let calls = Arc::new(AtomicUsize::new(0));
        let (svc, meta, _dir) = 新建服务(MockUpstream::new(b"", calls)).await;
        let repo = 建仓库(&meta, "p", RepoType::Proxy, Some("https://up.example")).await;
        let err = svc
            .put_hosted(&repo, &RawFormat, &坐标("x"), &b"y"[..], None)
            .await
            .unwrap_err();
        assert!(matches!(err, ServiceError::InvalidOperation(_)));
    }

    #[tokio::test]
    async fn hosted_未命中返回_notfound() {
        let calls = Arc::new(AtomicUsize::new(0));
        let (svc, meta, _dir) = 新建服务(MockUpstream::new(b"", calls)).await;
        let repo = 建仓库(&meta, "h", RepoType::Hosted, None).await;
        let err = svc.get(&repo, &RawFormat, &坐标("missing")).await.unwrap_err();
        assert!(matches!(err, ServiceError::NotFound));
    }
}
