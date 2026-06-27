//! 统一指标时序的元数据存取（FR-105，ADR-0027）。
//!
//! 与 `usage.rs` 同属元数据访问层，仅在 `MetaStore` 上扩展通用扁平时序表 `metric_samples`
//! 的读写与清理，以及供采样任务复用的轻量存储 / 仓库计数；SQLite 仍是元数据唯一真源，
//! 其他模块经此读写，不绕过直连 DB。
//!
//! 设计要点：
//! - **扁平时序**：一行 = 一个指标键在一个时刻的标量取值（`metric_key, ts, value`），不做
//!   高基数标签 / per-repo 多维（避免量级失控）。
//! - **批量落库**：一批样本落同一事务（沿用 usage 批量范式），整批要么可见、要么回滚。
//! - **保留期 + 行数兜底**：按保留天数删旧样本，并设行数硬上限兜底，防撑爆 SQLite。
//! - **本机内部、不外发**：时序为本机内部运行数据，落本地、默认不外发。

use super::{MetaError, MetaStore};

/// 单条时序样本（查询返回）。
#[derive(Debug, Clone, PartialEq, sqlx::FromRow)]
pub struct MetricSample {
    /// 指标键，如 `host.cpu_percent`。
    pub metric_key: String,
    /// 采样时刻（Unix 毫秒，UTC）。
    pub ts: i64,
    /// 标量取值。
    pub value: f64,
}

/// 单条时序样本写入入参。
#[derive(Debug, Clone)]
pub struct NewMetricSample {
    /// 指标键，如 `host.cpu_percent`。
    pub metric_key: String,
    /// 采样时刻（Unix 毫秒，UTC）。
    pub ts: i64,
    /// 标量取值。
    pub value: f64,
}

/// 一天的毫秒数（保留期 cutoff 计算用）。
const MILLIS_PER_DAY: i64 = 86_400_000;

impl MetaStore {
    /// 批量落库一组时序样本（同一事务）。空批直接返回 Ok，不开事务。
    ///
    /// 整批落在同一事务内，保证要么整批可见、要么回滚；单批失败由调用方按
    /// 「采样失败不影响业务」降级处理。
    pub async fn insert_metric_samples(
        &self,
        samples: &[NewMetricSample],
    ) -> Result<(), MetaError> {
        if samples.is_empty() {
            return Ok(());
        }
        let mut tx = self.pool().begin().await?;
        for s in samples {
            sqlx::query("INSERT INTO metric_samples (metric_key, ts, value) VALUES (?, ?, ?)")
                .bind(&s.metric_key)
                .bind(s.ts)
                .bind(s.value)
                .execute(&mut *tx)
                .await?;
        }
        tx.commit().await?;
        Ok(())
    }

    /// 按指标键 + 时间范围（闭区间）取原始样本，按 ts 升序。
    pub async fn query_metric_samples(
        &self,
        metric_key: &str,
        from: i64,
        to: i64,
    ) -> Result<Vec<MetricSample>, MetaError> {
        let rows = sqlx::query_as::<_, MetricSample>(
            "SELECT metric_key, ts, value FROM metric_samples \
             WHERE metric_key = ? AND ts >= ? AND ts <= ? ORDER BY ts ASC",
        )
        .bind(metric_key)
        .bind(from)
        .bind(to)
        .fetch_all(self.pool())
        .await?;
        Ok(rows)
    }

    /// 保留期清理：删除早于 `now_ms - retention_days * 一天` 的样本，返回删除行数。
    ///
    /// cutoff 用 i64 计算避免溢出（`retention_days as i64 * 一天毫秒`）。
    pub async fn prune_metric_samples_by_age(
        &self,
        retention_days: u32,
        now_ms: i64,
    ) -> Result<u64, MetaError> {
        let cutoff = now_ms - (retention_days as i64) * MILLIS_PER_DAY;
        let affected = sqlx::query("DELETE FROM metric_samples WHERE ts < ?")
            .bind(cutoff)
            .execute(self.pool())
            .await?
            .rows_affected();
        Ok(affected)
    }

    /// 行数兜底：超过 max_rows 时删 id 最小（最旧）的若干行，使总数回落到上限内。返回删除行数。
    pub async fn prune_metric_samples_by_max_rows(&self, max_rows: u64) -> Result<u64, MetaError> {
        let affected = sqlx::query(
            "DELETE FROM metric_samples WHERE id IN ( \
                SELECT id FROM metric_samples ORDER BY id ASC \
                LIMIT MAX(0, (SELECT COUNT(*) FROM metric_samples) - ?) \
             )",
        )
        .bind(max_rows as i64)
        .execute(self.pool())
        .await?
        .rows_affected();
        Ok(affected)
    }

    /// 仓库总数（供存储指标采样）。
    pub async fn count_repositories(&self) -> Result<i64, MetaError> {
        let count: i64 = sqlx::query_scalar("SELECT COUNT(*) FROM repositories")
            .fetch_one(self.pool())
            .await?;
        Ok(count)
    }

    /// 去重 blob 数（按 sha256 去重，供存储指标采样）。
    pub async fn count_distinct_blobs(&self) -> Result<i64, MetaError> {
        let count: i64 = sqlx::query_scalar("SELECT COUNT(DISTINCT sha256) FROM artifacts")
            .fetch_one(self.pool())
            .await?;
        Ok(count)
    }

    /// 存储总字节：按 sha256 去重后求和，避免同一 blob 被多个制品重复计字节。
    pub async fn total_blob_bytes(&self) -> Result<i64, MetaError> {
        let total: i64 = sqlx::query_scalar(
            "SELECT COALESCE(SUM(size), 0) FROM \
             (SELECT sha256, MAX(size) AS size FROM artifacts GROUP BY sha256)",
        )
        .fetch_one(self.pool())
        .await?;
        Ok(total)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::meta::{NewArtifact, NewRepository, RepoType, Visibility};

    /// 便捷：构造一条样本。
    fn 样本(key: &str, ts: i64, value: f64) -> NewMetricSample {
        NewMetricSample {
            metric_key: key.to_string(),
            ts,
            value,
        }
    }

    #[tokio::test]
    async fn 写入后按键与范围读回且按_ts_升序() {
        let store = MetaStore::open_in_memory().await.unwrap();
        // 乱序写入同键的三条样本，外加一条异键样本
        store
            .insert_metric_samples(&[
                样本("host.cpu_percent", 30, 3.0),
                样本("host.cpu_percent", 10, 1.0),
                样本("host.cpu_percent", 20, 2.0),
                样本("host.memory_percent", 15, 9.0),
            ])
            .await
            .unwrap();

        // 按键 + 范围 [10,30] 读回，应按 ts 升序、不含异键
        let rows = store
            .query_metric_samples("host.cpu_percent", 10, 30)
            .await
            .unwrap();
        let tss: Vec<i64> = rows.iter().map(|r| r.ts).collect();
        assert_eq!(tss, vec![10, 20, 30]);
        assert!(rows.iter().all(|r| r.metric_key == "host.cpu_percent"));

        // 范围外样本不返回：[15,25] 只命中 ts=20
        let mid = store
            .query_metric_samples("host.cpu_percent", 15, 25)
            .await
            .unwrap();
        assert_eq!(mid.len(), 1);
        assert_eq!(mid[0].ts, 20);
        assert_eq!(mid[0].value, 2.0);
    }

    #[tokio::test]
    async fn 空批写入不报错() {
        let store = MetaStore::open_in_memory().await.unwrap();
        store.insert_metric_samples(&[]).await.unwrap();
        let rows = store
            .query_metric_samples("host.cpu_percent", 0, i64::MAX)
            .await
            .unwrap();
        assert!(rows.is_empty());
    }

    #[tokio::test]
    async fn 保留期清理删旧留新() {
        let store = MetaStore::open_in_memory().await.unwrap();
        // now=10 天处；保留 1 天 → cutoff = now - 1 天。旧样本在 cutoff 之前、新样本在之后
        let now_ms = 10 * MILLIS_PER_DAY;
        let 旧 = now_ms - 2 * MILLIS_PER_DAY; // 早于 cutoff，应删
        let 新 = now_ms - 1; // 晚于 cutoff，应留
        store
            .insert_metric_samples(&[样本("k", 旧, 1.0), 样本("k", 新, 2.0)])
            .await
            .unwrap();

        let removed = store.prune_metric_samples_by_age(1, now_ms).await.unwrap();
        assert_eq!(removed, 1);

        let rows = store.query_metric_samples("k", 0, i64::MAX).await.unwrap();
        assert_eq!(rows.len(), 1);
        assert_eq!(rows[0].ts, 新);
    }

    #[tokio::test]
    async fn 行数兜底删最旧并回落上限() {
        let store = MetaStore::open_in_memory().await.unwrap();
        for ts in 1..=5 {
            store
                .insert_metric_samples(&[样本("k", ts, ts as f64)])
                .await
                .unwrap();
        }
        // 上限 3：删最旧 2 条（ts=1,2），留最新 3 条
        let removed = store.prune_metric_samples_by_max_rows(3).await.unwrap();
        assert_eq!(removed, 2);
        let rows = store.query_metric_samples("k", 0, i64::MAX).await.unwrap();
        let tss: Vec<i64> = rows.iter().map(|r| r.ts).collect();
        assert_eq!(tss, vec![3, 4, 5]);

        // 已在上限内：不再删
        let again = store.prune_metric_samples_by_max_rows(3).await.unwrap();
        assert_eq!(again, 0);
    }

    /// 便捷：建仓库返回 id。
    async fn 建仓库(store: &MetaStore, name: &str) -> String {
        store
            .create_repository(NewRepository {
                name,
                format: "raw",
                r#type: RepoType::Hosted,
                visibility: Visibility::Public,
                upstream_url: None,
                upstream_auth_ref: None,
            })
            .await
            .unwrap()
    }

    /// 便捷：构造制品写入入参（指定 sha256 与 size）。
    fn 制品<'a>(repo_id: &'a str, path: &'a str, sha256: &'a str, size: i64) -> NewArtifact<'a> {
        NewArtifact {
            repo_id,
            path,
            size,
            sha256,
            sha1: "s1",
            md5: "m",
            sha512: "s5",
            content_type: None,
            cached: false,
        }
    }

    #[tokio::test]
    async fn 存储与仓库计数_去重_sha256_只计一次() {
        let store = MetaStore::open_in_memory().await.unwrap();
        // 空库时计数均为 0
        assert_eq!(store.count_repositories().await.unwrap(), 0);
        assert_eq!(store.count_distinct_blobs().await.unwrap(), 0);
        assert_eq!(store.total_blob_bytes().await.unwrap(), 0);

        let r1 = 建仓库(&store, "r1").await;
        let r2 = 建仓库(&store, "r2").await;
        assert_eq!(store.count_repositories().await.unwrap(), 2);

        // 同 sha256 不同 path 两条（同一 blob 被两处引用），另有一条独立 blob
        store
            .upsert_artifact(制品(&r1, "a.txt", "共享sha", 100))
            .await
            .unwrap();
        store
            .upsert_artifact(制品(&r2, "b.txt", "共享sha", 100))
            .await
            .unwrap();
        store
            .upsert_artifact(制品(&r1, "c.txt", "独立sha", 30))
            .await
            .unwrap();

        // 去重 blob：共享sha + 独立sha = 2
        assert_eq!(store.count_distinct_blobs().await.unwrap(), 2);
        // 去重字节：共享sha 计一次 100 + 独立sha 30 = 130（共享不重复计）
        assert_eq!(store.total_blob_bytes().await.unwrap(), 130);
    }
}
