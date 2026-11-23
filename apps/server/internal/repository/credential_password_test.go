package repository

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
)

func passwordChangeFixture(t *testing.T, db *persistence.DB) (*UserRepo, CredentialPasswordChange) {
	t.Helper()
	r := NewUserRepo(db)
	id, err := r.Create("change-target", "opaque-fixture-original", "user")
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := r.ReadCredentialPassword(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return r, CredentialPasswordChange{Target: snapshot, NewHash: "opaque-fixture-next", Event: SecurityEvent{
		EventID: NewSecurityEventID(), OperationID: NewSecurityEventID(), EventType: SecurityEventPasswordChanged,
		ActorKind: "local_operator", ProxyTrust: "unknown", Committed: "committed", Outcome: "succeeded", OccurredAt: time.Now().UTC(),
	}}
}

func nextPasswordFixture(change CredentialPasswordChange, hash string) CredentialPasswordChange {
	change.Event.EventID, change.Event.OperationID = NewSecurityEventID(), NewSecurityEventID()
	change.NewHash = hash
	return change
}

func TestCredentialPasswordCASAndReceipt(t *testing.T) {
	db := openSecurityEventDB(t)
	r, first := passwordChangeFixture(t, db)
	ctx := context.Background()
	unchanged := nextPasswordFixture(first, "")
	if changed, err := r.ApplyCredentialPassword(ctx, unchanged); changed || err != nil {
		t.Fatalf("当前相同口令误报：changed=%v err=%v", changed, err)
	}

	// 两个请求已经观察到同一旧代次，随后同步竞争真实 SQLite 写事务。
	second := nextPasswordFixture(first, "opaque-fixture-other")
	var wg sync.WaitGroup
	start := make(chan struct{})
	type result struct {
		changed bool
		err     error
		input   CredentialPasswordChange
	}
	results := make(chan result, 2)
	for _, in := range []CredentialPasswordChange{first, second} {
		wg.Go(func() {
			<-start
			changed, err := r.ApplyCredentialPassword(ctx, in)
			results <- result{changed, err, in}
		})
	}
	close(start)
	wg.Wait()
	close(results)
	var winner CredentialPasswordChange
	var wins, stale int
	for got := range results {
		switch {
		case got.err == nil && got.changed:
			winner = got.input
			wins++
		case errors.Is(got.err, ErrPasswordVersionConflict) && !got.changed:
			stale++
		default:
			t.Fatalf("非预期竞争裁决：changed=%v err=%v", got.changed, got.err)
		}
	}
	if wins != 1 || stale != 1 {
		t.Fatalf("竞争裁决通过=%d 陈旧=%d", wins, stale)
	}
	if changed, err := r.ApplyCredentialPassword(ctx, unchanged); changed || !errors.Is(err, ErrPasswordVersionConflict) {
		t.Fatalf("陈旧相同口令误报：changed=%v err=%v", changed, err)
	}
	if changed, err := r.ApplyCredentialPassword(ctx, winner); changed || err != nil {
		t.Fatalf("原操作重复执行写入：changed=%v err=%v", changed, err)
	}

	// 模拟提交确认通道失去响应：真实持久回执仍可独立回读，取消不取消回读本身。
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if confirmed, err := r.confirmCredentialPassword(canceled, winner); !confirmed || err != nil {
		t.Fatalf("原操作回执确认失败：%v", err)
	}
	wrong := winner
	wrong.Target.ID++
	if _, err := r.ApplyCredentialPassword(ctx, wrong); !errors.Is(err, ErrPasswordVersionConflict) {
		t.Fatalf("异目标不能认领原回执：%v", err)
	}
	if confirmed, err := r.confirmCredentialPassword(ctx, wrong); confirmed || !errors.Is(err, ErrPasswordCommitUnknown) {
		t.Fatalf("确认异目标回执：%v", err)
	}
	wrong = winner
	wrong.Event.ActorKind, wrong.Event.ActorUserID = "user", &winner.Target.ID
	if _, err := r.ApplyCredentialPassword(ctx, wrong); !errors.Is(err, ErrPasswordVersionConflict) {
		t.Fatalf("异主体不能认领原回执：%v", err)
	}
	current, err := r.ReadCredentialPassword(ctx, first.Target.ID)
	if err != nil || current.Epoch != first.Target.Epoch+1 || current.PasswordHash != winner.NewHash {
		t.Fatal("竞争后未保留唯一获胜状态")
	}
	assertCredentialPasswordCounts(t, db, 1)
}

func TestCredentialPasswordDeferredCommitFailure(t *testing.T) {
	db := openSecurityEventDB(t)
	r, change := passwordChangeFixture(t, db)
	ctx := context.Background()
	_, err := db.Exec(`CREATE TABLE password_test_parent (id INTEGER PRIMARY KEY);
		CREATE TABLE password_test_child (parent INTEGER REFERENCES password_test_parent(id) DEFERRABLE INITIALLY DEFERRED);
		CREATE TRIGGER password_test_commit_failure AFTER INSERT ON credential_password_receipt BEGIN INSERT INTO password_test_child VALUES (7); END`)
	if err != nil {
		t.Fatal(err)
	}
	if confirmed, err := r.ApplyCredentialPassword(ctx, change); confirmed || !errors.Is(err, ErrPasswordCommitUnknown) {
		t.Fatalf("延迟约束提交不应宣称成功：%v", err)
	}
	after, err := r.ReadCredentialPassword(ctx, change.Target.ID)
	if err != nil || after.Epoch != change.Target.Epoch || after.PasswordHash != change.Target.PasswordHash {
		t.Fatal("提交失败却看到了未提交的状态")
	}
	assertCredentialPasswordCounts(t, db, 0)
	if _, err := db.Exec(`DROP TRIGGER password_test_commit_failure`); err != nil {
		t.Fatal(err)
	}
	if confirmed, err := r.ApplyCredentialPassword(ctx, nextPasswordFixture(change, change.NewHash)); !confirmed || err != nil {
		t.Fatalf("正常写入未恢复：%v", err)
	}
	assertCredentialPasswordCounts(t, db, 1)
}

func TestCredentialPasswordTemporarySQLiteBusy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "password-busy.db")
	writer, err := persistence.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = writer.Close() })
	if err := writer.Migrate(); err != nil {
		t.Fatal(err)
	}
	conn, err := sqlx.Connect("sqlite", "file:"+filepath.ToSlash(path)+"?_pragma=busy_timeout(25)&_pragma=foreign_keys(1)&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	db := &persistence.DB{DB: conn}
	r, change := passwordChangeFixture(t, db)
	tx := securityEventTx(t, writer)
	ctx := context.Background()
	if changed, err := r.ApplyCredentialPassword(ctx, change); changed || !IsSQLiteWriteUnavailable(err) || errors.Is(err, ErrPasswordCommitUnknown) {
		t.Fatalf("真实写竞争裁决不正确：%v", err)
	}
	assertCredentialPasswordCounts(t, db, 0)
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if changed, err := r.ApplyCredentialPassword(ctx, nextPasswordFixture(change, change.NewHash)); !changed || err != nil {
		t.Fatalf("锁释放后新正常操作失败：%v", err)
	}
	assertCredentialPasswordCounts(t, db, 1)
}

func assertCredentialPasswordCounts(t *testing.T, db *persistence.DB, want int) {
	t.Helper()
	for _, query := range []string{
		`SELECT count(*) FROM security_event WHERE event_type='auth.password_changed'`,
		`SELECT count(*) FROM security_event_object WHERE object_kind='user'`,
		`SELECT count(*) FROM credential_password_receipt`,
	} {
		var got int
		if err := db.Get(&got, query); err != nil || got != want {
			t.Fatalf("%s: 得到 %d，预期 %d，错误 %v", query, got, want, err)
		}
	}
}

func TestCredentialPasswordEventObjectScoped(t *testing.T) {
	_, change := passwordChangeFixture(t, openSecurityEventDB(t))
	e := change.Event
	valid := SecurityEventObject{Ordinal: 1, ObjectKind: "user", ObjectID: "1"}
	if err := validateSecurityEvent(e, []SecurityEventObject{valid}); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []SecurityEventObject{
		{Ordinal: 1, ObjectKind: "user", ObjectID: "01"},
		{Ordinal: 1, ObjectKind: "user", ObjectID: "+1"},
		{Ordinal: 1, ObjectKind: "user", ObjectID: "0"},
		{Ordinal: 1, ObjectKind: "user", ObjectID: "9223372036854775808"},
		{Ordinal: 1, ObjectKind: "user", ObjectID: "1", TargetVersion: "2"},
		{Ordinal: 1, ObjectKind: "user", ObjectID: "1", CanonicalPath: "user"},
		{Ordinal: 1, ObjectKind: "asset", CanonicalPath: "user"},
	} {
		if err := validateSecurityEvent(e, []SecurityEventObject{invalid}); !errors.Is(err, ErrInvalidSecurityEvent) {
			t.Fatal("改密对象验证意外允许额外字段或非规范 ID")
		}
	}
	e.RequestID = NewSecurityEventID()
	if err := validateSecurityEvent(e, []SecurityEventObject{valid}); !errors.Is(err, ErrInvalidSecurityEvent) {
		t.Fatal("本机操作不能携带 HTTP 请求号")
	}
	asset, _ := securityEventAssetFixture()
	if err := validateSecurityEvent(asset, []SecurityEventObject{valid}); !errors.Is(err, ErrInvalidSecurityEvent) {
		t.Fatal("资产事件不得接纳用户对象")
	}
}
