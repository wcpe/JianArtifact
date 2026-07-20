package persistence

import (
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Migrate 以前向追加方式应用尚未执行的迁移脚本。
//
// 迁移器零外部依赖：读取内嵌 migrations/*.sql，按文件名字典序（0001、0002…）
// 顺序执行；已应用的版本记录在 schema_migrations 表，重复调用幂等。
// 每个迁移文件在单个事务内执行，失败回滚，保证 schema 一致性。
// 对齐 docs/adr/0002-sqlite-filesystem-storage.md「schema 由迁移脚本管理」。
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
		if err := db.applyMigration(name, version); err != nil {
			return err
		}
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
