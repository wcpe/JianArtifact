//! 审计日志的元数据存取（FR-31，ADR-0015）。
//!
//! 与 `meta/mod.rs` 同属元数据访问层，仅在 `MetaStore` 上扩展 `audit_log` 表读写；
//! SQLite 仍是元数据唯一真源，其他模块经此读写，不绕过直连 DB。
//!
//! 设计要点：
//! - 写入由 `api` 的异步写入任务批量调用 `insert_audit_batch`，主请求路径不直接写库。
//! - 保留期轮转（按时间 + 行数兜底）由后台任务调用 `prune_audit_by_age` / `prune_audit_by_max_rows`。
//! - 凭据 / 密钥绝不入此表：脱敏在入库前（`api` 层）完成，本层只负责落库与检索。

use super::{MetaError, MetaStore};

/// 单条审计写入入参（不持有所有权之外的引用，便于跨 await 在写入任务中聚批）。
///
/// 字段对齐 `audit_log` 表；`id` 与 `ts` 由 DB 自增 / 默认填充，不在入参中给出。
#[derive(Debug, Clone)]
pub struct NewAuditEntry {
    /// 行为主体：用户名或 `anonymous`，绝不记凭据。
    pub actor: String,
    /// 主体身份种类：session | token | basic | anonymous。
    pub actor_kind: String,
    /// 关联请求 ID（x-request-id），可空。
    pub request_id: Option<String>,
    /// 来源 IP，可空。
    pub source_ip: Option<String>,
    /// 事件动作枚举（如 `login`、`repo.create`）。
    pub action: String,
    /// 受影响仓库名，可空。
    pub target_repo: Option<String>,
    /// 受影响对象坐标 / 路径，可空。
    pub target: Option<String>,
    /// 结果：success | denied | error。
    pub result: String,
    /// 结构化补充（JSON 文本），禁含凭据 / 隐私，可空。
    pub detail: Option<String>,
}

/// 审计日志记录（查询返回视图），字段对齐 `audit_log` 表。
#[derive(Debug, Clone, sqlx::FromRow)]
pub struct AuditEntry {
    /// 自增主键。
    pub id: i64,
    /// 事件时间（ISO8601 / UTC）。
    pub ts: String,
    /// 行为主体（用户名或 anonymous）。
    pub actor: String,
    /// 主体身份种类。
    pub actor_kind: String,
    /// 关联请求 ID。
    pub request_id: Option<String>,
    /// 来源 IP。
    pub source_ip: Option<String>,
    /// 事件动作。
    pub action: String,
    /// 受影响仓库名。
    pub target_repo: Option<String>,
    /// 受影响对象坐标 / 路径。
    pub target: Option<String>,
    /// 结果。
    pub result: String,
    /// 结构化补充。
    pub detail: Option<String>,
}

/// 审计查询过滤条件（均可选；分页用 offset/limit）。
#[derive(Debug, Clone, Default)]
pub struct AuditQuery<'a> {
    /// 按动作精确过滤。
    pub action: Option<&'a str>,
    /// 按仓库名精确过滤。
    pub target_repo: Option<&'a str>,
    /// 按主体（用户名）精确过滤。
    pub actor: Option<&'a str>,
    /// 分页起点。
    pub offset: i64,
    /// 分页容量。
    pub limit: i64,
}

impl MetaStore {
    /// 批量写入审计日志。空批直接返回，单条失败由调用方按"采集失败不影响业务"降级处理。
    ///
    /// 逐条 INSERT 落在同一事务内，保证一批要么整批可见、要么整批回滚，避免半截可见。
    pub async fn insert_audit_batch(&self, entries: &[NewAuditEntry]) -> Result<(), MetaError> {
        if entries.is_empty() {
            return Ok(());
        }
        let mut tx = self.pool().begin().await?;
        for e in entries {
            sqlx::query(
                "INSERT INTO audit_log \
                    (actor, actor_kind, request_id, source_ip, action, target_repo, target, result, detail) \
                 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
            )
            .bind(&e.actor)
            .bind(&e.actor_kind)
            .bind(&e.request_id)
            .bind(&e.source_ip)
            .bind(&e.action)
            .bind(&e.target_repo)
            .bind(&e.target)
            .bind(&e.result)
            .bind(&e.detail)
            .execute(&mut *tx)
            .await?;
        }
        tx.commit().await?;
        Ok(())
    }

    /// 按过滤条件分页查询审计日志，按时间倒序（最新在前）。
    pub async fn query_audit(&self, query: &AuditQuery<'_>) -> Result<Vec<AuditEntry>, MetaError> {
        // 各过滤项用 `? IS NULL OR col = ?` 模式，绑定值为 None 时该项不生效，避免拼接多分支 SQL
        let records = sqlx::query_as::<_, AuditEntry>(
            "SELECT id, ts, actor, actor_kind, request_id, source_ip, action, \
                    target_repo, target, result, detail \
             FROM audit_log \
             WHERE (? IS NULL OR action = ?) \
               AND (? IS NULL OR target_repo = ?) \
               AND (? IS NULL OR actor = ?) \
             ORDER BY id DESC \
             LIMIT ? OFFSET ?",
        )
        .bind(query.action)
        .bind(query.action)
        .bind(query.target_repo)
        .bind(query.target_repo)
        .bind(query.actor)
        .bind(query.actor)
        .bind(query.limit)
        .bind(query.offset)
        .fetch_all(self.pool())
        .await?;
        Ok(records)
    }

    /// 统计满足过滤条件的审计日志总数（供分页 total，与 query_audit 同条件）。
    pub async fn count_audit(&self, query: &AuditQuery<'_>) -> Result<i64, MetaError> {
        let count: i64 = sqlx::query_scalar(
            "SELECT COUNT(*) FROM audit_log \
             WHERE (? IS NULL OR action = ?) \
               AND (? IS NULL OR target_repo = ?) \
               AND (? IS NULL OR actor = ?)",
        )
        .bind(query.action)
        .bind(query.action)
        .bind(query.target_repo)
        .bind(query.target_repo)
        .bind(query.actor)
        .bind(query.actor)
        .fetch_one(self.pool())
        .await?;
        Ok(count)
    }

    /// 审计日志总行数（不带过滤），供行数兜底轮转判断。
    pub async fn count_audit_total(&self) -> Result<i64, MetaError> {
        let count: i64 = sqlx::query_scalar("SELECT COUNT(*) FROM audit_log")
            .fetch_one(self.pool())
            .await?;
        Ok(count)
    }

    /// 按保留天数删除过期审计行（ts 早于 now - retention_days 天）。返回删除行数。
    ///
    /// 用 SQLite 的 `datetime('now', '-N days')` 比较，避免引入额外时间库。
    pub async fn prune_audit_by_age(&self, retention_days: u32) -> Result<u64, MetaError> {
        // 拼接到修饰符字符串里的是受控的整数（u32），无注入风险
        let modifier = format!("-{retention_days} days");
        let affected = sqlx::query("DELETE FROM audit_log WHERE ts < datetime('now', ?)")
            .bind(modifier)
            .execute(self.pool())
            .await?
            .rows_affected();
        Ok(affected)
    }

    /// 行数兜底：超过 max_rows 时删最旧的若干行，使总数回落到上限内。返回删除行数。
    pub async fn prune_audit_by_max_rows(&self, max_rows: u64) -> Result<u64, MetaError> {
        // 删除 id 最小（最旧）的 N 条，N = 当前总数 - 上限；上限内则不删
        let affected = sqlx::query(
            "DELETE FROM audit_log WHERE id IN ( \
                SELECT id FROM audit_log ORDER BY id ASC \
                LIMIT MAX(0, (SELECT COUNT(*) FROM audit_log) - ?) \
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

    /// 便捷：构造一条最小审计入参。
    fn 入参(action: &str, result: &str) -> NewAuditEntry {
        NewAuditEntry {
            actor: "alice".to_string(),
            actor_kind: "session".to_string(),
            request_id: Some("req-1".to_string()),
            source_ip: Some("127.0.0.1".to_string()),
            action: action.to_string(),
            target_repo: Some("libs".to_string()),
            target: None,
            result: result.to_string(),
            detail: None,
        }
    }

    #[tokio::test]
    async fn 批量写入后可查出并按时间倒序() {
        let store = MetaStore::open_in_memory().await.unwrap();
        store
            .insert_audit_batch(&[
                入参("repo.create", "success"),
                入参("repo.delete", "success"),
            ])
            .await
            .unwrap();

        let all = store
            .query_audit(&AuditQuery {
                limit: 50,
                ..Default::default()
            })
            .await
            .unwrap();
        assert_eq!(all.len(), 2);
        // id DESC：后写入的 repo.delete 在前
        assert_eq!(all[0].action, "repo.delete");
        assert_eq!(all[1].action, "repo.create");
        assert_eq!(store.count_audit(&AuditQuery::default()).await.unwrap(), 2);
    }

    #[tokio::test]
    async fn 空批写入不报错且不增行() {
        let store = MetaStore::open_in_memory().await.unwrap();
        store.insert_audit_batch(&[]).await.unwrap();
        assert_eq!(store.count_audit_total().await.unwrap(), 0);
    }

    #[tokio::test]
    async fn 按动作与仓库过滤分页() {
        let store = MetaStore::open_in_memory().await.unwrap();
        store
            .insert_audit_batch(&[
                入参("repo.create", "success"),
                入参("artifact.upload", "success"),
                入参("artifact.upload", "denied"),
            ])
            .await
            .unwrap();

        let uploads = store
            .query_audit(&AuditQuery {
                action: Some("artifact.upload"),
                limit: 50,
                ..Default::default()
            })
            .await
            .unwrap();
        assert_eq!(uploads.len(), 2);
        assert!(uploads.iter().all(|e| e.action == "artifact.upload"));
        assert_eq!(
            store
                .count_audit(&AuditQuery {
                    action: Some("artifact.upload"),
                    ..Default::default()
                })
                .await
                .unwrap(),
            2
        );

        // 仓库过滤命中全部（均为 libs），换不存在仓库则为空
        let none = store
            .query_audit(&AuditQuery {
                target_repo: Some("不存在"),
                limit: 50,
                ..Default::default()
            })
            .await
            .unwrap();
        assert!(none.is_empty());

        // 分页 limit=1 只返回一条
        let page = store
            .query_audit(&AuditQuery {
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
                .insert_audit_batch(&[入参("login", "success")])
                .await
                .unwrap();
        }
        assert_eq!(store.count_audit_total().await.unwrap(), 5);

        // 上限 3：应删最旧 2 条，留最新 3 条
        let removed = store.prune_audit_by_max_rows(3).await.unwrap();
        assert_eq!(removed, 2);
        assert_eq!(store.count_audit_total().await.unwrap(), 3);

        // 已在上限内：不再删
        let again = store.prune_audit_by_max_rows(3).await.unwrap();
        assert_eq!(again, 0);
    }

    #[tokio::test]
    async fn 保留期轮转只删过期行() {
        let store = MetaStore::open_in_memory().await.unwrap();
        // 一条新鲜行（默认 ts = now）
        store
            .insert_audit_batch(&[入参("login", "success")])
            .await
            .unwrap();
        // 一条 100 天前的旧行：直接以显式 ts 入库以构造确定的过期场景
        sqlx::query(
            "INSERT INTO audit_log (ts, actor, actor_kind, action, result) \
             VALUES (datetime('now', '-100 days'), 'bob', 'session', 'login', 'success')",
        )
        .execute(store.pool())
        .await
        .unwrap();
        assert_eq!(store.count_audit_total().await.unwrap(), 2);

        // 保留 90 天：删掉 100 天前那条，留下新鲜行
        let removed = store.prune_audit_by_age(90).await.unwrap();
        assert_eq!(removed, 1);
        let remaining = store
            .query_audit(&AuditQuery {
                limit: 50,
                ..Default::default()
            })
            .await
            .unwrap();
        assert_eq!(remaining.len(), 1);
        assert_eq!(remaining[0].actor, "alice");
    }
}
