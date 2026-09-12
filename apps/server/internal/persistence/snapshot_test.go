package persistence

import (
	"database/sql"
	"math/rand"
	"path/filepath"
	"sync"
	"testing"
)

// TestSnapshotFileProducesOpenableCopy 验证快照是一个可直接打开、内容完整的库。
func TestSnapshotFileProducesOpenableCopy(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "src.db")
	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("Migrate：%v", err)
	}
	if _, err := db.Exec("INSERT INTO setting(key, value) VALUES('probe', 'v')"); err != nil {
		t.Fatalf("写入 setting：%v", err)
	}

	dst := filepath.Join(dir, "snap.db")
	if err := SnapshotFile(dbPath, dst); err != nil {
		t.Fatalf("SnapshotFile：%v", err)
	}

	snap, err := sql.Open("sqlite", snapshotDSN(dst))
	if err != nil {
		t.Fatalf("打开快照：%v", err)
	}
	defer func() { _ = snap.Close() }()

	var value string
	if err := snap.QueryRow("SELECT value FROM setting WHERE key='probe'").Scan(&value); err != nil {
		t.Fatalf("读取快照 setting：%v", err)
	}
	if value != "v" {
		t.Fatalf("快照 setting.value = %q，期望 %q", value, "v")
	}
}

// TestSnapshotFileRejectsExistingTarget 验证目标已存在时明确报错，
// 避免调用方在未清理旧文件的情况下误以为拿到了新快照。
func TestSnapshotFileRejectsExistingTarget(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "src.db")
	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	dst := filepath.Join(dir, "snap.db")
	if err := SnapshotFile(dbPath, dst); err != nil {
		t.Fatalf("首次 SnapshotFile：%v", err)
	}
	if err := SnapshotFile(dbPath, dst); err == nil {
		t.Fatal("目标已存在时 SnapshotFile 应当报错")
	}
}

// TestSnapshotIsConsistentUnderConcurrentWrites 是热备份的核心正确性验证：
// 维护 a+b=100 的不变量，在持续事务写入的同时取快照，快照里必须永远满足该不变量
// （即快照落在某个已提交事务边界上，绝不会读到写了一半的状态），
// 且并发写入不得因快照而失败（WAL 下读不阻塞写、独立连接不抢主池）。
func TestSnapshotIsConsistentUnderConcurrentWrites(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "src.db")
	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if _, err := db.Exec(`CREATE TABLE probe_inv (id INTEGER PRIMARY KEY, a INTEGER NOT NULL, b INTEGER NOT NULL)`); err != nil {
		t.Fatalf("建表：%v", err)
	}
	if _, err := db.Exec(`INSERT INTO probe_inv (id, a, b) VALUES (1, 50, 50)`); err != nil {
		t.Fatalf("初始化：%v", err)
	}

	const rounds = 200
	var (
		wg       sync.WaitGroup
		writeErr error
		stop     = make(chan struct{})
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		rng := rand.New(rand.NewSource(1))
		for i := 0; i < rounds; i++ {
			a := rng.Intn(101)
			tx, err := db.Begin()
			if err != nil {
				writeErr = err
				return
			}
			if _, err := tx.Exec(`UPDATE probe_inv SET a=?, b=? WHERE id=1`, a, 100-a); err != nil {
				_ = tx.Rollback()
				writeErr = err
				return
			}
			if err := tx.Commit(); err != nil {
				writeErr = err
				return
			}
			select {
			case <-stop:
				return
			default:
			}
		}
	}()

	dst := filepath.Join(dir, "snap.db")
	if err := SnapshotFile(dbPath, dst); err != nil {
		close(stop)
		wg.Wait()
		t.Fatalf("SnapshotFile：%v", err)
	}
	close(stop)
	wg.Wait()

	if writeErr != nil {
		t.Fatalf("快照期间并发写入失败（不应被快照阻塞）：%v", writeErr)
	}

	snap, err := sql.Open("sqlite", snapshotDSN(dst))
	if err != nil {
		t.Fatalf("打开快照：%v", err)
	}
	defer func() { _ = snap.Close() }()

	var a, b int
	if err := snap.QueryRow(`SELECT a, b FROM probe_inv WHERE id=1`).Scan(&a, &b); err != nil {
		t.Fatalf("读取快照：%v", err)
	}
	if a+b != 100 {
		t.Fatalf("快照破坏了事务不变量：a=%d b=%d（期望 a+b=100）", a, b)
	}

	var integrity string
	if err := snap.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil {
		t.Fatalf("integrity_check：%v", err)
	}
	if integrity != "ok" {
		t.Fatalf("快照完整性检查 = %q，期望 ok", integrity)
	}
}

// TestSnapshotFileRejectsEmptyPath 验证空路径参数被明确拒绝。
func TestSnapshotFileRejectsEmptyPath(t *testing.T) {
	if err := SnapshotFile("", filepath.Join(t.TempDir(), "x.db")); err == nil {
		t.Fatal("空 dbPath 应当报错")
	}
	if err := SnapshotFile(filepath.Join(t.TempDir(), "y.db"), ""); err == nil {
		t.Fatal("空 dstPath 应当报错")
	}
}
