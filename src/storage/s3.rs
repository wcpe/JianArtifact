//! S3 兼容对象存储后端（FR-30 / ADR-0014），仅在启用 `s3` 编译特性时编入。
//!
//! 写入语义与本地后端等价（ADR-0014 §4）：先把入站流写到本地临时文件并边写边算四种摘要、
//! 得到 sha256 后再以内容寻址 key 流式上传到 S3；上传成功才返回，失败清理临时文件不留孤儿对象。
//! 凭据真源在配置 / 环境（沿用 AWS SDK 标准环境变量），绝不入库、不进日志、不进 DB 明文（ADR-0014 §7）。

use std::path::{Path, PathBuf};

use aws_sdk_s3::config::{BehaviorVersion, Region};
use aws_sdk_s3::primitives::ByteStream;
use aws_sdk_s3::types::{CompletedMultipartUpload, CompletedPart};
use aws_sdk_s3::Client;
use aws_smithy_types::byte_stream::Length;
use tokio::io::AsyncRead;

use super::{content_key, BlobDigests, BlobReader, BlobStore, StorageError};

/// 分段上传的分片大小（8 MiB）：大于该阈值的对象走 multipart，逐段从临时文件读出（ADR-0014 §5）。
const MULTIPART_CHUNK_SIZE: u64 = 8 * 1024 * 1024;

/// S3 后端运行期配置（已解析自 [`crate::config::S3Settings`]，凭据不在内）。
#[derive(Debug, Clone)]
pub struct S3Config {
    /// 端点 URL（兼容 MinIO 等自建网关；为 None 时由 region 推断 AWS 端点）。
    pub endpoint: Option<String>,
    /// 区域。
    pub region: String,
    /// 存储桶名。
    pub bucket: String,
    /// 对象 key 前缀（与 sha256 内容寻址键拼接）。
    pub prefix: String,
    /// 是否使用 path-style 寻址（MinIO 等需 true）。
    pub path_style: bool,
    /// 本地临时文件中转目录（上传前算 sha256 用）。
    pub tmp_dir: PathBuf,
}

impl S3Config {
    /// 从配置层 [`crate::config::S3Settings`] 与临时目录构造运行期配置。
    pub fn from_settings(settings: &crate::config::S3Settings, tmp_dir: &Path) -> Self {
        Self {
            endpoint: settings.endpoint.clone(),
            region: settings.region.clone(),
            bucket: settings.bucket.clone(),
            prefix: settings.prefix.clone(),
            path_style: settings.path_style,
            tmp_dir: tmp_dir.to_path_buf(),
        }
    }
}

/// S3 兼容对象存储 blob 后端。
#[derive(Debug, Clone)]
pub struct S3Store {
    /// S3 客户端（内部为连接池，克隆廉价）。
    client: Client,
    /// 存储桶名。
    bucket: String,
    /// 对象 key 前缀。
    prefix: String,
    /// 本地临时文件中转目录。
    tmp_dir: PathBuf,
}

impl S3Store {
    /// 按配置建立 S3 客户端并确保临时目录存在。
    ///
    /// HTTP 客户端显式用 `aws-smithy-http-client` 的 rustls-ring（纯 rustls + ring，零原生 C 依赖，
    /// 不引入 aws-lc-rs）；凭据由 AWS SDK 默认链解析（标准环境变量 / 配置），本进程不持久化凭据。
    pub async fn connect(cfg: S3Config) -> Result<Self, StorageError> {
        tokio::fs::create_dir_all(&cfg.tmp_dir).await?;

        // 纯 rustls + ring 的 HTTP 客户端（避免默认 https client 拖入 aws-lc-rs 原生加密）
        let http_client = aws_smithy_http_client::Builder::new()
            .tls_provider(aws_smithy_http_client::tls::Provider::Rustls(
                aws_smithy_http_client::tls::rustls_provider::CryptoMode::Ring,
            ))
            .build_https();

        let shared = aws_config::defaults(BehaviorVersion::latest())
            .region(Region::new(cfg.region.clone()))
            .http_client(http_client)
            .load()
            .await;

        let mut builder = aws_sdk_s3::config::Builder::from(&shared)
            // path-style 寻址：MinIO 等自建网关与不支持 vhost-style 的实现需要
            .force_path_style(cfg.path_style);
        if let Some(endpoint) = &cfg.endpoint {
            builder = builder.endpoint_url(endpoint.clone());
        }
        let client = Client::from_conf(builder.build());

        Ok(Self {
            client,
            bucket: cfg.bucket,
            prefix: cfg.prefix,
            tmp_dir: cfg.tmp_dir,
        })
    }

    /// 内容寻址对象 key：`{prefix}{sha256[0..2]}/{sha256[2..]}`，与本地分桶布局同构。
    fn object_key(&self, sha256: &str) -> String {
        format!("{}{}", self.prefix, content_key(sha256))
    }

    /// 判断对象是否存在（HEAD）。
    async fn head_exists(&self, key: &str) -> Result<bool, StorageError> {
        match self
            .client
            .head_object()
            .bucket(&self.bucket)
            .key(key)
            .send()
            .await
        {
            Ok(_) => Ok(true),
            Err(e) => {
                // 404 / NoSuchKey 视作不存在；其余为后端错误
                let svc = e.into_service_error();
                if svc.is_not_found() {
                    Ok(false)
                } else {
                    Err(StorageError::Backend(svc.to_string()))
                }
            }
        }
    }

    /// 把本地临时文件以最终 key 上传到 S3：小对象单次 PUT，大对象 multipart 流式逐段。
    async fn upload_temp_file(
        &self,
        tmp_path: &Path,
        key: &str,
        size: u64,
    ) -> Result<(), StorageError> {
        if size <= MULTIPART_CHUNK_SIZE {
            // 小对象：单次 PUT，从文件流式读出（不整体载入内存）
            let body = ByteStream::from_path(tmp_path)
                .await
                .map_err(|e| StorageError::Backend(e.to_string()))?;
            self.client
                .put_object()
                .bucket(&self.bucket)
                .key(key)
                .body(body)
                .send()
                .await
                .map_err(|e| StorageError::Backend(e.into_service_error().to_string()))?;
            return Ok(());
        }
        self.multipart_upload(tmp_path, key, size).await
    }

    /// 分段上传：逐段从临时文件按 offset/length 读出发送，峰值内存不随对象体积增长（ADR-0014 §5）。
    async fn multipart_upload(
        &self,
        tmp_path: &Path,
        key: &str,
        size: u64,
    ) -> Result<(), StorageError> {
        let created = self
            .client
            .create_multipart_upload()
            .bucket(&self.bucket)
            .key(key)
            .send()
            .await
            .map_err(|e| StorageError::Backend(e.into_service_error().to_string()))?;
        let upload_id = created
            .upload_id()
            .ok_or_else(|| StorageError::Backend("S3 未返回 multipart upload_id".to_string()))?
            .to_string();

        // 逐段上传：任一段失败即中止并返回错误（外层据此清理，不留半截对象）
        let parts_result = self.upload_all_parts(tmp_path, key, &upload_id, size).await;
        let parts = match parts_result {
            Ok(parts) => parts,
            Err(e) => {
                // 中止分段上传，清理 S3 上的未完成分片
                let _ = self
                    .client
                    .abort_multipart_upload()
                    .bucket(&self.bucket)
                    .key(key)
                    .upload_id(&upload_id)
                    .send()
                    .await;
                return Err(e);
            }
        };

        // 完成分段上传
        let completed = CompletedMultipartUpload::builder()
            .set_parts(Some(parts))
            .build();
        self.client
            .complete_multipart_upload()
            .bucket(&self.bucket)
            .key(key)
            .upload_id(&upload_id)
            .multipart_upload(completed)
            .send()
            .await
            .map_err(|e| StorageError::Backend(e.into_service_error().to_string()))?;
        Ok(())
    }

    /// 按固定分片大小逐段上传整个临时文件，返回已完成分片清单（含 part_number 与 ETag）。
    async fn upload_all_parts(
        &self,
        tmp_path: &Path,
        key: &str,
        upload_id: &str,
        size: u64,
    ) -> Result<Vec<CompletedPart>, StorageError> {
        let part_count = size.div_ceil(MULTIPART_CHUNK_SIZE);
        let mut parts = Vec::with_capacity(part_count as usize);
        for i in 0..part_count {
            let offset = i * MULTIPART_CHUNK_SIZE;
            // 末段可能不足整片：按剩余字节数精确读取
            let this_len = MULTIPART_CHUNK_SIZE.min(size - offset);
            let part_number = (i + 1) as i32;

            let body = ByteStream::read_from()
                .path(tmp_path)
                .offset(offset)
                .length(Length::Exact(this_len))
                .build()
                .await
                .map_err(|e| StorageError::Backend(e.to_string()))?;

            let resp = self
                .client
                .upload_part()
                .bucket(&self.bucket)
                .key(key)
                .upload_id(upload_id)
                .part_number(part_number)
                .body(body)
                .send()
                .await
                .map_err(|e| StorageError::Backend(e.into_service_error().to_string()))?;

            parts.push(
                CompletedPart::builder()
                    .part_number(part_number)
                    .set_e_tag(resp.e_tag().map(str::to_string))
                    .build(),
            );
        }
        Ok(parts)
    }
}

impl BlobStore for S3Store {
    async fn put<R>(&self, reader: R) -> Result<BlobDigests, StorageError>
    where
        R: AsyncRead + Unpin + Send,
    {
        // ① 先把入站流写本地临时文件并边写边算四种摘要（失败已在内部清理临时文件）
        let tmp_path = self.tmp_dir.join(uuid::Uuid::new_v4().to_string());
        let digests = super::stream_to_temp_file(reader, &tmp_path).await?;
        let key = self.object_key(&digests.sha256);

        // ② 幂等：同 sha256 即同内容，已存在则跳过上传并删临时文件
        let result = async {
            if self.head_exists(&key).await? {
                return Ok(());
            }
            // ③ 内容寻址流式上传到 S3（小对象单次 PUT / 大对象 multipart）
            self.upload_temp_file(&tmp_path, &key, digests.size).await
        }
        .await;

        // 无论成功与否都删本地临时文件，不留中转垃圾
        let _ = tokio::fs::remove_file(&tmp_path).await;
        result.map(|()| digests)
    }

    async fn get(&self, sha256: &str) -> Result<BlobReader, StorageError> {
        let key = self.object_key(sha256);
        let resp = match self
            .client
            .get_object()
            .bucket(&self.bucket)
            .key(&key)
            .send()
            .await
        {
            Ok(resp) => resp,
            Err(e) => {
                let svc = e.into_service_error();
                if svc.is_no_such_key() {
                    return Err(StorageError::NotFound(sha256.to_string()));
                }
                return Err(StorageError::Backend(svc.to_string()));
            }
        };

        // 流式 GET：把 SDK 字节流适配为 tokio AsyncRead，不在内存中聚合整对象（ADR-0014 §5）
        let reader = resp.body.into_async_read();
        Ok(Box::new(reader))
    }

    async fn delete(&self, sha256: &str) -> Result<(), StorageError> {
        let key = self.object_key(sha256);
        // S3 删除对不存在的对象亦返回成功，天然幂等
        self.client
            .delete_object()
            .bucket(&self.bucket)
            .key(&key)
            .send()
            .await
            .map_err(|e| StorageError::Backend(e.into_service_error().to_string()))?;
        Ok(())
    }

    async fn exists(&self, sha256: &str) -> Result<bool, StorageError> {
        let key = self.object_key(sha256);
        self.head_exists(&key).await
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use aws_sdk_s3::config::Credentials;

    /// 构造一个仅用于测试 key 推导的最小 S3Store（不实际连 S3）。
    fn 测试用_store(prefix: &str) -> S3Store {
        // 用占位客户端构造：测试只调用纯计算方法 object_key，不发起网络请求
        let conf = aws_sdk_s3::Config::builder()
            .behavior_version(BehaviorVersion::latest())
            .region(Region::new("test"))
            .credentials_provider(Credentials::new("ak", "sk", None, None, "test"))
            .build();
        S3Store {
            client: Client::from_conf(conf),
            bucket: "b".to_string(),
            prefix: prefix.to_string(),
            tmp_dir: std::env::temp_dir(),
        }
    }

    #[test]
    fn object_key_含前缀且与本地布局同构() {
        let store = 测试用_store("repo/");
        let sha = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad";
        assert_eq!(
            store.object_key(sha),
            "repo/ba/7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
        );
    }

    #[test]
    fn object_key_空前缀时即内容寻址键() {
        let store = 测试用_store("");
        let sha = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855";
        assert_eq!(store.object_key(sha), content_key(sha));
    }
}
