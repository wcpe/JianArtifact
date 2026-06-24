//! 用户组/团队、组成员与组级仓库 ACL 的元数据存取（FR-49 / ADR-0007）。
//!
//! 与 `meta/repo.rs` 同属元数据访问层，在 `MetaStore` 上扩展组相关读写；
//! SQLite 仍是元数据唯一真源，其他模块经此读写，不绕过直连 DB。
//! 授权判定仍由 `authz` 纯函数完成：本层只负责把"用户经组继承的权限集合"取出，
//! 与直接-用户 ACL 并集后交判定，组继承不改既有直接-ACL 判定结论。

use uuid::Uuid;

use super::{MetaError, MetaStore, Permission};

/// 用户组记录，字段对齐 `groups` 表。
#[derive(Debug, Clone, sqlx::FromRow)]
pub struct GroupRecord {
    /// 组主键。
    pub id: String,
    /// 组名（唯一）。
    pub name: String,
    /// 创建时间（ISO8601）。
    pub created_at: String,
}

/// 组成员记录：某用户属于某组（连同用户名便于管理界面展示）。
#[derive(Debug, Clone, sqlx::FromRow)]
pub struct GroupMemberRecord {
    /// 成员用户主键。
    pub user_id: String,
    /// 成员用户名。
    pub username: String,
}

/// 组级 ACL 条目记录，字段对齐 `repo_group_acl` 表。
#[derive(Debug, Clone, sqlx::FromRow)]
pub struct GroupAclRecord {
    /// 组 ACL 条目主键。
    pub id: String,
    /// 所属仓库主键。
    pub repo_id: String,
    /// 被授权组主键。
    pub group_id: String,
    /// 权限动作字符串（read | write | delete | admin）。
    pub permission: String,
}

impl MetaStore {
    /// 创建用户组。组名已存在时返回底层唯一约束错误（name UNIQUE）。
    pub async fn create_group(&self, name: &str) -> Result<String, MetaError> {
        let id = Uuid::new_v4().to_string();
        sqlx::query("INSERT INTO groups (id, name) VALUES (?, ?)")
            .bind(&id)
            .bind(name)
            .execute(self.pool())
            .await?;
        Ok(id)
    }

    /// 按主键查用户组；不存在时返回 None。
    pub async fn get_group_by_id(&self, id: &str) -> Result<Option<GroupRecord>, MetaError> {
        let record = sqlx::query_as::<_, GroupRecord>(
            "SELECT id, name, created_at FROM groups WHERE id = ?",
        )
        .bind(id)
        .fetch_optional(self.pool())
        .await?;
        Ok(record)
    }

    /// 列出全部用户组，按创建时间升序。
    pub async fn list_groups(&self) -> Result<Vec<GroupRecord>, MetaError> {
        let records = sqlx::query_as::<_, GroupRecord>(
            "SELECT id, name, created_at FROM groups ORDER BY created_at ASC, id ASC",
        )
        .fetch_all(self.pool())
        .await?;
        Ok(records)
    }

    /// 删除用户组（级联清理其成员关系与组 ACL，由外键 ON DELETE CASCADE 保证）。
    ///
    /// 返回是否命中记录（false 表示组不存在）。
    pub async fn delete_group(&self, id: &str) -> Result<bool, MetaError> {
        let affected = sqlx::query("DELETE FROM groups WHERE id = ?")
            .bind(id)
            .execute(self.pool())
            .await?
            .rows_affected();
        Ok(affected > 0)
    }

    /// 把用户加入组。重复加入同一组返回底层唯一约束错误（复合主键保证幂等边界）。
    pub async fn add_group_member(&self, group_id: &str, user_id: &str) -> Result<(), MetaError> {
        sqlx::query("INSERT INTO user_groups (group_id, user_id) VALUES (?, ?)")
            .bind(group_id)
            .bind(user_id)
            .execute(self.pool())
            .await?;
        Ok(())
    }

    /// 把用户移出组。返回是否命中记录（false 表示该用户本不在组内）。
    pub async fn remove_group_member(
        &self,
        group_id: &str,
        user_id: &str,
    ) -> Result<bool, MetaError> {
        let affected = sqlx::query("DELETE FROM user_groups WHERE group_id = ? AND user_id = ?")
            .bind(group_id)
            .bind(user_id)
            .execute(self.pool())
            .await?
            .rows_affected();
        Ok(affected > 0)
    }

    /// 列出某组的全部成员（连同用户名），按用户名升序。
    pub async fn list_group_members(
        &self,
        group_id: &str,
    ) -> Result<Vec<GroupMemberRecord>, MetaError> {
        let records = sqlx::query_as::<_, GroupMemberRecord>(
            "SELECT ug.user_id AS user_id, u.username AS username \
             FROM user_groups ug JOIN users u ON u.id = ug.user_id \
             WHERE ug.group_id = ? ORDER BY u.username ASC",
        )
        .bind(group_id)
        .fetch_all(self.pool())
        .await?;
        Ok(records)
    }

    /// 为某组在某仓库授予一条 ACL（四级动作之一）。
    ///
    /// 同一 (repo, group, permission) 重复授予时返回底层唯一约束错误（由唯一索引保证）。
    pub async fn create_group_acl(
        &self,
        repo_id: &str,
        group_id: &str,
        permission: Permission,
    ) -> Result<String, MetaError> {
        let id = Uuid::new_v4().to_string();
        sqlx::query(
            "INSERT INTO repo_group_acl (id, repo_id, group_id, permission) VALUES (?, ?, ?, ?)",
        )
        .bind(&id)
        .bind(repo_id)
        .bind(group_id)
        .bind(permission.as_str())
        .execute(self.pool())
        .await?;
        Ok(id)
    }

    /// 列出某仓库的全部组 ACL 条目，按组主键升序。
    pub async fn list_group_acl_by_repo(
        &self,
        repo_id: &str,
    ) -> Result<Vec<GroupAclRecord>, MetaError> {
        let records = sqlx::query_as::<_, GroupAclRecord>(
            "SELECT id, repo_id, group_id, permission FROM repo_group_acl \
             WHERE repo_id = ? ORDER BY group_id ASC, permission ASC",
        )
        .bind(repo_id)
        .fetch_all(self.pool())
        .await?;
        Ok(records)
    }

    /// 按主键查组 ACL 条目；不存在时返回 None（用于删除前的归属校验）。
    pub async fn get_group_acl_by_id(&self, id: &str) -> Result<Option<GroupAclRecord>, MetaError> {
        let record = sqlx::query_as::<_, GroupAclRecord>(
            "SELECT id, repo_id, group_id, permission FROM repo_group_acl WHERE id = ?",
        )
        .bind(id)
        .fetch_optional(self.pool())
        .await?;
        Ok(record)
    }

    /// 删除一条组 ACL 条目。返回是否命中记录。
    pub async fn delete_group_acl(&self, id: &str) -> Result<bool, MetaError> {
        let affected = sqlx::query("DELETE FROM repo_group_acl WHERE id = ?")
            .bind(id)
            .execute(self.pool())
            .await?
            .rows_affected();
        Ok(affected > 0)
    }

    /// 查某用户经其所属各组在某仓库继承的权限集合（可能多条、可能跨多个组）。
    ///
    /// 供授权判定与直接-用户 ACL 并集：JOIN 成员关系与组 ACL，取该用户经任一所属组
    /// 获得的全部动作。授权判定（[`crate::authz`]）对并集按动作蕴含关系给出结论。
    pub async fn list_user_group_permissions(
        &self,
        repo_id: &str,
        user_id: &str,
    ) -> Result<Vec<Permission>, MetaError> {
        let rows: Vec<(String,)> = sqlx::query_as(
            "SELECT rga.permission FROM repo_group_acl rga \
             JOIN user_groups ug ON ug.group_id = rga.group_id \
             WHERE rga.repo_id = ? AND ug.user_id = ?",
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
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::meta::{NewRepository, RepoType, Role, Visibility};

    /// 建一个测试用用户，返回 id。
    async fn 建用户(store: &MetaStore, name: &str) -> String {
        store.create_user(name, "哈希", Role::User).await.unwrap()
    }

    /// 建一个测试用私有仓库，返回 id。
    async fn 建仓库(store: &MetaStore, name: &str) -> String {
        store
            .create_repository(NewRepository {
                name,
                format: "raw",
                r#type: RepoType::Hosted,
                visibility: Visibility::Private,
                upstream_url: None,
                upstream_auth_ref: None,
            })
            .await
            .unwrap()
    }

    #[tokio::test]
    async fn 组_增查删与重名约束() {
        let store = MetaStore::open_in_memory().await.unwrap();
        let gid = store.create_group("dev-team").await.unwrap();
        // 同名再建应失败（name UNIQUE）
        assert!(store.create_group("dev-team").await.is_err());

        let got = store.get_group_by_id(&gid).await.unwrap().unwrap();
        assert_eq!(got.name, "dev-team");
        assert_eq!(store.list_groups().await.unwrap().len(), 1);

        assert!(store.delete_group(&gid).await.unwrap());
        assert!(store.get_group_by_id(&gid).await.unwrap().is_none());
        // 删不存在组返回 false
        assert!(!store.delete_group("无此组").await.unwrap());
    }

    #[tokio::test]
    async fn 成员_增移列与重复加入约束() {
        let store = MetaStore::open_in_memory().await.unwrap();
        let gid = store.create_group("g").await.unwrap();
        let u1 = 建用户(&store, "u1").await;
        let u2 = 建用户(&store, "u2").await;

        store.add_group_member(&gid, &u1).await.unwrap();
        // 重复加入同一组应失败（复合主键）
        assert!(store.add_group_member(&gid, &u1).await.is_err());
        store.add_group_member(&gid, &u2).await.unwrap();

        let members = store.list_group_members(&gid).await.unwrap();
        assert_eq!(members.len(), 2);
        assert_eq!(members[0].username, "u1");

        // 移除一个成员
        assert!(store.remove_group_member(&gid, &u1).await.unwrap());
        assert_eq!(store.list_group_members(&gid).await.unwrap().len(), 1);
        // 移除本不在组内的用户返回 false
        assert!(!store.remove_group_member(&gid, &u1).await.unwrap());
    }

    #[tokio::test]
    async fn 删组级联清理成员与组_acl() {
        let store = MetaStore::open_in_memory().await.unwrap();
        let gid = store.create_group("g").await.unwrap();
        let uid = 建用户(&store, "u").await;
        let rid = 建仓库(&store, "r").await;
        store.add_group_member(&gid, &uid).await.unwrap();
        store
            .create_group_acl(&rid, &gid, Permission::Write)
            .await
            .unwrap();

        assert!(store.delete_group(&gid).await.unwrap());
        // 级联应已清掉其成员与组 ACL
        assert!(store.list_group_members(&gid).await.unwrap().is_empty());
        assert!(store.list_group_acl_by_repo(&rid).await.unwrap().is_empty());
    }

    #[tokio::test]
    async fn 删用户级联清理其组成员关系() {
        let store = MetaStore::open_in_memory().await.unwrap();
        let gid = store.create_group("g").await.unwrap();
        let uid = 建用户(&store, "u").await;
        store.add_group_member(&gid, &uid).await.unwrap();
        assert_eq!(store.list_group_members(&gid).await.unwrap().len(), 1);

        assert!(store.delete_user(&uid).await.unwrap());
        // 外键级联应已清掉该用户的组成员关系
        assert!(store.list_group_members(&gid).await.unwrap().is_empty());
    }

    #[tokio::test]
    async fn 组_acl_增列删与重复约束() {
        let store = MetaStore::open_in_memory().await.unwrap();
        let gid = store.create_group("g").await.unwrap();
        let rid = 建仓库(&store, "r").await;

        let aid = store
            .create_group_acl(&rid, &gid, Permission::Read)
            .await
            .unwrap();
        // 同 (repo, group, permission) 重复授予应失败
        assert!(store
            .create_group_acl(&rid, &gid, Permission::Read)
            .await
            .is_err());
        // 同组可再授 write（不同 permission）
        store
            .create_group_acl(&rid, &gid, Permission::Write)
            .await
            .unwrap();

        assert_eq!(store.list_group_acl_by_repo(&rid).await.unwrap().len(), 2);
        assert!(store.get_group_acl_by_id(&aid).await.unwrap().is_some());
        assert!(store.delete_group_acl(&aid).await.unwrap());
        assert!(store.get_group_acl_by_id(&aid).await.unwrap().is_none());
        assert!(!store.delete_group_acl("无此条目").await.unwrap());
    }

    #[tokio::test]
    async fn 用户经组继承权限集合() {
        let store = MetaStore::open_in_memory().await.unwrap();
        let g1 = store.create_group("g1").await.unwrap();
        let g2 = store.create_group("g2").await.unwrap();
        let uid = 建用户(&store, "u").await;
        let rid = 建仓库(&store, "r").await;

        // 用户属于两个组：g1 在该仓库被授 read，g2 被授 write
        store.add_group_member(&g1, &uid).await.unwrap();
        store.add_group_member(&g2, &uid).await.unwrap();
        store
            .create_group_acl(&rid, &g1, Permission::Read)
            .await
            .unwrap();
        store
            .create_group_acl(&rid, &g2, Permission::Write)
            .await
            .unwrap();

        let mut perms = store.list_user_group_permissions(&rid, &uid).await.unwrap();
        perms.sort_by_key(|p| p.as_str());
        // 经两组继承到 read 与 write
        assert_eq!(perms, vec![Permission::Read, Permission::Write]);

        // 非成员用户继承不到任何组权限
        let other = 建用户(&store, "other").await;
        assert!(store
            .list_user_group_permissions(&rid, &other)
            .await
            .unwrap()
            .is_empty());
    }

    #[tokio::test]
    async fn 退组后不再继承组权限() {
        let store = MetaStore::open_in_memory().await.unwrap();
        let gid = store.create_group("g").await.unwrap();
        let uid = 建用户(&store, "u").await;
        let rid = 建仓库(&store, "r").await;
        store.add_group_member(&gid, &uid).await.unwrap();
        store
            .create_group_acl(&rid, &gid, Permission::Write)
            .await
            .unwrap();
        assert_eq!(
            store
                .list_user_group_permissions(&rid, &uid)
                .await
                .unwrap()
                .len(),
            1
        );

        // 退组后继承权限即失效
        store.remove_group_member(&gid, &uid).await.unwrap();
        assert!(store
            .list_user_group_permissions(&rid, &uid)
            .await
            .unwrap()
            .is_empty());
    }
}
