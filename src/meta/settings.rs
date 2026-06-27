//! 动态配置 KV 存取（FR-106，ADR-0028）：`app_settings` 表的读写。
//!
//! 与 `meta/mod.rs` 同属元数据访问层，仅在 `MetaStore` 上扩展 `app_settings` 表读写；
//! SQLite 仍是元数据唯一真源，其他模块经此读写、不绕过直连 DB。本表只存「非密钥」动态配置节
//! （凭据 / bootstrap 项绝不入库，由装配层白名单把关），值为该节的 JSON 片段。

use std::time::{SystemTime, UNIX_EPOCH};

use super::{MetaError, MetaStore};

/// 动态配置记录（查询返回）。
#[derive(Debug, Clone, sqlx::FromRow)]
pub struct SettingRecord {
    /// 配置键（点分路径，唯一）。
    pub key: String,
    /// 配置值（该节的 JSON 片段）。
    pub value_json: String,
}

/// 取当前 Unix 秒（UTC）；系统时钟早于纪元时回落 0（仅作 updated_at 标记，不参与逻辑判定）。
fn now_unix_secs() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_secs() as i64)
        .unwrap_or(0)
}

impl MetaStore {
    /// 读取全部动态配置项（键 + JSON 值），按键升序。装配层据此构造 DB 覆盖 map。
    pub async fn load_settings(&self) -> Result<Vec<(String, String)>, MetaError> {
        let rows = sqlx::query_as::<_, SettingRecord>(
            "SELECT key, value_json FROM app_settings ORDER BY key ASC",
        )
        .fetch_all(self.pool())
        .await?;
        Ok(rows.into_iter().map(|r| (r.key, r.value_json)).collect())
    }

    /// 新增或更新一个动态配置项（upsert）。`value_json` 为该节序列化后的 JSON 片段。
    ///
    /// 调用方（装配层 / PATCH 端点）须保证 `key` 在白名单内、`value_json` 不含任何凭据明文。
    pub async fn upsert_setting(&self, key: &str, value_json: &str) -> Result<(), MetaError> {
        sqlx::query(
            "INSERT INTO app_settings (key, value_json, updated_at) \
             VALUES (?, ?, ?) \
             ON CONFLICT (key) DO UPDATE SET \
                value_json = excluded.value_json, \
                updated_at = excluded.updated_at",
        )
        .bind(key)
        .bind(value_json)
        .bind(now_unix_secs())
        .execute(self.pool())
        .await?;
        Ok(())
    }

    /// 删除一个动态配置项（恢复为文件默认 / env）。返回是否命中记录。
    pub async fn delete_setting(&self, key: &str) -> Result<bool, MetaError> {
        let affected = sqlx::query("DELETE FROM app_settings WHERE key = ?")
            .bind(key)
            .execute(self.pool())
            .await?
            .rows_affected();
        Ok(affected > 0)
    }
}

#[cfg(test)]
mod tests {
    use crate::meta::MetaStore;

    #[tokio::test]
    async fn 空库_load_settings_为空() {
        let store = MetaStore::open_in_memory().await.unwrap();
        assert!(store.load_settings().await.unwrap().is_empty());
    }

    #[tokio::test]
    async fn upsert_后能读回_且覆盖同键() {
        let store = MetaStore::open_in_memory().await.unwrap();
        store.upsert_setting("limits", "{\"a\":1}").await.unwrap();
        let rows = store.load_settings().await.unwrap();
        assert_eq!(rows, vec![("limits".to_string(), "{\"a\":1}".to_string())]);
        // 同键再 upsert 覆盖旧值
        store.upsert_setting("limits", "{\"a\":2}").await.unwrap();
        let rows = store.load_settings().await.unwrap();
        assert_eq!(rows, vec![("limits".to_string(), "{\"a\":2}".to_string())]);
    }

    #[tokio::test]
    async fn delete_后读不到_返回命中标记() {
        let store = MetaStore::open_in_memory().await.unwrap();
        store.upsert_setting("protection", "{}").await.unwrap();
        assert!(store.delete_setting("protection").await.unwrap());
        assert!(store.load_settings().await.unwrap().is_empty());
        // 再删不存在的键返回 false
        assert!(!store.delete_setting("protection").await.unwrap());
    }

    #[tokio::test]
    async fn 多键_按键升序返回() {
        let store = MetaStore::open_in_memory().await.unwrap();
        store.upsert_setting("vuln", "{}").await.unwrap();
        store.upsert_setting("limits", "{}").await.unwrap();
        store.upsert_setting("protection", "{}").await.unwrap();
        let keys: Vec<String> = store
            .load_settings()
            .await
            .unwrap()
            .into_iter()
            .map(|(k, _)| k)
            .collect();
        assert_eq!(keys, vec!["limits", "protection", "vuln"]);
    }
}
