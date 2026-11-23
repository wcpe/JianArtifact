package persistence

import (
	"path/filepath"
	"testing"
)

func TestCredentialPasswordMigrationFrom0055(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "credential-upgrade.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.ensureMigrationTable(); err != nil {
		t.Fatal(err)
	}
	files, err := migrationFiles()
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		version := migrationVersion(file)
		if version >= "0056" {
			break
		}
		if err := db.applyMigration(file, version); err != nil {
			t.Fatal(err)
		}
	}
	if version, err := db.CurrentVersion(); err != nil || version != "0055" {
		t.Fatalf("旧版迁移水位：%q %v", version, err)
	}
	_, err = db.Exec(`INSERT INTO user (id,username,password_hash,role,status,web_login_disabled) VALUES (372,'legacy-admin','opaque-old-hash','admin','disabled',1);
		INSERT INTO api_token (user_id,name,token_digest,revoked_at) VALUES (372,'legacy','opaque-old-digest','2026-01-01 00:00:00');
		INSERT INTO revoked_token (jti,expires_at) VALUES ('legacy-jti',99999)`)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(); err != nil {
		t.Fatal(err)
	}
	for _, check := range []string{
		`SELECT count(*) FROM schema_migrations WHERE version='0056'`,
		`SELECT count(*) FROM user WHERE id=372 AND username='legacy-admin' AND password_hash='opaque-old-hash' AND role='admin' AND status='disabled' AND web_login_disabled=1 AND account_epoch=1`,
		`SELECT count(*) FROM api_token WHERE user_id=372 AND token_digest='opaque-old-digest' AND revoked_at='2026-01-01 00:00:00'`,
		`SELECT count(*) FROM revoked_token WHERE jti='legacy-jti' AND expires_at=99999`,
		`SELECT NOT EXISTS (SELECT 1 FROM credential_password_receipt)`,
	} {
		var n int
		if err := db.Get(&n, check); err != nil || n != 1 {
			t.Fatalf("迁移兼容断言失败：%s, %d %v", check, n, err)
		}
	}
	if _, err := db.Exec(`UPDATE user SET account_epoch=0 WHERE id=372`); err == nil {
		t.Fatal("非正代次被接受")
	}
}
