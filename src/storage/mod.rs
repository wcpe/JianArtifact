//! blob 存储层：制品本体落文件系统，DB 仅存索引与 sha256（ADR-0002）。
//!
//! 写入语义：先写临时文件并边写边算四种摘要，校验通过后再原子落定到以 sha256
//! 寻址的最终路径；任何中断都不会留下半截正式 blob（FR-69 多校验和、流式处理）。

use std::path::PathBuf;

use digest::Digest;
use md5::Md5;
use sha1::Sha1;
use sha2::{Sha256, Sha512};
use tokio::io::{AsyncRead, AsyncReadExt, AsyncWriteExt};

#[cfg(feature = "s3")]
mod s3;
#[cfg(feature = "s3")]
pub use s3::{S3Config, S3Store};

/// blob 读取句柄的统一类型：装箱的异步读流。
///
/// 两种后端各自返回不同的具体读句柄（本地是文件、S3 是网络字节流），统一装箱为该 trait 对象，
/// 使上层流式下载逻辑对后端无感（ADR-0014 §6）。
pub type BlobReader = Box<dyn AsyncRead + Send + Unpin>;

/// 单次流式写入算得的四种摘要与字节数。
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct BlobDigests {
    /// 内容字节数。
    pub size: u64,
    /// sha256（同时用于 blob 寻址）。
    pub sha256: String,
    /// sha1（主要为客户端兼容）。
    pub sha1: String,
    /// md5（主要为客户端兼容）。
    pub md5: String,
    /// sha512。
    pub sha512: String,
}

/// 存储层错误。
#[derive(Debug, thiserror::Error)]
pub enum StorageError {
    /// 底层 IO 错误。
    #[error("blob 存储 IO 失败: {0}")]
    Io(#[from] std::io::Error),
    /// 请求的 blob 不存在。
    #[error("blob 不存在: {0}")]
    NotFound(String),
    /// 对象存储后端错误（仅 S3 后端，消息已脱敏不含凭据）。
    #[cfg(feature = "s3")]
    #[error("对象存储后端失败: {0}")]
    Backend(String),
}

/// blob 存储抽象。本地文件系统为默认实现，S3 兼容对象存储为可选 opt-in 实现（ADR-0014）。
#[allow(async_fn_in_trait)]
pub trait BlobStore {
    /// 流式写入：从 `reader` 读取全部内容，边写边算四种摘要，
    /// 校验后落定为以 sha256 寻址的 blob，返回摘要信息。
    async fn put<R>(&self, reader: R) -> Result<BlobDigests, StorageError>
    where
        R: AsyncRead + Unpin + Send;

    /// 按 sha256 流式打开 blob 读取句柄；不存在时返回 NotFound。
    async fn get(&self, sha256: &str) -> Result<BlobReader, StorageError>;

    /// 按 sha256 删除 blob；不存在时视为成功（幂等）。
    async fn delete(&self, sha256: &str) -> Result<(), StorageError>;

    /// 判断 blob 是否存在。
    async fn exists(&self, sha256: &str) -> Result<bool, StorageError>;
}

/// 按 sha256 推导内容寻址的相对存储键：前两位分桶 + 其余作为对象名。
///
/// 本地文件系统与 S3 共用同一布局（S3 再叠加配置的 `prefix`），保证两后端寻址同构（ADR-0014 §4）。
pub fn content_key(sha256: &str) -> String {
    let (prefix, rest) = sha256.split_at(2.min(sha256.len()));
    format!("{prefix}/{rest}")
}

/// 一次读取的缓冲区大小（64 KiB），保证大文件不整体载入内存。
const READ_BUFFER_SIZE: usize = 64 * 1024;

/// 本地文件系统 blob 存储。
#[derive(Debug, Clone)]
pub struct LocalFsStore {
    /// blob 存储根目录。
    root: PathBuf,
}

impl LocalFsStore {
    /// 基于给定根目录构造，并确保根目录及临时子目录存在。
    pub async fn new(root: impl Into<PathBuf>) -> Result<Self, StorageError> {
        let root = root.into();
        tokio::fs::create_dir_all(&root).await?;
        tokio::fs::create_dir_all(root.join("tmp")).await?;
        Ok(Self { root })
    }

    /// 计算 blob 的最终落定路径：按 sha256 前两位分桶，避免单目录文件过多。
    fn blob_path(&self, sha256: &str) -> PathBuf {
        let (prefix, rest) = sha256.split_at(2.min(sha256.len()));
        self.root.join(prefix).join(rest)
    }

    /// 临时文件目录。
    fn tmp_dir(&self) -> PathBuf {
        self.root.join("tmp")
    }
}

/// 流式把 `reader` 写入临时文件 `tmp_path`，边写边算四种摘要，返回摘要信息。
///
/// 失败时清理半截临时文件并返回错误（不留垃圾）；成功时临时文件保留，由调用方负责后续落定 / 上传。
/// 本地与 S3 后端共用此逻辑，保证「先落本地校验、再按内容寻址落定」的写入语义一致（ADR-0014 §4）。
async fn stream_to_temp_file<R>(
    mut reader: R,
    tmp_path: &std::path::Path,
) -> Result<BlobDigests, StorageError>
where
    R: AsyncRead + Unpin + Send,
{
    let mut tmp_file = tokio::fs::File::create(tmp_path).await?;

    let mut sha256 = Sha256::new();
    let mut sha1 = Sha1::new();
    let mut md5 = Md5::new();
    let mut sha512 = Sha512::new();
    let mut size: u64 = 0;
    let mut buf = vec![0u8; READ_BUFFER_SIZE];

    // 流式读取 → 边写盘边喂哈希；任一步失败都清理临时文件
    let write_result = async {
        loop {
            let n = reader.read(&mut buf).await?;
            if n == 0 {
                break;
            }
            let chunk = &buf[..n];
            tmp_file.write_all(chunk).await?;
            sha256.update(chunk);
            sha1.update(chunk);
            md5.update(chunk);
            sha512.update(chunk);
            size += n as u64;
        }
        tmp_file.flush().await?;
        tmp_file.sync_all().await?;
        Ok::<(), std::io::Error>(())
    }
    .await;

    if let Err(e) = write_result {
        // 清理半截临时文件，不留垃圾
        let _ = tokio::fs::remove_file(tmp_path).await;
        return Err(StorageError::Io(e));
    }

    Ok(BlobDigests {
        size,
        sha256: hex_encode(&sha256.finalize()),
        sha1: hex_encode(&sha1.finalize()),
        md5: hex_encode(&md5.finalize()),
        sha512: hex_encode(&sha512.finalize()),
    })
}

impl BlobStore for LocalFsStore {
    async fn put<R>(&self, reader: R) -> Result<BlobDigests, StorageError>
    where
        R: AsyncRead + Unpin + Send,
    {
        // 临时文件名用随机 UUID，避免并发写互相覆盖
        let tmp_path = self.tmp_dir().join(uuid::Uuid::new_v4().to_string());
        // 流式写临时文件并算四种摘要（失败已在内部清理临时文件）
        let digests = stream_to_temp_file(reader, &tmp_path).await?;

        // 原子落定：先建分桶目录，再 rename 临时文件到最终路径
        let final_path = self.blob_path(&digests.sha256);
        if let Some(parent) = final_path.parent() {
            tokio::fs::create_dir_all(parent).await?;
        }
        // 若内容已存在（同 sha256），直接复用并删除临时文件，幂等
        if tokio::fs::try_exists(&final_path).await? {
            let _ = tokio::fs::remove_file(&tmp_path).await;
        } else {
            tokio::fs::rename(&tmp_path, &final_path).await?;
        }

        Ok(digests)
    }

    async fn get(&self, sha256: &str) -> Result<BlobReader, StorageError> {
        let path = self.blob_path(sha256);
        match tokio::fs::File::open(&path).await {
            // 装箱为统一读句柄类型，上层对后端无感（ADR-0014 §6）
            Ok(file) => Ok(Box::new(file)),
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => {
                Err(StorageError::NotFound(sha256.to_string()))
            }
            Err(e) => Err(StorageError::Io(e)),
        }
    }

    async fn delete(&self, sha256: &str) -> Result<(), StorageError> {
        let path = self.blob_path(sha256);
        match tokio::fs::remove_file(&path).await {
            Ok(()) => Ok(()),
            // 不存在视为成功，保证删除幂等
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(()),
            Err(e) => Err(StorageError::Io(e)),
        }
    }

    async fn exists(&self, sha256: &str) -> Result<bool, StorageError> {
        Ok(tokio::fs::try_exists(self.blob_path(sha256)).await?)
    }
}

/// 构造 blob 后端时的错误。
#[derive(Debug, thiserror::Error)]
pub enum BackendInitError {
    /// 初始化失败（IO / 连接 / 配置缺失等）。
    #[error("初始化 blob 存储失败: {0}")]
    Init(String),
    /// 配置选用了 S3 后端，但二进制未启用 `s3` 编译特性。
    #[error("配置选用 S3 后端，但本二进制未启用 `s3` 编译特性；请使用启用该特性的构建，或改回 backend = \"fs\"")]
    S3FeatureDisabled,
    /// 选用 S3 后端但缺少 `[data.storage.s3]` 子配置。
    #[error("配置选用 S3 后端，但缺少 [data.storage.s3] 子配置")]
    MissingS3Settings,
}

/// blob 后端运行期分发：构造期按配置二选一，上层只依赖 [`BlobStore`] trait，对后端无感（ADR-0014 §3）。
#[derive(Clone)]
pub enum BlobBackend {
    /// 本地文件系统后端（默认）。
    Fs(LocalFsStore),
    /// S3 兼容对象存储后端（可选 opt-in，仅启用 `s3` 特性时可用）。
    #[cfg(feature = "s3")]
    S3(S3Store),
}

impl BlobBackend {
    /// 按运行期配置构造 blob 后端。
    ///
    /// - `backend = "fs"`（默认）：用本地文件系统，blob 落 `local_root`，临时文件中转目录 `tmp_dir`。
    /// - `backend = "s3"`：需启用 `s3` 编译特性，否则直接报错退出（不静默回退本地，ADR-0014 §1）。
    ///
    /// `tmp_dir` 仅 S3 后端使用（上传前本地临时文件中转算 sha256）。
    pub async fn from_config(
        storage: &crate::config::StorageConfig,
        local_root: &std::path::Path,
        #[cfg_attr(not(feature = "s3"), allow(unused_variables))] tmp_dir: &std::path::Path,
    ) -> Result<Self, BackendInitError> {
        use crate::config::StorageBackend;
        match storage.backend {
            StorageBackend::Fs => {
                let store = LocalFsStore::new(local_root)
                    .await
                    .map_err(|e| BackendInitError::Init(e.to_string()))?;
                Ok(BlobBackend::Fs(store))
            }
            StorageBackend::S3 => {
                #[cfg(feature = "s3")]
                {
                    let settings = storage
                        .s3
                        .as_ref()
                        .ok_or(BackendInitError::MissingS3Settings)?;
                    let cfg = S3Config::from_settings(settings, tmp_dir);
                    let store = S3Store::connect(cfg)
                        .await
                        .map_err(|e| BackendInitError::Init(e.to_string()))?;
                    Ok(BlobBackend::S3(store))
                }
                #[cfg(not(feature = "s3"))]
                {
                    Err(BackendInitError::S3FeatureDisabled)
                }
            }
        }
    }
}

impl BlobStore for BlobBackend {
    async fn put<R>(&self, reader: R) -> Result<BlobDigests, StorageError>
    where
        R: AsyncRead + Unpin + Send,
    {
        match self {
            BlobBackend::Fs(s) => s.put(reader).await,
            #[cfg(feature = "s3")]
            BlobBackend::S3(s) => s.put(reader).await,
        }
    }

    async fn get(&self, sha256: &str) -> Result<BlobReader, StorageError> {
        match self {
            BlobBackend::Fs(s) => s.get(sha256).await,
            #[cfg(feature = "s3")]
            BlobBackend::S3(s) => s.get(sha256).await,
        }
    }

    async fn delete(&self, sha256: &str) -> Result<(), StorageError> {
        match self {
            BlobBackend::Fs(s) => s.delete(sha256).await,
            #[cfg(feature = "s3")]
            BlobBackend::S3(s) => s.delete(sha256).await,
        }
    }

    async fn exists(&self, sha256: &str) -> Result<bool, StorageError> {
        match self {
            BlobBackend::Fs(s) => s.exists(sha256).await,
            #[cfg(feature = "s3")]
            BlobBackend::S3(s) => s.exists(sha256).await,
        }
    }
}

/// 把字节切片编码为小写十六进制字符串。
fn hex_encode(bytes: &[u8]) -> String {
    use std::fmt::Write;
    let mut s = String::with_capacity(bytes.len() * 2);
    for b in bytes {
        // 写入固定宽度两位十六进制，向 String 写不会失败
        let _ = write!(s, "{:02x}", b);
    }
    s
}

#[cfg(test)]
mod tests {
    use super::*;
    use tokio::io::AsyncReadExt;

    /// 空内容的四种摘要标准向量。
    const EMPTY_SHA256: &str = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855";
    const EMPTY_SHA1: &str = "da39a3ee5e6b4b0d3255bfef95601890afd80709";
    const EMPTY_MD5: &str = "d41d8cd98f00b204e9800998ecf8427e";
    const EMPTY_SHA512: &str = "cf83e1357eefb8bdf1542850d66d8007d620e4050b5715dc83f4a921d36ce9ce47d0d13c5d85f2b0ff8318d2877eec2f63b931bd47417a81a538327af927da3e";

    /// "abc" 的四种摘要标准向量。
    const ABC_SHA256: &str = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad";
    const ABC_SHA1: &str = "a9993e364706816aba3e25717850c26c9cd0d89d";
    const ABC_MD5: &str = "900150983cd24fb0d6963f7d28e17f72";
    const ABC_SHA512: &str = "ddaf35a193617abacc417349ae20413112e6fa4e89a97ea20a9eeee64b55d39a2192992a274fc1a836ba3c23a3feebbd454d4423643ce80e2a9ac94fa54ca49f";

    async fn 新建临时存储() -> (LocalFsStore, tempfile::TempDir) {
        let dir = tempfile::tempdir().unwrap();
        let store = LocalFsStore::new(dir.path().join("blobs")).await.unwrap();
        (store, dir)
    }

    #[tokio::test]
    async fn put_对已知向量算出正确的四种摘要() {
        let (store, _dir) = 新建临时存储().await;

        let d = store.put(&b"abc"[..]).await.unwrap();
        assert_eq!(d.size, 3);
        assert_eq!(d.sha256, ABC_SHA256);
        assert_eq!(d.sha1, ABC_SHA1);
        assert_eq!(d.md5, ABC_MD5);
        assert_eq!(d.sha512, ABC_SHA512);
    }

    #[tokio::test]
    async fn put_空内容算出正确的四种摘要() {
        let (store, _dir) = 新建临时存储().await;
        let d = store.put(&b""[..]).await.unwrap();
        assert_eq!(d.size, 0);
        assert_eq!(d.sha256, EMPTY_SHA256);
        assert_eq!(d.sha1, EMPTY_SHA1);
        assert_eq!(d.md5, EMPTY_MD5);
        assert_eq!(d.sha512, EMPTY_SHA512);
    }

    #[tokio::test]
    async fn put_后可_get_回完全相同的内容() {
        let (store, _dir) = 新建临时存储().await;
        let content = b"JianArtifact blob roundtrip";
        let d = store.put(&content[..]).await.unwrap();

        let mut file = store.get(&d.sha256).await.unwrap();
        let mut read_back = Vec::new();
        file.read_to_end(&mut read_back).await.unwrap();
        assert_eq!(read_back, content);
    }

    #[tokio::test]
    async fn exists_反映_put_与_delete() {
        let (store, _dir) = 新建临时存储().await;
        let d = store.put(&b"hello"[..]).await.unwrap();
        assert!(store.exists(&d.sha256).await.unwrap());

        store.delete(&d.sha256).await.unwrap();
        assert!(!store.exists(&d.sha256).await.unwrap());
    }

    #[tokio::test]
    async fn get_不存在的_blob_返回_notfound() {
        let (store, _dir) = 新建临时存储().await;
        // BlobReader（装箱读流）不实现 Debug，故不用 unwrap_err，改用 match 断言错误变体
        match store.get(EMPTY_SHA256).await {
            Err(StorageError::NotFound(_)) => {}
            other => panic!("期望 NotFound，实际: {:?}", other.map(|_| "<字节流>")),
        }
    }

    #[tokio::test]
    async fn delete_不存在的_blob_幂等成功() {
        let (store, _dir) = 新建临时存储().await;
        // 删除从未写入的 blob 不应报错
        store.delete(ABC_SHA256).await.unwrap();
    }

    #[tokio::test]
    async fn put_相同内容两次幂等不报错() {
        let (store, _dir) = 新建临时存储().await;
        let d1 = store.put(&b"same"[..]).await.unwrap();
        let d2 = store.put(&b"same"[..]).await.unwrap();
        assert_eq!(d1, d2);
        assert!(store.exists(&d1.sha256).await.unwrap());
    }

    #[test]
    fn content_key_按前两位分桶寻址() {
        // 正常 64 位 sha256：前两位作桶，其余作对象名
        assert_eq!(
            content_key(ABC_SHA256),
            "ba/7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
        );
        // 与本地分桶布局同构：桶名即 blob_path 的第一层目录
        assert!(content_key(EMPTY_SHA256).starts_with("e3/"));
    }

    #[test]
    fn content_key_短串不越界() {
        // 不足两位时不 panic（split_at 取 min）
        assert_eq!(content_key("a"), "a/");
        assert_eq!(content_key(""), "/");
    }

    #[tokio::test]
    async fn get_装箱后仍可流式读回内容() {
        // 验证 get 返回类型改为 Box<dyn AsyncRead> 后本地路径不回归
        let (store, _dir) = 新建临时存储().await;
        let content = b"boxed reader roundtrip";
        let d = store.put(&content[..]).await.unwrap();

        let mut reader = store.get(&d.sha256).await.unwrap();
        let mut buf = Vec::new();
        reader.read_to_end(&mut buf).await.unwrap();
        assert_eq!(buf, content);
    }

    #[tokio::test]
    async fn from_config_默认构造本地文件系统后端() {
        let dir = tempfile::tempdir().unwrap();
        let storage = crate::config::StorageConfig::default();
        // BlobBackend 不实现 Debug，故用 match 而非 unwrap 断言变体
        match BlobBackend::from_config(&storage, &dir.path().join("blobs"), dir.path()).await {
            Ok(BlobBackend::Fs(_)) => {}
            _ => panic!("默认配置应构造本地文件系统后端"),
        }
    }

    /// 配置选 S3 但未启用 `s3` 编译特性时，构造直接报错退出（不静默回退本地，ADR-0014 §1）。
    #[cfg(not(feature = "s3"))]
    #[tokio::test]
    async fn from_config_选_s3_但未启用特性则报错() {
        use crate::config::{StorageBackend, StorageConfig};
        let dir = tempfile::tempdir().unwrap();
        let storage = StorageConfig {
            backend: StorageBackend::S3,
            s3: None,
        };
        match BlobBackend::from_config(&storage, &dir.path().join("blobs"), dir.path()).await {
            Err(BackendInitError::S3FeatureDisabled) => {}
            _ => panic!("未启用 s3 特性时选 S3 后端应报 S3FeatureDisabled"),
        }
    }

    /// 启用 `s3` 特性但缺少 `[data.storage.s3]` 子配置时报错。
    #[cfg(feature = "s3")]
    #[tokio::test]
    async fn from_config_选_s3_但缺子配置则报错() {
        use crate::config::{StorageBackend, StorageConfig};
        let dir = tempfile::tempdir().unwrap();
        let storage = StorageConfig {
            backend: StorageBackend::S3,
            s3: None,
        };
        match BlobBackend::from_config(&storage, &dir.path().join("blobs"), dir.path()).await {
            Err(BackendInitError::MissingS3Settings) => {}
            _ => panic!("缺 S3 子配置应报 MissingS3Settings"),
        }
    }
}
