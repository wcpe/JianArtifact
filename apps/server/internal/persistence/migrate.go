package persistence

import (
	"embed"
	"fmt"
	"io/fs"
	"log"
	"sort"
	"strings"
	"time"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Migrate 以前向追加方式应用尚未执行的迁移脚本。
//
// 迁移器零外部依赖：读取内嵌 migrations/*.sql，按文件名字典序（0001、0002…）
// 顺序执行；已应用的版本记录在 schema_migrations 表，重复调用幂等。
// 每个迁移文件在单个事务内执行，失败回滚，保证 schema 一致性。
// 对齐 docs/adr/0002-sqlite-filesystem-storage.md「schema 由迁移脚本管理」。
//
// **独占性口径**：Migrate 假设调用期没有其他并发的 schema 变更，也假设没有第二个进程
// 同时迁移同一个库。这一假设**由调用时机保证**（wiring.go 的 openServices 在开始监听前
// 调用一次），而不是由连接池上限保证——把池限制为 1 条连接从来就不提供跨进程互斥，
// 进程内的并发调用在旧实现下同样会交错（两个调用都可能在对方提交前读到「未应用」）。
// 池放宽后每个迁移事务都以 BEGIN IMMEDIATE 持有写锁，因此单个迁移脚本自身仍是原子且
// 与业务写入互斥的；若将来需要多进程/多实例共用一个库，应补一把基于 schema_migrations
// 的抢占式锁，而不是回退连接池。
func (db *DB) Migrate() error {
	if err := db.ensureMigrationTable(); err != nil {
		return err
	}
	applied, err := db.appliedVersions()
	if err != nil {
		return err
	}
	files, err := migrationFiles()
	if err != nil {
		return err
	}
	for _, name := range files {
		version := migrationVersion(name)
		if applied[version] {
			continue
		}
		// 每个待应用的迁移打开始/结束两行。大库上的建索引（如 asset 全表扫描）
		// 可能持续数分钟，期间进程不监听端口；没有这两行日志，运维只能看到
		// 「容器一直没起来」，无法判断是卡在哪个迁移。
		log.Printf("数据库迁移开始：%s", name)
		start := time.Now()
		if err := db.applyMigration(name, version); err != nil {
			log.Printf("数据库迁移失败：%s（耗时 %v）：%v", name, time.Since(start).Round(time.Millisecond), err)
			return err
		}
		log.Printf("数据库迁移完成：%s（耗时 %v）", name, time.Since(start).Round(time.Millisecond))
	}
	return nil
}

// CurrentVersion 返回已应用的最新迁移版本；无任何迁移时返回空串。
func (db *DB) CurrentVersion() (string, error) {
	if err := db.ensureMigrationTable(); err != nil {
		return "", err
	}
	var version string
	err := db.Get(&version, `SELECT COALESCE(MAX(version), '') FROM schema_migrations`)
	if err != nil {
		return "", fmt.Errorf("读取迁移版本：%w", err)
	}
	return version, nil
}

func (db *DB) ensureMigrationTable() error {
	const ddl = `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    TEXT PRIMARY KEY,
		applied_at TEXT NOT NULL DEFAULT (datetime('now'))
	)`
	if _, err := db.Exec(ddl); err != nil {
		return fmt.Errorf("创建 schema_migrations 表：%w", err)
	}
	return nil
}

func (db *DB) appliedVersions() (map[string]bool, error) {
	var versions []string
	if err := db.Select(&versions, `SELECT version FROM schema_migrations`); err != nil {
		return nil, fmt.Errorf("读取已应用迁移：%w", err)
	}
	set := make(map[string]bool, len(versions))
	for _, v := range versions {
		set[v] = true
	}
	return set, nil
}

func (db *DB) applyMigration(name, version string) error {
	body, err := migrationFS.ReadFile("migrations/" + name)
	if err != nil {
		return fmt.Errorf("读取迁移 %s：%w", name, err)
	}
	tx, err := db.Beginx()
	if err != nil {
		return fmt.Errorf("开启迁移事务 %s：%w", name, err)
	}
	if _, err := tx.Exec(string(body)); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("执行迁移 %s：%w", name, err)
	}
	if _, err := tx.Exec(`INSERT INTO schema_migrations (version) VALUES (?)`, version); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("记录迁移 %s：%w", name, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交迁移 %s：%w", name, err)
	}
	return nil
}

// migrationFiles 返回按文件名升序排列的迁移脚本名（不含目录前缀）。
func migrationFiles() ([]string, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("枚举迁移目录：%w", err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names, nil
}

// migrationVersion 从文件名截取版本号：0001_init.sql -> 0001。
func migrationVersion(name string) string {
	base := strings.TrimSuffix(name, ".sql")
	if i := strings.Index(base, "_"); i > 0 {
		return base[:i]
	}
	return base
}
