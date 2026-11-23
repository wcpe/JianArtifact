package repository

import (
	"context"
	"errors"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
)

func openSecurityEventDB(t *testing.T) *persistence.DB {
	t.Helper()
	db, err := persistence.Open(filepath.Join(t.TempDir(), "security-event.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatal(err)
	}
	return db
}

func securityEventFixture(ip string) SecurityEvent {
	e := SecurityEvent{
		EventID: NewSecurityEventID(), EventType: "auth.attempt",
		OccurredAt: time.Date(2026, time.May, 30, 10, 0, 0, 123456789, time.UTC),
		ActorKind:  "unknown", Outcome: "denied", Committed: "not_applicable", ProxyTrust: "unknown",
		AuthMethod: "password", AuthStrength: "none", ErrorCode: "invalid_credentials",
	}
	if ip != "" {
		e.PeerIP = netip.MustParseAddr(ip)
		e.ClientIP = e.PeerIP
		e.ProxyTrust = "direct"
	}
	return e
}

func securityEventAssetFixture() (SecurityEvent, []SecurityEventObject) {
	e := securityEventFixture("192.0.2.1")
	e.EventType, e.Outcome, e.Committed, e.ErrorCode = "asset.change_committed", "succeeded", "committed", ""
	e.OperationID = NewSecurityEventID()
	return e, []SecurityEventObject{{ObjectKind: "asset", CanonicalPath: "pkg/版本.tar", Digest: "sha256:" + strings.Repeat("a", 64)}}
}

func securityEventTx(t *testing.T, db *persistence.DB) *sqlx.Tx {
	t.Helper()
	tx, err := db.BeginTxx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	return tx
}

func writeSecurityEvent(t *testing.T, db *persistence.DB, repo *SecurityEventRepo, e SecurityEvent, objects []SecurityEventObject) {
	t.Helper()
	tx := securityEventTx(t, db)
	if err := repo.InsertTx(context.Background(), tx, e, objects); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestSecurityEventTransactionAndHistory(t *testing.T) {
	db := openSecurityEventDB(t)
	repo := NewSecurityEventRepo(db)
	ctx := context.Background()
	user, err := db.Exec(`INSERT INTO user(username,password_hash) VALUES ('security-event-user','test')`)
	if err != nil {
		t.Fatal(err)
	}
	id, err := user.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	result, err := db.Exec(`INSERT INTO repository(name,format,type) VALUES ('event-history','raw','hosted')`)
	if err != nil {
		t.Fatal(err)
	}
	rid, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	e, objects := securityEventAssetFixture()
	e.ActorKind, e.ActorUserID, e.ActorNameSnapshot, e.RepositoryID = "user", &id, "security-event-user", &rid
	objects[0].RepositoryID = &rid
	objects = append(objects, SecurityEventObject{ObjectKind: "asset", CanonicalPath: "pkg/second.tar"})
	tx := securityEventTx(t, db)
	if _, err := tx.Exec(`UPDATE repository SET name='renamed-history' WHERE id=?`, rid); err != nil {
		t.Fatal(err)
	}
	if err := repo.InsertTx(ctx, tx, e, objects); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Get(ctx, e.EventID); !errors.Is(err, ErrSecurityEventNotFound) {
		t.Fatalf("未提交事件不应被另一连接读取：%v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{`DELETE FROM user WHERE id=?`, `DELETE FROM repository WHERE id=?`} {
		value := rid
		if strings.Contains(query, "FROM user") {
			value = id
		}
		if _, err := db.Exec(query, value); err != nil {
			t.Fatal(err)
		}
	}
	got, err := repo.Get(ctx, e.EventID)
	if err != nil || got.Sequence <= 0 || got.EventVersion != 1 || got.HTTPStatus != nil || got.Committed != "committed" || got.IngestedAt.IsZero() || !got.OccurredAt.Equal(e.OccurredAt.Truncate(time.Millisecond)) || got.ActorUserID == nil || *got.ActorUserID != id || got.RepositoryID == nil || *got.RepositoryID != rid {
		t.Fatalf("历史事件或回执边界错误：%+v，%v", got, err)
	}
	page, err := repo.ListObjects(ctx, e.EventID, 0, 1)
	if err != nil || !page.HasMore || len(page.Items) != 1 || page.Items[0].RepositoryID == nil || *page.Items[0].RepositoryID != rid || page.Items[0].CanonicalPath != objects[0].CanonicalPath {
		t.Fatalf("历史对象第一页：%+v，%v", page, err)
	}
	page, err = repo.ListObjects(ctx, e.EventID, page.NextOrdinal, 1)
	if err != nil || page.HasMore || len(page.Items) != 1 || page.Items[0].CanonicalPath != objects[1].CanonicalPath || page.NextOrdinal != 0 {
		t.Fatalf("历史对象末页：%+v，%v", page, err)
	}
	replay := e
	replay.EventID = NewSecurityEventID()
	tx = securityEventTx(t, db)
	if err := repo.InsertTx(ctx, tx, replay, objects); !errors.Is(err, ErrSecurityEventConflict) {
		t.Fatalf("同操作成功重写须返回冲突，得到 %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Get(ctx, replay.EventID); !errors.Is(err, ErrSecurityEventNotFound) {
		t.Fatalf("不应留下重复事件：%v", err)
	}
	status := 202
	replay.EventType, replay.Committed, replay.HTTPStatus = "asset.response_observed", "unknown", &status
	writeSecurityEvent(t, db, repo, replay, nil)
	observed, err := repo.Get(ctx, replay.EventID)
	if err != nil || observed.HTTPStatus == nil || *observed.HTTPStatus != status || observed.Committed != "unknown" {
		t.Fatalf("响应观察不能改写回执：%+v，%v", observed, err)
	}
}

func TestSecurityEventFailuresRollBackBusiness(t *testing.T) {
	db := openSecurityEventDB(t)
	repo := NewSecurityEventRepo(db)
	ctx := context.Background()
	const faultMarker = "internal_object_storage_failure"
	if _, err := db.Exec(`CREATE TRIGGER inject_object_failure BEFORE INSERT ON security_event_object
		WHEN NEW.ordinal = 2 BEGIN SELECT RAISE(ABORT, 'internal_object_storage_failure'); END`); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name       string
		secondPath string
		want       error
	}{{"白名单拒绝", "pkg/file?bad=value", ErrInvalidSecurityEvent}, {"明细写失败", "pkg/second.tar", ErrSecurityEventUnavailable}} {
		t.Run(tc.name, func(t *testing.T) {
			e, objects := securityEventAssetFixture()
			objects = append(objects, SecurityEventObject{ObjectKind: "asset", CanonicalPath: tc.secondPath})
			tx := securityEventTx(t, db)
			if _, err := tx.Exec(`INSERT INTO repository(name,format,type) VALUES ('rollback-business','raw','hosted')`); err != nil {
				t.Fatal(err)
			}
			err := repo.InsertTx(ctx, tx, e, objects)
			if !errors.Is(err, tc.want) || strings.Contains(err.Error(), faultMarker) || strings.Contains(err.Error(), tc.secondPath) {
				t.Fatalf("预期脱敏错误 %v，得到 %v", tc.want, err)
			}
			var partial int
			if err := tx.Get(&partial, `SELECT COUNT(*) FROM security_event WHERE event_id=?`, e.EventID); err != nil || partial != 0 {
				t.Fatalf("仓储不应保留残缺事件：%d，%v", partial, err)
			}
			if err := tx.Rollback(); err != nil {
				t.Fatal(err)
			}
			if err := db.Get(&partial, `SELECT COUNT(*) FROM repository WHERE name='rollback-business'`); err != nil || partial != 0 {
				t.Fatalf("调用方应回滚业务：%d，%v", partial, err)
			}
		})
	}
}

func TestSecurityEventOptionalDisplayNormalization(t *testing.T) {
	for _, tc := range []struct {
		value            string
		wantUA, wantName string
	}{
		{"构建工具/1.0", "构建工具/1.0", "构建工具/1.0"},
		{strings.Repeat("甲", 43), strings.Repeat("甲", 43), ""},
		{strings.Repeat("甲", 86), "", ""},
		{"Authorization: Bearer syntheticvalue", "", ""},
		{"client\u202E spoof", "", ""},
		{string([]byte{0xff}), "", ""},
	} {
		if got := SecurityEventUserAgent(tc.value); got != tc.wantUA {
			t.Errorf("UA 归一结果长度 %d，预期 %d", len(got), len(tc.wantUA))
		}
		if got := SecurityEventActorNameSnapshot(tc.value); got != tc.wantName || SecurityEventRepositoryNameSnapshot(tc.value) != tc.wantName {
			t.Errorf("名称归一结果长度 %d，预期 %d", len(got), len(tc.wantName))
		}
	}
}

func TestSecurityEventCanonicalAssetPathRoundTrip(t *testing.T) {
	db := openSecurityEventDB(t)
	assets, events := NewAssetRepo(db), NewSecurityEventRepo(db)
	ctx := context.Background()
	result, err := db.Exec(`INSERT INTO repository(name,format,type) VALUES ('canonical-paths','raw','hosted')`)
	if err != nil {
		t.Fatal(err)
	}
	repoID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, path string }{
		{"关键词空格和中文", "docs/Basic 使用指南.txt"},
		{"合法的凭据提示字样", "pkg/keyjson/jat_notes.tgz"},
		{"多字节路径恰好1024字节", "pkg/" + strings.Repeat("版", 340)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := tc.path
			e, objects := securityEventAssetFixture()
			if err := assets.Upsert(repoID, path, strings.TrimPrefix(objects[0].Digest, "sha256:"), 8, "text/plain", "", ""); err != nil {
				t.Fatal(err)
			}
			stored, err := assets.GetByPath(repoID, path)
			if err != nil || stored.Path != path {
				t.Fatalf("资产权威路径往返失败：%v", err)
			}
			objects[0].CanonicalPath, objects[0].RepositoryID = stored.Path, &repoID
			writeSecurityEvent(t, db, events, e, objects)
			page, err := events.ListObjects(ctx, e.EventID, 0, 1)
			if err != nil || len(page.Items) != 1 || page.Items[0].CanonicalPath != stored.Path {
				t.Fatalf("安全事件明细须原样保存路径：%v", err)
			}
			filter := SecurityEventFilter{From: e.OccurredAt.Add(-time.Minute), To: e.OccurredAt.Add(time.Minute), CanonicalPath: stored.Path, RepositoryID: &repoID}
			matches, err := events.List(ctx, filter, nil)
			if err != nil || len(matches.Items) != 1 || matches.Items[0].EventID != e.EventID {
				t.Fatalf("内部精确查询须命中同一权威路径：%v", err)
			}
		})
	}
}

func TestSecurityEventValidationAndBudgets(t *testing.T) {
	db := openSecurityEventDB(t)
	repo := NewSecurityEventRepo(db)
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		mutate func(*SecurityEvent, *[]SecurityEventObject)
	}{
		{"非枚举错误", func(e *SecurityEvent, _ *[]SecurityEventObject) { e.ErrorCode = "application_exception" }},
		{"非法版本", func(e *SecurityEvent, _ *[]SecurityEventObject) { e.EventVersion = 2 }},
		{"提交不能冒充响应", func(e *SecurityEvent, _ *[]SecurityEventObject) { status := 200; e.HTTPStatus = &status }},
		{"提交不可缺对象", func(_ *SecurityEvent, o *[]SecurityEventObject) { *o = nil }},
		{"UA 非法 UTF8", func(e *SecurityEvent, _ *[]SecurityEventObject) { e.UserAgent = string([]byte{0xff}) }},
		{"UA 超字节", func(e *SecurityEvent, _ *[]SecurityEventObject) { e.UserAgent = strings.Repeat("甲", 86) }},
		{"控制字符", func(e *SecurityEvent, _ *[]SecurityEventObject) { e.UserAgent = "client\r\nspoof" }},
		{"路径不截断", func(_ *SecurityEvent, o *[]SecurityEventObject) { (*o)[0].CanonicalPath = strings.Repeat("a", 1025) }},
		{"路径非法 UTF8", func(_ *SecurityEvent, o *[]SecurityEventObject) {
			(*o)[0].CanonicalPath = "pkg/" + string([]byte{0xff})
		}},
		{"路径控制字符", func(_ *SecurityEvent, o *[]SecurityEventObject) { (*o)[0].CanonicalPath = "pkg/file\nname" }},
		{"路径格式字符", func(_ *SecurityEvent, o *[]SecurityEventObject) { (*o)[0].CanonicalPath = "pkg/file\u202ename" }},
		{"路径不接受空段", func(_ *SecurityEvent, o *[]SecurityEventObject) { (*o)[0].CanonicalPath = "pkg//file" }},
		{"路径不接受点段", func(_ *SecurityEvent, o *[]SecurityEventObject) { (*o)[0].CanonicalPath = "pkg/../file" }},
		{"凭据标识禁止秘密载体", func(e *SecurityEvent, _ *[]SecurityEventObject) { e.CredentialID = "jat_syntheticvalue" }},
		{"会话标识禁止 JWT", func(e *SecurityEvent, _ *[]SecurityEventObject) { e.SessionID = "header.payload.signature" }},
		{"不接受凭据摘要作为标识", func(e *SecurityEvent, _ *[]SecurityEventObject) { e.CredentialID = strings.Repeat("a", 64) }},
		{"禁止原始头", func(e *SecurityEvent, _ *[]SecurityEventObject) { e.UserAgent = "Authorization: Bearer syntheticvalue" }},
		{"对象数超限", func(_ *SecurityEvent, o *[]SecurityEventObject) { *o = make([]SecurityEventObject, 257) }},
		{"总预算独立生效", func(_ *SecurityEvent, o *[]SecurityEventObject) {
			v := SecurityEventObject{ObjectKind: "asset", CanonicalPath: strings.Repeat("a", 1024)}
			*o = make([]SecurityEventObject, 256)
			for i := range *o {
				(*o)[i] = v
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, objects := securityEventAssetFixture()
			tc.mutate(&e, &objects)
			tx := securityEventTx(t, db)
			defer func() { _ = tx.Rollback() }()
			if err := repo.InsertTx(ctx, tx, e, objects); !errors.Is(err, ErrInvalidSecurityEvent) {
				t.Fatalf("不合规输入必须拒绝：%v", err)
			}
		})
	}
}

func TestSecurityEventIPAndSnapshotList(t *testing.T) {
	db := openSecurityEventDB(t)
	repo := NewSecurityEventRepo(db)
	ctx := context.Background()
	var events []SecurityEvent
	for _, ip := range []string{"192.0.2.0", "2001:db8::1", "::ffff:192.0.2.255", "192.0.3.0"} {
		e := securityEventFixture(ip)
		writeSecurityEvent(t, db, repo, e, nil)
		events = append(events, e)
	}
	trusted := securityEventFixture("203.0.113.5")
	trusted.ProxyTrust = "trusted"
	trusted.SourceTrustVersion = "config-version"
	trusted.PeerIP = netip.MustParseAddr("2001:db8:1::1")
	trusted.ForwardedChain = []netip.Addr{netip.MustParseAddr("::ffff:203.0.113.5"), netip.MustParseAddr("2001:db8:2::1")}
	// 独立时间窗验证地址链编码，避免与后续分页样本混合。
	trusted.OccurredAt = trusted.OccurredAt.Add(-2 * time.Hour)
	writeSecurityEvent(t, db, repo, trusted, nil)
	forwarded, err := repo.Get(ctx, trusted.EventID)
	if err != nil || len(forwarded.ForwardedChain) != 2 || forwarded.ForwardedChain[0] != trusted.ClientIP || forwarded.ForwardedChain[1] != trusted.ForwardedChain[1] || forwarded.PeerIP != trusted.PeerIP {
		t.Fatalf("转发链须保存规范地址及顺序：%+v，%v", forwarded, err)
	}
	base := SecurityEventFilter{From: events[0].OccurredAt.Add(-time.Hour), To: events[0].OccurredAt.Add(time.Hour)}
	for _, tc := range []struct {
		ip, cidr string
		want     int
	}{{"::ffff:192.0.2.255", "", 1}, {"", "192.0.2.0/24", 2}, {"", "::ffff:192.0.2.50/120", 2}, {"", "2001:db8::/32", 1}, {"", "::/0", 1}, {"", "0.0.0.0/0", 3}} {
		f := base
		f.IP, f.CIDR = tc.ip, tc.cidr
		page, err := repo.List(ctx, f, nil)
		if err != nil || len(page.Items) != tc.want {
			t.Fatalf("精确 IP/CIDR %q %q：条数 %d，错误 %v", tc.ip, tc.cidr, len(page.Items), err)
		}
	}
	row, err := repo.Get(ctx, events[2].EventID)
	if err != nil || row.ClientIP != netip.MustParseAddr("192.0.2.255") {
		t.Fatalf("IPv4 映射须归一：%+v，%v", row.ClientIP, err)
	}
	for _, invalid := range []SecurityEventFilter{
		{From: base.From, To: base.To, IP: "192.0.2.1", CIDR: "192.0.2.0/24"},
		{From: base.From, To: base.To, CIDR: "::ffff:192.0.2.0/95"},
		{From: base.From, To: base.To, IP: "fe80::1%eth0"},
		{From: base.From, To: base.To, Limit: 101},
		{From: base.From, To: base.From.Add(31 * 24 * time.Hour)},
	} {
		if _, err := repo.List(ctx, invalid, nil); !errors.Is(err, ErrInvalidSecurityEventFilter) {
			t.Fatalf("非法过滤器获得 %v", err)
		}
	}
	base.Limit = 2
	first, err := repo.List(ctx, base, nil)
	if err != nil || first.Next == nil || !first.HasMore || len(first.Items) != 2 || first.Items[0].EventID != events[3].EventID || first.Items[1].EventID != events[2].EventID {
		t.Fatalf("第一页快照：%+v，%v", first, err)
	}
	wrongFilter := base
	wrongFilter.CIDR = "192.0.2.0/24"
	if _, err := repo.List(ctx, wrongFilter, first.Next); !errors.Is(err, ErrInvalidSecurityEventFilter) {
		t.Fatalf("不得沿用其他查询的游标：%v", err)
	}
	late := securityEventFixture("192.0.2.5")
	late.OccurredAt = late.OccurredAt.Add(-time.Minute)
	writeSecurityEvent(t, db, repo, late, nil)
	last, err := repo.List(ctx, base, first.Next)
	if err != nil || last.SnapshotWatermark != first.SnapshotWatermark || last.Next != nil || last.HasMore || len(last.Items) != 2 || last.Items[0].EventID != events[1].EventID || last.Items[1].EventID != events[0].EventID {
		t.Fatalf("迟到记录不能混入快照或漏重：%+v，%v", last, err)
	}
	f := base
	f.CIDR = "192.0.2.0/24"
	index, conditions, params, err := securityEventConditions(f)
	if err != nil {
		t.Fatal(err)
	}
	query, args := securityEventListSQL(index, conditions, params, first.SnapshotWatermark, 0, f.Limit)
	rows, err := db.Queryx("EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var plan strings.Builder
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(detail + "\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.String(), "SEARCH") || !strings.Contains(plan.String(), "idx_security_event_client_ip") || !strings.Contains(plan.String(), "client_ip>?") {
		t.Fatalf("CIDR 必须命中二进制地址范围索引：%s", plan.String())
	}
	t.Logf("查询计划：%s", plan.String())
}

func TestSecurityEventFilterFingerprint(t *testing.T) {
	at := securityEventFixture("").OccurredAt.Truncate(time.Millisecond)
	base := SecurityEventFilter{From: at, To: at.Add(time.Minute), IP: "192.0.2.5", Limit: 1}
	setCIDR := func(f *SecurityEventFilter, value string) { f.IP, f.CIDR = "", value }
	fingerprint := func(f SecurityEventFilter) [32]byte {
		t.Helper()
		got, err := SecurityEventFilterFingerprint(f)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	want := fingerprint(base)
	equivalent := base
	equivalent.IP, equivalent.Limit = "::ffff:192.0.2.5", 100
	equivalent.From = at.In(time.FixedZone("测试时区", 8*3600)).Add(500 * time.Microsecond)
	equivalent.To = base.To.In(time.FixedZone("测试时区", 8*3600))
	if got := fingerprint(equivalent); got != want {
		t.Fatal("等价地址、时间及可变页大小不得改写指纹")
	}
	v6, expanded := base, base
	v6.IP, expanded.IP = "2001:db8::5", "2001:0db8:0:0:0:0:0:5"
	if fingerprint(v6) != fingerprint(expanded) {
		t.Fatal("等价 IPv6 表示必须得到同一指纹")
	}
	cidr, mappedCIDR := base, base
	setCIDR(&cidr, "192.0.2.5/24")
	setCIDR(&mappedCIDR, "::ffff:192.0.2.0/120")
	if fingerprint(cidr) != fingerprint(mappedCIDR) {
		t.Fatal("等价网段和地址掩码必须得到同一指纹")
	}
	for _, tc := range []struct {
		name   string
		change func(*SecurityEventFilter)
	}{
		{"地址值", func(f *SecurityEventFilter) { f.IP = "192.0.2.6" }},
		{"地址匹配方式", func(f *SecurityEventFilter) { setCIDR(f, "192.0.2.5/32") }},
		{"发生时间", func(f *SecurityEventFilter) { f.From = f.From.Add(time.Millisecond) }},
		{"事件结果", func(f *SecurityEventFilter) { f.Outcome = "denied" }},
		{"对象路径", func(f *SecurityEventFilter) { f.CanonicalPath = "pkg/file.tar" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := base
			tc.change(&f)
			if got := fingerprint(f); got == want {
				t.Fatal("改变查询条件必须改变指纹")
			}
		})
	}
	for _, tc := range []struct {
		name   string
		change func(*SecurityEventFilter)
	}{
		{"地址互斥", func(f *SecurityEventFilter) { f.CIDR = "192.0.2.0/24" }},
		{"非法地址", func(f *SecurityEventFilter) { f.IP = "fe80::1%link" }},
		{"非法网段", func(f *SecurityEventFilter) { setCIDR(f, "::ffff:192.0.2.0/95") }},
		{"空窗口", func(f *SecurityEventFilter) { f.To = f.From }},
		{"时间跨度超限", func(f *SecurityEventFilter) { f.To = f.From.Add(31 * 24 * time.Hour) }},
		{"页大小超限", func(f *SecurityEventFilter) { f.Limit = 101 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := base
			tc.change(&f)
			if _, err := SecurityEventFilterFingerprint(f); !errors.Is(err, ErrInvalidSecurityEventFilter) {
				t.Fatalf("无效筛选须返回既有哨兵：%v", err)
			}
		})
	}
}

func TestSecurityEventFilterFingerprintMatchesPagination(t *testing.T) {
	db := openSecurityEventDB(t)
	repo, ctx := NewSecurityEventRepo(db), context.Background()
	e := securityEventFixture("192.0.2.5")
	at := e.OccurredAt
	for range 3 {
		e.EventID = NewSecurityEventID()
		writeSecurityEvent(t, db, repo, e, nil)
	}
	filter := SecurityEventFilter{From: at.Add(-time.Minute), To: at.Add(time.Minute), IP: e.ClientIP.String(), Limit: 1}
	fp, err := SecurityEventFilterFingerprint(filter)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := repo.List(ctx, filter, nil)
	if err != nil || plain.Next == nil || plain.Next.FilterFingerprint != fp {
		t.Fatalf("普通分页首屏指纹不一致：%v", err)
	}
	first, err := repo.ListView(ctx, filter, nil, 0)
	if err != nil || first.Next == nil || first.Next.FilterFingerprint != fp {
		t.Fatalf("留存分页首屏指纹不一致：%v", err)
	}
	filter.IP, filter.Limit = "::ffff:192.0.2.5", 2
	continued, err := repo.ListView(ctx, filter, first.Next, first.RetentionGeneration)
	if err != nil || len(continued.Items) != 2 || continued.HasMore || continued.SnapshotWatermark != first.SnapshotWatermark {
		t.Fatalf("等价筛选与新页大小应沿用快照：%+v，%v", continued, err)
	}
	filter.Outcome = "denied"
	if _, err := repo.ListView(ctx, filter, first.Next, first.RetentionGeneration); !errors.Is(err, ErrInvalidSecurityEventFilter) {
		t.Fatalf("续页绑定必须拒绝不同筛选：%v", err)
	}
}
