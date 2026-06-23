//! 仓库、ACL 与制品索引的元数据存取（FR-06/07/10/13）。
//!
//! 与 `meta/mod.rs` 同属元数据访问层，仅在 `MetaStore` 上扩展仓库相关读写；
//! SQLite 仍是元数据唯一真源，其他模块经此读写，不绕过直连 DB。

use uuid::Uuid;

use super::{MetaError, MetaStore};

/// 仓库可见性。以小写字符串存储于 DB，避免魔法字符串散落各处。
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Visibility {
    /// 公开：匿名可读。
    Public,
    /// 私有：对未授权（含匿名）一律拒绝。
    Private,
}

impl Visibility {
    /// 转为入库的字符串表示。
    pub fn as_str(self) -> &'static str {
        match self {
            Visibility::Public => "public",
            Visibility::Private => "private",
        }
    }

    /// 从 DB 字符串解析可见性；未知值按最严格回退为 Private，绝不误判为公开。
    pub fn from_db_str(s: &str) -> Self {
        match s {
            "public" => Visibility::Public,
            // 未知 / 损坏取值一律降级为私有，防止意外公开私有仓库
            _ => Visibility::Private,
        }
    }
}

/// 仓库类型。
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum RepoType {
    /// 托管型：直接接收上传并提供下载。
    Hosted,
    /// 代理型：代理上游并缓存。
    Proxy,
}

impl RepoType {
    /// 转为入库的字符串表示。
    pub fn as_str(self) -> &'static str {
        match self {
            RepoType::Hosted => "hosted",
            RepoType::Proxy => "proxy",
        }
    }

    /// 从 DB 字符串解析类型；未知值回退为 hosted（不引入上游拉取行为）。
    pub fn from_db_str(s: &str) -> Self {
        match s {
            "proxy" => RepoType::Proxy,
            _ => RepoType::Hosted,
        }
    }
}

/// 每仓库 ACL 的权限级别。
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Permission {
    /// 读权限。
    Read,
    /// 写权限。
    Write,
}

impl Permission {
    /// 转为入库的字符串表示。
    pub fn as_str(self) -> &'static str {
        match self {
            Permission::Read => "read",
            Permission::Write => "write",
        }
    }

    /// 从 DB 字符串解析权限；未知值按最小权限回退为 read，绝不误授写权限。
    pub fn from_db_str(s: &str) -> Self {
        match s {
            "write" => Permission::Write,
            _ => Permission::Read,
        }
    }
}

/// 仓库记录，字段对齐 `repositories` 表。
#[derive(Debug, Clone, sqlx::FromRow)]
pub struct RepositoryRecord {
    /// 仓库主键。
    pub id: String,
    /// 仓库名（唯一）。
    pub name: String,
    /// 格式字符串（maven | npm | docker | raw）。
    pub format: String,
    /// 类型字符串（hosted | proxy）。
    pub r#type: String,
    /// 可见性字符串（public | private）。
    pub visibility: String,
    /// 上游地址（proxy 适用）。
    pub upstream_url: Option<String>,
    /// 上游凭据引用（仅存引用，真值走配置 / env，绝不入库明文）。
    pub upstream_auth_ref: Option<String>,
    /// 创建时间（ISO8601）。
    pub created_at: String,
}

/// 仓库创建入参：把可枚举字段以类型表达，避免字符串散落。
#[derive(Debug, Clone)]
pub struct NewRepository<'a> {
    /// 仓库名。
    pub name: &'a str,
    /// 格式字符串（由上层校验合法性）。
    pub format: &'a str,
    /// 类型。
    pub r#type: RepoType,
    /// 可见性。
    pub visibility: Visibility,
    /// 上游地址（proxy 适用）。
    pub upstream_url: Option<&'a str>,
    /// 上游凭据引用（仅引用，不含真值）。
    pub upstream_auth_ref: Option<&'a str>,
}

/// ACL 条目记录，字段对齐 `repo_acl` 表。
#[derive(Debug, Clone, sqlx::FromRow)]
pub struct AclRecord {
    /// ACL 条目主键。
    pub id: String,
    /// 所属仓库主键。
    pub repo_id: String,
    /// 被授权用户主键。
    pub user_id: String,
    /// 权限字符串（read | write）。
    pub permission: String,
}

/// 制品索引记录，字段对齐 `artifacts` 表（DB 仅存索引与多校验和）。
#[derive(Debug, Clone, sqlx::FromRow)]
pub struct ArtifactRecord {
    /// 制品主键。
    pub id: String,
    /// 所属仓库主键。
    pub repo_id: String,
    /// 制品路径（仓库内唯一）。
    pub path: String,
    /// 字节大小。
    pub size: i64,
    /// sha256 摘要（blob 寻址以此为准）。
    pub sha256: String,
    /// 内容类型（可空）。
    pub content_type: Option<String>,
    /// 是否为 proxy 缓存制品（0/1）。
    pub cached: i64,
    /// 创建时间（ISO8601）。
    pub created_at: String,
}

impl MetaStore {
    /// 创建仓库。仓库名已存在时返回底层唯一约束错误（name UNIQUE）。
    ///
    /// 上游凭据真值绝不入库，DB 仅在 `upstream_auth_ref` 存引用。
    pub async fn create_repository(&self, repo: NewRepository<'_>) -> Result<String, MetaError> {
        let id = Uuid::new_v4().to_string();
        sqlx::query(
            "INSERT INTO repositories \
                (id, name, format, type, visibility, upstream_url, upstream_auth_ref) \
             VALUES (?, ?, ?, ?, ?, ?, ?)",
        )
        .bind(&id)
        .bind(repo.name)
        .bind(repo.format)
        .bind(repo.r#type.as_str())
        .bind(repo.visibility.as_str())
        .bind(repo.upstream_url)
        .bind(repo.upstream_auth_ref)
        .execute(self.pool())
        .await?;
        Ok(id)
    }

    /// 按主键查仓库；不存在时返回 None。
    pub async fn get_repository_by_id(
        &self,
        id: &str,
    ) -> Result<Option<RepositoryRecord>, MetaError> {
        let record = sqlx::query_as::<_, RepositoryRecord>(
            "SELECT id, name, format, type, visibility, upstream_url, upstream_auth_ref, created_at \
             FROM repositories WHERE id = ?",
        )
        .bind(id)
        .fetch_optional(self.pool())
        .await?;
        Ok(record)
    }

    /// 列出全部仓库，按创建时间升序。鉴权过滤由上层按身份处理。
    pub async fn list_repositories(&self) -> Result<Vec<RepositoryRecord>, MetaError> {
        let records = sqlx::query_as::<_, RepositoryRecord>(
            "SELECT id, name, format, type, visibility, upstream_url, upstream_auth_ref, created_at \
             FROM repositories ORDER BY created_at ASC, id ASC",
        )
        .fetch_all(self.pool())
        .await?;
        Ok(records)
    }

    /// 更新仓库的可配置字段：可见性、上游地址、上游凭据引用。
    ///
    /// 仅按需更新传入的字段：None 时保持原值不变。返回是否命中记录。
    pub async fn update_repository(
        &self,
        id: &str,
        visibility: Option<Visibility>,
        upstream_url: Option<&str>,
        upstream_auth_ref: Option<&str>,
    ) -> Result<bool, MetaError> {
        // 用 COALESCE 让 NULL 入参保持原值，避免拼接多条 SQL 分支
        let affected = sqlx::query(
            "UPDATE repositories SET \
                visibility = COALESCE(?, visibility), \
                upstream_url = COALESCE(?, upstream_url), \
                upstream_auth_ref = COALESCE(?, upstream_auth_ref) \
             WHERE id = ?",
        )
        .bind(visibility.map(|v| v.as_str()))
        .bind(upstream_url)
        .bind(upstream_auth_ref)
        .bind(id)
        .execute(self.pool())
        .await?
        .rows_affected();
        Ok(affected > 0)
    }

    /// 删除仓库（级联删除其 ACL 与制品索引，由外键 ON DELETE CASCADE 保证）。
    ///
    /// 返回是否命中记录（false 表示仓库不存在）。
    pub async fn delete_repository(&self, id: &str) -> Result<bool, MetaError> {
        let affected = sqlx::query("DELETE FROM repositories WHERE id = ?")
            .bind(id)
            .execute(self.pool())
            .await?
            .rows_affected();
        Ok(affected > 0)
    }

    /// 为某用户在某仓库授予一条 ACL（read 或 write）。
    ///
    /// 同一 (repo, user, permission) 重复授予时返回底层唯一约束错误（由唯一索引保证）。
    pub async fn create_acl(
        &self,
        repo_id: &str,
        user_id: &str,
        permission: Permission,
    ) -> Result<String, MetaError> {
        let id = Uuid::new_v4().to_string();
        sqlx::query("INSERT INTO repo_acl (id, repo_id, user_id, permission) VALUES (?, ?, ?, ?)")
            .bind(&id)
            .bind(repo_id)
            .bind(user_id)
            .bind(permission.as_str())
            .execute(self.pool())
            .await?;
        Ok(id)
    }

    /// 列出某仓库的全部 ACL 条目，按用户主键升序。
    pub async fn list_acl_by_repo(&self, repo_id: &str) -> Result<Vec<AclRecord>, MetaError> {
        let records = sqlx::query_as::<_, AclRecord>(
            "SELECT id, repo_id, user_id, permission FROM repo_acl \
             WHERE repo_id = ? ORDER BY user_id ASC, permission ASC",
        )
        .bind(repo_id)
        .fetch_all(self.pool())
        .await?;
        Ok(records)
    }

    /// 按主键查 ACL 条目；不存在时返回 None（用于删除前的归属校验）。
    pub async fn get_acl_by_id(&self, id: &str) -> Result<Option<AclRecord>, MetaError> {
        let record = sqlx::query_as::<_, AclRecord>(
            "SELECT id, repo_id, user_id, permission FROM repo_acl WHERE id = ?",
        )
        .bind(id)
        .fetch_optional(self.pool())
        .await?;
        Ok(record)
    }

    /// 删除一条 ACL 条目。返回是否命中记录。
    pub async fn delete_acl(&self, id: &str) -> Result<bool, MetaError> {
        let affected = sqlx::query("DELETE FROM repo_acl WHERE id = ?")
            .bind(id)
            .execute(self.pool())
            .await?
            .rows_affected();
        Ok(affected > 0)
    }

    /// 查某用户对某仓库的 ACL 权限集合（可能含 read 与 write 多条）。
    ///
    /// 供授权判定取该用户在该仓库的所有授权，由 authz 纯函数综合判定。
    pub async fn list_user_permissions(
        &self,
        repo_id: &str,
        user_id: &str,
    ) -> Result<Vec<Permission>, MetaError> {
        let rows: Vec<(String,)> = sqlx::query_as(
            "SELECT permission FROM repo_acl WHERE repo_id = ? AND user_id = ?",
        )
        .bind(repo_id)
        .bind(user_id)
        .fetch_all(self.pool())
        .await?;
        Ok(rows
            .into_iter()
            .map(|(p,)| Permission::from_db_str(&p))
            .collect())
    }

    /// 列出某用户拥有读或写权限的仓库主键集合（供列表端点过滤私有仓库）。
    pub async fn list_repo_ids_with_read(&self, user_id: &str) -> Result<Vec<String>, MetaError> {
        // read 与 write 都意味着可读，故只要在 ACL 中命中该用户即视为可读
        let rows: Vec<(String,)> =
            sqlx::query_as("SELECT DISTINCT repo_id FROM repo_acl WHERE user_id = ?")
                .bind(user_id)
                .fetch_all(self.pool())
                .await?;
        Ok(rows.into_iter().map(|(r,)| r).collect())
    }

    /// 列出某仓库的制品索引，按路径升序。鉴权过滤由上层处理。
    pub async fn list_artifacts_by_repo(
        &self,
        repo_id: &str,
    ) -> Result<Vec<ArtifactRecord>, MetaError> {
        let records = sqlx::query_as::<_, ArtifactRecord>(
            "SELECT id, repo_id, path, size, sha256, content_type, cached, created_at \
             FROM artifacts WHERE repo_id = ? ORDER BY path ASC",
        )
        .bind(repo_id)
        .fetch_all(self.pool())
        .await?;
        Ok(records)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::meta::Role;

    /// 建一个测试用用户，返回 id。
    async fn 建用户(store: &MetaStore, name: &str) -> String {
        store.create_user(name, "哈希", Role::User).await.unwrap()
    }

    /// 建一个测试用仓库，返回 id。
    async fn 建仓库(store: &MetaStore, name: &str, vis: Visibility) -> String {
        store
            .create_repository(NewRepository {
                name,
                format: "raw",
                r#type: RepoType::Hosted,
                visibility: vis,
                upstream_url: None,
                upstream_auth_ref: None,
            })
            .await
            .unwrap()
    }

    #[test]
    fn 枚举字符串往返与未知值降级() {
        assert_eq!(Visibility::from_db_str("public"), Visibility::Public);
        // 未知 / 损坏一律降级为私有，绝不误判公开
        assert_eq!(Visibility::from_db_str("открыт"), Visibility::Private);
        assert_eq!(Visibility::from_db_str(""), Visibility::Private);
        assert_eq!(RepoType::from_db_str("proxy"), RepoType::Proxy);
        assert_eq!(RepoType::from_db_str("未知"), RepoType::Hosted);
        // 未知权限降级为 read，绝不误授写
        assert_eq!(Permission::from_db_str("write"), Permission::Write);
        assert_eq!(Permission::from_db_str("admin"), Permission::Read);
    }

    #[tokio::test]
    async fn 建仓库后可按主键查出() {
        let store = MetaStore::open_in_memory().await.unwrap();
        let id = 建仓库(&store, "libs", Visibility::Private).await;
        let got = store.get_repository_by_id(&id).await.unwrap().unwrap();
        assert_eq!(got.name, "libs");
        assert_eq!(got.format, "raw");
        assert_eq!(got.r#type, "hosted");
        assert_eq!(got.visibility, "private");
    }

    #[tokio::test]
    async fn 仓库名唯一约束拒绝重复() {
        let store = MetaStore::open_in_memory().await.unwrap();
        建仓库(&store, "dup", Visibility::Public).await;
        let err = store
            .create_repository(NewRepository {
                name: "dup",
                format: "npm",
                r#type: RepoType::Hosted,
                visibility: Visibility::Public,
                upstream_url: None,
                upstream_auth_ref: None,
            })
            .await;
        assert!(err.is_err());
        assert_eq!(store.list_repositories().await.unwrap().len(), 1);
    }

    #[tokio::test]
    async fn 更新仓库可见性按需生效() {
        let store = MetaStore::open_in_memory().await.unwrap();
        let id = 建仓库(&store, "r", Visibility::Public).await;
        assert!(store
            .update_repository(&id, Some(Visibility::Private), None, None)
            .await
            .unwrap());
        let got = store.get_repository_by_id(&id).await.unwrap().unwrap();
        assert_eq!(got.visibility, "private");
        // 更新不存在仓库返回 false
        assert!(!store
            .update_repository("无此仓库", Some(Visibility::Public), None, None)
            .await
            .unwrap());
    }

    #[tokio::test]
    async fn 代理仓库仅存上游凭据引用() {
        let store = MetaStore::open_in_memory().await.unwrap();
        let id = store
            .create_repository(NewRepository {
                name: "mirror",
                format: "maven",
                r#type: RepoType::Proxy,
                visibility: Visibility::Public,
                upstream_url: Some("https://repo1.maven.org/maven2"),
                upstream_auth_ref: Some("upstream-cred-1"),
            })
            .await
            .unwrap();
        let got = store.get_repository_by_id(&id).await.unwrap().unwrap();
        assert_eq!(got.upstream_url.as_deref(), Some("https://repo1.maven.org/maven2"));
        // DB 仅存引用，不含凭据真值
        assert_eq!(got.upstream_auth_ref.as_deref(), Some("upstream-cred-1"));
    }

    #[tokio::test]
    async fn 删除仓库级联清理_acl() {
        let store = MetaStore::open_in_memory().await.unwrap();
        let uid = 建用户(&store, "u").await;
        let rid = 建仓库(&store, "r", Visibility::Private).await;
        store.create_acl(&rid, &uid, Permission::Read).await.unwrap();
        assert_eq!(store.list_acl_by_repo(&rid).await.unwrap().len(), 1);

        assert!(store.delete_repository(&rid).await.unwrap());
        // 外键级联应已清掉其 ACL
        assert!(store.list_acl_by_repo(&rid).await.unwrap().is_empty());
        assert!(!store.delete_repository("无此仓库").await.unwrap());
    }

    #[tokio::test]
    async fn acl_增列删与重复约束() {
        let store = MetaStore::open_in_memory().await.unwrap();
        let uid = 建用户(&store, "u").await;
        let rid = 建仓库(&store, "r", Visibility::Private).await;

        let aid = store.create_acl(&rid, &uid, Permission::Read).await.unwrap();
        // 同 (repo,user,permission) 重复授予应失败
        assert!(store.create_acl(&rid, &uid, Permission::Read).await.is_err());
        // 但同一用户可再授 write（不同 permission）
        store.create_acl(&rid, &uid, Permission::Write).await.unwrap();

        let list = store.list_acl_by_repo(&rid).await.unwrap();
        assert_eq!(list.len(), 2);

        // 按主键查与删除
        assert!(store.get_acl_by_id(&aid).await.unwrap().is_some());
        assert!(store.delete_acl(&aid).await.unwrap());
        assert!(store.get_acl_by_id(&aid).await.unwrap().is_none());
        assert!(!store.delete_acl("无此条目").await.unwrap());
    }

    #[tokio::test]
    async fn 查用户权限集合与可读仓库列表() {
        let store = MetaStore::open_in_memory().await.unwrap();
        let uid = 建用户(&store, "u").await;
        let r1 = 建仓库(&store, "r1", Visibility::Private).await;
        let r2 = 建仓库(&store, "r2", Visibility::Private).await;
        store.create_acl(&r1, &uid, Permission::Read).await.unwrap();
        store.create_acl(&r1, &uid, Permission::Write).await.unwrap();
        store.create_acl(&r2, &uid, Permission::Write).await.unwrap();

        let mut perms = store.list_user_permissions(&r1, &uid).await.unwrap();
        perms.sort_by_key(|p| p.as_str());
        assert_eq!(perms, vec![Permission::Read, Permission::Write]);

        // 仅 write 也算可读
        let mut readable = store.list_repo_ids_with_read(&uid).await.unwrap();
        readable.sort();
        let mut expect = vec![r1.clone(), r2.clone()];
        expect.sort();
        assert_eq!(readable, expect);

        // 无任何授权的仓库不在权限集合中
        let none = store.list_user_permissions("无此仓库", &uid).await.unwrap();
        assert!(none.is_empty());
    }

    #[tokio::test]
    async fn 列出仓库制品索引() {
        let store = MetaStore::open_in_memory().await.unwrap();
        let rid = 建仓库(&store, "r", Visibility::Public).await;
        // 当前批次无制品写入路径，直接插一条索引验证读取
        sqlx::query(
            "INSERT INTO artifacts (id, repo_id, path, size, sha256, sha1, md5, sha512) \
             VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
        )
        .bind(Uuid::new_v4().to_string())
        .bind(&rid)
        .bind("a/b/c.txt")
        .bind(12i64)
        .bind("sha256值")
        .bind("sha1值")
        .bind("md5值")
        .bind("sha512值")
        .execute(store.pool())
        .await
        .unwrap();

        let list = store.list_artifacts_by_repo(&rid).await.unwrap();
        assert_eq!(list.len(), 1);
        assert_eq!(list[0].path, "a/b/c.txt");
        assert_eq!(list[0].sha256, "sha256值");
        // 空仓库返回空表
        let empty = 建仓库(&store, "empty", Visibility::Public).await;
        assert!(store.list_artifacts_by_repo(&empty).await.unwrap().is_empty());
    }
}
