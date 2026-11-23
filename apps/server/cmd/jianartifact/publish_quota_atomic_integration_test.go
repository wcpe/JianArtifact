package main

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

func TestAtomicPublishQuotaHTTP(t *testing.T) {
	for _, tc := range []struct {
		name, format, path   string
		stream, fail, policy bool
	}{
		{"Raw 已知长度", "raw", "atomic/known.txt", false, false, true},
		{"Raw 流式实际字节", "raw", "atomic/stream.txt", true, false, true},
		{"Maven 共用单件上传", "maven", "com/example/demo/1.0/demo-1.0.jar", false, false, true},
		{"结算故障时回滚", "raw", "atomic/rollback.txt", false, true, true},
		{"无策略不生成预留", "raw", "atomic/no-policy.txt", false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, svc := newSourceTestApplication(t, tc.format)
			repo, err := svc.repoSvc.Create("atomic-quota", tc.format, "hosted", "public", "", repository.RepositoryConfig{})
			if err != nil {
				t.Fatal(err)
			}
			_, user, err := svc.authSvc.Bootstrap("quota-test-admin", auditTestPassword)
			if err != nil {
				t.Fatal(err)
			}
			token, _, err := svc.tokenSvc.Create(user.ID, "quota-test-token")
			if err != nil {
				t.Fatal(err)
			}
			if tc.policy {
				if _, err := svc.db.Exec(`INSERT INTO publish_policy(user_id,repository_id,max_assets_hour,max_bytes_day,max_file_bytes) VALUES(?,?,?,?,?)`, user.ID, repo.ID, 3, 128, 64); err != nil {
					t.Fatal(err)
				}
			}
			if tc.fail {
				if _, err := svc.db.Exec(`CREATE TRIGGER test_fail_quota_settle BEFORE UPDATE OF settled_at ON quota_reservation BEGIN SELECT RAISE(ABORT, 'injected-quota-test'); END`); err != nil {
					t.Fatal(err)
				}
			}
			server := newSourceTestServer(t, newApplicationHandler(cfg, svc, nil), "192.0.2.10:4321", true)
			content := []byte("synthetic artifact body")
			var reader io.Reader = bytes.NewReader(content)
			if tc.stream {
				reader = io.LimitReader(reader, int64(len(content)))
			}
			path := "/repository/" + repo.Name + "/" + tc.path
			req, err := http.NewRequest(http.MethodPut, server.URL+path, reader)
			if err != nil {
				t.Fatal(err)
			}
			if tc.stream && req.ContentLength != 0 {
				t.Fatal("测试未构造未知内容长度")
			}
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("X-Forwarded-For", "203.0.113.31")
			req.Header.Set("X-Forwarded-Proto", "https")
			resp, err := server.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			data, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			if tc.fail {
				if resp.StatusCode != 503 || !strings.Contains(string(data), `"security_audit_unavailable"`) || strings.Contains(string(data), "injected-quota-test") {
					t.Errorf("结算故障的对外响应：%d %s", resp.StatusCode, data)
				}
				status, _, _ := auditTestRequest(t, server, "GET", path, "", "", "")
				if status != 404 {
					t.Errorf("未提交资产仍可读：%d", status)
				}
				for _, query := range []string{`SELECT count(*) FROM asset`, `SELECT count(*) FROM asset_path_fts WHERE asset_path_fts MATCH 'rollback'`, `SELECT count(*) FROM replication_operation_outbox`, `SELECT count(*) FROM security_event WHERE event_type='asset.change_committed'`, `SELECT count(*) FROM quota_usage`, `SELECT count(*) FROM quota_reservation`} {
					var count int
					if err := svc.db.Get(&count, query); err != nil || count != 0 {
						t.Errorf("事务回滚残留：%s → %d / %v", query, count, err)
					}
				}
				return
			}
			if resp.StatusCode != 201 {
				t.Fatalf("发布未成功：%d %s", resp.StatusCode, data)
			}
			var operation string
			if err := svc.db.Get(&operation, `SELECT e.operation_id FROM security_event e JOIN replication_operation_outbox o ON o.operation_id=e.operation_id JOIN asset_mutation m ON m.id=e.operation_id WHERE e.request_id=? AND e.actor_user_id=? AND e.repository_id=? AND m.status='completed'`, resp.Header.Get("X-Request-ID"), user.ID, repo.ID); err != nil || operation == "" {
				t.Fatalf("原完成证据不一致：%q / %v", operation, err)
			}
			var rows int
			want := 0
			if tc.policy {
				want = 1
			}
			if err := svc.db.Get(&rows, `SELECT count(*) FROM quota_reservation WHERE settled_at IS NOT NULL AND user_id=? AND repository_id=? AND assets=1 AND bytes=?`, user.ID, repo.ID, len(content)); err != nil || rows != want {
				t.Fatalf("实际结算行数：%d / %v；应为 %d", rows, err, want)
			}
			if err := svc.db.Get(&rows, `SELECT count(*) FROM quota_usage WHERE user_id=? AND repository_id=? AND used_assets=1 AND used_bytes=?`, user.ID, repo.ID, len(content)); err != nil || rows != want*2 {
				t.Fatalf("时/日账本计数：%d / %v；应为 %d", rows, err, want*2)
			}
		})
	}
}
