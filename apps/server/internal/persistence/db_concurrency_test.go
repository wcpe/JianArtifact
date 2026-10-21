package persistence

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
)

// 本文件验证「连接池上限 > 1 之后 SQLite 仍然可用」这一批改动最核心的风险：
// 并发读写时是否出现 SQLITE_BUSY（错误文案 "database is locked"）。
//
// 三个用例分工：
//   - 并发读：多 goroutine 同时 SELECT，验证 WAL 的读并发真的被吃到了（无错、且确实并行）。
//   - 并发读写混合：多写者 + 多读者同时跑，验证写串行化与 busy_timeout 覆盖得住。
//   - deferred 对照：同一负载换回默认的 BEGIN DEFERRED，用于证明 _txlock=immediate
//     不是「保险起见加上去的」，而是必须的（去掉它这条用例会红）。

// 并发用例的规模：写者数 × 每写者写入轮次决定总事务数；读者数决定并发读压力。
// 取值偏小以免拖慢常规测试，但足以让多个事务真实交叠（本机实测总耗时 < 2s）。
const (
	concurrencyWriters    = 6
	concurrencyReaders    = 6
	concurrencyWriteTimes = 40
)

// openForConcurrencyTest 建一张最小的被测表并返回连接。
// 刻意不用完整 Migrate：并发正确性与业务 schema 无关，小表能让事务更快交叠。
func openForConcurrencyTest(t *testing.T, dsn func(string) string) *DB {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "concurrency.db")
	db, err := openWithDSN(dsn(dbPath), DefaultMaxOpenConns)
	if err != nil {
		t.Fatalf("打开数据库：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	// 建表与插种子行走 autocommit（非事务语句），不参与本用例的并发假设。
	if _, err := db.Exec(`CREATE TABLE probe (
		id    INTEGER PRIMARY KEY AUTOINCREMENT,
		k     TEXT NOT NULL UNIQUE,
		note  TEXT NOT NULL DEFAULT '',
		touch INTEGER NOT NULL DEFAULT 0
	)`); err != nil {
		t.Fatalf("建表 probe：%v", err)
	}
	if _, err := db.Exec(`INSERT INTO probe(k) VALUES ('seed')`); err != nil {
		t.Fatalf("写入种子行：%v", err)
	}
	return db
}

// openWithDSN 用完整 DSN 打开连接并套用与生产一致的池参数。
func openWithDSN(dsn string, maxConns int) (*DB, error) {
	inner, err := sqlx.Connect("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	inner.SetMaxOpenConns(maxConns)
	inner.SetMaxIdleConns(maxConns)
	return &DB{DB: inner}, nil
}

// deferredDSN 是「把 _txlock 退回默认 deferred」的对照组 DSN。
// 除了这一项，其余 pragma 与生产 DSN 完全一致，确保对比只动了一个变量。
func deferredDSN(dbPath string) string {
	return strings.Replace(buildDSN(dbPath), "_txlock=immediate", "_txlock=deferred", 1)
}

// immediateDSN 显式标注生产 DSN（保持用例成对出现，一眼能看出对比的是哪两个形态）。
func immediateDSN(dbPath string) string { return buildDSN(dbPath) }

// isLockedError 判断错误是否为 SQLITE_BUSY / SQLITE_LOCKED 的表现。
func isLockedError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "database is locked") ||
		strings.Contains(msg, "database table is locked") ||
		strings.Contains(msg, "sqlite_busy") ||
		strings.Contains(msg, "sqlite_locked")
}

// TestConcurrentReadsDoNotBlock 验证多个 goroutine 同时 SELECT 不报错，
// 且确实并发执行。
//
// 这条用例同时量化「连接池上限 1 → 8」的收益：同一负载分别在池上限 1 与 8 下各跑一遍，
// 单连接组必然把读请求排成一条队（耗时 ≈ 读者数 × 单次持有时间），多连接组则并行。
// 这正是线上「所有接口都慢」的根因——全部读请求排在同一条连接上。
func TestConcurrentReadsDoNotBlock(t *testing.T) {
	const readers = 8
	const holdPerQuery = 40 * time.Millisecond
	// 串行上界 = 读者数 × 轮数 × 单次持有时间：池上限为 1 时的理论耗时。
	serialUpper := time.Duration(readers*2) * holdPerQuery

	// 池上限 1（改造前的行为）作为基线。
	serial := openForConcurrencyTest(t, immediateDSN)
	serial.SetMaxOpenConns(1)
	serial.SetMaxIdleConns(1)
	serialElapsed := runReadHoldLoad(t, serial, readers, holdPerQuery)
	if serialElapsed < serialUpper {
		t.Fatalf("基线（池上限 1）耗时 %s 短于串行上界 %s，说明基线本身没有串行化，对照无效",
			serialElapsed.Round(time.Millisecond), serialUpper)
	}

	// 池上限 8（改造后）：必须显著快于串行基线。
	parallel := openForConcurrencyTest(t, immediateDSN)
	parallelElapsed := runReadHoldLoad(t, parallel, readers, holdPerQuery)
	if parallelElapsed >= serialUpper {
		t.Fatalf("并发读未真正并行：耗时 %s ≥ 串行上界 %s（连接池上限 %d）",
			parallelElapsed.Round(time.Millisecond), serialUpper, DefaultMaxOpenConns)
	}
	t.Logf("并发读对照：%d 读者 × 2 轮 —— 池上限 1 耗时 %s，池上限 %d 耗时 %s（串行上界 %s，实测提速 %.1f×）",
		readers, serialElapsed.Round(time.Millisecond), DefaultMaxOpenConns,
		parallelElapsed.Round(time.Millisecond), serialUpper,
		float64(serialElapsed)/float64(parallelElapsed))
}

// runReadHoldLoad 跑「多读者各自钉住一条连接查询、并持有到本次查询结束」的负载，返回总耗时。
//
// 必须用 db.Conn 显式钉住连接、并在持有期间 sleep：真实的慢查询在语句执行期间一直占着
// 连接，池上限 1 时后到的请求才只能在池外排队。若 sleep 放在语句返回之后，连接早已归还，
// 串行基线就不成立（对照会失效）。
func runReadHoldLoad(t *testing.T, db *DB, readers int, holdPerQuery time.Duration) time.Duration {
	t.Helper()
	var wg sync.WaitGroup
	errCh := make(chan error, readers)
	started := time.Now()
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for round := 0; round < 2; round++ {
				if err := runOneHeldRead(db, holdPerQuery); err != nil {
					errCh <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("并发读出现错误：%v", err)
	}
	return time.Since(started)
}

// runOneHeldRead 取一条连接、在它上面查一次、持有到 sleep 结束再归还。
func runOneHeldRead(db *DB, hold time.Duration) error {
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("取连接：%w", err)
	}
	defer func() { _ = conn.Close() }()
	var note string
	if err := conn.QueryRowContext(ctx, `SELECT note FROM probe WHERE k = 'seed'`).Scan(&note); err != nil {
		return fmt.Errorf("并发读：%w", err)
	}
	time.Sleep(hold)
	return nil
}

// TestConcurrentWriteAndReadNoLockedError 验证「多写者 + 多读者」同时跑不出现
// "database is locked"，且写入结果完整（没有丢事务、没有脏值）。
//
// 每个写事务都是**先读后写**：先 SELECT 拿当前 touch，再 UPDATE 自增。
// 这正是 deferred 事务最危险的形态——读锁升级写锁时 SQLite 会立刻返回 SQLITE_BUSY
// 且不进入 busy handler。生产 DSN 用 BEGIN IMMEDIATE 把写锁前置，因此不应报错。
func TestConcurrentWriteAndReadNoLockedError(t *testing.T) {
	runWriteReadMix(t, "生产 DSN（BEGIN IMMEDIATE）", immediateDSN, false)
}

// TestDeferredTransactionLocksUnderSameLoad 是对照组：同样的负载、同样的连接池，
// 只把事务模式退回默认的 BEGIN DEFERRED。
//
// 期望结果是**出现 database is locked**——它证明 _txlock=immediate 是必要配置，
// 而不是可以随手删掉的装饰。若某天驱动或 SQLite 行为变化使 deferred 也不再报错，
// 本用例会失败并提示可以重新评估该配置（而不是让配置静默地失去理由）。
func TestDeferredTransactionLocksUnderSameLoad(t *testing.T) {
	runWriteReadMix(t, "对照 DSN（BEGIN DEFERRED）", deferredDSN, true)
}

// runWriteReadMix 跑「多写者 + 多读者」混合负载。
// wantLocked 为 true 时要求确实观测到锁冲突（对照组），为 false 时要求一次都不出现（生产组）。
func runWriteReadMix(t *testing.T, label string, dsn func(string) string, wantLocked bool) {
	t.Helper()
	db := openForConcurrencyTest(t, dsn)

	var lockErrs atomic.Int64
	var otherErrs atomic.Int64
	var firstOther error
	var otherOnce sync.Once
	var writes atomic.Int64
	firstLockMsg := make(chan string, 1)

	recordErr := func(err error) {
		if isLockedError(err) {
			lockErrs.Add(1)
			select {
			case firstLockMsg <- err.Error():
			default:
			}
			return
		}
		otherErrs.Add(1)
		otherOnce.Do(func() { firstOther = err })
	}

	stop := make(chan struct{})
	var readersWg sync.WaitGroup
	var writersWg sync.WaitGroup

	// 读者：在写者刷新的同时不断读，验证「写不阻塞读」。
	for i := 0; i < concurrencyReaders; i++ {
		readersWg.Add(1)
		go func() {
			defer readersWg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				var touch int
				if err := db.Get(&touch, `SELECT touch FROM probe WHERE k = 'seed'`); err != nil {
					recordErr(fmt.Errorf("读者查询：%w", err))
				}
			}
		}()
	}

	// 写者：先读后写的事务，模拟真实的「读当前状态再更新」。
	for i := 0; i < concurrencyWriters; i++ {
		writersWg.Add(1)
		go func(w int) {
			defer writersWg.Done()
			key := fmt.Sprintf("k-%d", w)
			for round := 0; round < concurrencyWriteTimes; round++ {
				err := func() error {
					tx, err := db.Beginx()
					if err != nil {
						return fmt.Errorf("开启事务：%w", err)
					}
					defer func() { _ = tx.Rollback() }()
					// 先读：把事务变成「读—写」而不是纯写，才能压到锁升级路径。
					var touch int
					err = tx.Get(&touch, `SELECT touch FROM probe WHERE k = 'seed'`)
					if err != nil {
						return fmt.Errorf("事务内读：%w", err)
					}
					if _, err := tx.Exec(`UPDATE probe SET touch = touch + 1 WHERE k = 'seed'`); err != nil {
						return fmt.Errorf("事务内写 seed：%w", err)
					}
					if _, err := tx.Exec(
						`INSERT INTO probe(k, note) VALUES (?, ?)
						 ON CONFLICT(k) DO UPDATE SET note = excluded.note, touch = probe.touch + 1`,
						key, fmt.Sprintf("%d-%d", w, round)); err != nil {
						return fmt.Errorf("事务内写自有行：%w", err)
					}
					return tx.Commit()
				}()
				if err != nil {
					recordErr(err)
					continue
				}
				writes.Add(1)
			}
		}(i)
	}

	// 写者跑完后停读者：读者的退出条件由写者进度决定，不引入额外的超时等待。
	writersWg.Wait()
	close(stop)
	readersWg.Wait()

	if firstOther != nil {
		t.Fatalf("%s：出现非锁类错误：%v", label, firstOther)
	}

	locked := lockErrs.Load()
	if wantLocked {
		if locked == 0 {
			t.Fatalf("%s：期望观测到 database is locked，实际 0 次。"+
				"说明当前驱动/SQLite 已不再受 deferred 锁升级限制，可重新评估 DSN 里的 _txlock",
				label)
		}
		var msg string
		select {
		case msg = <-firstLockMsg:
		default:
		}
		t.Logf("%s：实测出现 %d 次锁冲突（首条：%s）——这正是生产 DSN 必须用 BEGIN IMMEDIATE 的原因",
			label, locked, msg)
		return
	}

	if locked > 0 {
		var msg string
		select {
		case msg = <-firstLockMsg:
		default:
		}
		t.Fatalf("%s：出现 %d 次 database is locked（首条：%s），连接池上限 %d 不可用",
			label, locked, msg, DefaultMaxOpenConns)
	}

	// 生产组还应验证写入完整性：任何一次成功提交都必须体现在最终计数上。
	var seedTouch int
	if err := db.Get(&seedTouch, `SELECT touch FROM probe WHERE k = 'seed'`); err != nil {
		t.Fatalf("%s：读取最终 seed.touch：%v", label, err)
	}
	// 每次成功写事务对 seed 自增 1，种子行初值为 0，因此最终值必须恰好等于成功事务数。
	if want := int(writes.Load()); seedTouch != want {
		t.Fatalf("%s：seed.touch = %d，期望 %d（成功写事务 %d）——写丢失或串行化被破坏",
			label, seedTouch, want, writes.Load())
	}
	t.Logf("%s：%d 写者 × %d 轮 = %d 次成功写事务，%d 读者并发，锁冲突 0 次，seed.touch = %d（与成功提交数一致）",
		label, concurrencyWriters, concurrencyWriteTimes, writes.Load(), concurrencyReaders, seedTouch)
}

// TestNewConnectionsOpenWhileWriteLocked 验证一个只有「池上限 > 1」才会暴露的隐患：
// DSN 里的 pragma 是**每连接**生效的，新连接建立时要执行 journal_mode(WAL) 等语句。
// 若此时别的连接正持有写锁，pragma 会不会因取不到锁而让连接建立失败？
//
// 结论（本用例锁定）：不会。journal_mode 已是 WAL 时该 pragma 是幂等的空操作，
// 不需要独占访问；且 busy_timeout 由驱动在其余 pragma 之前设置，短暂竞争会重试。
// 这条用例的价值在于：它让「池放宽后建连不再安全」这种猜测有据可查——若哪天驱动改了
// pragma 下发顺序（busy_timeout 不再优先），本用例会红。
func TestNewConnectionsOpenWhileWriteLocked(t *testing.T) {
	db := openForConcurrencyTest(t, immediateDSN)

	// 持有一个写事务，制造「别的连接正持写锁」的窗口。
	tx, err := db.Beginx()
	if err != nil {
		t.Fatalf("开启写事务：%v", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	if _, err := tx.Exec(`UPDATE probe SET touch = touch + 1 WHERE k = 'seed'`); err != nil {
		t.Fatalf("写事务内更新：%v", err)
	}

	// 同时从多个 goroutine 读：池里只占用了 1 条写连接，其余读者必须**新建连接**才能服务，
	// 于是每个新建连接都会在写锁被持有的窗口内执行 pragma。
	const readers = DefaultMaxOpenConns
	var wg sync.WaitGroup
	errCh := make(chan error, readers)
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var touch int
			// 读不阻塞于写（WAL 的语义），因此这里不应遇到锁错误，也不应建连失败。
			if err := db.Get(&touch, `SELECT touch FROM probe WHERE k = 'seed'`); err != nil {
				errCh <- err
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("写锁持有期间新建连接/读取失败：%v", err)
	}

	if err := tx.Commit(); err != nil {
		t.Fatalf("提交写事务：%v", err)
	}
	committed = true

	// 反复建连的路径也要健康：把 idle 连接全部关掉，强制下一批查询重新建连。
	db.SetMaxIdleConns(0)
	timed := time.Now()
	for i := 0; i < 3*DefaultMaxOpenConns; i++ {
		var touch int
		if err := db.Get(&touch, `SELECT touch FROM probe WHERE k = 'seed'`); err != nil {
			t.Fatalf("重建连接后读取失败（第 %d 次）：%v", i, err)
		}
	}
	t.Logf("写锁窗口内 %d 次并发读 + %d 次强制重建连接全部成功（耗时 %s）",
		readers, 3*DefaultMaxOpenConns, time.Since(timed).Round(time.Millisecond))
}

// TestBuildDSNUsesImmediateTxLock 锁定 DSN 的关键约定：主库必须用 BEGIN IMMEDIATE。
// 这条断言把「为什么要 immediate」写进代码，避免后续被人当成冗余参数删掉。
func TestBuildDSNUsesImmediateTxLock(t *testing.T) {
	dsn := buildDSN(filepath.Join(t.TempDir(), "x.db"))
	if !strings.Contains(dsn, "_txlock=immediate") {
		t.Fatalf("主库 DSN 必须带 _txlock=immediate（否则并发写会撞 database is locked），实际：%s", dsn)
	}
	for _, want := range []string{"busy_timeout%285000%29", "journal_mode%28WAL%29", "foreign_keys%28ON%29", "synchronous%28NORMAL%29"} {
		if !strings.Contains(dsn, want) {
			t.Fatalf("DSN 缺少既有 pragma %s，实际：%s", want, dsn)
		}
	}
}

// TestSnapshotDSNStaysDeferred 锁定快照连接不使用 immediate：
// 快照连接只做一次性读取（VACUUM INTO 会自行取所需的读锁），
// 取写锁只会与主库写者无谓竞争。
func TestSnapshotDSNStaysDeferred(t *testing.T) {
	dsn := snapshotDSN(filepath.Join(t.TempDir(), "x.db"))
	if strings.Contains(dsn, "_txlock") {
		t.Fatalf("快照 DSN 不应设置 _txlock（保持驱动默认 deferred），实际：%s", dsn)
	}
}

// TestOpenDefaultsToConfiguredMaxConns 验证 Open 与 OpenWithMaxConns 的池参数落地：
// 非法上限回落默认值，显式上限被采用。
func TestOpenDefaultsToConfiguredMaxConns(t *testing.T) {
	dir := t.TempDir()

	db, err := Open(filepath.Join(dir, "default.db"))
	if err != nil {
		t.Fatalf("Open：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if got := db.Stats().MaxOpenConnections; got != DefaultMaxOpenConns {
		t.Fatalf("Open 的池上限 = %d，期望 %d", got, DefaultMaxOpenConns)
	}

	explicit, err := OpenWithMaxConns(filepath.Join(dir, "explicit.db"), 3)
	if err != nil {
		t.Fatalf("OpenWithMaxConns：%v", err)
	}
	t.Cleanup(func() { _ = explicit.Close() })
	if got := explicit.Stats().MaxOpenConnections; got != 3 {
		t.Fatalf("显式池上限 = %d，期望 3", got)
	}

	// 非正数必须回落默认值而不是交给 database/sql（它的 0 表示无上限）。
	for _, bad := range []int{0, -1} {
		fallback, err := OpenWithMaxConns(filepath.Join(dir, fmt.Sprintf("bad%d.db", bad)), bad)
		if err != nil {
			t.Fatalf("OpenWithMaxConns(%d)：%v", bad, err)
		}
		if got := fallback.Stats().MaxOpenConnections; got != DefaultMaxOpenConns {
			t.Fatalf("上限 %d 应回落默认 %d，实际 %d", bad, DefaultMaxOpenConns, got)
		}
		_ = fallback.Close()
	}
}

// TestBuildDSNKeepsFilePathIntact 验证新增的查询参数没有破坏 DSN 的路径部分：
// 路径原样跟在 "file:" 之后、参数一律在 "?" 之后（Windows 上的盘符与反斜杠不得被转义）。
func TestBuildDSNKeepsFilePathIntact(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "with space.db")
	dsn := buildDSN(dbPath)

	head, tail, found := strings.Cut(dsn, "?")
	if !found {
		t.Fatalf("DSN 缺少查询分隔符 ?，实际：%s", dsn)
	}
	if head != "file:"+dbPath {
		t.Fatalf("DSN 路径部分 = %q，期望 %q", head, "file:"+dbPath)
	}
	if !strings.Contains(tail, "_txlock=immediate") {
		t.Fatalf("DSN 查询部分缺少 _txlock=immediate，实际：%s", tail)
	}
}
