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
    /// 聚合型（group / virtual）：自身不存 blob，按有序成员解析读取（FR-136）。
    Group,
}

impl RepoType {
    /// 转为入库的字符串表示。
    pub fn as_str(self) -> &'static str {
        match self {
            RepoType::Hosted => "hosted",
            RepoType::Proxy => "proxy",
            RepoType::Group => "group",
        }
    }

    /// 从 DB 字符串解析类型；未知值回退为 hosted（不引入上游拉取与聚合解析行为）。
    pub fn from_db_str(s: &str) -> Self {
        match s {
            "proxy" => RepoType::Proxy,
            "group" => RepoType::Group,
            _ => RepoType::Hosted,
        }
    }
}

/// 每仓库 ACL 的权限动作（四级动作，FR-48 / ADR-0007）。
///
/// 动作自低到高为 read < write < delete < admin；高动作蕴含低动作的能力，
/// 蕴含关系在授权判定（[`crate::authz`]）中体现，本枚举仅表达单条 ACL 授予的动作。
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Permission {
    /// 读权限（下载 / 浏览 / 详情）。
    Read,
    /// 写权限（上传 / 发布 / 覆盖）。
    Write,
    /// 删除权限（删除制品 / 缓存）。
    Delete,
    /// 仓库级管理权限（配置 / 删除仓库 / 维护其 ACL）。
    Admin,
}

impl Permission {
    /// 转为入库的字符串表示。
    pub fn as_str(self) -> &'static str {
        match self {
            Permission::Read => "read",
            Permission::Write => "write",
            Permission::Delete => "delete",
            Permission::Admin => "admin",
        }
    }

    /// 从 DB 字符串解析权限；未知值按最小权限回退为 read，绝不误授更高动作。
    pub fn from_db_str(s: &str) -> Self {
        match s {
            "write" => Permission::Write,
            "delete" => Permission::Delete,
            "admin" => Permission::Admin,
            // 未知 / 损坏取值一律降级为最小权限 read，绝不误授写 / 删 / 管理
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
    /// 格式字符串（maven | npm | docker | raw | pypi）。
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
    /// 权限动作字符串（read | write | delete | admin）。
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
    /// sha1 摘要（主要为客户端兼容）。
    pub sha1: String,
    /// md5 摘要（主要为客户端兼容）。
    pub md5: String,
    /// sha512 摘要。
    pub sha512: String,
    /// 内容类型（可空）。
    pub content_type: Option<String>,
    /// 是否为 proxy 缓存制品（0/1）。
    pub cached: i64,
    /// 创建时间（ISO8601）。
    pub created_at: String,
}

/// 制品索引写入入参：四校验和与大小由 blob 落盘时算得后传入。
#[derive(Debug, Clone)]
pub struct NewArtifact<'a> {
    /// 所属仓库主键。
    pub repo_id: &'a str,
    /// 制品路径（仓库内唯一）。
    pub path: &'a str,
    /// 字节大小。
    pub size: i64,
    /// sha256 摘要（blob 寻址以此为准）。
    pub sha256: &'a str,
    /// sha1 摘要。
    pub sha1: &'a str,
    /// md5 摘要。
    pub md5: &'a str,
    /// sha512 摘要。
    pub sha512: &'a str,
    /// 内容类型（可空）。
    pub content_type: Option<&'a str>,
    /// 是否为 proxy 缓存制品。
    pub cached: bool,
}

/// 跨仓库搜索命中记录：制品索引连同所属仓库的名称、格式与可见性。
#[derive(Debug, Clone, sqlx::FromRow)]
pub struct ArtifactSearchHit {
    /// 所属仓库主键。
    pub repo_id: String,
    /// 所属仓库名。
    pub repo_name: String,
    /// 所属仓库格式。
    pub repo_format: String,
    /// 所属仓库可见性字符串（public | private）。
    pub repo_visibility: String,
    /// 制品路径。
    pub path: String,
    /// sha256 摘要。
    pub sha256: String,
    /// 字节大小。
    pub size: i64,
    /// 创建时间。
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

    /// 按仓库名查仓库；不存在时返回 None（格式端点据路径中的仓库名定位仓库）。
    pub async fn get_repository_by_name(
        &self,
        name: &str,
    ) -> Result<Option<RepositoryRecord>, MetaError> {
        let record = sqlx::query_as::<_, RepositoryRecord>(
            "SELECT id, name, format, type, visibility, upstream_url, upstream_auth_ref, created_at \
             FROM repositories WHERE name = ?",
        )
        .bind(name)
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
        let rows: Vec<(String,)> =
            sqlx::query_as("SELECT permission FROM repo_acl WHERE repo_id = ? AND user_id = ?")
                .bind(repo_id)
                .bind(user_id)
                .fetch_all(self.pool())
                .await?;
        Ok(rows
            .into_iter()
            .map(|(p,)| Permission::from_db_str(&p))
            .collect())
    }

    /// 列出某用户拥有读权限的仓库主键集合（供列表端点过滤私有仓库）。
    ///
    /// 可读来源有二并取并集：① 直接授予该用户的 ACL；② 该用户经所属各组继承的组 ACL。
    /// 任一动作（read / write / delete / admin）都蕴含可读，故命中任一即视为可读（FR-49）。
    pub async fn list_repo_ids_with_read(&self, user_id: &str) -> Result<Vec<String>, MetaError> {
        // 直接-用户 ACL 与经组继承的组 ACL 取并集（UNION 自动去重）
        let rows: Vec<(String,)> = sqlx::query_as(
            "SELECT repo_id FROM repo_acl WHERE user_id = ? \
             UNION \
             SELECT rga.repo_id FROM repo_group_acl rga \
             JOIN user_groups ug ON ug.group_id = rga.group_id \
             WHERE ug.user_id = ?",
        )
        .bind(user_id)
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
            "SELECT id, repo_id, path, size, sha256, sha1, md5, sha512, content_type, cached, created_at \
             FROM artifacts WHERE repo_id = ? ORDER BY path ASC",
        )
        .bind(repo_id)
        .fetch_all(self.pool())
        .await?;
        Ok(records)
    }

    /// 列出某仓库内位于给定路径前缀下的制品索引（FR-75 目录浏览），按路径升序。
    ///
    /// `prefix` 为已归一化的目录前缀（空串表示仓库根，否则形如 `dir/`，调用方负责补尾斜杠）。
    /// 用 `LIKE prefix||'%' ESCAPE '\'` 做前缀匹配，并对前缀中的 `%`/`_`/`\` 转义，避免通配符
    /// 把兄弟前缀（如 `docsx/`）误纳入 `docs/` 的列举。鉴权过滤由上层处理。
    pub async fn list_artifacts_under_prefix(
        &self,
        repo_id: &str,
        prefix: &str,
    ) -> Result<Vec<ArtifactRecord>, MetaError> {
        // 空前缀（仓库根）等价列全仓，复用既有查询，避免无谓 LIKE
        if prefix.is_empty() {
            return self.list_artifacts_by_repo(repo_id).await;
        }
        let pattern = format!("{}%", escape_like(prefix));
        let records = sqlx::query_as::<_, ArtifactRecord>(
            "SELECT id, repo_id, path, size, sha256, sha1, md5, sha512, content_type, cached, created_at \
             FROM artifacts WHERE repo_id = ? AND path LIKE ? ESCAPE '\\' ORDER BY path ASC",
        )
        .bind(repo_id)
        .bind(pattern)
        .fetch_all(self.pool())
        .await?;
        Ok(records)
    }

    /// 按 (仓库, 路径) 查制品索引；不存在时返回 None。
    pub async fn get_artifact(
        &self,
        repo_id: &str,
        path: &str,
    ) -> Result<Option<ArtifactRecord>, MetaError> {
        let record = sqlx::query_as::<_, ArtifactRecord>(
            "SELECT id, repo_id, path, size, sha256, sha1, md5, sha512, content_type, cached, created_at \
             FROM artifacts WHERE repo_id = ? AND path = ?",
        )
        .bind(repo_id)
        .bind(path)
        .fetch_optional(self.pool())
        .await?;
        Ok(record)
    }

    /// 落定一条制品索引（upsert）。
    ///
    /// 同 (仓库, 路径) 已存在时整体覆盖为新内容（覆盖策略由上层 Format 先行判定，
    /// 此处仅负责落库）。本层不接触 blob 本体，仅写索引与多校验和。
    pub async fn upsert_artifact(&self, art: NewArtifact<'_>) -> Result<String, MetaError> {
        let id = Uuid::new_v4().to_string();
        // ON CONFLICT 命中 (repo_id, path) 唯一索引时覆盖；id 与 created_at 保持原值
        sqlx::query(
            "INSERT INTO artifacts \
                (id, repo_id, path, size, sha256, sha1, md5, sha512, content_type, cached) \
             VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?) \
             ON CONFLICT (repo_id, path) DO UPDATE SET \
                size = excluded.size, \
                sha256 = excluded.sha256, \
                sha1 = excluded.sha1, \
                md5 = excluded.md5, \
                sha512 = excluded.sha512, \
                content_type = excluded.content_type, \
                cached = excluded.cached",
        )
        .bind(&id)
        .bind(art.repo_id)
        .bind(art.path)
        .bind(art.size)
        .bind(art.sha256)
        .bind(art.sha1)
        .bind(art.md5)
        .bind(art.sha512)
        .bind(art.content_type)
        .bind(art.cached as i64)
        .execute(self.pool())
        .await?;
        Ok(id)
    }

    /// 删除一条制品索引（按仓库 + 路径）。返回是否命中记录。
    ///
    /// 仅删索引；blob 本体的删除由上层（storage）单独处理，以保证次序与回滚可控。
    pub async fn delete_artifact(&self, repo_id: &str, path: &str) -> Result<bool, MetaError> {
        let affected = sqlx::query("DELETE FROM artifacts WHERE repo_id = ? AND path = ?")
            .bind(repo_id)
            .bind(path)
            .execute(self.pool())
            .await?
            .rows_affected();
        Ok(affected > 0)
    }

    /// 统计某 sha256 在所有仓库索引中的引用计数（用于删 blob 前判断是否仍被引用）。
    pub async fn count_artifacts_by_sha256(&self, sha256: &str) -> Result<i64, MetaError> {
        let count: i64 = sqlx::query_scalar("SELECT COUNT(*) FROM artifacts WHERE sha256 = ?")
            .bind(sha256)
            .fetch_one(self.pool())
            .await?;
        Ok(count)
    }

    /// 跨仓库搜索制品：按路径关键字（LIKE）匹配，连带所属仓库信息返回。
    ///
    /// 鉴权过滤由上层按调用方读权限处理——本层只负责检索 + 可选格式过滤 + 分页，
    /// 不在此判定可见性（绝不在 SQL 内静默放行，过滤职责清晰单一）。
    pub async fn search_artifacts(
        &self,
        keyword: &str,
        format: Option<&str>,
        offset: i64,
        limit: i64,
    ) -> Result<Vec<ArtifactSearchHit>, MetaError> {
        // LIKE 通配：把关键字夹在 % 之间做包含匹配；keyword 经参数绑定，无注入风险
        let pattern = format!("%{keyword}%");
        let records = sqlx::query_as::<_, ArtifactSearchHit>(
            "SELECT a.repo_id AS repo_id, r.name AS repo_name, r.format AS repo_format, \
                    r.visibility AS repo_visibility, a.path AS path, a.sha256 AS sha256, \
                    a.size AS size, a.created_at AS created_at \
             FROM artifacts a JOIN repositories r ON r.id = a.repo_id \
             WHERE a.path LIKE ? AND (? IS NULL OR r.format = ?) \
             ORDER BY r.name ASC, a.path ASC \
             LIMIT ? OFFSET ?",
        )
        .bind(&pattern)
        .bind(format)
        .bind(format)
        .bind(limit)
        .bind(offset)
        .fetch_all(self.pool())
        .await?;
        Ok(records)
    }

    /// 设定 group 仓库的有序成员列表（FR-136）：先清后插，position 按入参顺序从 0 递增。
    ///
    /// 在单事务内「删旧 + 插新」，避免并发下出现半截成员列表。`member_ids` 顺序即解析顺序。
    /// group 自身存储在 repositories 表（type='group'），本方法只维护其成员关联。
    pub async fn set_repo_group_members(
        &self,
        group_repo_id: &str,
        member_ids: &[String],
    ) -> Result<(), MetaError> {
        let mut tx = self.pool().begin().await?;
        sqlx::query("DELETE FROM repository_group_members WHERE group_repo_id = ?")
            .bind(group_repo_id)
            .execute(&mut *tx)
            .await?;
        for (position, member_id) in member_ids.iter().enumerate() {
            sqlx::query(
                "INSERT INTO repository_group_members (group_repo_id, member_repo_id, position) \
                 VALUES (?, ?, ?)",
            )
            .bind(group_repo_id)
            .bind(member_id)
            .bind(position as i64)
            .execute(&mut *tx)
            .await?;
        }
        tx.commit().await?;
        Ok(())
    }

    /// 列出 group 仓库的有序成员仓库记录（FR-136），按 position 升序连表取出。
    ///
    /// 供 group GET 解析按序遍历；返回成员仓库的完整记录（格式 / 类型 / 可见性等），
    /// 鉴权过滤与逐成员命中判定由上层（api）处理，本层只负责按序取成员。
    pub async fn list_repo_group_members(
        &self,
        group_repo_id: &str,
    ) -> Result<Vec<RepositoryRecord>, MetaError> {
        let records = sqlx::query_as::<_, RepositoryRecord>(
            "SELECT r.id, r.name, r.format, r.type, r.visibility, r.upstream_url, \
                    r.upstream_auth_ref, r.created_at \
             FROM repository_group_members m \
             JOIN repositories r ON r.id = m.member_repo_id \
             WHERE m.group_repo_id = ? \
             ORDER BY m.position ASC",
        )
        .bind(group_repo_id)
        .fetch_all(self.pool())
        .await?;
        Ok(records)
    }
}

/// 转义 LIKE 模式中的特殊字符（`\` / `%` / `_`），配合 `ESCAPE '\'` 使其按字面匹配。
///
/// 仅用于前缀匹配场景：调用方拼接后再追加 `%` 通配，故此处不引入额外通配语义，
/// 避免用户路径里的 `%`/`_` 被当作通配符把兄弟前缀误纳入列举。
fn escape_like(input: &str) -> String {
    let mut out = String::with_capacity(input.len());
    for ch in input.chars() {
        if matches!(ch, '\\' | '%' | '_') {
            out.push('\\');
        }
        out.push(ch);
    }
    out
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
        // 四级动作字符串往返
        assert_eq!(Permission::from_db_str("read"), Permission::Read);
        assert_eq!(Permission::from_db_str("write"), Permission::Write);
        assert_eq!(Permission::from_db_str("delete"), Permission::Delete);
        assert_eq!(Permission::from_db_str("admin"), Permission::Admin);
        // 未知 / 损坏权限降级为最小权限 read，绝不误授写 / 删 / 管理
        assert_eq!(Permission::from_db_str("superadmin"), Permission::Read);
        assert_eq!(Permission::from_db_str(""), Permission::Read);
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
        assert_eq!(
            got.upstream_url.as_deref(),
            Some("https://repo1.maven.org/maven2")
        );
        // DB 仅存引用，不含凭据真值
        assert_eq!(got.upstream_auth_ref.as_deref(), Some("upstream-cred-1"));
    }

    #[tokio::test]
    async fn 删除仓库级联清理_acl() {
        let store = MetaStore::open_in_memory().await.unwrap();
        let uid = 建用户(&store, "u").await;
        let rid = 建仓库(&store, "r", Visibility::Private).await;
        store
            .create_acl(&rid, &uid, Permission::Read)
            .await
            .unwrap();
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

        let aid = store
            .create_acl(&rid, &uid, Permission::Read)
            .await
            .unwrap();
        // 同 (repo,user,permission) 重复授予应失败
        assert!(store
            .create_acl(&rid, &uid, Permission::Read)
            .await
            .is_err());
        // 但同一用户可再授 write（不同 permission）
        store
            .create_acl(&rid, &uid, Permission::Write)
            .await
            .unwrap();

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
        store
            .create_acl(&r1, &uid, Permission::Write)
            .await
            .unwrap();
        store
            .create_acl(&r2, &uid, Permission::Write)
            .await
            .unwrap();

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

    /// 便捷：构造制品写入入参。
    fn 制品<'a>(repo_id: &'a str, path: &'a str, sha256: &'a str) -> NewArtifact<'a> {
        NewArtifact {
            repo_id,
            path,
            size: 3,
            sha256,
            sha1: "sha1值",
            md5: "md5值",
            sha512: "sha512值",
            content_type: Some("text/plain"),
            cached: false,
        }
    }

    #[tokio::test]
    async fn 列出仓库制品索引() {
        let store = MetaStore::open_in_memory().await.unwrap();
        let rid = 建仓库(&store, "r", Visibility::Public).await;
        store
            .upsert_artifact(制品(&rid, "a/b/c.txt", "sha256值"))
            .await
            .unwrap();

        let list = store.list_artifacts_by_repo(&rid).await.unwrap();
        assert_eq!(list.len(), 1);
        assert_eq!(list[0].path, "a/b/c.txt");
        assert_eq!(list[0].sha256, "sha256值");
        // 四校验和均被项目出来
        assert_eq!(list[0].sha1, "sha1值");
        assert_eq!(list[0].md5, "md5值");
        assert_eq!(list[0].sha512, "sha512值");
        // 空仓库返回空表
        let empty = 建仓库(&store, "empty", Visibility::Public).await;
        assert!(store
            .list_artifacts_by_repo(&empty)
            .await
            .unwrap()
            .is_empty());
    }

    #[tokio::test]
    async fn 按前缀列举制品_仅命中前缀且不串入兄弟前缀() {
        let store = MetaStore::open_in_memory().await.unwrap();
        let rid = 建仓库(&store, "r", Visibility::Public).await;
        for p in ["docs/a.txt", "docs/sub/b.txt", "docsx/c.txt", "top.txt"] {
            store.upsert_artifact(制品(&rid, p, "s")).await.unwrap();
        }
        // 列举 docs/ 前缀：命中 docs/a.txt 与 docs/sub/b.txt，不含兄弟前缀 docsx/c.txt
        let mut paths: Vec<String> = store
            .list_artifacts_under_prefix(&rid, "docs/")
            .await
            .unwrap()
            .into_iter()
            .map(|r| r.path)
            .collect();
        paths.sort();
        assert_eq!(paths, vec!["docs/a.txt", "docs/sub/b.txt"]);

        // 空前缀（仓库根）等价列全仓
        let all = store.list_artifacts_under_prefix(&rid, "").await.unwrap();
        assert_eq!(all.len(), 4);
    }

    #[tokio::test]
    async fn 按前缀列举制品_前缀含通配符按字面匹配() {
        let store = MetaStore::open_in_memory().await.unwrap();
        let rid = 建仓库(&store, "r", Visibility::Public).await;
        // 路径里真实含 % 与 _ 字符，转义后应按字面匹配，不当通配符
        store
            .upsert_artifact(制品(&rid, "a%b/x.txt", "s"))
            .await
            .unwrap();
        store
            .upsert_artifact(制品(&rid, "axb/y.txt", "s"))
            .await
            .unwrap();
        let paths: Vec<String> = store
            .list_artifacts_under_prefix(&rid, "a%b/")
            .await
            .unwrap()
            .into_iter()
            .map(|r| r.path)
            .collect();
        assert_eq!(paths, vec!["a%b/x.txt"], "% 应按字面匹配，不通配 axb/");
    }

    #[test]
    fn 转义_like_特殊字符() {
        assert_eq!(escape_like("a%b"), "a\\%b");
        assert_eq!(escape_like("a_b"), "a\\_b");
        assert_eq!(escape_like("a\\b"), "a\\\\b");
        assert_eq!(escape_like("docs/"), "docs/");
    }

    #[tokio::test]
    async fn 制品_upsert_覆盖同路径并可按路径查出() {
        let store = MetaStore::open_in_memory().await.unwrap();
        let rid = 建仓库(&store, "r", Visibility::Public).await;
        store
            .upsert_artifact(制品(&rid, "x/y.bin", "旧sha"))
            .await
            .unwrap();
        // 同 (仓库, 路径) 再次写入应覆盖而非新增
        store
            .upsert_artifact(NewArtifact {
                size: 9,
                ..制品(&rid, "x/y.bin", "新sha")
            })
            .await
            .unwrap();

        let list = store.list_artifacts_by_repo(&rid).await.unwrap();
        assert_eq!(list.len(), 1, "覆盖不应新增第二条");
        let one = store.get_artifact(&rid, "x/y.bin").await.unwrap().unwrap();
        assert_eq!(one.sha256, "新sha");
        assert_eq!(one.size, 9);
        // 查不存在路径返回 None
        assert!(store
            .get_artifact(&rid, "无此路径")
            .await
            .unwrap()
            .is_none());
    }

    #[tokio::test]
    async fn 删除制品索引与_sha256_引用计数() {
        let store = MetaStore::open_in_memory().await.unwrap();
        let r1 = 建仓库(&store, "r1", Visibility::Public).await;
        let r2 = 建仓库(&store, "r2", Visibility::Public).await;
        // 两个仓库引用同一 sha256
        store
            .upsert_artifact(制品(&r1, "p", "共享sha"))
            .await
            .unwrap();
        store
            .upsert_artifact(制品(&r2, "p", "共享sha"))
            .await
            .unwrap();
        assert_eq!(store.count_artifacts_by_sha256("共享sha").await.unwrap(), 2);

        // 删一条后引用计数减一（blob 仍被另一仓库引用，不应被清理）
        assert!(store.delete_artifact(&r1, "p").await.unwrap());
        assert_eq!(store.count_artifacts_by_sha256("共享sha").await.unwrap(), 1);
        // 删不存在的返回 false
        assert!(!store.delete_artifact(&r1, "p").await.unwrap());
    }

    #[tokio::test]
    async fn 跨仓库搜索按关键字与格式过滤分页() {
        let store = MetaStore::open_in_memory().await.unwrap();
        let maven = store
            .create_repository(NewRepository {
                name: "maven-repo",
                format: "maven",
                r#type: RepoType::Hosted,
                visibility: Visibility::Public,
                upstream_url: None,
                upstream_auth_ref: None,
            })
            .await
            .unwrap();
        let raw = 建仓库(&store, "raw-repo", Visibility::Private).await;
        store
            .upsert_artifact(制品(&maven, "com/foo/lib-1.0.jar", "s1"))
            .await
            .unwrap();
        store
            .upsert_artifact(制品(&raw, "docs/lib-readme.txt", "s2"))
            .await
            .unwrap();
        store
            .upsert_artifact(制品(&maven, "com/bar/other-1.0.jar", "s3"))
            .await
            .unwrap();

        // 关键字 lib 命中两条（跨两个仓库），含私有仓库命中——鉴权过滤在上层
        let hits = store.search_artifacts("lib", None, 0, 50).await.unwrap();
        assert_eq!(hits.len(), 2);
        // 命中里应带回所属仓库可见性，供上层据读权限过滤
        assert!(hits.iter().any(|h| h.repo_visibility == "private"));

        // 限定格式 maven 只命中 maven 仓库那条
        let maven_only = store
            .search_artifacts("lib", Some("maven"), 0, 50)
            .await
            .unwrap();
        assert_eq!(maven_only.len(), 1);
        assert_eq!(maven_only[0].repo_name, "maven-repo");

        // 分页 limit=1 只返回一条
        let page = store.search_artifacts("lib", None, 0, 1).await.unwrap();
        assert_eq!(page.len(), 1);
    }

    #[tokio::test]
    async fn group_成员有序设定与按序取出() {
        let store = MetaStore::open_in_memory().await.unwrap();
        let g = store
            .create_repository(NewRepository {
                name: "maven-group",
                format: "maven",
                r#type: RepoType::Group,
                visibility: Visibility::Public,
                upstream_url: None,
                upstream_auth_ref: None,
            })
            .await
            .unwrap();
        // group 类型应正确入库与解析
        let rec = store.get_repository_by_id(&g).await.unwrap().unwrap();
        assert_eq!(rec.r#type, "group");
        assert_eq!(RepoType::from_db_str(&rec.r#type), RepoType::Group);

        let a = 建仓库(&store, "a", Visibility::Public).await;
        let b = 建仓库(&store, "b", Visibility::Private).await;
        // 顺序 [b, a]：解析顺序应原样保留
        store
            .set_repo_group_members(&g, &[b.clone(), a.clone()])
            .await
            .unwrap();
        let members = store.list_repo_group_members(&g).await.unwrap();
        let ids: Vec<&str> = members.iter().map(|m| m.id.as_str()).collect();
        assert_eq!(ids, vec![b.as_str(), a.as_str()], "成员应按 position 升序");

        // 重设为 [a]：先清后插，旧成员 b 不再出现
        store
            .set_repo_group_members(&g, std::slice::from_ref(&a))
            .await
            .unwrap();
        let members = store.list_repo_group_members(&g).await.unwrap();
        assert_eq!(members.len(), 1);
        assert_eq!(members[0].id, a);

        // 空 group 合法：成员列表为空
        store.set_repo_group_members(&g, &[]).await.unwrap();
        assert!(store.list_repo_group_members(&g).await.unwrap().is_empty());
    }

    #[tokio::test]
    async fn 删除成员仓库经外键级联移出_group() {
        let store = MetaStore::open_in_memory().await.unwrap();
        let g = store
            .create_repository(NewRepository {
                name: "g",
                format: "raw",
                r#type: RepoType::Group,
                visibility: Visibility::Public,
                upstream_url: None,
                upstream_auth_ref: None,
            })
            .await
            .unwrap();
        let a = 建仓库(&store, "a", Visibility::Public).await;
        let b = 建仓库(&store, "b", Visibility::Public).await;
        store
            .set_repo_group_members(&g, &[a.clone(), b.clone()])
            .await
            .unwrap();
        assert_eq!(store.list_repo_group_members(&g).await.unwrap().len(), 2);

        // 删除成员仓库 a：经外键级联从 group 成员中移除，仅余 b
        assert!(store.delete_repository(&a).await.unwrap());
        let members = store.list_repo_group_members(&g).await.unwrap();
        assert_eq!(members.len(), 1);
        assert_eq!(members[0].id, b);

        // 删除 group 自身：成员关联级联清理，但成员仓库 b 仍在
        assert!(store.delete_repository(&g).await.unwrap());
        assert!(store.get_repository_by_id(&b).await.unwrap().is_some());
    }
}
