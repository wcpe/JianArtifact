package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/api"
	"github.com/wcpe/jianartifact/apps/server/internal/config"
)

func TestPasswordSecurityHTTP(t *testing.T) {
	cfg, svc := newSourceTestApplication(t, "raw")
	adminToken, admin, err := svc.authSvc.Bootstrap("password-auditor", auditTestPassword)
	if err != nil {
		t.Fatal(err)
	}
	target, err := svc.userSvc.Create("password-target", auditTestPassword, "user")
	if err != nil {
		t.Fatal(err)
	}
	memberToken, _, err := svc.authSvc.Login(target.Username, auditTestPassword)
	if err != nil {
		t.Fatal(err)
	}
	startTestSecurityAudit(t, svc)
	srv := httptest.NewServer(newApplicationHandler(cfg, svc, nil))
	t.Cleanup(srv.Close)
	path := func(id int64) string { return fmt.Sprintf("/api/v1/users/%d/password", id) }
	change := func(token string, id int64, password string) (int, string, string) {
		return auditTestRequest(t, srv, http.MethodPost, path(id), "application/json", fmt.Sprintf(`{"password":%q}`, password), token)
	}
	query := func(path string) string {
		t.Helper()
		code, _, body := auditTestRequest(t, srv, http.MethodGet, path, "", "", adminToken)
		if code != http.StatusOK {
			t.Fatalf("审计查询返回 %d: %s", code, body)
		}
		return body
	}
	list := func() []api.SecurityAuditEvent {
		now := time.Now().UTC().Truncate(time.Millisecond)
		params := url.Values{"from": {now.Add(-time.Minute).Format(time.RFC3339Nano)}, "to": {now.Add(time.Minute).Format(time.RFC3339Nano)}, "eventType": {"auth.password_changed"}}
		return queryDecode[api.SecurityAuditEventPage](t, query(securityEventsPath+"?"+params.Encode())).Items
	}
	state := func() (string, int64) {
		t.Helper()
		var row struct {
			Hash  string `db:"password_hash"`
			Epoch int64  `db:"account_epoch"`
		}
		if err := svc.db.Get(&row, "SELECT password_hash,account_epoch FROM user WHERE id=?", target.ID); err != nil {
			t.Fatal(err)
		}
		return row.Hash, row.Epoch
	}
	const newPassword = "local-test-password-changed"
	if status, _, _ := change(memberToken, admin.ID, newPassword); status != http.StatusForbidden {
		t.Fatalf("普通用户改他人口令返回 %d", status)
	}
	status, requestID, _ := change(adminToken, target.ID, newPassword)
	if status != http.StatusNoContent || requestID == "" {
		t.Fatalf("正常改密返回 %d", status)
	}
	items := list()
	if len(items) != 1 {
		t.Fatalf("预期立即查到唯一改密提交，实际 %d", len(items))
	}
	e := items[0]
	if e.ActorUserId == nil || *e.ActorUserId != admin.ID || e.RequestId == nil || *e.RequestId != requestID || e.OperationId == nil || *e.OperationId == requestID || e.EventType != "auth.password_changed" || e.Committed != "committed" || e.AuthMethod == nil || *e.AuthMethod != "web_jwt" {
		t.Fatal("改密事件未绑定真实操作者和操作")
	}
	detail := securityEventsPath + "/" + url.PathEscape(e.EventId)
	objectBody := query(detail + "/objects")
	objects := queryDecode[api.SecurityAuditEventObjectPage](t, objectBody).Items
	if len(objects) != 1 || objects[0].Ordinal != 1 || objects[0].ObjectKind != "user" || objects[0].ObjectId == nil || *objects[0].ObjectId != strconv.FormatInt(target.ID, 10) {
		t.Fatal("改密对象不是规范目标用户")
	}
	for _, field := range []string{"repositoryId", "canonicalPath", "sizeBytes", "digest", "targetVersion", "epoch", newPassword, "$argon2id$"} {
		if strings.Contains(objectBody, field) {
			t.Fatalf("对象回读包含不允许的字段形态：%q", field)
		}
	}
	if _, _, err := svc.authSvc.Login(target.Username, newPassword); err != nil {
		t.Fatal("新口令不能登录")
	}
	hash, epoch := state()
	if epoch != 2 {
		t.Fatalf("首次真实改密后代次为 %d", epoch)
	}
	if status, _, _ := change(memberToken, target.ID, newPassword); status != http.StatusNoContent {
		t.Fatalf("本人相同口令返回 %d", status)
	}
	if current, currentEpoch := state(); current != hash || currentEpoch != epoch || len(list()) != 1 {
		t.Fatal("相同口令产生了变更")
	}
	for _, table := range []string{"security_event_object", "credential_password_receipt"} {
		if _, err := svc.db.Exec("CREATE TRIGGER password_test_failure BEFORE INSERT ON " + table + " BEGIN SELECT RAISE(ABORT,'private-db-detail'); END"); err != nil {
			t.Fatal(err)
		}
		status, _, body := change(memberToken, target.ID, "local-test-password-not-committed")
		if _, err := svc.db.Exec("DROP TRIGGER password_test_failure"); err != nil {
			t.Fatal(err)
		}
		if status != http.StatusServiceUnavailable || !strings.Contains(body, "security_audit_unavailable") || strings.Contains(body, "private-db-detail") || strings.Contains(body, "operationId") {
			t.Fatalf("故障未使用安全对外错误，status=%d", status)
		}
		if current, currentEpoch := state(); current != hash || currentEpoch != epoch || len(list()) != 1 {
			t.Fatal("写失败后改密状态发生偏移")
		}
	}
	var receipts int
	if err := svc.db.Get(&receipts, "SELECT count(*) FROM credential_password_receipt WHERE target_user_id=?", target.ID); err != nil || receipts != 1 {
		t.Fatalf("回执数=%d err=%v", receipts, err)
	}
	if _, err := svc.db.Exec(`UPDATE user SET account_epoch=9223372036854775807 WHERE id=?`, target.ID); err != nil {
		t.Fatal(err)
	}
	if status, _, body := change(memberToken, target.ID, "local-test-password-version-conflict"); status != http.StatusConflict || !strings.Contains(body, "credential_version_conflict") || strings.Contains(body, "operationId") {
		t.Fatalf("有界代次拒绝未返回约定错误：%d", status)
	}
	if current, currentEpoch := state(); current != hash || currentEpoch != 9223372036854775807 || len(list()) != 1 {
		t.Fatal("版本冲突造成状态变化")
	}
}

func TestPasswordSecurityCLI(t *testing.T) {
	t.Setenv(config.EnvDataDir, t.TempDir())
	t.Setenv(config.EnvJWTSecret, "password-reset-audit-test-key-local-only")
	const name = "local-reset-audited"
	if err := adminReset([]string{"--username", name, "--password", "local-fixture-first-secret"}); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	svc, err := openServices(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.db.Close() })
	var initial int
	if err := svc.db.Get(&initial, `SELECT count(*) FROM credential_password_receipt`); err != nil || initial != 0 {
		t.Fatal("首次创建不能冒称改密")
	}
	if err := adminReset([]string{"--username", name, "--password", "local-fixture-next-secret"}); err != nil {
		t.Fatal(err)
	}
	var matches int
	err = svc.db.Get(&matches, `SELECT count(*) FROM credential_password_receipt r
		JOIN user u ON r.target_user_id=u.id
		JOIN security_event e ON r.event_id=e.event_id AND r.operation_id=e.operation_id
		JOIN security_event_object o ON e.sequence=o.event_sequence AND o.ordinal=1
		WHERE u.username=? AND u.account_epoch=2 AND r.actor_kind='local_operator' AND r.actor_user_id IS NULL
		AND e.event_type='auth.password_changed' AND e.actor_kind='local_operator' AND e.actor_user_id IS NULL
		AND e.request_id='' AND e.origin_request_id='' AND e.peer_ip_family=0 AND e.client_ip_family=0 AND e.proxy_trust='unknown'
		AND o.object_kind='user' AND o.object_id=CAST(u.id AS TEXT)`, name)
	if err != nil || matches != 1 {
		t.Fatalf("本机终态未绑定正确的操作者和对象：%d %v", matches, err)
	}
	if !canLogin(t, name, "local-fixture-next-secret") {
		t.Fatal("本机改密后新口令不能登录")
	}
}
