package httpserver_test

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
)

// 本包 15 处测试环境构造都做同一件事：在新临时目录里 Open 一个新库，再 Migrate 一遍全部
// 迁移脚本。迁移对**空库**的产物是确定的：45 个脚本各一个事务、每事务一次 fsync，
// 实测单次 60~130ms；而本包一次运行要构造上百个环境，这部分是纯重复开销。
//
// 处理办法：进程内一次性迁移出一份模板库并缓存其文件字节，此后每个用例把模板字节
// 写进自己的临时目录再打开。每个用例仍然拿到**独立可写的库文件 + 独立连接**，
// 隔离性、可写性、判据一律不变，只是不再重复跑迁移。
//
// 模板目录在生成后立即删除、进程内只留字节，因此不存在跨用例共享磁盘文件的清理时序问题。
var (
	migratedTemplateOnce sync.Once
	migratedTemplateData []byte
	migratedTemplateErr  error
)

// openMigratedDB 在 dbPath 处提供一个已应用全部迁移的 SQLite 库；语义等价于
// `persistence.Open(dbPath)` 紧接 `db.Migrate()`（含连接在用例结束时关闭的清理）。
func openMigratedDB(t *testing.T, dbPath string) *persistence.DB {
	t.Helper()
	data, err := migratedTemplate()
	if err != nil {
		t.Fatalf("生成已迁移数据库模板：%v", err)
	}
	if err := os.WriteFile(dbPath, data, 0o600); err != nil {
		t.Fatalf("写入已迁移数据库：%v", err)
	}
	db, err := persistence.Open(dbPath)
	if err != nil {
		t.Fatalf("打开数据库：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// migratedTemplate 惰性生成模板库并返回其文件字节。
func migratedTemplate() ([]byte, error) {
	migratedTemplateOnce.Do(func() {
		dir, err := os.MkdirTemp("", "jian-httpserver-migrated-template")
		if err != nil {
			migratedTemplateErr = err
			return
		}
		defer func() { _ = os.RemoveAll(dir) }()
		tplPath := filepath.Join(dir, "template.db")
		db, err := persistence.Open(tplPath)
		if err != nil {
			migratedTemplateErr = err
			return
		}
		if err := db.Migrate(); err != nil {
			_ = db.Close()
			migratedTemplateErr = err
			return
		}
		// 关闭最后一个连接会把 WAL checkpoint 回主库文件，因此主库文件即完整迁移结果。
		if err := db.Close(); err != nil {
			migratedTemplateErr = err
			return
		}
		data, err := os.ReadFile(tplPath)
		if err != nil {
			migratedTemplateErr = err
			return
		}
		migratedTemplateData = data
	})
	if migratedTemplateErr != nil {
		return nil, migratedTemplateErr
	}
	return migratedTemplateData, nil
}
