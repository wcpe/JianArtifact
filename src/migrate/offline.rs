//! Nexus OSS 迁移**离线 blob store 入口**（FR-37，ADR-0006）。
//!
//! 当源 Nexus 已下线、只剩其文件型 blob store 目录时，本模块从该本地目录读取并
//! **枚举 / 预览**可迁移内容：解析 Nexus 文件型 blob store 的磁盘布局
//! （`content/` 分片目录 + 每个 blob 一份 `.properties` 元数据），枚举其中的 blob
//! 及基本元数据（所属 repo / 坐标 / sha1 / 大小），按 repo 分组返回，作为离线迁移的
//! 发现 / 预览步骤。
//!
//! **范围纪律**：仅做离线 blob store 的连接（指向本地路径）+ 枚举 / 预览，
//! **不读取也不搬运 blob 本体**（`.bytes` 内容搬运属 FR-38/39，本批严禁实现）。
//!
//! 关键约束：
//! - 纯文件系统读取，不依赖任何外部服务；解析逻辑尽量做成无副作用纯函数便于穷举测试。
//! - 损坏 / 缺字段 / 软删的 blob 元数据须容错跳过，不让单个坏文件中断整次枚举。
//! - 依赖方向：仅依赖 `std`，不反向依赖上层；api 层薄编排调用之。

use std::collections::BTreeMap;
use std::path::{Path, PathBuf};

use super::MigrateError;

/// Nexus 文件型 blob store 根下存放 blob 的内容子目录名。
const CONTENT_DIR: &str = "content";

/// blob 元数据文件（Java Properties 格式）的扩展名。
const PROPERTIES_EXT: &str = "properties";

/// blob 本体文件的扩展名（与同名 `.properties` 同目录、同文件名主干）。
const BYTES_EXT: &str = "bytes";

/// `.properties` 中所属 repo 名的键（Nexus 3.x 实际写出的键，含 3.70.2）。
const KEY_BUCKET_REPO_NAME: &str = "@Bucket.repo-name";

/// `.properties` 中所属 repo 名的回退键（历史 / 其他版本可能写出 `@Repo.repo-name`）。
const KEY_REPO_NAME: &str = "@Repo.repo-name";

/// `.properties` 中 blob 逻辑名（路径 / 坐标）的键。
const KEY_BLOB_NAME: &str = "@BlobStore.blob-name";

/// `.properties` 中 sha1 校验和的键。
const KEY_SHA1: &str = "sha1";

/// `.properties` 中 blob 字节大小的键。
const KEY_SIZE: &str = "size";

/// `.properties` 中软删标记的键（值为 `true` 表示该 blob 已被逻辑删除）。
const KEY_DELETED: &str = "deleted";

/// 从离线 blob store 枚举出的单个 blob 的基本元数据（迁移预览项）。
///
/// 仅承载迁移发现所需的基本信息，不含 blob 本体。`size` 为 `.properties` 中声明的
/// 字节数（缺失或非法时为 None，不臆造）。
#[derive(Debug, Clone, PartialEq, Eq, serde::Serialize)]
pub struct OfflineBlobSummary {
    /// blob 逻辑名（源系统中的路径 / 坐标，取自 `@BlobStore.blob-name`）。
    pub blob_name: String,
    /// sha1 校验和（取自 `sha1`；缺失为 None）。
    pub sha1: Option<String>,
    /// blob 字节大小（取自 `size`；缺失或非法为 None）。
    pub size: Option<u64>,
}

/// 离线 blob store 枚举结果中的单个仓库分组（按 repo 聚合的预览项）。
#[derive(Debug, Clone, PartialEq, Eq, serde::Serialize)]
pub struct OfflineRepoSummary {
    /// 仓库名（取自各 blob `.properties` 的 `@Bucket.repo-name`，回退 `@Repo.repo-name`）。
    pub repo_name: String,
    /// 该仓库下枚举到的 blob 数量。
    pub blob_count: usize,
    /// 该仓库下的 blob 预览项列表。
    pub blobs: Vec<OfflineBlobSummary>,
}

/// 解析一份 Nexus blob `.properties` 文本得到的中间结果（含所属 repo，便于按 repo 归组）。
#[derive(Debug, Clone, PartialEq, Eq)]
struct ParsedBlobProperties {
    /// 所属仓库名。
    repo_name: String,
    /// blob 预览项。
    summary: OfflineBlobSummary,
}

/// 解析一行 Java Properties。返回 `Some((key, value))`，注释 / 空行返回 None。
///
/// 仅覆盖 Nexus blob `.properties` 实际使用的简单子集：`#` / `!` 行首注释、空行跳过；
/// 其余按首个 `=`（其次 `:`）分隔键值，两侧裁空白。不处理续行 / Unicode 转义等
/// 完整 Properties 规范特性——Nexus 写出的元数据不依赖这些特性，按简单优先不过度实现。
fn parse_properties_line(line: &str) -> Option<(&str, &str)> {
    let trimmed = line.trim_start();
    // 空行或注释行（`#` / `!` 起头）跳过
    if trimmed.is_empty() || trimmed.starts_with('#') || trimmed.starts_with('!') {
        return None;
    }
    // 以首个 `=` 或 `:` 作为键值分隔符
    let sep = trimmed
        .find('=')
        .or_else(|| trimmed.find(':'))
        .filter(|&i| i > 0)?;
    let key = trimmed[..sep].trim();
    let value = trimmed[sep + 1..].trim();
    if key.is_empty() {
        return None;
    }
    Some((key, value))
}

/// 把 `.properties` 文本解析为键值映射（纯函数，便于穷举测试）。
fn parse_properties(text: &str) -> BTreeMap<String, String> {
    text.lines()
        .filter_map(parse_properties_line)
        .map(|(k, v)| (k.to_string(), v.to_string()))
        .collect()
}

/// 把一份 blob `.properties` 文本解析为预览项。
///
/// 容错策略：缺所属 repo 名（`@Bucket.repo-name`，回退 `@Repo.repo-name`）或缺 blob 名（`@BlobStore.blob-name`）
/// 视为不可用元数据，返回 None 由调用方跳过；软删（`deleted=true`）的 blob 同样跳过——
/// 它在源系统中已被逻辑删除，不属可迁移内容。`sha1` / `size` 缺失不影响枚举，仅置 None。
fn parse_blob_properties(text: &str) -> Option<ParsedBlobProperties> {
    let props = parse_properties(text);

    // 软删的 blob 不属可迁移内容，直接跳过
    if props.get(KEY_DELETED).map(|v| v == "true").unwrap_or(false) {
        return None;
    }

    // repo 名与 blob 名是归组与定位的必要信息，缺任一即视为不可用元数据。
    // repo 名优先取 Nexus 3.x 实际键 `@Bucket.repo-name`，回退历史键 `@Repo.repo-name`。
    let repo_name = props
        .get(KEY_BUCKET_REPO_NAME)
        .or_else(|| props.get(KEY_REPO_NAME))?
        .trim();
    let blob_name = props.get(KEY_BLOB_NAME)?.trim();
    if repo_name.is_empty() || blob_name.is_empty() {
        return None;
    }

    // size 非法（非数字）按缺失处理，不臆造、不中断枚举
    let size = props
        .get(KEY_SIZE)
        .and_then(|s| s.trim().parse::<u64>().ok());
    let sha1 = props
        .get(KEY_SHA1)
        .map(|s| s.trim().to_string())
        .filter(|s| !s.is_empty());

    Some(ParsedBlobProperties {
        repo_name: repo_name.to_string(),
        summary: OfflineBlobSummary {
            blob_name: blob_name.to_string(),
            sha1,
            size,
        },
    })
}

/// 在给定 blob store 根目录下定位 `content/` 子目录。
///
/// 根目录不存在 / 非目录、或其下无 `content/` 子目录时报 [`MigrateError::Invalid`]，
/// 由调用方修正路径（按 400 处理）。
fn locate_content_dir(root: &Path) -> Result<PathBuf, MigrateError> {
    if !root.is_dir() {
        return Err(MigrateError::Invalid(
            "blob store 路径不存在或不是目录".to_string(),
        ));
    }
    let content = root.join(CONTENT_DIR);
    if !content.is_dir() {
        return Err(MigrateError::Invalid(
            "blob store 路径下缺少 content 目录，疑似不是 Nexus 文件型 blob store".to_string(),
        ));
    }
    Ok(content)
}

/// 递归收集 `content/` 目录下所有 `.properties` 文件路径。
///
/// Nexus 文件型 blob store 按 `content/vol-XX/chap-YY/<id>.properties` 两级分片存放，
/// 此处对子目录深度不作假设、统一递归遍历。单个条目读取失败（权限等）记 WARN 后跳过，
/// 不中断整次遍历。
fn collect_properties_files(content_dir: &Path) -> Result<Vec<PathBuf>, MigrateError> {
    let mut files = Vec::new();
    let mut stack = vec![content_dir.to_path_buf()];
    while let Some(dir) = stack.pop() {
        let entries = match std::fs::read_dir(&dir) {
            Ok(e) => e,
            Err(e) => {
                tracing::warn!(目录 = %dir.display(), 错误 = %e, "读取 blob store 子目录失败，跳过");
                continue;
            }
        };
        for entry in entries.flatten() {
            let path = entry.path();
            match entry.file_type() {
                Ok(ft) if ft.is_dir() => stack.push(path),
                Ok(ft) if ft.is_file() => {
                    if path.extension().and_then(|e| e.to_str()) == Some(PROPERTIES_EXT) {
                        files.push(path);
                    }
                }
                _ => {}
            }
        }
    }
    Ok(files)
}

/// 把按 repo 归组的中间映射整理为有序的仓库摘要列表（纯函数，便于穷举测试）。
///
/// repo 名按字典序、每仓库内 blob 按 blob_name 字典序排序，保证枚举结果稳定可比较。
fn group_by_repo(parsed: Vec<ParsedBlobProperties>) -> Vec<OfflineRepoSummary> {
    let mut by_repo: BTreeMap<String, Vec<OfflineBlobSummary>> = BTreeMap::new();
    for p in parsed {
        by_repo.entry(p.repo_name).or_default().push(p.summary);
    }
    by_repo
        .into_iter()
        .map(|(repo_name, mut blobs)| {
            blobs.sort_by(|a, b| a.blob_name.cmp(&b.blob_name));
            OfflineRepoSummary {
                repo_name,
                blob_count: blobs.len(),
                blobs,
            }
        })
        .collect()
}

/// 从本地 Nexus 文件型 blob store 目录枚举可迁移内容，按 repo 分组返回（离线迁移发现步骤）。
///
/// `root` 为本地 blob store 根目录路径。仅解析 `.properties` 元数据并按 repo 归组，
/// **不读取 blob 本体**。损坏 / 缺字段 / 软删的元数据容错跳过，不中断整次枚举。
pub fn enumerate_blob_store(root: &Path) -> Result<Vec<OfflineRepoSummary>, MigrateError> {
    let content_dir = locate_content_dir(root)?;
    tracing::info!(根目录 = %root.display(), "开始枚举离线 Nexus blob store");

    let files = collect_properties_files(&content_dir)?;

    let mut parsed = Vec::new();
    let mut skipped = 0usize;
    for file in &files {
        // 单个文件读取失败（权限 / 瞬时 IO）记 WARN 后跳过，不中断整次枚举
        let text = match std::fs::read_to_string(file) {
            Ok(t) => t,
            Err(e) => {
                tracing::warn!(文件 = %file.display(), 错误 = %e, "读取 blob 元数据失败，跳过");
                skipped += 1;
                continue;
            }
        };
        match parse_blob_properties(&text) {
            Some(p) => parsed.push(p),
            None => skipped += 1,
        }
    }

    let repos = group_by_repo(parsed);
    tracing::info!(
        仓库数 = repos.len(),
        跳过数 = skipped,
        "离线 Nexus blob store 枚举完成"
    );
    Ok(repos)
}

/// 离线 blob store 中可搬运的单个 blob 条目：含所属 repo、逻辑名与本体 `.bytes` 文件路径。
///
/// 与预览项 [`OfflineBlobSummary`] 的区别：本条目额外携带 `.bytes` 本体路径，供搬运阶段（FR-38）
/// 流式读取内容。仅当同目录存在同主干的 `.bytes` 文件时才产出（缺本体的元数据无可搬运内容）。
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct OfflineBlobEntry {
    /// 所属仓库名（取自 `@Bucket.repo-name`，回退 `@Repo.repo-name`）。
    pub repo_name: String,
    /// blob 逻辑名（源系统中的路径 / 坐标，取自 `@BlobStore.blob-name`）。
    pub blob_name: String,
    /// blob 本体文件（`.bytes`）的绝对路径。
    pub bytes_path: PathBuf,
}

/// 枚举离线 blob store 中**可搬运**的 blob 条目（含 `.bytes` 本体路径），供 FR-38 搬运编排。
///
/// 与 [`enumerate_blob_store`]（预览，仅解析元数据）相比，本函数额外定位每个有效 `.properties`
/// 对应的同主干 `.bytes` 本体文件：本体缺失的条目被跳过（无可搬运内容）。软删 / 损坏 / 缺字段的
/// 元数据同样容错跳过，不中断整次枚举。仍**不读取** blob 字节，仅返回其路径，搬运时再流式读。
pub fn enumerate_blob_entries(root: &Path) -> Result<Vec<OfflineBlobEntry>, MigrateError> {
    let content_dir = locate_content_dir(root)?;
    let files = collect_properties_files(&content_dir)?;

    let mut entries = Vec::new();
    for prop_path in &files {
        let text = match std::fs::read_to_string(prop_path) {
            Ok(t) => t,
            Err(e) => {
                tracing::warn!(文件 = %prop_path.display(), 错误 = %e, "读取 blob 元数据失败，跳过");
                continue;
            }
        };
        let Some(parsed) = parse_blob_properties(&text) else {
            continue;
        };
        // 同主干的 `.bytes` 本体须存在才可搬运
        let bytes_path = prop_path.with_extension(BYTES_EXT);
        if !bytes_path.is_file() {
            tracing::warn!(
                元数据 = %prop_path.display(),
                "缺少对应 .bytes 本体文件，跳过该 blob 的搬运"
            );
            continue;
        }
        entries.push(OfflineBlobEntry {
            repo_name: parsed.repo_name,
            blob_name: parsed.summary.blob_name,
            bytes_path,
        });
    }
    Ok(entries)
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::fs;

    /// 构造一份合法的 Nexus blob `.properties` 文本（用真实 Nexus 3.x 的 `@Bucket.repo-name` 键）。
    fn sample_properties(repo: &str, blob_name: &str, sha1: &str, size: &str) -> String {
        format!(
            "#Mon Jun 24 00:00:00 UTC 2026\n\
             @BlobStore.created-by=admin\n\
             @BlobStore.content-type=application/octet-stream\n\
             @Bucket.repo-name={repo}\n\
             size={size}\n\
             @BlobStore.blob-name={blob_name}\n\
             creationTime=1700000000000\n\
             sha1={sha1}\n\
             deleted=false\n"
        )
    }

    /// 在临时目录下铺一个最小可用的 Nexus 文件型 blob store 布局，返回根目录。
    fn build_sample_store(root: &Path, files: &[(&str, &str)]) {
        let chap = root.join(CONTENT_DIR).join("vol-01").join("chap-01");
        fs::create_dir_all(&chap).unwrap();
        for (name, content) in files {
            fs::write(chap.join(name), content).unwrap();
        }
    }

    #[test]
    fn 解析_properties_行跳过注释与空行并切分键值() {
        assert_eq!(parse_properties_line("#comment"), None);
        assert_eq!(parse_properties_line("!bang comment"), None);
        assert_eq!(parse_properties_line("   "), None);
        assert_eq!(parse_properties_line("sha1=abc"), Some(("sha1", "abc")));
        // 两侧空白被裁剪
        assert_eq!(
            parse_properties_line("  size = 123  "),
            Some(("size", "123"))
        );
        // 冒号亦可作分隔符
        assert_eq!(parse_properties_line("k:v"), Some(("k", "v")));
        // 无分隔符的行无效
        assert_eq!(parse_properties_line("novalue"), None);
        // 键为空无效
        assert_eq!(parse_properties_line("=v"), None);
    }

    #[test]
    fn 解析合法_blob_properties_取基本元数据() {
        let text = sample_properties(
            "maven-releases",
            "/org/example/app/1.0/app-1.0.jar",
            "a1b2",
            "1024",
        );
        let parsed = parse_blob_properties(&text).unwrap();
        assert_eq!(parsed.repo_name, "maven-releases");
        assert_eq!(
            parsed.summary,
            OfflineBlobSummary {
                blob_name: "/org/example/app/1.0/app-1.0.jar".to_string(),
                sha1: Some("a1b2".to_string()),
                size: Some(1024),
            }
        );
    }

    #[test]
    fn 软删的_blob_被跳过() {
        let text = "@Repo.repo-name=r\n@BlobStore.blob-name=/x\ndeleted=true\n";
        assert_eq!(parse_blob_properties(text), None);
    }

    #[test]
    fn 真实_nexus_用_bucket_repo_name_键() {
        // Nexus 3.x（含 3.70.2）实际写出的 blob .properties 用 `@Bucket.repo-name` 标记所属仓库，
        // 须被识别（此前仅认 `@Repo.repo-name` 导致离线枚举对真实 blob store 返回空）。
        let text = "@Bucket.repo-name=maven-releases\n@BlobStore.blob-name=/com/jian/demo/1.0.0/demo-1.0.0.jar\nsize=21\nsha1=abc\n";
        let p = parse_blob_properties(text).unwrap();
        assert_eq!(p.repo_name, "maven-releases");
        assert_eq!(p.summary.blob_name, "/com/jian/demo/1.0.0/demo-1.0.0.jar");
    }

    #[test]
    fn 旧_repo_repo_name_键作回退仍识别() {
        // 兼容历史 / 其他版本写出的 `@Repo.repo-name`：作为回退仍应识别。
        let text = "@Repo.repo-name=r\n@BlobStore.blob-name=/x\n";
        assert_eq!(parse_blob_properties(text).unwrap().repo_name, "r");
    }

    #[test]
    fn 缺_repo_名或_blob_名视为不可用() {
        // 缺 repo 名
        assert_eq!(
            parse_blob_properties("@BlobStore.blob-name=/x\nsha1=a\n"),
            None
        );
        // 缺 blob 名
        assert_eq!(parse_blob_properties("@Repo.repo-name=r\nsha1=a\n"), None);
        // repo 名为空白
        assert_eq!(
            parse_blob_properties("@Repo.repo-name=   \n@BlobStore.blob-name=/x\n"),
            None
        );
    }

    #[test]
    fn sha1_或_size_缺失或非法时置_none_不中断() {
        // 完全缺 sha1 / size
        let p = parse_blob_properties("@Repo.repo-name=r\n@BlobStore.blob-name=/x\n").unwrap();
        assert_eq!(p.summary.sha1, None);
        assert_eq!(p.summary.size, None);
        // size 非数字按缺失处理
        let p2 = parse_blob_properties(
            "@Repo.repo-name=r\n@BlobStore.blob-name=/x\nsize=notanumber\nsha1=\n",
        )
        .unwrap();
        assert_eq!(p2.summary.size, None);
        // 空 sha1 视为缺失
        assert_eq!(p2.summary.sha1, None);
    }

    #[test]
    fn 按_repo_归组并按字典序稳定排序() {
        let parsed = vec![
            ParsedBlobProperties {
                repo_name: "b-repo".to_string(),
                summary: OfflineBlobSummary {
                    blob_name: "/z".to_string(),
                    sha1: None,
                    size: None,
                },
            },
            ParsedBlobProperties {
                repo_name: "a-repo".to_string(),
                summary: OfflineBlobSummary {
                    blob_name: "/m".to_string(),
                    sha1: None,
                    size: None,
                },
            },
            ParsedBlobProperties {
                repo_name: "a-repo".to_string(),
                summary: OfflineBlobSummary {
                    blob_name: "/a".to_string(),
                    sha1: None,
                    size: None,
                },
            },
        ];
        let groups = group_by_repo(parsed);
        assert_eq!(groups.len(), 2);
        // repo 名按字典序：a-repo 在前
        assert_eq!(groups[0].repo_name, "a-repo");
        assert_eq!(groups[0].blob_count, 2);
        // 仓库内 blob 名按字典序：/a 在 /m 前
        assert_eq!(groups[0].blobs[0].blob_name, "/a");
        assert_eq!(groups[0].blobs[1].blob_name, "/m");
        assert_eq!(groups[1].repo_name, "b-repo");
    }

    #[test]
    fn 枚举完整_blob_store_布局按_repo_分组() {
        let tmp = tempfile::tempdir().unwrap();
        let root = tmp.path();
        build_sample_store(
            root,
            &[
                (
                    "blob-1.properties",
                    &sample_properties("maven-releases", "/a/app-1.0.jar", "sha-a", "10"),
                ),
                // 同 repo 第二个 blob
                (
                    "blob-2.properties",
                    &sample_properties("maven-releases", "/a/app-2.0.jar", "sha-b", "20"),
                ),
                // 另一 repo
                (
                    "blob-3.properties",
                    &sample_properties("npm-hosted", "/pkg/-/pkg-1.0.0.tgz", "sha-c", "30"),
                ),
                // .bytes 本体文件（不应被当作元数据读取）
                ("blob-1.bytes", "二进制本体占位，不该被解析"),
            ],
        );

        let repos = enumerate_blob_store(root).unwrap();
        assert_eq!(repos.len(), 2);
        // maven-releases 在前（字典序），含 2 个 blob
        assert_eq!(repos[0].repo_name, "maven-releases");
        assert_eq!(repos[0].blob_count, 2);
        assert_eq!(repos[0].blobs[0].blob_name, "/a/app-1.0.jar");
        assert_eq!(repos[1].repo_name, "npm-hosted");
        assert_eq!(repos[1].blob_count, 1);
    }

    #[test]
    fn 枚举跳过软删与损坏的元数据() {
        let tmp = tempfile::tempdir().unwrap();
        let root = tmp.path();
        build_sample_store(
            root,
            &[
                (
                    "good.properties",
                    &sample_properties("r", "/good", "sha", "1"),
                ),
                // 软删
                (
                    "deleted.properties",
                    "@Repo.repo-name=r\n@BlobStore.blob-name=/deleted\ndeleted=true\n",
                ),
                // 损坏 / 缺必要字段
                (
                    "broken.properties",
                    "这是一段完全无法解析的内容没有任何键值",
                ),
                ("empty.properties", ""),
            ],
        );

        let repos = enumerate_blob_store(root).unwrap();
        // 仅保留 1 个有效 blob
        assert_eq!(repos.len(), 1);
        assert_eq!(repos[0].repo_name, "r");
        assert_eq!(repos[0].blob_count, 1);
        assert_eq!(repos[0].blobs[0].blob_name, "/good");
    }

    #[test]
    fn 路径不存在或缺_content_目录报无效() {
        // 路径不存在
        let missing = Path::new("D:/__不存在的_blob_store_路径__/x");
        assert!(matches!(
            enumerate_blob_store(missing),
            Err(MigrateError::Invalid(_))
        ));
        // 存在但无 content 子目录
        let tmp = tempfile::tempdir().unwrap();
        assert!(matches!(
            enumerate_blob_store(tmp.path()),
            Err(MigrateError::Invalid(_))
        ));
    }

    #[test]
    fn 空_content_目录得空列表() {
        let tmp = tempfile::tempdir().unwrap();
        fs::create_dir_all(tmp.path().join(CONTENT_DIR)).unwrap();
        let repos = enumerate_blob_store(tmp.path()).unwrap();
        assert!(repos.is_empty());
    }

    #[test]
    fn 枚举可搬运条目仅含有_bytes_本体者() {
        let tmp = tempfile::tempdir().unwrap();
        let root = tmp.path();
        build_sample_store(
            root,
            &[
                // 有 .properties 且有同主干 .bytes：可搬运
                (
                    "blob-1.properties",
                    &sample_properties("maven-proxy", "/a/app-1.0.jar", "sha-a", "10"),
                ),
                ("blob-1.bytes", "本体内容A"),
                // 有 .properties 但缺 .bytes：跳过
                (
                    "blob-2.properties",
                    &sample_properties("maven-proxy", "/a/app-2.0.jar", "sha-b", "20"),
                ),
                // 软删：跳过（即便有 .bytes）
                (
                    "blob-3.properties",
                    "@Repo.repo-name=maven-proxy\n@BlobStore.blob-name=/a/del.jar\ndeleted=true\n",
                ),
                ("blob-3.bytes", "已删本体"),
            ],
        );

        let mut entries = enumerate_blob_entries(root).unwrap();
        entries.sort_by(|a, b| a.blob_name.cmp(&b.blob_name));
        assert_eq!(entries.len(), 1);
        assert_eq!(entries[0].repo_name, "maven-proxy");
        assert_eq!(entries[0].blob_name, "/a/app-1.0.jar");
        assert!(entries[0].bytes_path.is_file());
    }
}
