//! 使用分析（访问 / 下载统计）的元数据存取（FR-57，ADR-0009）。
//!
//! 与 `meta/mod.rs` 同属元数据访问层，仅在 `MetaStore` 上扩展 `usage_stats` / `usage_events`
//! 表读写；SQLite 仍是元数据唯一真源，其他模块经此读写，不绕过直连 DB。
//!
//! 设计要点：
//! - **聚合为主**：以 UPSERT 累加聚合计数（`usage_stats`），并发下计数准确、存储增长可控。
//! - **明细可选**：明细（`usage_events`）仅在配置开启时写入，行数兜底由后台裁剪，避免撑爆 SQLite。
//! - **异步落库**：写入由 `api` 的异步写入任务批量调用，主请求路径不直接写库。
//! - **隐私红线**：数据落本地、默认不外发；`actor` 只记用户名或 anonymous，绝不记凭据。
//! - **供 FR-58**：提供内部聚合查询入口（热门制品 / 仓库用量 / 单项计数）供后续数据面板使用，
//!   本批不做富数据面板 UI。

use super::{MetaError, MetaStore};

/// 使用事件动作种类。以小写字符串入库，避免魔法字符串散落。
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum UsageAction {
    /// 访问 / 浏览（如制品详情查看）。
    Access,
    /// 下载（如制品 GET 拉取成功）。
    Download,
}

impl UsageAction {
    /// 入库字符串。
    pub fn as_str(self) -> &'static str {
        match self {
            UsageAction::Access => "access",
            UsageAction::Download => "download",
        }
    }
}

/// 单条使用事件写入入参（不持有所有权之外的引用，便于在写入任务中跨 await 聚批）。
#[derive(Debug, Clone)]
pub struct NewUsageEvent {
    /// 目标仓库名。
    pub repo_name: String,
    /// 制品仓库内路径（仓库级聚合时为空串）。
    pub repo_path: String,
    /// 动作：access | download。
    pub action: String,
    /// 行为主体：用户名或 anonymous，绝不记凭据。
    pub actor: String,
    /// 来源 IP，可空。
    pub source_ip: Option<String>,
}

/// 聚合计数视图（查询返回），供 FR-58 数据面板使用。
#[derive(Debug, Clone, PartialEq, Eq, sqlx::FromRow)]
pub struct UsageStatRow {
    /// 目标仓库名。
    pub repo_name: String,
    /// 制品仓库内路径（仓库级聚合时为空串）。
    pub repo_path: String,
    /// 动作：access | download。
    pub action: String,
    /// 累计次数。
    pub count: i64,
    /// 最近一次发生时间（ISO8601 / UTC）。
    pub last_at: String,
}

impl MetaStore {
    /// 批量聚合落库使用事件：对每条事件做计数 UPSERT 累加；若开启明细则同批写入明细行。
    ///
    /// 一批的聚合累加与明细写入落在同一事务内，保证整批要么可见、要么回滚（避免半截可见）；
    /// 单批失败由调用方按"采集失败不影响业务"降级处理。`write_detail` 为 false 时不写明细。
    pub async fn insert_usage_batch(
        &self,
        events: &[NewUsageEvent],
        write_detail: bool,
    ) -> Result<(), MetaError> {
        if events.is_empty() {
            return Ok(());
        }
        let mut tx = self.pool().begin().await?;
        for e in events {
            // 聚合计数 UPSERT：首次插入计数 1，冲突则在原值上 +1 并刷新 last_at。
            // 累加在 SQL 内完成，多并发批次串行落库时计数不丢失、不重复。
            sqlx::query(
                "INSERT INTO usage_stats (repo_name, repo_path, action, count, last_at) \
                 VALUES (?, ?, ?, 1, CURRENT_TIMESTAMP) \
                 ON CONFLICT(repo_name, repo_path, action) \
                 DO UPDATE SET count = count + 1, last_at = CURRENT_TIMESTAMP",
            )
            .bind(&e.repo_name)
            .bind(&e.repo_path)
            .bind(&e.action)
            .execute(&mut *tx)
            .await?;

            if write_detail {
                sqlx::query(
                    "INSERT INTO usage_events (repo_name, repo_path, action, actor, source_ip) \
                     VALUES (?, ?, ?, ?, ?)",
                )
                .bind(&e.repo_name)
                .bind(&e.repo_path)
                .bind(&e.action)
                .bind(&e.actor)
                .bind(&e.source_ip)
                .execute(&mut *tx)
                .await?;
            }
        }
        tx.commit().await?;
        Ok(())
    }

    /// 查询单个（仓库 + 路径 + 动作）的累计计数；无记录视为 0。供内部聚合 / 测试使用。
    pub async fn usage_count(
        &self,
        repo_name: &str,
        repo_path: &str,
        action: UsageAction,
    ) -> Result<i64, MetaError> {
        let count: Option<i64> = sqlx::query_scalar(
            "SELECT count FROM usage_stats \
             WHERE repo_name = ? AND repo_path = ? AND action = ?",
        )
        .bind(repo_name)
        .bind(repo_path)
        .bind(action.as_str())
        .fetch_optional(self.pool())
        .await?;
        Ok(count.unwrap_or(0))
    }

    /// 列出某动作下计数最高的前 N 条聚合（热门制品 / 仓库用量），按计数倒序。
    ///
    /// 供 FR-58 数据面板的内部聚合查询入口；本批仅提供查询，不做面板 UI。
    pub async fn top_usage_by_action(
        &self,
        action: UsageAction,
        limit: i64,
    ) -> Result<Vec<UsageStatRow>, MetaError> {
        let rows = sqlx::query_as::<_, UsageStatRow>(
            "SELECT repo_name, repo_path, action, count, last_at FROM usage_stats \
             WHERE action = ? ORDER BY count DESC, repo_name ASC, repo_path ASC LIMIT ?",
        )
        .bind(action.as_str())
        .bind(limit)
        .fetch_all(self.pool())
        .await?;
        Ok(rows)
    }

    /// 使用明细总行数（不带过滤），供明细量级兜底裁剪判断。
    pub async fn count_usage_events(&self) -> Result<i64, MetaError> {
        let count: i64 = sqlx::query_scalar("SELECT COUNT(*) FROM usage_events")
            .fetch_one(self.pool())
            .await?;
        Ok(count)
    }

    /// 明细量级兜底：超过 max_rows 时删最旧的若干行，使总数回落到上限内。返回删除行数。
    ///
    /// 仅裁剪明细（`usage_events`），聚合计数（`usage_stats`）是长期统计真源、不随之删除。
    pub async fn prune_usage_events_by_max_rows(&self, max_rows: u64) -> Result<u64, MetaError> {
        // 删除 id 最小（最旧）的 N 条，N = 当前总数 - 上限；上限内则不删
        let affected = sqlx::query(
            "DELETE FROM usage_events WHERE id IN ( \
                SELECT id FROM usage_events ORDER BY id ASC \
                LIMIT MAX(0, (SELECT COUNT(*) FROM usage_events) - ?) \
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

    /// 便捷：构造一条最小使用事件。
    fn 事件(repo: &str, path: &str, action: UsageAction) -> NewUsageEvent {
        NewUsageEvent {
            repo_name: repo.to_string(),
            repo_path: path.to_string(),
            action: action.as_str().to_string(),
            actor: "anonymous".to_string(),
            source_ip: Some("127.0.0.1".to_string()),
        }
    }

    #[tokio::test]
    async fn 聚合_upsert_累加计数() {
        let store = MetaStore::open_in_memory().await.unwrap();
        // 同一（仓库 + 路径 + 动作）多次落库应累加为一行计数，而非多行
        store
            .insert_usage_batch(
                &[
                    事件("libs", "a/b.jar", UsageAction::Download),
                    事件("libs", "a/b.jar", UsageAction::Download),
                ],
                false,
            )
            .await
            .unwrap();
        store
            .insert_usage_batch(&[事件("libs", "a/b.jar", UsageAction::Download)], false)
            .await
            .unwrap();

        assert_eq!(
            store
                .usage_count("libs", "a/b.jar", UsageAction::Download)
                .await
                .unwrap(),
            3
        );
        // 不同动作独立计数
        assert_eq!(
            store
                .usage_count("libs", "a/b.jar", UsageAction::Access)
                .await
                .unwrap(),
            0
        );
    }

    #[tokio::test]
    async fn 空批写入不报错且不计数() {
        let store = MetaStore::open_in_memory().await.unwrap();
        store.insert_usage_batch(&[], false).await.unwrap();
        store.insert_usage_batch(&[], true).await.unwrap();
        assert_eq!(store.count_usage_events().await.unwrap(), 0);
    }

    #[tokio::test]
    async fn 明细开关控制是否写明细() {
        let store = MetaStore::open_in_memory().await.unwrap();
        // 关明细：只累加聚合、不落明细
        store
            .insert_usage_batch(&[事件("r", "p", UsageAction::Access)], false)
            .await
            .unwrap();
        assert_eq!(store.count_usage_events().await.unwrap(), 0);
        assert_eq!(
            store
                .usage_count("r", "p", UsageAction::Access)
                .await
                .unwrap(),
            1
        );

        // 开明细：聚合与明细同时记
        store
            .insert_usage_batch(&[事件("r", "p", UsageAction::Access)], true)
            .await
            .unwrap();
        assert_eq!(store.count_usage_events().await.unwrap(), 1);
        assert_eq!(
            store
                .usage_count("r", "p", UsageAction::Access)
                .await
                .unwrap(),
            2
        );
    }

    #[tokio::test]
    async fn 热门聚合按计数倒序() {
        let store = MetaStore::open_in_memory().await.unwrap();
        // c 下载 3 次、a 下载 1 次、b 下载 2 次
        for _ in 0..3 {
            store
                .insert_usage_batch(&[事件("repo", "c", UsageAction::Download)], false)
                .await
                .unwrap();
        }
        store
            .insert_usage_batch(&[事件("repo", "a", UsageAction::Download)], false)
            .await
            .unwrap();
        for _ in 0..2 {
            store
                .insert_usage_batch(&[事件("repo", "b", UsageAction::Download)], false)
                .await
                .unwrap();
        }
        // 另有一条 access，不应混入 download 的 top 查询
        store
            .insert_usage_batch(&[事件("repo", "c", UsageAction::Access)], false)
            .await
            .unwrap();

        let top = store
            .top_usage_by_action(UsageAction::Download, 10)
            .await
            .unwrap();
        assert_eq!(top.len(), 3);
        assert_eq!(top[0].repo_path, "c");
        assert_eq!(top[0].count, 3);
        assert_eq!(top[1].repo_path, "b");
        assert_eq!(top[2].repo_path, "a");
        // limit 截断
        let top1 = store
            .top_usage_by_action(UsageAction::Download, 1)
            .await
            .unwrap();
        assert_eq!(top1.len(), 1);
        assert_eq!(top1[0].repo_path, "c");
    }

    #[tokio::test]
    async fn 明细行数兜底删最旧() {
        let store = MetaStore::open_in_memory().await.unwrap();
        for _ in 0..5 {
            store
                .insert_usage_batch(&[事件("r", "p", UsageAction::Download)], true)
                .await
                .unwrap();
        }
        assert_eq!(store.count_usage_events().await.unwrap(), 5);

        // 上限 3：删最旧 2 条，留最新 3 条；聚合计数不受影响仍为 5
        let removed = store.prune_usage_events_by_max_rows(3).await.unwrap();
        assert_eq!(removed, 2);
        assert_eq!(store.count_usage_events().await.unwrap(), 3);
        assert_eq!(
            store
                .usage_count("r", "p", UsageAction::Download)
                .await
                .unwrap(),
            5
        );

        // 已在上限内：不再删
        let again = store.prune_usage_events_by_max_rows(3).await.unwrap();
        assert_eq!(again, 0);
    }
}
