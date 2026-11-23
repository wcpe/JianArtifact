package repository

import (
	"context"
	"database/sql/driver"
	"errors"
	"strconv"
	"time"

	"github.com/jmoiron/sqlx"
	"modernc.org/sqlite"
)

const securityAuditResetTimeout = 250 * time.Millisecond

// writeLowPriority 仅借用一个连接；抢不到写锁立即让路，不改变业务连接的忙等待策略。
func (r *SecurityCoverageRepo) writeLowPriority(ctx context.Context, f func(*sqlx.Tx) error) error {
	conn, err := r.db.Connx(ctx)
	if err != nil {
		return securityAuditSQLiteError(err)
	}
	defer func() { _ = conn.Close() }()
	var original int
	if err := conn.GetContext(ctx, &original, `PRAGMA busy_timeout`); err != nil {
		return securityAuditSQLiteError(err)
	}
	defer restoreSecurityAuditBusyTimeout(conn, original)
	if _, err := conn.ExecContext(ctx, `PRAGMA busy_timeout=0`); err != nil {
		return securityAuditSQLiteError(err)
	}
	tx, err := conn.BeginTxx(ctx, nil)
	if err != nil {
		return securityAuditSQLiteError(err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := f(tx); err != nil {
		return err
	}
	return securityAuditSQLiteError(tx.Commit())
}

func restoreSecurityAuditBusyTimeout(conn *sqlx.Conn, original int) {
	ctx, cancel := context.WithTimeout(context.Background(), securityAuditResetTimeout)
	defer cancel()
	if _, err := conn.ExecContext(ctx, `PRAGMA busy_timeout=`+strconv.Itoa(original)); err != nil {
		// 无法恢复即丢弃该连接，禁止带着采集策略归还业务池。
		_ = conn.Raw(func(any) error { return driver.ErrBadConn })
	}
}

func securityAuditSQLiteError(err error) error {
	if err == nil {
		return nil
	}
	var sqliteErr *sqlite.Error
	if errors.As(err, &sqliteErr) {
		const sqliteBusy, sqliteLocked = 5, 6
		if base := sqliteErr.Code() & 0xff; base == sqliteBusy || base == sqliteLocked {
			return ErrSecurityCoverageBusy
		}
	}
	return ErrSecurityEventUnavailable
}
