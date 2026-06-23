//! 元数据访问层：SQLite 是元数据的唯一真源，其他模块经此读写，不绕过直连 DB。
//!
//! 提供连接池初始化、跑迁移，以及用户与 API Token 的增删改查。

use std::path::Path;

use sqlx::sqlite::{SqliteConnectOptions, SqlitePoolOptions};
use sqlx::SqlitePool;
use uuid::Uuid;

mod repo;

pub use repo::{
    AclRecord, ArtifactRecord, NewRepository, Permission, RepoType, RepositoryRecord, Visibility,
};

/// 全局角色。以小写字符串存储于 DB，避免魔法字符串散落各处。
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Role {
    /// 管理员：管理用户、仓库、ACL 与全局配置。
    Admin,
    /// 普通注册用户。
    User,
}

impl Role {
    /// 转为入库的字符串表示。
    pub fn as_str(self) -> &'static str {
        match self {
            Role::Admin => "admin",
            Role::User => "user",
        }
    }

    /// 从 DB 字符串解析角色；未知值按最小权限回退为 User，避免越权。
    pub fn from_db_str(s: &str) -> Self {
        match s {
            "admin" => Role::Admin,
            // 未知 / 损坏取值一律降级为普通用户，绝不误判为管理员
            _ => Role::User,
        }
    }
}

/// 用户记录（不含口令明文；password_hash 为 argon2 哈希）。
#[derive(Debug, Clone, sqlx::FromRow)]
pub struct UserRecord {
    /// 用户主键。
    pub id: String,
    /// 用户名（唯一）。
    pub username: String,
    /// argon2 口令哈希。
    pub password_hash: String,
    /// 全局角色字符串（admin | user）。
    pub role: String,
    /// 是否被禁用（0/1）。
    pub disabled: i64,
    /// 创建时间（ISO8601）。
    pub created_at: String,
}

/// API Token 元数据记录（不含明文与哈希之外的敏感项；列表时哈希也不回显）。
#[derive(Debug, Clone, sqlx::FromRow)]
pub struct TokenRecord {
    /// Token 主键。
    pub id: String,
    /// 所属用户主键。
    pub user_id: String,
    /// Token 名称（用户自定义，便于辨识用途）。
    pub name: String,
    /// 创建时间（ISO8601）。
    pub created_at: String,
    /// 最近使用时间（ISO8601）；从未使用为 None。
    pub last_used_at: Option<String>,
    /// 是否已吊销（0/1）。
    pub revoked: i64,
}

/// Token 命中后连同所属用户解析出的身份信息，供认证中间件使用。
#[derive(Debug, Clone, sqlx::FromRow)]
pub struct TokenIdentity {
    /// Token 主键。
    pub token_id: String,
    /// 所属用户主键。
    pub user_id: String,
    /// 所属用户名。
    pub username: String,
    /// 所属用户全局角色字符串（admin | user）。
    pub role: String,
    /// 所属用户是否被禁用（0/1）。
    pub disabled: i64,
}

/// 元数据存储错误。
#[derive(Debug, thiserror::Error)]
pub enum MetaError {
    /// 底层 SQLite / sqlx 错误。
    #[error("数据库操作失败: {0}")]
    Database(#[from] sqlx::Error),
    /// 数据库迁移失败。
    #[error("数据库迁移失败: {0}")]
    Migrate(#[from] sqlx::migrate::MigrateError),
}

/// 元数据存储：持有 SQLite 连接池。
#[derive(Debug, Clone)]
pub struct MetaStore {
    pool: SqlitePool,
}

impl MetaStore {
    /// 打开（必要时创建）数据库文件，开启 WAL，并跑迁移建表。
    ///
    /// `db_path` 指向 SQLite 文件，其父目录须已存在（由启动流程负责创建）。
    pub async fn open(db_path: &Path) -> Result<Self, MetaError> {
        let options = SqliteConnectOptions::new()
            .filename(db_path)
            .create_if_missing(true)
            // 开启 WAL 提升并发读写表现
            .journal_mode(sqlx::sqlite::SqliteJournalMode::Wal)
            // 启用外键约束（SQLite 默认关闭）
            .foreign_keys(true);

        let pool = SqlitePoolOptions::new().connect_with(options).await?;

        // 跑编译期嵌入的迁移脚本（migrations/ 目录）
        sqlx::migrate!("./migrations").run(&pool).await?;

        Ok(Self { pool })
    }

    /// 连接池的内部访问入口，供同模块内其他文件（如 repo.rs）复用同一连接池。
    ///
    /// 仅限 crate 内 `meta` 模块内部使用，不对外暴露原始连接以保持唯一访问入口。
    pub(crate) fn pool(&self) -> &SqlitePool {
        &self.pool
    }

    /// 仅用于测试：基于内存数据库构造（每个连接独立，故连接数限制为 1）。
    #[cfg(test)]
    pub async fn open_in_memory() -> Result<Self, MetaError> {
        let options = SqliteConnectOptions::new()
            .filename(":memory:")
            .foreign_keys(true);
        let pool = SqlitePoolOptions::new()
            .max_connections(1)
            .connect_with(options)
            .await?;
        sqlx::migrate!("./migrations").run(&pool).await?;
        Ok(Self { pool })
    }

    /// 统计用户总数（用于首启引导判定空库）。
    pub async fn count_users(&self) -> Result<i64, MetaError> {
        let count: i64 = sqlx::query_scalar("SELECT COUNT(*) FROM users")
            .fetch_one(&self.pool)
            .await?;
        Ok(count)
    }

    /// 创建用户。口令哈希由调用方计算后传入，本层不接触明文。
    ///
    /// 用户名已存在时返回底层唯一约束错误（由 username UNIQUE 保证不重复）。
    pub async fn create_user(
        &self,
        username: &str,
        password_hash: &str,
        role: Role,
    ) -> Result<String, MetaError> {
        let id = Uuid::new_v4().to_string();
        sqlx::query(
            "INSERT INTO users (id, username, password_hash, role) VALUES (?, ?, ?, ?)",
        )
        .bind(&id)
        .bind(username)
        .bind(password_hash)
        .bind(role.as_str())
        .execute(&self.pool)
        .await?;
        Ok(id)
    }

    /// 按用户名查用户；不存在时返回 None。
    pub async fn get_user_by_username(
        &self,
        username: &str,
    ) -> Result<Option<UserRecord>, MetaError> {
        let record = sqlx::query_as::<_, UserRecord>(
            "SELECT id, username, password_hash, role, disabled, created_at \
             FROM users WHERE username = ?",
        )
        .bind(username)
        .fetch_optional(&self.pool)
        .await?;
        Ok(record)
    }

    /// 按主键查用户；不存在时返回 None。
    pub async fn get_user_by_id(&self, id: &str) -> Result<Option<UserRecord>, MetaError> {
        let record = sqlx::query_as::<_, UserRecord>(
            "SELECT id, username, password_hash, role, disabled, created_at \
             FROM users WHERE id = ?",
        )
        .bind(id)
        .fetch_optional(&self.pool)
        .await?;
        Ok(record)
    }

    /// 列出全部用户，按创建时间升序。供管理员用户管理界面使用。
    pub async fn list_users(&self) -> Result<Vec<UserRecord>, MetaError> {
        let records = sqlx::query_as::<_, UserRecord>(
            "SELECT id, username, password_hash, role, disabled, created_at \
             FROM users ORDER BY created_at ASC, id ASC",
        )
        .fetch_all(&self.pool)
        .await?;
        Ok(records)
    }

    /// 更新用户的角色与禁用状态（管理员用户管理）。
    ///
    /// 仅按需更新传入的字段：`role` / `disabled` 为 None 时保持原值不变。
    /// 返回是否命中了某条用户记录（false 表示用户不存在）。
    pub async fn update_user(
        &self,
        id: &str,
        role: Option<Role>,
        disabled: Option<bool>,
    ) -> Result<bool, MetaError> {
        // 用 COALESCE 让 NULL 入参保持原值，避免拼接多条 SQL 分支
        let affected = sqlx::query(
            "UPDATE users SET \
                role = COALESCE(?, role), \
                disabled = COALESCE(?, disabled) \
             WHERE id = ?",
        )
        .bind(role.map(|r| r.as_str()))
        .bind(disabled.map(|d| d as i64))
        .bind(id)
        .execute(&self.pool)
        .await?
        .rows_affected();
        Ok(affected > 0)
    }

    /// 删除用户（级联删除其 Token 与 ACL，由外键 ON DELETE CASCADE 保证）。
    ///
    /// 返回是否命中了某条用户记录（false 表示用户不存在）。
    pub async fn delete_user(&self, id: &str) -> Result<bool, MetaError> {
        let affected = sqlx::query("DELETE FROM users WHERE id = ?")
            .bind(id)
            .execute(&self.pool)
            .await?
            .rows_affected();
        Ok(affected > 0)
    }

    /// 为用户签发 API Token：仅存其哈希，绝不存明文。返回新 Token 主键。
    pub async fn create_token(
        &self,
        user_id: &str,
        name: &str,
        token_hash: &str,
    ) -> Result<String, MetaError> {
        let id = Uuid::new_v4().to_string();
        sqlx::query("INSERT INTO tokens (id, user_id, name, token_hash) VALUES (?, ?, ?, ?)")
            .bind(&id)
            .bind(user_id)
            .bind(name)
            .bind(token_hash)
            .execute(&self.pool)
            .await?;
        Ok(id)
    }

    /// 按 Token 哈希查未吊销 Token 连同其所属用户的身份信息。
    ///
    /// 只返回未吊销（revoked = 0）的 Token；吊销或不存在均返回 None。
    /// 哈希比对在 SQL 层做等值匹配，调用方应已用稳定哈希算法算好入参。
    pub async fn get_token_identity_by_hash(
        &self,
        token_hash: &str,
    ) -> Result<Option<TokenIdentity>, MetaError> {
        let record = sqlx::query_as::<_, TokenIdentity>(
            "SELECT t.id AS token_id, u.id AS user_id, u.username AS username, \
                    u.role AS role, u.disabled AS disabled \
             FROM tokens t JOIN users u ON u.id = t.user_id \
             WHERE t.token_hash = ? AND t.revoked = 0",
        )
        .bind(token_hash)
        .fetch_optional(&self.pool)
        .await?;
        Ok(record)
    }

    /// 列出某用户自己的全部 Token 元数据（不含明文与哈希），按创建时间升序。
    pub async fn list_tokens_by_user(&self, user_id: &str) -> Result<Vec<TokenRecord>, MetaError> {
        let records = sqlx::query_as::<_, TokenRecord>(
            "SELECT id, user_id, name, created_at, last_used_at, revoked \
             FROM tokens WHERE user_id = ? ORDER BY created_at ASC, id ASC",
        )
        .bind(user_id)
        .fetch_all(&self.pool)
        .await?;
        Ok(records)
    }

    /// 按主键查 Token 元数据；不存在时返回 None（用于吊销前的归属校验）。
    pub async fn get_token_by_id(&self, id: &str) -> Result<Option<TokenRecord>, MetaError> {
        let record = sqlx::query_as::<_, TokenRecord>(
            "SELECT id, user_id, name, created_at, last_used_at, revoked \
             FROM tokens WHERE id = ?",
        )
        .bind(id)
        .fetch_optional(&self.pool)
        .await?;
        Ok(record)
    }

    /// 吊销指定 Token（置 revoked = 1）。返回是否命中记录。
    pub async fn revoke_token(&self, id: &str) -> Result<bool, MetaError> {
        let affected = sqlx::query("UPDATE tokens SET revoked = 1 WHERE id = ?")
            .bind(id)
            .execute(&self.pool)
            .await?
            .rows_affected();
        Ok(affected > 0)
    }

    /// 更新 Token 最近使用时间为当前时刻（命中鉴权后调用）。
    pub async fn touch_token_last_used(&self, id: &str) -> Result<(), MetaError> {
        sqlx::query("UPDATE tokens SET last_used_at = CURRENT_TIMESTAMP WHERE id = ?")
            .bind(id)
            .execute(&self.pool)
            .await?;
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn 空库计数为零() {
        let store = MetaStore::open_in_memory().await.unwrap();
        assert_eq!(store.count_users().await.unwrap(), 0);
    }

    #[tokio::test]
    async fn 建用户后计数递增且可按名查出() {
        let store = MetaStore::open_in_memory().await.unwrap();
        let id = store
            .create_user("alice", "哈希值", Role::Admin)
            .await
            .unwrap();
        assert_eq!(store.count_users().await.unwrap(), 1);

        let found = store.get_user_by_username("alice").await.unwrap().unwrap();
        assert_eq!(found.id, id);
        assert_eq!(found.username, "alice");
        assert_eq!(found.role, "admin");
        assert_eq!(found.disabled, 0);
    }

    #[tokio::test]
    async fn 查不存在的用户返回_none() {
        let store = MetaStore::open_in_memory().await.unwrap();
        assert!(store.get_user_by_username("无此人").await.unwrap().is_none());
    }

    #[tokio::test]
    async fn 用户名唯一约束拒绝重复() {
        let store = MetaStore::open_in_memory().await.unwrap();
        store
            .create_user("bob", "哈希1", Role::User)
            .await
            .unwrap();
        // 同名再建应失败（username UNIQUE）
        let err = store.create_user("bob", "哈希2", Role::User).await;
        assert!(err.is_err());
        // 失败后计数不应增加
        assert_eq!(store.count_users().await.unwrap(), 1);
    }

    #[test]
    fn 角色字符串往返与未知值降级() {
        assert_eq!(Role::from_db_str("admin"), Role::Admin);
        assert_eq!(Role::from_db_str("user"), Role::User);
        // 未知 / 损坏值一律降级为普通用户，绝不误升管理员
        assert_eq!(Role::from_db_str("superuser"), Role::User);
        assert_eq!(Role::from_db_str(""), Role::User);
    }

    #[tokio::test]
    async fn 按主键查用户与列表() {
        let store = MetaStore::open_in_memory().await.unwrap();
        let id = store.create_user("alice", "h", Role::Admin).await.unwrap();
        let by_id = store.get_user_by_id(&id).await.unwrap().unwrap();
        assert_eq!(by_id.username, "alice");
        assert!(store.get_user_by_id("无此主键").await.unwrap().is_none());

        store.create_user("bob", "h", Role::User).await.unwrap();
        let all = store.list_users().await.unwrap();
        assert_eq!(all.len(), 2);
    }

    #[tokio::test]
    async fn 更新用户角色与禁用按需生效() {
        let store = MetaStore::open_in_memory().await.unwrap();
        let id = store.create_user("u", "h", Role::User).await.unwrap();

        // 仅改禁用，不动角色
        assert!(store.update_user(&id, None, Some(true)).await.unwrap());
        let u = store.get_user_by_id(&id).await.unwrap().unwrap();
        assert_eq!(u.disabled, 1);
        assert_eq!(u.role, "user");

        // 仅改角色，禁用保持
        assert!(store
            .update_user(&id, Some(Role::Admin), None)
            .await
            .unwrap());
        let u = store.get_user_by_id(&id).await.unwrap().unwrap();
        assert_eq!(u.role, "admin");
        assert_eq!(u.disabled, 1);

        // 更新不存在用户返回 false
        assert!(!store
            .update_user("无此人", Some(Role::User), None)
            .await
            .unwrap());
    }

    #[tokio::test]
    async fn 删除用户级联删除其_token() {
        let store = MetaStore::open_in_memory().await.unwrap();
        let uid = store.create_user("u", "h", Role::User).await.unwrap();
        store.create_token(&uid, "t", "hash1").await.unwrap();
        assert_eq!(store.list_tokens_by_user(&uid).await.unwrap().len(), 1);

        assert!(store.delete_user(&uid).await.unwrap());
        assert!(store.get_user_by_id(&uid).await.unwrap().is_none());
        // 外键级联应已清掉其 Token
        assert!(store.list_tokens_by_user(&uid).await.unwrap().is_empty());
        // 删除不存在用户返回 false
        assert!(!store.delete_user("无此人").await.unwrap());
    }

    #[tokio::test]
    async fn token_签发_命中身份_吊销后不再命中() {
        let store = MetaStore::open_in_memory().await.unwrap();
        let uid = store.create_user("dev", "h", Role::User).await.unwrap();
        let tid = store.create_token(&uid, "ci", "tok-hash").await.unwrap();

        // 哈希命中可解析出身份
        let ident = store
            .get_token_identity_by_hash("tok-hash")
            .await
            .unwrap()
            .unwrap();
        assert_eq!(ident.token_id, tid);
        assert_eq!(ident.user_id, uid);
        assert_eq!(ident.username, "dev");
        assert_eq!(ident.role, "user");
        assert_eq!(ident.disabled, 0);

        // 吊销后哈希不再命中
        assert!(store.revoke_token(&tid).await.unwrap());
        assert!(store
            .get_token_identity_by_hash("tok-hash")
            .await
            .unwrap()
            .is_none());
        // 未知哈希不命中
        assert!(store
            .get_token_identity_by_hash("不存在")
            .await
            .unwrap()
            .is_none());
    }

    #[tokio::test]
    async fn token_列表归属与最近使用更新() {
        let store = MetaStore::open_in_memory().await.unwrap();
        let uid = store.create_user("dev", "h", Role::User).await.unwrap();
        let other = store.create_user("other", "h", Role::User).await.unwrap();
        let tid = store.create_token(&uid, "ci", "h1").await.unwrap();
        store.create_token(&other, "x", "h2").await.unwrap();

        // 列表只含本人 Token
        let mine = store.list_tokens_by_user(&uid).await.unwrap();
        assert_eq!(mine.len(), 1);
        assert_eq!(mine[0].id, tid);
        assert!(mine[0].last_used_at.is_none());

        // 触达最近使用后应有时间戳
        store.touch_token_last_used(&tid).await.unwrap();
        let after = store.get_token_by_id(&tid).await.unwrap().unwrap();
        assert!(after.last_used_at.is_some());
    }
}
