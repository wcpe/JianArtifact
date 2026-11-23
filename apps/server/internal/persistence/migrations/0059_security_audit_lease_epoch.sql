-- 采集写者租约改按单调代次裁决：接管只看代次，不依赖墓碑行的存续；
-- 租约表降级为只保留最近若干代的审计环，持久记录数量由此有界。
ALTER TABLE security_audit_checkpoint ADD COLUMN lease_epoch INTEGER NOT NULL DEFAULT 0;
ALTER TABLE security_audit_worker ADD COLUMN epoch INTEGER NOT NULL DEFAULT 0;
CREATE INDEX security_audit_worker_epoch ON security_audit_worker (epoch);
