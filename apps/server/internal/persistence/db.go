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

// DefaultMaxOpenConns 是连接池上限的默认值，也是 Open 的缺省口径。
// 与 config 侧常量 defaultSQLiteMaxConns（config.go）保持同值：
// 装配层始终注入 JIAN_SQLITE_MAX_CONNS 的解析结果，本常量只在 Open（测试、工具、
// 未装配调用）时兜底。
//
// 为什么 8：WAL 下读到的是同一份磁盘页的多个快照，读并发收益在 8 条连接上已基本吃满
// （再多只是增加 SQLite 侧的页面缓存副本与文件句柄，收益递减）；而写永远只有一个赢家，
// 池再大也不会提高写吞吐。
const DefaultMaxOpenConns = 8

// Open 以默认连接池上限（DefaultMaxOpenConns）连接指定路径的 SQLite 文件。
// 需要显式上限（装配层传入配置值）时用 OpenWithMaxConns。
func Open(dbPath string) (*DB, error) {
	return OpenWithMaxConns(dbPath, DefaultMaxOpenConns)
}

// OpenWithMaxConns 连接指定路径的 SQLite 文件并应用运行期 pragma：
// WAL 日志模式（并发读写）、外键约束、忙等待超时、NORMAL 同步级别。
// 连接建立后立即 Ping 以尽早暴露不可用（供 /readyz 复用）。
//
// 并发口径（为什么连接数 > 1 不会破坏一致性）：
//
//   - WAL 支持**多读 + 单写**：读事务各自看到一致的快照，且不阻塞写、写也不阻塞读。
//     因此把池上限放宽到 N 条，读侧获得真正的并发，正确性不受影响。
//   - 写事务由 SQLite 自身串行化：同一时刻只有一个连接能拿到 RESERVED 锁，其余写者在
//     锁外等待。串行化保证来自数据库引擎（文件锁 + WAL），**不依赖连接数**——
//     此前把池限制为 1 条，只是把并发请求排到池外，并未提供任何额外保证。
//   - 写者与写者 / 写者与读者之间的短暂锁竞争由 DSN 里的 busy_timeout(5000) 处理：
//     拿不到锁时驱动会重试而非立即报错。
//   - 事务一律以 BEGIN IMMEDIATE 开启（DSN 的 _txlock=immediate）：deferred 事务在
//     「先读后写」的锁升级点被 SQLite 直接判 SQLITE_BUSY（不进入 busy handler，因为
//     继续等待会形成死锁），把池放宽后这会变成可复现的 "database is locked"。
//     提前取写锁让所有竞争都落在 busy_timeout 的覆盖范围内，见 db_concurrency_test.go
//     的对照用例。
//
// 一条随之而来的编码约束：**事务内的一切语句都必须走那个事务**（tx.Get / tx.Exec），
// 不得在持事务期间改用 r.db（连接池）。旧实现的池上限为 1 时，这种写法会立刻自己死锁、
// 在测试里当场暴露；池放宽后它会静默地在另一条连接上执行，读到事务外的旧快照——
// 从「必然报错」退化成「偶发读到脏值」。仓库里所有 *Tx 方法（如 AssetRepo.CountAndSizeByRepoTx、
// RepoRepo.GetByIDTx）都只接受 tx，新增代码请沿用这一形态。
func OpenWithMaxConns(dbPath string, maxConns int) (*DB, error) {
	if maxConns <= 0 {
		// 防御：配置层已保证正数，这里兜底避免调用方传 0（database/sql 的 0 表示无上限）。
		maxConns = DefaultMaxOpenConns
	}
	dsn := buildDSN(dbPath)
	inner, err := sqlx.Connect("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("连接 SQLite（%s）：%w", dbPath, err)
	}
	inner.SetMaxOpenConns(maxConns)
	// idle 数与上限取齐：SQLite 连接是本地文件句柄，重开的代价是重新打开文件并重放
	// DSN 里的 pragma（busy_timeout / journal_mode / foreign_keys / synchronous 都是
	// 每连接设置），而不是像网络连接那样有建连握手与鉴权。若 idle 小于 max，抖动负载下
	// 会反复关掉又重建连接，白白付出 pragma 重放成本。SQLite 每连接的内存开销约百 KB
	// 量级（页面缓存 + 语句缓存，默认缓存很小），8 条完全可接受。
	inner.SetMaxIdleConns(maxConns)
	// 不设 SetConnMaxLifetime / SetConnMaxIdleTime：这两个参数是为了回收「网络连接」的
	// 服务端超时与失效，本地文件连接不存在这类失效。主动过期反而会在回收瞬间触发上面
	// 说的 pragma 重放。
	return &DB{DB: inner}, nil
}

// buildDSN 组装 modernc.org/sqlite 的 DSN，pragma 以 _pragma 查询参数下发。
//
// _txlock=immediate：所有（非只读）事务以 BEGIN IMMEDIATE 开启，在建事务时就把写锁
// 拿到手。这是连接数 > 1 时避免 "database is locked" 的关键——deferred 事务从读锁
// 升级到写锁时，若别的连接已持有写锁，SQLite 会立刻返回 SQLITE_BUSY 且**不调用**
// busy handler（继续等会与对方的读锁互相死锁），busy_timeout 因此形同虚设。
func buildDSN(dbPath string) string {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "foreign_keys(ON)")
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Set("_txlock", "immediate")
	return "file:" + dbPath + "?" + q.Encode()
}
