//! 漏洞公告镜像的元数据存取（FR-70，ADR-0012）。
//!
//! 与 `meta/mod.rs` 同属元数据访问层，在 `MetaStore` 上扩展漏洞镜像相关读写；
//! SQLite 仍是元数据唯一真源，`vuln` 模块经此落库，不绕过直连 DB。
//!
//! 本层仅做忠实落库（公告 + 受影响坐标 + 刷新状态），不实现按制品坐标的匹配（FR-71）。
//! 接收基础类型 / 本层定义的记录结构，**不反向依赖上层 `vuln` 的类型**，保持依赖单向。

use uuid::Uuid;

use super::{MetaError, MetaStore};

/// 待落库的单条漏洞公告（由上层 `vuln` 解析后转入，不含本机 created_at）。
#[derive(Debug, Clone)]
pub struct NewAdvisory {
    /// 公告唯一标识。
    pub id: String,
    /// 数据来源标识（如 osv）。
    pub source: String,
    /// 简要描述。
    pub summary: Option<String>,
    /// 详细描述。
    pub details: Option<String>,
    /// 严重度。
    pub severity: Option<String>,
    /// 上游最近修改时间（ISO8601）。
    pub modified: Option<String>,
    /// 发布时间（ISO8601）。
    pub published: Option<String>,
    /// 受影响坐标（逐包展开）。
    pub affected: Vec<NewAffected>,
}

/// 待落库的单个受影响坐标。
#[derive(Debug, Clone)]
pub struct NewAffected {
    /// 生态名。
    pub ecosystem: String,
    /// 包坐标名。
    pub package: String,
    /// 受影响版本范围（原始 JSON 文本）。
    pub ranges: Option<String>,
    /// 受影响具体版本列表（原始 JSON 文本）。
    pub versions: Option<String>,
}

/// 镜像刷新状态记录。
#[derive(Debug, Clone, sqlx::FromRow)]
pub struct MirrorStateRecord {
    /// 数据来源标识。
    pub source: String,
    /// 镜像的生态。
    pub ecosystem: String,
    /// 最近一次成功刷新时间（ISO8601）；从未刷新为 None。
    pub last_refreshed: Option<String>,
    /// 最近一次刷新落库的公告条数。
    pub advisory_count: i64,
}

impl MetaStore {
    /// 幂等落库单条漏洞公告：在一个事务内 upsert 公告行，并整体替换其受影响坐标行。
    ///
    /// 同一公告 id 反复落库结果一致（覆盖旧值、不留重复坐标行），支持刷新幂等。
    /// 受影响坐标先删后插，避免上游公告调整坐标后本机残留陈旧行。
    pub async fn upsert_advisory(&self, adv: &NewAdvisory) -> Result<(), MetaError> {
        let mut tx = self.pool().begin().await?;

        // upsert 公告主行：主键冲突时覆盖（公告内容可能随上游修订变化）
        sqlx::query(
            "INSERT INTO vuln_advisories \
                (id, source, summary, details, severity, modified, published) \
             VALUES (?, ?, ?, ?, ?, ?, ?) \
             ON CONFLICT(id) DO UPDATE SET \
                source = excluded.source, \
                summary = excluded.summary, \
                details = excluded.details, \
                severity = excluded.severity, \
                modified = excluded.modified, \
                published = excluded.published",
        )
        .bind(&adv.id)
        .bind(&adv.source)
        .bind(&adv.summary)
        .bind(&adv.details)
        .bind(&adv.severity)
        .bind(&adv.modified)
        .bind(&adv.published)
        .execute(&mut *tx)
        .await?;

        // 整体替换受影响坐标：先清旧行
        sqlx::query("DELETE FROM vuln_advisory_affected WHERE advisory_id = ?")
            .bind(&adv.id)
            .execute(&mut *tx)
            .await?;

        // 再逐条插入新坐标行
        for aff in &adv.affected {
            sqlx::query(
                "INSERT INTO vuln_advisory_affected \
                    (id, advisory_id, ecosystem, package, ranges, versions) \
                 VALUES (?, ?, ?, ?, ?, ?)",
            )
            .bind(Uuid::new_v4().to_string())
            .bind(&adv.id)
            .bind(&aff.ecosystem)
            .bind(&aff.package)
            .bind(&aff.ranges)
            .bind(&aff.versions)
            .execute(&mut *tx)
            .await?;
        }

        tx.commit().await?;
        Ok(())
    }

    /// 统计本机已落库的漏洞公告总数。
    pub async fn count_advisories(&self) -> Result<i64, MetaError> {
        let count: i64 = sqlx::query_scalar("SELECT COUNT(*) FROM vuln_advisories")
            .fetch_one(self.pool())
            .await?;
        Ok(count)
    }

    /// 统计某公告的受影响坐标行数（供测试与运维核对）。
    pub async fn count_advisory_affected(&self, advisory_id: &str) -> Result<i64, MetaError> {
        let count: i64 =
            sqlx::query_scalar("SELECT COUNT(*) FROM vuln_advisory_affected WHERE advisory_id = ?")
                .bind(advisory_id)
                .fetch_one(self.pool())
                .await?;
        Ok(count)
    }

    /// 记录某来源某生态的一次成功刷新状态（last_refreshed 置为当前时刻）。
    ///
    /// 主键冲突时覆盖，便于幂等刷新后观察最近状态与落库条数。
    pub async fn record_mirror_refresh(
        &self,
        source: &str,
        ecosystem: &str,
        advisory_count: i64,
    ) -> Result<(), MetaError> {
        sqlx::query(
            "INSERT INTO vuln_mirror_state \
                (source, ecosystem, last_refreshed, advisory_count) \
             VALUES (?, ?, CURRENT_TIMESTAMP, ?) \
             ON CONFLICT(source, ecosystem) DO UPDATE SET \
                last_refreshed = CURRENT_TIMESTAMP, \
                advisory_count = excluded.advisory_count",
        )
        .bind(source)
        .bind(ecosystem)
        .bind(advisory_count)
        .execute(self.pool())
        .await?;
        Ok(())
    }

    /// 查询某来源某生态的刷新状态；从未刷新返回 None。
    pub async fn get_mirror_state(
        &self,
        source: &str,
        ecosystem: &str,
    ) -> Result<Option<MirrorStateRecord>, MetaError> {
        let record = sqlx::query_as::<_, MirrorStateRecord>(
            "SELECT source, ecosystem, last_refreshed, advisory_count \
             FROM vuln_mirror_state WHERE source = ? AND ecosystem = ?",
        )
        .bind(source)
        .bind(ecosystem)
        .fetch_optional(self.pool())
        .await?;
        Ok(record)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// 构造一条带单个受影响坐标的待落库公告。
    fn 样例公告(id: &str) -> NewAdvisory {
        NewAdvisory {
            id: id.to_string(),
            source: "osv".to_string(),
            summary: Some("摘要".to_string()),
            details: None,
            severity: Some("CVSS:3.1/...".to_string()),
            modified: Some("2023-01-01T00:00:00Z".to_string()),
            published: None,
            affected: vec![NewAffected {
                ecosystem: "Maven".to_string(),
                package: "g:a".to_string(),
                ranges: Some("[]".to_string()),
                versions: None,
            }],
        }
    }

    #[tokio::test]
    async fn 落库公告与受影响坐标可计数() {
        let store = MetaStore::open_in_memory().await.unwrap();
        store.upsert_advisory(&样例公告("OSV-1")).await.unwrap();
        assert_eq!(store.count_advisories().await.unwrap(), 1);
        assert_eq!(store.count_advisory_affected("OSV-1").await.unwrap(), 1);
    }

    #[tokio::test]
    async fn 同_id_重复落库幂等不重复() {
        let store = MetaStore::open_in_memory().await.unwrap();
        store.upsert_advisory(&样例公告("OSV-1")).await.unwrap();
        // 第二次落库同 id：公告数与坐标数都不应翻倍
        let mut 改版 = 样例公告("OSV-1");
        改版.summary = Some("改后的摘要".to_string());
        store.upsert_advisory(&改版).await.unwrap();
        assert_eq!(store.count_advisories().await.unwrap(), 1);
        assert_eq!(store.count_advisory_affected("OSV-1").await.unwrap(), 1);
    }

    #[tokio::test]
    async fn 重复落库整体替换受影响坐标() {
        let store = MetaStore::open_in_memory().await.unwrap();
        // 首次两个坐标
        let mut adv = 样例公告("OSV-2");
        adv.affected.push(NewAffected {
            ecosystem: "npm".to_string(),
            package: "lodash".to_string(),
            ranges: None,
            versions: None,
        });
        store.upsert_advisory(&adv).await.unwrap();
        assert_eq!(store.count_advisory_affected("OSV-2").await.unwrap(), 2);

        // 再次落库仅一个坐标：旧坐标行应被整体替换为一条
        store.upsert_advisory(&样例公告("OSV-2")).await.unwrap();
        assert_eq!(store.count_advisory_affected("OSV-2").await.unwrap(), 1);
    }

    #[tokio::test]
    async fn 删除公告级联清理受影响坐标() {
        let store = MetaStore::open_in_memory().await.unwrap();
        store.upsert_advisory(&样例公告("OSV-3")).await.unwrap();
        // 外键 ON DELETE CASCADE：删公告应清掉其坐标行
        sqlx::query("DELETE FROM vuln_advisories WHERE id = ?")
            .bind("OSV-3")
            .execute(store.pool())
            .await
            .unwrap();
        assert_eq!(store.count_advisory_affected("OSV-3").await.unwrap(), 0);
    }

    #[tokio::test]
    async fn 刷新状态记录与查询() {
        let store = MetaStore::open_in_memory().await.unwrap();
        assert!(store
            .get_mirror_state("osv", "Maven")
            .await
            .unwrap()
            .is_none());

        store
            .record_mirror_refresh("osv", "Maven", 42)
            .await
            .unwrap();
        let state = store
            .get_mirror_state("osv", "Maven")
            .await
            .unwrap()
            .unwrap();
        assert_eq!(state.advisory_count, 42);
        assert!(state.last_refreshed.is_some());

        // 再次刷新覆盖条数
        store
            .record_mirror_refresh("osv", "Maven", 50)
            .await
            .unwrap();
        let state = store
            .get_mirror_state("osv", "Maven")
            .await
            .unwrap()
            .unwrap();
        assert_eq!(state.advisory_count, 50);
    }
}
