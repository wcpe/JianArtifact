package persistence

import (
	"path/filepath"
	"testing"
)

// TestLegacyTokenExpiryBackfill 校验 0076：双 NULL 迁移前旧令牌被回填
// 30 天宽限期与所属用户当前代次；混合行（单字段 NULL）不动，按失效处理。
func TestLegacyTokenExpiryBackfill(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "legacy-token-expiry-upgrade.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.ensureMigrationTable(); err != nil {
		t.Fatal(err)
	}
	files, err := migrationFiles()
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		v := migrationVersion(file)
		if v > "0075" {
			break
		}
		if err := db.applyMigration(file, v); err != nil {
			t.Fatal(err)
		}
	}
	// 双 NULL 旧行（0057 之前签发的遗留令牌）与已吊销旧行。
	if _, err = db.Exec(`INSERT INTO user(id,username,password_hash,status,account_epoch) VALUES(41,'legacy-ci','opaque','active',2)`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO api_token(user_id,name,token_digest) VALUES(41,'legacy','opaque-legacy-digest')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(); err != nil {
		t.Fatal(err)
	}
	// 回填后：expires_at 在未来（宽限期约 30 天）、account_epoch 取用户当前代次 2。
	var n int
	if err := db.Get(&n, `SELECT count(*) FROM api_token WHERE user_id=41 AND account_epoch=2 AND expires_at>strftime('%s','now')+2500000 AND expires_at<strftime('%s','now')+2700000`); err != nil || n != 1 {
		t.Fatalf("双 NULL 旧行应按 30 天宽限与用户代次回填：%d %v", n, err)
	}
	// 迁移幂等：再次执行不破坏已回填值。
	if err := db.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := db.Get(&n, `SELECT count(*) FROM api_token WHERE user_id=41 AND account_epoch=2 AND expires_at IS NOT NULL`); err != nil || n != 1 {
		t.Fatalf("重复迁移后回填值应稳定：%d %v", n, err)
	}
}

// TestLegacyTokenBackfillLeavesMixedRows 校验单字段 NULL 的异常行不被回填复活。
func TestLegacyTokenBackfillLeavesMixedRows(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "legacy-token-mixed.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.ensureMigrationTable(); err != nil {
		t.Fatal(err)
	}
	files, err := migrationFiles()
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		v := migrationVersion(file)
		if v > "0075" {
			break
		}
		if err := db.applyMigration(file, v); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = db.Exec(`INSERT INTO user(id,username,password_hash,status,account_epoch) VALUES(42,'mixed','opaque','active',1)`); err != nil {
		t.Fatal(err)
	}
	// 有期限但无代次的混合行：0057 后不存在这种签发路径，属异常态。
	if _, err = db.Exec(`INSERT INTO api_token(user_id,name,token_digest,expires_at) VALUES(42,'mixed','opaque-mixed',9999999999)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.Get(&n, `SELECT count(*) FROM api_token WHERE user_id=42 AND account_epoch IS NULL`); err != nil || n != 1 {
		t.Fatalf("混合行不应被回填代次：%d %v", n, err)
	}
}
