//! 目录浏览折叠逻辑（FR-75）：把同前缀的制品索引折叠为「一层」目录项。
//!
//! 纯函数、无副作用（不查库、不碰 blob），便于穷举测试。给定一个已归一化的目录前缀
//! 与该前缀下命中的制品列表，产出该层的「直接子目录 + 直接文件」两类条目（类文件浏览器），
//! 不做整棵子树的扁平铺开。鉴权过滤由上层先行处理，本层只折叠。

use crate::meta::ArtifactRecord;

/// 目录项类型：子目录或文件。
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum DirEntryKind {
    /// 子目录（其下还有更深层制品）。
    Folder,
    /// 文件（叶子制品）。
    File,
}

impl DirEntryKind {
    /// 稳定字符串名（供 API 序列化与前端区分）。
    pub fn as_str(self) -> &'static str {
        match self {
            DirEntryKind::Folder => "folder",
            DirEntryKind::File => "file",
        }
    }
}

/// 一条目录项：名称 + 类型，文件附带元数据（子目录元数据为空）。
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct DirEntry {
    /// 本层内的条目名（不含前缀，不含尾斜杠）。
    pub name: String,
    /// 条目类型。
    pub kind: DirEntryKind,
    /// 文件字节大小（子目录为 None）。
    pub size: Option<i64>,
    /// 文件 sha256（子目录为 None）。
    pub sha256: Option<String>,
    /// 文件创建时间（子目录为 None）。
    pub created_at: Option<String>,
}

/// 把前缀下命中的制品折叠为一层目录项。
///
/// `prefix` 为已归一化的目录前缀：空串表示仓库根，否则形如 `dir/`（以 `/` 结尾）。
/// 仅取每条制品相对前缀的**第一段**：第一段后还有 `/` 的归为子目录（去重），否则为文件。
/// 结果按「目录在前、文件在后，各自名称升序」排序，贴近文件浏览器习惯。
pub fn collapse_directory_entries(prefix: &str, records: &[ArtifactRecord]) -> Vec<DirEntry> {
    use std::collections::BTreeMap;

    // 子目录名 → 占位（去重）；文件名 → 记录引用
    let mut folders: BTreeMap<String, ()> = BTreeMap::new();
    let mut files: BTreeMap<String, &ArtifactRecord> = BTreeMap::new();

    for rec in records {
        // 去掉前缀，只看相对部分（防御性：不以前缀开头的记录跳过）
        let Some(relative) = rec.path.strip_prefix(prefix) else {
            continue;
        };
        if relative.is_empty() {
            continue;
        }
        match relative.split_once('/') {
            // 第一段后仍有 `/` → 是子目录（取第一段为目录名）
            Some((first, _rest)) => {
                if !first.is_empty() {
                    folders.entry(first.to_string()).or_insert(());
                }
            }
            // 无更深 `/` → 当前层的文件
            None => {
                files.insert(relative.to_string(), rec);
            }
        }
    }

    let mut entries: Vec<DirEntry> = Vec::with_capacity(folders.len() + files.len());
    // 目录在前（升序）
    for name in folders.into_keys() {
        entries.push(DirEntry {
            name,
            kind: DirEntryKind::Folder,
            size: None,
            sha256: None,
            created_at: None,
        });
    }
    // 文件在后（升序），附带元数据
    for (name, rec) in files {
        entries.push(DirEntry {
            name,
            kind: DirEntryKind::File,
            size: Some(rec.size),
            sha256: Some(rec.sha256.clone()),
            created_at: Some(rec.created_at.clone()),
        });
    }
    entries
}

#[cfg(test)]
mod tests {
    use super::*;

    /// 构造一条最小制品记录。
    fn 制品(path: &str) -> ArtifactRecord {
        ArtifactRecord {
            id: "id".to_string(),
            repo_id: "r".to_string(),
            path: path.to_string(),
            size: 7,
            sha256: "abc".to_string(),
            sha1: "s".to_string(),
            md5: "s".to_string(),
            sha512: "s".to_string(),
            content_type: None,
            cached: 0,
            created_at: "2026-01-01T00:00:00Z".to_string(),
        }
    }

    #[test]
    fn 折叠一层_区分子目录与文件并去重() {
        let recs = vec![
            制品("dir/a.txt"),
            制品("dir/sub/b.txt"),
            制品("dir/sub/c.txt"), // sub 应去重为一条目录
            制品("dir/z.bin"),
        ];
        let entries = collapse_directory_entries("dir/", &recs);
        // 目录在前（sub），文件在后（a.txt、z.bin）升序
        assert_eq!(entries.len(), 3);
        assert_eq!(entries[0].name, "sub");
        assert_eq!(entries[0].kind, DirEntryKind::Folder);
        assert_eq!(entries[1].name, "a.txt");
        assert_eq!(entries[1].kind, DirEntryKind::File);
        assert_eq!(entries[1].size, Some(7));
        assert_eq!(entries[1].sha256.as_deref(), Some("abc"));
        assert_eq!(entries[2].name, "z.bin");
    }

    #[test]
    fn 折叠根目录_空前缀取第一段() {
        let recs = vec![制品("dir/a.txt"), 制品("top.txt")];
        let entries = collapse_directory_entries("", &recs);
        assert_eq!(entries.len(), 2);
        assert_eq!(entries[0].name, "dir");
        assert_eq!(entries[0].kind, DirEntryKind::Folder);
        assert_eq!(entries[1].name, "top.txt");
        assert_eq!(entries[1].kind, DirEntryKind::File);
    }

    #[test]
    fn 折叠不扁平铺开深层文件() {
        let recs = vec![制品("dir/sub/b.txt")];
        let entries = collapse_directory_entries("dir/", &recs);
        // 只见 sub 目录，不见 b.txt
        assert_eq!(entries.len(), 1);
        assert_eq!(entries[0].name, "sub");
        assert_eq!(entries[0].kind, DirEntryKind::Folder);
    }

    #[test]
    fn 折叠跳过不属于前缀的记录() {
        let recs = vec![制品("other/x.txt")];
        let entries = collapse_directory_entries("dir/", &recs);
        assert!(entries.is_empty());
    }
}
