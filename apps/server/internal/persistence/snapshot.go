package persistence

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strings"
)

// SnapshotFile 对 dbPath 执行 `VACUUM INTO dstPath`，产出一个已 checkpoint 的
// 一致性快照文件（事务边界一致、不含 -wal/-shm 残留）。
//
// 为什么必须另开独立连接：VACUUM INTO 是一条**持有长读事务**的语句，整个快照期间都要占住
// 它所在的那条连接。若在主池上执行，它会一直吃掉池里的一条连接（几 GB 的库上是分钟量级），
// 主池上限就只有剩余几条可用——对一台只有 8 条连接的节点等同于明显降容。WAL 模式下读不
// 阻塞写，因此这里单独打开一条连接读快照，主池的读写完全不受影响。
//
// dstPath 必须不存在：SQLite 的 VACUUM INTO 在目标已存在时报错，调用方需先清理。
func SnapshotFile(dbPath, dstPath string) error {
	if dbPath == "" || dstPath == "" {
		return fmt.Errorf("快照路径不能为空")
	}
	if _, err := os.Stat(dstPath); err == nil {
		return fmt.Errorf("快照目标已存在：%s", dstPath)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("检查快照目标 %s：%w", dstPath, err)
	}

	conn, err := sql.Open("sqlite", snapshotDSN(dbPath))
	if err != nil {
		return fmt.Errorf("打开快照连接：%w", err)
	}
	defer func() { _ = conn.Close() }()
	// 这里是**专用单用途池**，不是「为了拿独占锁」：本函数只会发出一条 VACUUM INTO，
	// 把上限设为 1 是为了不额外开无用连接，与主池的并发上限无关。
	conn.SetMaxOpenConns(1)

	// VACUUM INTO 的路径是 SQL 字面量而非绑定参数，必须自行转义单引号（SQLite 不支持该语句的参数绑定）。
	// dstPath 由调用方（备份 / 快照）给出，已对单引号转义，不存在注入面。
	//nolint:gosec // G202：非外部输入拼接，且已转义
	stmt := "VACUUM INTO '" + strings.ReplaceAll(dstPath, "'", "''") + "'"
	if _, err := conn.Exec(stmt); err != nil {
		return fmt.Errorf("执行 VACUUM INTO：%w", err)
	}
	return nil
}

// snapshotDSN 组装快照专用 DSN：只保留忙等待与外键，不重复下发 journal_mode。
// 快照连接是短暂的一次性连接，切换日志模式会引入不必要的写锁竞争；
// 复用主连接已建立的 WAL 状态即可。
//
// 刻意**不带** _txlock=immediate：本连接只做一次性只读快照，VACUUM INTO 自己会取所需
// 的读锁；提前取写锁只会与主池的写者无谓竞争（见 db_concurrency_test.go 的同名用例）。
func snapshotDSN(dbPath string) string {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "foreign_keys(ON)")
	return "file:" + dbPath + "?" + q.Encode()
}
