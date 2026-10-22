package persistence

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"sync/atomic"

	"github.com/jmoiron/sqlx"
	"modernc.org/sqlite"
)

// 本文件提供**仅供测试**使用的语句计数连接。
//
// 为什么需要它：ACL 批量化与 npm 搜索收口都是「把 O(N) 条查询压成常数条」的性能
// 改动，而这类改动的正确性证据只能来自「查询次数不随规模增长」——结果对得上并不能证明没退化。
// 语料规模与耗时都不足以作为断言依据（快慢受机器负载影响），因此直接在驱动层数语句。
//
// 实现上包在 driver.Connector 上，统计 Prepare/PrepareContext 与 connexec/query 四类入口：
// database/sql 只要能从底层连接直接执行（modernc 实现了 ExecerContext / QueryerContext），
// 就不会退化成 Prepare；两条路径都计入，保证计数与「实际发给数据库的语句数」一致。
//
// 生产代码一律用 Open：本函数额外包一层统计连接，会改变驱动的语句复用行为，不能用于生产。

// countingConnector 包装真实连接器并在建立连接时套上计数壳。
type countingConnector struct {
	inner driver.Connector
	count *int64
}

// Connect 建立连接并返回计数壳。
func (c countingConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.inner.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return countingConn{Conn: conn, count: c.count}, nil
}

// Driver 返回底层驱动（database/sql 用于选择默认参数转换策略）。
func (c countingConnector) Driver() driver.Driver { return c.inner.Driver() }

// countingConn 在每次语句执行前自增计数。
type countingConn struct {
	driver.Conn
	count *int64
}

// Prepare 统计「预处理语句」入口。
func (c countingConn) Prepare(query string) (driver.Stmt, error) {
	atomic.AddInt64(c.count, 1)
	return c.Conn.Prepare(query)
}

// PrepareContext 统计「带 ctx 的预处理语句」入口；底层不支持 ctx 时退回 Prepare（已计过一次，
// 故此处不再重复计数）。
func (c countingConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	if p, ok := c.Conn.(driver.ConnPrepareContext); ok {
		atomic.AddInt64(c.count, 1)
		return p.PrepareContext(ctx, query)
	}
	return c.Conn.Prepare(query)
}

// ExecContext 统计「直接执行」入口；底层不支持时返回 ErrSkip，由 database/sql 走 Prepare 路径
// （那条路径由 Prepare/PrepareContext 计数，不会遗漏也不会重复）。
func (c countingConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	e, ok := c.Conn.(driver.ExecerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	atomic.AddInt64(c.count, 1)
	return e.ExecContext(ctx, query, args)
}

// QueryContext 统计「直接查询」入口；语义同 ExecContext。
func (c countingConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	q, ok := c.Conn.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	atomic.AddInt64(c.count, 1)
	return q.QueryContext(ctx, query, args)
}

// BeginTx 必须显式转发，不能只靠内嵌 driver.Conn 的默认实现。
//
// 原因：database/sql 只有在底层实现了 driver.ConnBeginTx 时才把 TxOptions 交给驱动；
// 否则一旦调用方传入非默认选项（例如搜索用的只读事务），它会直接返回
// "driver does not support read-only transactions"，而不是忽略该选项。内嵌 driver.Conn
// 并不携带 BeginTx（它属于另一个可选接口），因此这里不写就会丢掉驱动的只读事务能力，
// 表现为「生产可用的只读事务在计数连接上整体失败」。这里不计数：事务本身不产生语句，
// 其内部的查询仍会经过 ExecContext/QueryContext 被统计。
func (c countingConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	b, ok := c.Conn.(driver.ConnBeginTx)
	if !ok {
		// 本包装器只用于测试，底层恒为 modernc.org/sqlite（实现了 ConnBeginTx）。
		// 不退回已废弃的 Conn.Begin：那会让只读事务静默失去语义，恰好掩盖本函数要防的问题。
		return nil, fmt.Errorf("计数连接：底层驱动不支持带选项的事务，无法保证只读语义")
	}
	return b.BeginTx(ctx, opts)
}

// OpenCountingForTest 打开一个**语句级计数**的 SQLite 连接，返回连接与计数器指针。
//
// 仅测试使用：语料建好之后请先把计数器归零，再执行被测路径，这样做出的差值才是那条路径真实
// 发出的语句数（建表、迁移、DSN 里的 pragma 都会计入，但只影响零点之前的值）。
func OpenCountingForTest(dbPath string) (*DB, *int64, error) {
	inner, err := sqlite.NewConnector(buildDSN(dbPath))
	if err != nil {
		return nil, nil, err
	}
	counter := new(int64)
	sqlDB := sql.OpenDB(countingConnector{inner: inner, count: counter})
	return &DB{DB: sqlx.NewDb(sqlDB, "sqlite")}, counter, nil
}
