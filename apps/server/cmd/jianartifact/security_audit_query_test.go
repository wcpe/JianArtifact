package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/wcpe/jianartifact/apps/server/internal/api"
	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

const collectionHealthPath = "/api/v1/observability/audit/collection-health"
const securityEventsPath = "/api/v1/observability/audit/security-events"

func TestSecurityAuditQueryHTTP(t *testing.T) {
	cfg, svc := newSourceTestApplication(t, "raw")
	admin, user, err := svc.authSvc.Bootstrap("query-admin", auditTestPassword)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.userSvc.Create("query-user", auditTestPassword, "user"); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(newApplicationHandler(cfg, svc, nil))
	t.Cleanup(server.Close)
	login := func(name string) string {
		t.Helper()
		status, _, body := auditTestRequest(t, server, "POST", "/api/v1/auth/login", "application/json", `{"username":"`+name+`","password":"`+auditTestPassword+`"}`, "")
		v := queryDecode[api.LoginResponse](t, body)
		if status != 200 || v.Token == "" {
			t.Fatalf("无法获取测试登录会话：%d", status)
		}
		return v.Token
	}
	ordinary, secondAdmin := login("query-user"), login("query-admin")
	machine, _, err := svc.tokenSvc.Create(user.ID, "query-machine")
	if err != nil {
		t.Fatal(err)
	}
	get := func(t *testing.T, path, token string, want int) string {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, server.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Request-ID", "caller-controlled-marker")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if token == "basic" {
			req.SetBasicAuth("query-admin", auditTestPassword)
		}
		res, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = res.Body.Close() }()
		var raw json.RawMessage
		if err := json.NewDecoder(res.Body).Decode(&raw); err != nil {
			t.Fatalf("%q: %d: %v", path, res.StatusCode, err)
		}
		if res.StatusCode != want {
			t.Fatalf("%q: 得到 %d，期望 %d：%s", path, res.StatusCode, want, raw)
		}
		if want >= 400 && strings.HasPrefix(path, "/api/v1/observability/audit/") {
			de := queryDecode[api.Error](t, string(raw))
			const marker = "caller-controlled-marker"
			code := map[int]string{400: "invalid_security_filter", 401: "unauthenticated", 403: "forbidden", 404: "security_event_not_found", 410: "security_cursor_expired", 503: "security_audit_unavailable"}[want]
			if de.Error.Code != code || de.OperationId != nil || de.RequestId == nil || *de.RequestId == "" || *de.RequestId != res.Header.Get("X-Request-ID") || strings.Contains(string(raw), marker) {
				t.Fatalf("不正确的脱敏错误：%s", raw)
			}
		}
		return string(raw)
	}
	t.Run("授权先于四路绑定或存在性", func(t *testing.T) {
		for _, path := range []string{securityEventsPath + "?limit=caller-controlled-marker", securityEventsPath + "/not-found?cursor=%ZZ", securityEventsPath + "/not-found/objects?limit=caller-controlled-marker", collectionHealthPath + "?caller-controlled-marker="} {
			get(t, path, "", 401)
			get(t, path, ordinary, 403)
			get(t, path, machine, 403)
			get(t, path, "basic", 403)
			get(t, path, admin, 400)
		}
		get(t, securityEventsPath+"/not-found", admin, 404)
		get(t, securityEventsPath+"/not-found/objects", admin, 404)
		mgr := auth.NewJWTManager(cfg.JWTSecret)
		claims, err := mgr.Parse(secondAdmin)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := svc.db.Exec(`UPDATE user SET web_login_disabled=1 WHERE id=?`, user.ID); err != nil {
			t.Fatal(err)
		}
		get(t, securityEventsPath+"?limit=caller-controlled-marker", secondAdmin, 403)
		if _, err := svc.db.Exec(`UPDATE user SET web_login_disabled=0 WHERE id=?`, user.ID); err != nil {
			t.Fatal(err)
		}
		revoked, jti, expiresAt, err := mgr.Issue(user.ID, "admin")
		if err != nil {
			t.Fatal(err)
		}
		if err := svc.authSvc.Logout(jti, expiresAt.Unix()); err != nil {
			t.Fatal(err)
		}
		get(t, securityEventsPath+"?limit=caller-controlled-marker", revoked, 401)
		claims.Subject = strconv.FormatInt(user.ID, 10)
		claims.ExpiresAt = jwt.NewNumericDate(time.Now().Add(-time.Minute))
		expired, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(cfg.JWTSecret)
		if err != nil {
			t.Fatal(err)
		}
		get(t, securityEventsPath+"?limit=caller-controlled-marker", expired, 401)
		get(t, securityEventsPath+"?limit=caller-controlled-marker", "invalid-test-session", 401)
	})
	occurred := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Millisecond)
	rangeQuery := "?from=" + occurred.Add(-time.Minute).Format(time.RFC3339Nano) + "&to=" + occurred.Add(time.Minute).Format(time.RFC3339Nano)
	t.Run("严格查询和诚实空页", func(t *testing.T) {
		for _, suffix := range []string{"&cursor=", "&cursor=x&cursor=y", "&count=1", "&offset=1", "&limit=0", "&limit=101", "&eventType=unknown", "&ip=127.0.0.1&cidr=127.0.0.0%2F8", "&requestId=%FF", "&digest=%ZZ", "&limit=1&%6cimit=1", "&canonicalPath=%20", "&requestId=x;discard=y"} {
			get(t, securityEventsPath+rangeQuery+suffix, admin, 400)
		}
		for _, suffix := range []string{"", "?from=2026-01-01", "?from=2026-01-01T00:00:00Z&to=2026-02-01T00:00:00Z", "?from=2026-01-01T00:00:00,1Z&to=2026-01-01T00:00:01Z"} {
			get(t, securityEventsPath+suffix, admin, 400)
		}
		page := queryDecode[api.SecurityAuditEventPage](t, get(t, securityEventsPath+rangeQuery, admin, 200))
		if page.Items == nil || len(page.Items) != 0 || page.NextCursor != nil || page.HasMore || page.Coverage != "unknown" || page.ProjectionHealth != "healthy" || page.ProjectionWatermark != page.SnapshotWatermark {
			t.Fatalf("空页错误：%+v", page)
		}
	})
	timeQuery := func(from, to string) string {
		return securityEventsPath + "?" + url.Values{"from": {from}, "to": {to}}.Encode()
	}
	t.Run("查询拒绝小于毫秒的边界", func(t *testing.T) {
		const from = "2026-01-01T00:00:00Z"
		const to = "2026-01-01T00:00:01Z"
		bad := timeQuery("2026-01-01T00:00:00.0000000001Z", to)
		get(t, bad, "", 401)
		get(t, bad, ordinary, 403)
		for _, tc := range []struct{ name, from, to string }{
			{"下界第四位", "2026-01-01T00:00:00.0011Z", to},
			{"上界第四位", from, "2026-01-01T00:00:01.0011Z"},
			{"下界超出纳秒的非零尾数", "2026-01-01T05:45:00.00100000001+05:45", to},
			{"上界超出纳秒的非零尾数", from, "2025-12-31T20:30:01.00100000001-03:30"},
		} {
			t.Run(tc.name, func(t *testing.T) { get(t, timeQuery(tc.from, tc.to), admin, 400) })
		}
	})
	t.Run("持久列表与对象页", func(t *testing.T) {
		ctx := context.Background()
		writeAt := func(t *testing.T, id string, count int, at time.Time) {
			t.Helper()
			event := repository.SecurityEvent{EventID: id, EventType: "protocol.write_denied", OccurredAt: at, ActorKind: "unknown", ProxyTrust: "direct", Outcome: "denied", Committed: "not_committed", SourceTrustVersion: "test-trust-marker", ClientIP: netip.MustParseAddr("192.0.2.19"), PeerIP: netip.MustParseAddr("192.0.2.19"), ErrorCode: "forbidden"}
			var objs []repository.SecurityEventObject
			for range count {
				objs = append(objs, repository.SecurityEventObject{ObjectKind: "asset", CanonicalPath: "packages/test.txt"})
			}
			tx, err := svc.db.BeginTxx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback() }()
			if err := repository.NewSecurityEventRepo(svc.db).InsertTx(ctx, tx, event, objs); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
		}
		write := func(id string, count int) { writeAt(t, id, count, occurred) }
		write("query-first", 0)
		write("query-second", 0)
		write("query-third", 3)
		path := securityEventsPath + rangeQuery + "&limit=1&ip=192.0.2.19"
		first := queryDecode[api.SecurityAuditEventPage](t, get(t, path, admin, 200))
		if !first.HasMore || first.NextCursor == nil || len(first.Items) != 1 || first.Items[0].EventId != "query-third" {
			t.Fatalf("首屏异常：%+v", first)
		}
		write("query-late", 0)
		secondPath := securityEventsPath + rangeQuery + "&limit=1&ip=%3A%3Affff%3A192.0.2.19&cursor=" + url.QueryEscape(*first.NextCursor)
		second := queryDecode[api.SecurityAuditEventPage](t, get(t, secondPath, admin, 200))
		if !second.HasMore || second.NextCursor == nil || len(second.Items) != 1 || second.Items[0].EventId != "query-second" || second.Coverage != first.Coverage || second.SnapshotWatermark != first.SnapshotWatermark || second.ProjectionWatermark != first.ProjectionWatermark {
			t.Fatalf("续页未稳定：%+v / %+v", first, second)
		}
		get(t, secondPath, secondAdmin, 400)
		get(t, path+"&outcome=denied&cursor="+*first.NextCursor, admin, 400)
		last := queryDecode[api.SecurityAuditEventPage](t, get(t, path+"&cursor="+*second.NextCursor, admin, 200))
		if last.HasMore || last.NextCursor != nil || len(last.Items) != 1 || last.Items[0].EventId != "query-first" {
			t.Fatalf("末页异常：%+v", last)
		}
		detailPath := securityEventsPath + "/query-third"
		raw := get(t, detailPath, admin, 200)
		detail := queryDecode[api.SecurityAuditEvent](t, raw)
		if detail.EventId != "query-third" || !detail.OccurredAt.Equal(occurred) || !detail.IngestedAt.After(occurred) || detail.ErrorCode == nil || *detail.ErrorCode != "forbidden" || strings.Contains(raw, "committedAt") || strings.Contains(raw, "test-trust-marker") || strings.Contains(raw, "ordinal") || strings.Contains(raw, "generation") {
			t.Fatalf("详情不是白名单投影：%s", raw)
		}
		objects := detailPath + "/objects?limit=1"
		op := queryDecode[api.SecurityAuditEventObjectPage](t, get(t, objects, admin, 200))
		if !op.HasMore || op.NextCursor == nil || len(op.Items) != 1 || op.Items[0].Ordinal != 1 {
			t.Fatalf("对象首屏异常：%+v", op)
		}
		op2 := queryDecode[api.SecurityAuditEventObjectPage](t, get(t, objects+"&cursor="+*op.NextCursor, admin, 200))
		if !op2.HasMore || op2.NextCursor == nil || len(op2.Items) != 1 || op2.Items[0].Ordinal != 2 {
			t.Fatalf("对象锚点异常：%+v", op2)
		}
		get(t, path+"&cursor="+*op.NextCursor, admin, 400)
		get(t, securityEventsPath+"/query-first/objects?cursor="+*op.NextCursor, admin, 400)
		get(t, objects+"&cursor="+*first.NextCursor, admin, 400)
		// 同节点新装配仍认可签名和会话，但必须拒绝旧启动栅栏。
		restarted, err := openServices(cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = restarted.db.Close() })
		original := server
		server = httptest.NewServer(newApplicationHandler(cfg, restarted, nil))
		t.Cleanup(server.Close)
		get(t, secondPath, admin, 410)
		get(t, objects+"&cursor="+*op.NextCursor, admin, 410)
		get(t, secondPath, secondAdmin, 400)
		server = original
		if _, err := svc.db.Exec(`DELETE FROM security_event WHERE event_id=?`, "query-first"); err != nil {
			t.Fatal(err)
		}
		get(t, secondPath, admin, 410)
		get(t, objects+"&cursor="+*op.NextCursor, admin, 410)
		get(t, secondPath, secondAdmin, 400)
		t.Run("整毫秒半开区间及等价游标", func(t *testing.T) {
			lower := occurred.Truncate(time.Second).Add(2*time.Minute + 100*time.Millisecond)
			upper := lower.Add(2 * time.Millisecond)
			writeAt(t, "time-before", 0, lower.Add(-time.Millisecond))
			writeAt(t, "time-lower", 0, lower)
			writeAt(t, "time-inner", 0, lower.Add(time.Millisecond))
			writeAt(t, "time-upper", 0, upper)
			canonical := timeQuery(lower.Format(time.RFC3339Nano), upper.Format(time.RFC3339Nano))
			page := queryDecode[api.SecurityAuditEventPage](t, get(t, canonical, admin, 200))
			if len(page.Items) != 2 || page.HasMore || page.Items[0].EventId != "time-inner" || page.Items[1].EventId != "time-lower" {
				t.Fatalf("应包含下界并排除上界：%+v", page)
			}
			first := queryDecode[api.SecurityAuditEventPage](t, get(t, canonical+"&limit=1", admin, 200))
			if !first.HasMore || first.NextCursor == nil || len(first.Items) != 1 || first.Items[0].EventId != "time-inner" {
				t.Fatalf("时间查询首屏异常：%+v", first)
			}
			// 同一时刻可带非整点时差与无损补零，续页必须仍使用相同筛选指纹。
			const padded = "2006-01-02T15:04:05.000000000000Z07:00"
			equivalent := timeQuery(lower.In(time.FixedZone("东", 5*3600+45*60)).Format(padded), upper.In(time.FixedZone("西", -3*3600-30*60)).Format(padded))
			last := queryDecode[api.SecurityAuditEventPage](t, get(t, equivalent+"&limit=1&cursor="+url.QueryEscape(*first.NextCursor), admin, 200))
			if last.HasMore || last.NextCursor != nil || len(last.Items) != 1 || last.Items[0].EventId != "time-lower" || last.SnapshotWatermark != first.SnapshotWatermark {
				t.Fatalf("等价时间表示改变了续页：%+v", last)
			}
		})
	})
	t.Run("未知采集不改变探针", func(t *testing.T) {
		health := queryDecode[api.SecurityAuditCollectionHealth](t, get(t, collectionHealthPath, admin, 200))
		if health.State != "starting" || health.Coverage != "unknown" || health.Watermark != nil || health.LastSuccess != nil {
			t.Fatalf("未知采集冒充已证状态：%+v", health)
		}
		for _, p := range []string{"/healthz", "/readyz"} {
			raw := get(t, p, "", 200)
			v := queryDecode[api.HealthStatus](t, raw)
			if v.Status != "ok" || v.Version != "" || v.AuditCollection == nil || *v.AuditCollection != "unknown" || strings.Contains(raw, "watermark") {
				t.Fatalf("公共探针泄露或损坏：%s", raw)
			}
		}
		if _, err := svc.db.Exec(`ALTER TABLE security_event_read_fence RENAME TO test_offline_read_fence`); err != nil {
			t.Fatal(err)
		}
		get(t, securityEventsPath+rangeQuery, admin, 503)
		if err := svc.db.Close(); err != nil {
			t.Fatal(err)
		}
		get(t, "/healthz", "", 200)
		get(t, "/readyz", "", 503)
		response, err := server.Client().Get(server.URL + "/metrics")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = response.Body.Close() }()
		body, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != 200 || !strings.Contains(string(body), `jianartifact_security_audit_collection_state{state="unknown"} 1`) || strings.Contains(string(body), "query-admin") {
			t.Fatalf("采集指标无效：%d", response.StatusCode)
		}
	})
}

func TestSecurityAuditQueryHTTPPersistedGap(t *testing.T) {
	cfg, svc := newSourceTestApplication(t, "raw")
	admin, _, err := svc.authSvc.Bootstrap("gap-admin", auditTestPassword)
	if err != nil {
		t.Fatal(err)
	}
	startTestSecurityAudit(t, svc)
	server := httptest.NewServer(newApplicationHandler(cfg, svc, nil))
	t.Cleanup(server.Close)
	deadline := time.Now().Add(5 * time.Second)
	wait := func(condition func() bool) {
		t.Helper()
		for !condition() {
			if time.Now().After(deadline) {
				t.Fatal("未观察到持久采集检查点")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	wait(func() bool { return !svc.securityAudit.Health().LastSuccess.IsZero() })
	for range 32 {
		svc.securityAudit.RecordAuthAttemptFailed(context.Background(), "password", http.StatusUnauthorized)
	}
	wait(func() bool { return svc.securityAudit.Health().LastGap.Reason == "source_rate" })
	status, _, body := auditTestRequest(t, server, "GET", collectionHealthPath, "", "", admin)
	health := queryDecode[api.SecurityAuditCollectionHealth](t, body)
	gap := health.LastGap
	if status != 200 || health.State != "running" || health.Coverage != "gapped" || health.Watermark == nil || *health.Watermark <= 0 || health.LastSuccess == nil || gap == nil || gap.Coverage != "gapped" || !gap.WindowEnd.After(gap.WindowStart) || gap.ObservedCount == nil || gap.SuppressedCount == nil || *gap.SuppressedCount == 0 || strings.Contains(body, "gap-admin") || strings.Contains(body, "clientIp") {
		t.Fatalf("详细采集缺口与已证计数不符：%d %s", status, body)
	}
	status, _, body = auditTestRequest(t, server, "GET", "/healthz", "", "", "")
	probe := queryDecode[api.HealthStatus](t, body)
	if status != 200 || probe.AuditCollection == nil || *probe.AuditCollection != "degraded" || strings.Contains(body, "source_rate") {
		t.Fatalf("公共探针没有收敛采集缺口：%d %s", status, body)
	}
}

func queryDecode[T any](t *testing.T, raw string) T {
	t.Helper()
	var out T
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatal(err)
	}
	return out
}
