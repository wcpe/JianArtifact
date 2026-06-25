//! 防护告警的元数据存取（FR-56，ADR-0017）。
//!
//! 与 `meta/mod.rs` 同属元数据访问层，仅在 `MetaStore` 上扩展 `protection_alerts` 表读写；
//! SQLite 仍是元数据唯一真源，其他模块经此读写，不绕过直连 DB。
//!
//! 设计要点（沿用审计 / 使用分析同款治理范式）：
//! - 写入由 `api` 的异步写入任务批量调用 `insert_alert_batch`，主请求路径不直接写库。
//! - 行数兜底裁剪由后台任务调用 `prune_alerts_by_max_rows`，防止明细撑爆 SQLite。
//! - 告警是本机内部数据：只落本地、不外发；本表不存任何凭据 / 密钥，detail 仅记结构化中文上下文。

use super::{MetaError, MetaStore};

/// 单条告警写入入参（不持有所有权之外的引用，便于在写入任务中跨 await 聚批）。
///
/// 字段对齐 `protection_alerts` 表；`id` 与 `ts` 由 DB 自增 / 默认填充，不在入参中给出。
#[derive(Debug, Clone)]
pub struct NewAlert {
    /// 防护维度（rate_limit / ban / cc_challenge / waf / slowloris）。
    pub dimension: String,
    /// 严重度（warn | error）。
    pub severity: String,
    /// 触发告警时的窗内观测计数。
    pub observed_value: i64,
    /// 触发告警的阈值。
    pub threshold: i64,
    /// 评估时间窗时长（秒）。
    pub window_secs: i64,
    /// 结构化补充（中文文案），禁含凭据 / 隐私，可空。
    pub detail: Option<String>,
}

/// 告警记录（查询返回视图），字段对齐 `protection_alerts` 表。
#[derive(Debug, Clone, sqlx::FromRow)]
pub struct AlertRecord {
    /// 自增主键。
    pub id: i64,
    /// 告警时间（ISO8601 / UTC）。
    pub ts: String,
    /// 防护维度。
    pub dimension: String,
    /// 严重度。
    pub severity: String,
    /// 触发告警时的窗内观测计数。
    pub observed_value: i64,
    /// 触发告警的阈值。
    pub threshold: i64,
    /// 评估时间窗时长（秒）。
    pub window_secs: i64,
    /// 结构化补充。
    pub detail: Option<String>,
}

/// 告警查询过滤条件（均可选；分页用 offset/limit）。
#[derive(Debug, Clone, Default)]
pub struct AlertQuery<'a> {
    /// 按维度精确过滤。
    pub dimension: Option<&'a str>,
    /// 分页起点。
    pub offset: i64,
    /// 分页容量。
    pub limit: i64,
}

impl MetaStore {
    /// 批量写入防护告警。空批直接返回，单条失败由调用方按"采集失败不影响业务"降级处理。
    ///
    /// 逐条 INSERT 落在同一事务内，保证一批要么整批可见、要么整批回滚，避免半截可见。
    pub async fn insert_alert_batch(&self, alerts: &[NewAlert]) -> Result<(), MetaError> {
        if alerts.is_empty() {
            return Ok(());
        }
        let mut tx = self.pool().begin().await?;
        for a in alerts {
            sqlx::query(
                "INSERT INTO protection_alerts \
                    (dimension, severity, observed_value, threshold, window_secs, detail) \
                 VALUES (?, ?, ?, ?, ?, ?)",
            )
            .bind(&a.dimension)
            .bind(&a.severity)
            .bind(a.observed_value)
            .bind(a.threshold)
            .bind(a.window_secs)
            .bind(&a.detail)
            .execute(&mut *tx)
            .await?;
        }
        tx.commit().await?;
        Ok(())
    }

    /// 按过滤条件分页查询告警，按时间倒序（最新在前）。
    pub async fn query_alerts(
        &self,
        query: &AlertQuery<'_>,
    ) -> Result<Vec<AlertRecord>, MetaError> {
        // 维度过滤用 `? IS NULL OR col = ?` 模式，绑定值为 None 时该项不生效，避免拼接多分支 SQL
        let records = sqlx::query_as::<_, AlertRecord>(
            "SELECT id, ts, dimension, severity, observed_value, threshold, window_secs, detail \
             FROM protection_alerts \
             WHERE (? IS NULL OR dimension = ?) \
             ORDER BY id DESC \
             LIMIT ? OFFSET ?",
        )
        .bind(query.dimension)
        .bind(query.dimension)
        .bind(query.limit)
        .bind(query.offset)
        .fetch_all(self.pool())
        .await?;
        Ok(records)
    }

    /// 统计满足过滤条件的告警总数（供分页 total，与 query_alerts 同条件）。
    pub async fn count_alerts(&self, query: &AlertQuery<'_>) -> Result<i64, MetaError> {
        let count: i64 = sqlx::query_scalar(
            "SELECT COUNT(*) FROM protection_alerts WHERE (? IS NULL OR dimension = ?)",
        )
        .bind(query.dimension)
        .bind(query.dimension)
        .fetch_one(self.pool())
        .await?;
        Ok(count)
    }

    /// 告警总行数（不带过滤），供行数兜底裁剪判断。
    pub async fn count_alerts_total(&self) -> Result<i64, MetaError> {
        let count: i64 = sqlx::query_scalar("SELECT COUNT(*) FROM protection_alerts")
            .fetch_one(self.pool())
            .await?;
        Ok(count)
    }

    /// 行数兜底：超过 max_rows 时删最旧的若干行，使总数回落到上限内。返回删除行数。
    pub async fn prune_alerts_by_max_rows(&self, max_rows: u64) -> Result<u64, MetaError> {
        // 删除 id 最小（最旧）的 N 条，N = 当前总数 - 上限；上限内则不删
        let affected = sqlx::query(
            "DELETE FROM protection_alerts WHERE id IN ( \
                SELECT id FROM protection_alerts ORDER BY id ASC \
                LIMIT MAX(0, (SELECT COUNT(*) FROM protection_alerts) - ?) \
             )",
        )
        .bind(max_rows as i64)
        .execute(self.pool())
        .await?
        .rows_affected();
        Ok(affected)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// 便捷：构造一条最小告警入参。
    fn 入参(dimension: &str, observed: i64, threshold: i64) -> NewAlert {
        NewAlert {
            dimension: dimension.to_string(),
            severity: "warn".to_string(),
            observed_value: observed,
            threshold,
            window_secs: 300,
            detail: Some("窗内计数达阈值".to_string()),
        }
    }

    #[tokio::test]
    async fn 批量写入后可查出并按时间倒序() {
        let store = MetaStore::open_in_memory().await.unwrap();
        store
            .insert_alert_batch(&[入参("rate_limit", 1000, 1000), 入参("waf", 600, 500)])
            .await
            .unwrap();

        let all = store
            .query_alerts(&AlertQuery {
                limit: 50,
                ..Default::default()
            })
            .await
            .unwrap();
        assert_eq!(all.len(), 2);
        // id DESC：后写入的 waf 在前
        assert_eq!(all[0].dimension, "waf");
        assert_eq!(all[1].dimension, "rate_limit");
        assert_eq!(store.count_alerts(&AlertQuery::default()).await.unwrap(), 2);
    }

    #[tokio::test]
    async fn 空批写入不报错且不增行() {
        let store = MetaStore::open_in_memory().await.unwrap();
        store.insert_alert_batch(&[]).await.unwrap();
        assert_eq!(store.count_alerts_total().await.unwrap(), 0);
    }

    #[tokio::test]
    async fn 按维度过滤分页() {
        let store = MetaStore::open_in_memory().await.unwrap();
        store
            .insert_alert_batch(&[
                入参("rate_limit", 1000, 1000),
                入参("waf", 600, 500),
                入参("waf", 700, 500),
            ])
            .await
            .unwrap();

        let waf = store
            .query_alerts(&AlertQuery {
                dimension: Some("waf"),
                limit: 50,
                ..Default::default()
            })
            .await
            .unwrap();
        assert_eq!(waf.len(), 2);
        assert!(waf.iter().all(|a| a.dimension == "waf"));
        assert_eq!(
            store
                .count_alerts(&AlertQuery {
                    dimension: Some("waf"),
                    ..Default::default()
                })
                .await
                .unwrap(),
            2
        );

        // 分页 limit=1 只返回一条
        let page = store
            .query_alerts(&AlertQuery {
                limit: 1,
                ..Default::default()
            })
            .await
            .unwrap();
        assert_eq!(page.len(), 1);
    }

    #[tokio::test]
    async fn 行数兜底删最旧() {
        let store = MetaStore::open_in_memory().await.unwrap();
        for _ in 0..5 {
            store
                .insert_alert_batch(&[入参("ban", 50, 50)])
                .await
                .unwrap();
        }
        assert_eq!(store.count_alerts_total().await.unwrap(), 5);

        // 上限 3：应删最旧 2 条，留最新 3 条
        let removed = store.prune_alerts_by_max_rows(3).await.unwrap();
        assert_eq!(removed, 2);
        assert_eq!(store.count_alerts_total().await.unwrap(), 3);

        // 已在上限内：不再删
        let again = store.prune_alerts_by_max_rows(3).await.unwrap();
        assert_eq!(again, 0);
    }
}
