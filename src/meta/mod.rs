//! 元数据访问层：SQLite 是元数据的唯一真源，其他模块经此读写，不绕过直连 DB。
//!
//! 本批仅提供地基所需的最小能力：连接池初始化、跑迁移，以及用户的建/计数/查重。

use std::path::Path;

use sqlx::sqlite::{SqliteConnectOptions, SqlitePoolOptions};
use sqlx::SqlitePool;
use uuid::Uuid;

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
}
