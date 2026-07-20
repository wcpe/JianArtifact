// Package persistence 负责 SQLite 连接、schema 迁移与事务基础设施。
//
// 分层（见 internal/doc.go）：位于依赖链底部，仅被 repository / auth 依赖，
// 不反向引用上层业务包。驱动使用 modernc.org/sqlite（纯 Go，无需 CGO），
// 经 jmoiron/sqlx 暴露带命名参数的便捷接口。
package persistence

import (
	"fmt"
	"net/url"

	"github.com/jmoiron/sqlx"

	// 注册纯 Go 的 sqlite 驱动（驱动名为 "sqlite"）。
	_ "modernc.org/sqlite"
)

// DB 是对 *sqlx.DB 的轻量包装，便于后续扩展（如按需注入统计钩子）。
type DB struct {
	*sqlx.DB
}

// Open 连接指定路径的 SQLite 文件并应用运行期 pragma：
// WAL 日志模式（并发读写）、外键约束、忙等待超时、NORMAL 同步级别。
// 连接建立后立即 Ping 以尽早暴露不可用（供 /readyz 复用）。
func Open(dbPath string) (*DB, error) {
	dsn := buildDSN(dbPath)
	inner, err := sqlx.Connect("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("连接 SQLite（%s）：%w", dbPath, err)
	}
	// modernc.org/sqlite 单连接串行化即可保证一致性；限制为单写连接避免
	// "database is locked"，读多时 WAL 已足够。
	inner.SetMaxOpenConns(1)
	return &DB{DB: inner}, nil
}

// buildDSN 组装 modernc.org/sqlite 的 DSN，pragma 以 _pragma 查询参数下发。
func buildDSN(dbPath string) string {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "foreign_keys(ON)")
	q.Add("_pragma", "synchronous(NORMAL)")
	return "file:" + dbPath + "?" + q.Encode()
}
