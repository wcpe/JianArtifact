package domain

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/auditctx"
	"github.com/wcpe/jianartifact/apps/server/internal/blobstore"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

func securityAuditDirect(ip netip.Addr) context.Context {
	return auditctx.WithSource(context.Background(), auditctx.Source{
		RequestID: "request-from-server", PeerIP: ip, ClientIP: ip, ProxyTrust: auditctx.ProxyTrustDirect, SourceTrustVersion: "v1",
	})
}

func securityAuditWait(t *testing.T, ready func() bool) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for !ready() {
		select {
		case <-tick.C:
		case <-deadline.C:
			t.Fatal("未观察到预期的审计状态")
		}
	}
}

func TestSecurityAuditAdmissionBudgets(t *testing.T) {
	start := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	t.Run("滚动来源和全局配额", func(t *testing.T) {
		var q securityAuditBuffer
		for i := range 100 {
			ip := netip.AddrFrom4([4]byte{192, 0, 2, byte(i / 10)})
			if reason := q.offer(repository.SecurityAuditObservation{}, ip, start, 600); reason != "" {
				t.Fatalf("预算内被抑制：%s", reason)
			}
		}
		if reason := q.offer(repository.SecurityAuditObservation{}, netip.MustParseAddr("198.51.100.2"), start.Add(time.Second-time.Millisecond), 600); reason != "global_rate" {
			t.Fatalf("跨秒边界仍须限制滑窗：%s", reason)
		}
		if reason := q.offer(repository.SecurityAuditObservation{}, netip.MustParseAddr("192.0.2.0"), start.Add(time.Second), 600); reason != "source_rate" {
			t.Fatalf("来源预算未限制：%s", reason)
		}
		if reason := q.offer(repository.SecurityAuditObservation{}, netip.MustParseAddr("192.0.2.0"), start.Add(time.Minute), 600); reason != "" {
			t.Fatalf("到期来源额度未回收：%s", reason)
		}
	})
	t.Run("来源表满不开新键且可回收", func(t *testing.T) {
		var q securityAuditBuffer
		for i := range 4096 {
			ip := netip.AddrFrom4([4]byte{10, byte(i >> 8), byte(i), 1})
			if reason := q.offer(repository.SecurityAuditObservation{}, ip, start.Add(time.Duration(i/100)*time.Second), 600); reason != "" {
				t.Fatalf("第 %d 个来源应入队：%s", i, reason)
			}
			q.release(q.take(1))
		}
		for i := range 200 {
			ip := netip.AddrFrom4([4]byte{198, 51, 100, byte(i)})
			if reason := q.offer(repository.SecurityAuditObservation{}, ip, start.Add(41*time.Second), 600); reason != "source_table_full" {
				t.Fatalf("来源表超限：%s", reason)
			}
		}
		if len(q.sources) != 4096 {
			t.Fatalf("来源表越界：%d", len(q.sources))
		}
		if reason := q.offer(repository.SecurityAuditObservation{}, netip.MustParseAddr("198.51.100.1"), start.Add(time.Minute), 600); reason != "" || len(q.sources) > 4096 {
			t.Fatalf("未回收到期来源：%s，%d", reason, len(q.sources))
		}
	})
	for _, tc := range []struct {
		name         string
		bytes, limit int
	}{{"条数", 512, 1024}, {"字节", 4096, 512}} {
		t.Run(tc.name, func(t *testing.T) {
			var q securityAuditBuffer
			for i := range tc.limit {
				ip := netip.AddrFrom4([4]byte{10, 0, byte(i >> 8), byte(i)})
				if reason := q.offer(repository.SecurityAuditObservation{}, ip, start.Add(time.Duration(i/100)*time.Second), tc.bytes); reason != "" {
					t.Fatalf("第 %d 条提前丢弃：%s", i, reason)
				}
			}
			flight := q.take(50)
			if reason := q.offer(repository.SecurityAuditObservation{}, netip.MustParseAddr("192.0.2.12"), start.Add(15*time.Second), tc.bytes); reason != "queue_full" {
				t.Fatalf("在途批次绕过预算：%s", reason)
			}
			q.release(flight)
			if reason := q.offer(repository.SecurityAuditObservation{}, netip.MustParseAddr("192.0.2.12"), start.Add(15*time.Second), tc.bytes); reason != "" {
				t.Fatalf("确认落库后未释放预算：%s", reason)
			}
		})
	}
}

func TestSecurityAuditGapBufferCompactsLongOutage(t *testing.T) {
	var gaps securityAuditGaps
	start := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	for i := range 3000 {
		gaps.add(start.Add(time.Duration(i)*time.Minute), "queue_full")
	}
	got := gaps.take(65)
	if len(got) > 65 || len(got) < 2 || got[0].Reason != "counter_overflow" || got[0].ObservedCount != nil || got[0].SuppressedCount != nil || got[0].Start != start {
		t.Fatalf("长故障的内存汇总或未知缺口不正确：记录数=%d，首项=%+v", len(got), got[0])
	}
	if got[len(got)-1].SuppressedCount == nil || *got[len(got)-1].SuppressedCount != 1 {
		t.Fatalf("最近计数未保留：%+v", got[len(got)-1])
	}
}

func TestSecurityAuditWorkerFailureRecoveryAndUncleanRestart(t *testing.T) {
	db := mutationTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := NewSecurityAuditService(db)
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close(context.Background()) }()
	securityAuditWait(t, func() bool { return !s.Health().LastSuccess.IsZero() })
	if _, err := db.Exec(`CREATE TRIGGER audit_deny_test BEFORE INSERT ON security_event BEGIN SELECT RAISE(ABORT, '异常原文不应出现在健康'); END`); err != nil {
		t.Fatal(err)
	}
	peer := netip.MustParseAddr("192.0.2.44")
	request := auditctx.WithSource(context.Background(), auditctx.Source{RequestID: "request-from-server", PeerIP: peer, ProxyTrust: auditctx.ProxyTrustInvalidForwarding})
	request = WithSecurityAuditIdentity(request, SecurityAuditIdentity{ActorUserID: 42, AuthMethod: "password", AuthStrength: "password"})
	for range 12 {
		s.RecordDenied(request, SecurityAuditDenied{EventType: "auth.attempt", ActorUserID: 42, AuthMethod: "password", ErrorCode: "invalid_credentials", HTTPStatus: 401})
	}
	securityAuditWait(t, func() bool { return s.Health().Failure == "storage_unavailable" })
	if got := s.Health(); got.Coverage != "unknown" || got.Watermark != 0 || got.Queued != 10 || got.SuppressedCount != 2 || strings.Contains(fmt.Sprintf("%+v", got), "192.0.2.44") {
		t.Fatalf("故障健康不诚实或暴露来源：%+v", got)
	}
	if _, err := db.Exec(`DROP TRIGGER audit_deny_test`); err != nil {
		t.Fatal(err)
	}
	securityAuditWait(t, func() bool { return s.Health().Watermark != 0 && s.Health().Queued == 0 })
	var saved int
	if err := db.Get(&saved, `SELECT count(*) FROM security_event WHERE event_type='auth.attempt' AND request_id='request-from-server' AND client_ip_family=0 AND length(client_ip)=0 AND actor_user_id IS NULL AND actor_kind='unknown' AND proxy_trust='invalid_forwarding' AND auth_method='password'`); err != nil || saved != 10 {
		t.Fatalf("拒绝事实未精确恢复或伪造身份：数量=%d，错误=%v", saved, err)
	}
	if got := s.Health(); got.Coverage != "gapped" || got.LastGap.Reason != "source_rate" {
		t.Fatalf("恢复后未给出有证抑制缺口：%+v", got)
	}
	cancel()
	securityAuditWait(t, func() bool { return s.Health().State == "stopped" })
	if err := s.Close(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatalf("意外停止不能宣称清洁：%v", err)
	}
	restarted := NewSecurityAuditService(db)
	if err := restarted.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = restarted.Close(context.Background()) }()
	securityAuditWait(t, func() bool { return restarted.Health().LastGap.Reason == "unclean_restart" })
	if got := restarted.Health(); got.Coverage != "unknown" || got.LastGap.ObservedCount != nil || got.LastGap.SuppressedCount != nil || got.Watermark == 0 {
		t.Fatalf("重启捏造缺失计数或丢失水位：%+v", got)
	}
	if err := restarted.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestSecurityAuditAuthSuccessRequiresVerifiedActor(t *testing.T) {
	db := mutationTestDB(t)
	s := NewSecurityAuditService(db)
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx := securityAuditDirect(netip.MustParseAddr("192.0.2.54"))
	ctx = WithSecurityAuditIdentity(ctx, SecurityAuditIdentity{ActorUserID: 42, AuthMethod: "password", AuthStrength: "mfa"})
	s.RecordAuthAttemptSucceeded(ctx, 0, "password", 200)
	s.RecordAuthAttemptSucceeded(ctx, 42, "", 200)
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	var total, verified int
	if err := db.Get(&total, `SELECT count(*) FROM security_event WHERE event_type='auth.attempt'`); err != nil {
		t.Fatal(err)
	}
	if err := db.Get(&verified, `SELECT count(*) FROM security_event WHERE event_type='auth.attempt' AND actor_user_id=42 AND auth_strength='mfa' AND committed='not_applicable'`); err != nil {
		t.Fatal(err)
	}
	if total != 1 || verified != 1 || s.Health().LastGap.Reason != "invalid_fact" {
		t.Fatalf("认证成功必须来自已验证主体，也不得冒充持久会话：%d/%d，%+v", verified, total, s.Health())
	}
}

func TestSecurityAuditAuthFailureDoesNotAttributeIdentity(t *testing.T) {
	db := mutationTestDB(t)
	s := NewSecurityAuditService(db)
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	ip := netip.MustParseAddr("192.0.2.55")
	ctx := WithSecurityAuditIdentity(securityAuditDirect(ip), SecurityAuditIdentity{
		ActorUserID: 42, AuthMethod: "password", AuthStrength: "mfa",
		CredentialID: "test-credential", SessionID: "test-session",
	})
	s.RecordAuthAttemptFailed(ctx, "password", 500)
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	page, err := repository.NewSecurityEventRepo(db).List(context.Background(), repository.SecurityEventFilter{
		From: now.Add(-time.Minute), To: now.Add(time.Minute), EventType: "auth.attempt",
	}, nil)
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("应持久化一条认证服务故障：条数=%d，错误=%v", len(page.Items), err)
	}
	got := page.Items[0]
	if got.Outcome != "failed" || got.ErrorCode != "internal_error" || got.Committed != "not_applicable" ||
		got.HTTPStatus == nil || *got.HTTPStatus != 500 || got.AuthMethod != "password" || got.ActorKind != "unknown" ||
		got.ActorUserID != nil || got.ActorNameSnapshot != "" || got.CredentialID != "" || got.SessionID != "" || got.AuthStrength != "" ||
		got.ClientIP != ip || got.RequestID != "request-from-server" {
		t.Fatalf("认证服务故障应保留请求事实但不关联主体：%+v", got)
	}
}

func TestSecurityAuditCloseDeadlineLeavesUnknownCoverage(t *testing.T) {
	db := mutationTestDB(t)
	s := NewSecurityAuditService(db)
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	securityAuditWait(t, func() bool { return !s.Health().LastSuccess.IsZero() })
	checkpoint := s.Health().CoveredThrough
	if _, err := db.Exec(`CREATE TRIGGER audit_cancel_test BEFORE INSERT ON security_event BEGIN SELECT RAISE(ABORT, '写入失败'); END`); err != nil {
		t.Fatal(err)
	}
	s.RecordDenied(context.Background(), SecurityAuditDenied{EventType: "protocol.write_denied", ErrorCode: "forbidden", HTTPStatus: 403})
	securityAuditWait(t, func() bool { return s.Health().Failure == "storage_unavailable" })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := s.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("不可靠的收尾必须返回截止错误：%v", err)
	}
	if got := s.Health(); got.State != "stopped" || got.Coverage != "unknown" || got.Queued != 0 || !got.CoveredThrough.Equal(checkpoint) || got.Watermark != 0 {
		t.Fatalf("关闭后未释放工人/队列，或为未保存的事件推进了持久水位：%+v", got)
	}
	if err := s.Start(context.Background()); err == nil {
		t.Fatal("同一实例不应启动第二个工人")
	}
}

func TestSecurityAuditCloseWaitsForWorkerCleanup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := NewSecurityAuditService(nil)
		run, stop := context.WithCancel(context.Background())
		s.started, s.cancel = true, stop
		releaseCleanup := make(chan struct{})
		go func() {
			<-run.Done()
			<-releaseCleanup
			s.finishWorker(run)
		}()
		ctx, cancel := context.WithDeadline(context.Background(), time.Now())
		defer cancel()
		closed := make(chan error, 1)
		go func() { closed <- s.Close(ctx) }()
		synctest.Wait()
		if len(closed) != 0 {
			t.Error("排空到期不得在工人释放资源前返回")
		}
		close(releaseCleanup)
		if err := <-closed; !errors.Is(err, context.DeadlineExceeded) || s.Health().State != "stopped" {
			t.Fatalf("应保留原始截止错误并确认工人已经退出：%v，%+v", err, s.Health())
		}
	})
}

func TestSecurityAuditClosePreservesConfirmedDrain(t *testing.T) {
	db := mutationTestDB(t)
	s := NewSecurityAuditService(db)
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now())
	defer cancel()
	for range 16 {
		if err := s.Close(ctx); err != nil {
			t.Fatalf("排空已经确认，不得因同时就绪的截止信号而失败：%v", err)
		}
	}
}

func TestSecurityAuditOpenFailureThenFreshBaseline(t *testing.T) {
	db := mutationTestDB(t)
	if _, err := db.Exec(`CREATE TRIGGER audit_open_fail BEFORE INSERT ON security_audit_checkpoint BEGIN SELECT RAISE(ABORT, '首次打开失败'); END`); err != nil {
		t.Fatal(err)
	}
	run, cancel := context.WithCancel(context.Background())
	defer cancel()
	lost := NewSecurityAuditService(db)
	if err := lost.Start(run); err != nil {
		t.Fatal(err)
	}
	lost.RecordDenied(context.Background(), SecurityAuditDenied{EventType: "protocol.write_denied", ErrorCode: "forbidden", HTTPStatus: 403})
	securityAuditWait(t, func() bool { return lost.Health().Failure == "storage_unavailable" })
	cancel()
	if err := lost.Close(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatalf("首次检查点未成功的退出不能确认为排空：%v", err)
	}
	var checkpoints, facts int
	if err := db.Get(&checkpoints, `SELECT count(*) FROM security_audit_checkpoint`); err != nil || checkpoints != 0 {
		t.Fatalf("失败的打开不能留下检查点：%d，%v", checkpoints, err)
	}
	if err := db.Get(&facts, `SELECT count(*) FROM security_event`); err != nil || facts != 0 {
		t.Fatalf("前一工人未持久化的事实不可恢复：%d，%v", facts, err)
	}
	if _, err := db.Exec(`DROP TRIGGER audit_open_fail`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TRIGGER audit_heartbeat_fail BEFORE UPDATE ON security_audit_checkpoint BEGIN SELECT RAISE(ABORT, '阻挡后续心跳'); END`); err != nil {
		t.Fatal(err)
	}
	fresh := NewSecurityAuditService(db)
	restart := time.Now().Truncate(time.Millisecond)
	if err := fresh.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = fresh.Close(ctx)
	}()
	securityAuditWait(t, func() bool { return !fresh.Health().LastSuccess.IsZero() })
	got := fresh.Health()
	if got.Coverage != "unknown" || got.ObservedCount != 0 || got.SuppressedCount != 0 || got.LastGap.Reason != "" ||
		got.CoveredFrom.Before(restart) || !got.CoveredThrough.Equal(got.CoveredFrom) {
		t.Fatalf("空时间段不得标为完整覆盖，不能猜测失联前的窗口与计数：%+v", got)
	}
	if _, err := db.Exec(`DROP TRIGGER audit_heartbeat_fail`); err != nil {
		t.Fatal(err)
	}
	securityAuditWait(t, func() bool { return fresh.Health().Coverage == "complete" })
	if latest := fresh.Health(); !latest.CoveredFrom.Equal(got.CoveredFrom) || !latest.CoveredThrough.After(latest.CoveredFrom) {
		t.Fatalf("只有新基线之后非空区间获证：%+v", latest)
	}
}

func TestSecurityAuditAssetHookAtomicAndCleanup(t *testing.T) {
	db := mutationTestDB(t)
	repoID, err := repository.NewRepoRepo(db).Create("audit-raw", "raw", "hosted", "private", "")
	if err != nil {
		t.Fatal(err)
	}
	assets := repository.NewAssetRepo(db)
	blobs := blobstore.NewStore(t.TempDir())
	hash, sha1sum, md5sum, size, err := blobs.Put(bytes.NewReader([]byte("审计提交后的清理")))
	if err != nil {
		t.Fatal(err)
	}
	if err := assets.Upsert(repoID, "objects/a.bin", hash, size, "application/octet-stream", sha1sum, md5sum); err != nil {
		t.Fatal(err)
	}
	asset, err := assets.GetByPath(repoID, "objects/a.bin")
	if err != nil {
		t.Fatal(err)
	}
	c, err := NewAssetMutationCoordinator(db, blobs)
	if err != nil {
		t.Fatal(err)
	}
	s := NewSecurityAuditService(db)
	request := securityAuditDirect(netip.MustParseAddr("203.0.113.42"))
	src, _ := auditctx.SourceFrom(request)
	snapshot, ok := src.Snapshot()
	if !ok {
		t.Fatal("无法创建可信发起快照")
	}
	worker := securityAuditDirect(netip.MustParseAddr("198.51.100.77"))
	operation := NewOperationID()
	items := []repository.AssetMutationItem{{RepositoryID: repoID, Path: asset.Path, Before: asset}}
	envelope, err := assetOperationEnvelope("audit-raw", operation, "audit-test-node", repository.OperationActor{}, items)
	if err != nil {
		t.Fatal(err)
	}
	count := func(table, predicate string) int {
		t.Helper()
		var n int
		if err := db.Get(&n, `SELECT count(*) FROM `+table+` WHERE `+predicate); err != nil {
			t.Fatal(err)
		}
		return n
	}
	for _, table := range []string{"security_event", "security_event_object"} {
		if _, err := db.Exec(`CREATE TRIGGER audit_abort BEFORE INSERT ON ` + table + ` BEGIN SELECT RAISE(ABORT, '注入审计写失败'); END`); err != nil {
			t.Fatal(err)
		}
		rejected := NewOperationID()
		attempt := envelope
		attempt.OperationID = rejected
		err := c.ApplyWithOutboxAndHook(items, attempt, s.CommitHookFromSnapshot(worker, snapshot, rejected, 42, *asset))
		if !errors.Is(err, repository.ErrSecurityEventUnavailable) {
			t.Fatalf("%s 写失败不应提交：%v", table, err)
		}
		if restored, err := assets.GetByPath(repoID, asset.Path); err != nil || restored.ID != asset.ID || !blobs.Exists(hash) ||
			count("security_event", "1=1") != 0 || count("security_event_object", "1=1") != 0 || count("replication_operation_outbox", "1=1") != 0 {
			t.Fatalf("%s 失败没有一起回滚：%v", table, err)
		}
		if _, err := db.Exec(`DROP TRIGGER audit_abort`); err != nil {
			t.Fatal(err)
		}
	}
	c.finalizeSnapshot = func(blobstore.RollbackSnapshot) error { return errors.New("注入提交后清理失败") }
	if err := c.ApplyWithOutboxAndHook(items, envelope, s.CommitHookFromSnapshot(worker, snapshot, operation, 42, *asset)); err != nil {
		t.Fatalf("提交后清理失败不应反转成功：%v", err)
	}
	if _, err := assets.GetByPath(repoID, asset.Path); err == nil || blobs.Exists(hash) || count("replication_operation_outbox", "1=1") != 1 {
		t.Fatalf("业务和 outbox 未提交：%v", err)
	}
	var found int
	err = db.Get(&found, `SELECT count(*) FROM security_event e JOIN security_event_object o ON o.event_sequence=e.sequence
		WHERE e.operation_id=? AND e.event_type='asset.change_committed' AND e.committed='committed' AND e.http_status IS NULL
		AND e.request_id='' AND e.origin_request_id='request-from-server' AND e.actor_user_id=42 AND e.auth_method=''
		AND e.peer_ip_family=0 AND length(e.peer_ip)=0 AND e.client_ip=X'CB00712A' AND o.canonical_path=? AND o.object_id=? AND o.digest=?`,
		operation, asset.Path, fmt.Sprint(asset.ID), "sha256:"+hash)
	if err != nil || found != 1 {
		t.Fatalf("已提交事件未绑定真实操作和对象或采了执行者来源：%d，%v", found, err)
	}
	c.finalizeSnapshot = blobs.FinalizeRollbackSnapshot
	if err := c.Recover(); err != nil || count("security_event", "1=1") != 1 {
		t.Fatalf("清理重试不能重放成功事件：%v", err)
	}
}
