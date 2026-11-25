package persistence

import (
	"path/filepath"
	"testing"
)

func TestCredentialAuthorityMigrationFrom0056(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "credential-authority-upgrade.db"))
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
		if v > "0056" {
			break
		}
		if err := db.applyMigration(file, v); err != nil {
			t.Fatal(err)
		}
	}
	if v, err := db.CurrentVersion(); v != "0056" || err != nil {
		t.Fatalf("升级基线=%q：%v", v, err)
	}
	_, err = db.Exec(`INSERT INTO user(id,username,password_hash,status,account_epoch) VALUES(346,'legacy-access','opaque-fixture','disabled',3);
	INSERT INTO api_token(user_id,name,token_digest) VALUES(346,'legacy','opaque-test-digest');
	INSERT INTO revoked_token(jti,expires_at) VALUES('old-fixture-jti',451)`)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(); err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{
		`SELECT count(*) FROM schema_migrations WHERE version='0057'`,
		`SELECT count(*) FROM user WHERE id=346 AND password_hash='opaque-fixture' AND status='disabled' AND account_epoch=3`,
		`SELECT count(*) FROM api_token WHERE user_id=346 AND token_digest='opaque-test-digest' AND account_epoch=3 AND expires_at>strftime('%s','now')`,
		`SELECT count(*) FROM revoked_token WHERE jti='old-fixture-jti' AND expires_at=451`,
		`SELECT NOT EXISTS(SELECT 1 FROM credential_session)`,
		`SELECT NOT EXISTS(SELECT 1 FROM credential_account_access_receipt)`,
	} {
		var got int
		if err := db.Get(&got, sql); err != nil || got != 1 {
			t.Fatalf("迁移断言 %s：%d %v", sql, got, err)
		}
	}
	if _, err := db.Exec(`UPDATE user SET account_epoch=4 WHERE id=346`); err != nil {
		t.Fatal(err)
	}
	var revision int64
	if err := db.Get(&revision, `SELECT credential_revision FROM user WHERE id=346`); err != nil || revision != 1 {
		t.Fatalf("账号代次未同序推进签发水位：%d %v", revision, err)
	}
}
