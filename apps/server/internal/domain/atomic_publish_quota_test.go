package domain

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jmoiron/sqlx"
	"github.com/wcpe/jianartifact/apps/server/internal/blobstore"
	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

func TestAtomicPublishQuotaCompletionAndRecovery(t *testing.T) {
	for _, tc := range []struct {
		name               string
		committed, unknown bool
	}{
		{"提交报错但回执已完成", true, false},
		{"确认未提交", false, false},
		{"回执未知但实际提交", true, true},
		{"回执未知且实际回滚", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := migratedTestDBPath(t, t.TempDir(), "quota.db")
			root := t.TempDir()
			db, err := persistence.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			repos, assets, usage := repository.NewRepoRepo(db), repository.NewAssetRepo(db), repository.NewPublishPolicyRepo(db)
			user, err := repository.NewUserRepo(db).Create("quota-recovery-user", "test-hash", "user")
			if err != nil {
				t.Fatal(err)
			}
			repo, err := repos.Create("raw", "raw", "hosted", "private", "")
			if err != nil {
				t.Fatal(err)
			}
			policy := PublishPolicyService{policies: usage}
			id, err := usage.Reserve(repository.PublishPolicy{UserID: user, RepositoryID: repo, MaxAssetsHour: 2, MaxBytesDay: 100}, 1, 80)
			if err != nil {
				t.Fatal(err)
			}
			svc := NewAssetService(repos, assets, blobstore.NewStore(root), nil)
			coordinator := svc.MutationCoordinator()
			complete, read := coordinator.completeMutation, coordinator.readCompletionStatus
			var opID string
			var writes, reads int
			reportErr := errors.New("受控提交返回错误")
			coordinator.completeMutation = func(id string, items []repository.AssetMutationItem, envelope *repository.OperationEnvelope, hooks ...repository.MutationCompletionHook) error {
				opID = id
				writes++
				if tc.committed {
					if err := complete(id, items, envelope, hooks...); err != nil {
						t.Fatalf("真实完成事务：%v", err)
					}
				} else {
					hooks = append(hooks, func(*sqlx.Tx) error { return reportErr })
					if err := complete(id, items, envelope, hooks...); !errors.Is(err, reportErr) {
						t.Fatalf("未触发真实事务回滚：%v", err)
					}
				}
				// 只替换提交调用的报告，不伪造实际 SQLite 落盘状态。
				return errors.Join(repository.ErrAssetMutationCommitUnconfirmed, reportErr)
			}
			coordinator.readCompletionStatus = func(ctx context.Context, id string) (string, error) {
				reads++
				if id != opID || id == "" {
					t.Fatalf("未重查原操作：%q / %q", id, opID)
				}
				if tc.unknown {
					return "", context.DeadlineExceeded
				}
				return read(ctx, id)
			}
			const contents = "quota recovery payload"
			asset, returned, err := svc.PutWithReservedSecurityAudit(context.Background(), NewSecurityAuditService(db), policy.CommitReservation(id), user, "raw", "recovery.txt", strings.NewReader(contents), "text/plain")
			if writes != 1 || reads != 1 {
				t.Fatalf("不应重做提交：完成 %d，回读 %d", writes, reads)
			}
			switch {
			case tc.unknown:
				if asset != nil || returned != opID || !errors.Is(err, errAssetCommitUnknown) {
					t.Fatalf("未保留原未决操作：%q / %v", returned, err)
				}
			case tc.committed:
				if asset == nil || returned != opID || err != nil {
					t.Fatalf("已完成回执未被识别：%q / %v", returned, err)
				}
			default:
				if returned != "" || !errors.Is(err, reportErr) {
					t.Fatalf("已证未提交却未返回原失败：%q / %v", returned, err)
				}
				if err := policy.Settle(id, false); err != nil {
					t.Fatal(err)
				}
			}
			want := 0
			if tc.committed {
				want = 1
			}
			assertRecoveredQuota(t, db, user, repo, id, opID, want, len(contents), !tc.committed && tc.unknown)
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				func() {
					reopened, err := persistence.Open(path)
					if err != nil {
						t.Fatal(err)
					}
					defer func() { _ = reopened.Close() }()
					fresh := NewAssetService(repository.NewRepoRepo(reopened), repository.NewAssetRepo(reopened), blobstore.NewStore(root), nil)
					if err := fresh.RecoverMutationIntents(); err != nil {
						t.Fatal(err)
					}
					if _, err := repository.NewPublishPolicyRepo(reopened).ReleaseExpired(); err != nil {
						t.Fatal(err)
					}
					assertRecoveredQuota(t, reopened, user, repo, id, opID, want, len(contents), false)
				}()
			}
		})
	}
}

func TestAtomicPublishQuotaReservationScopeAndSingleUse(t *testing.T) {
	db := mutationTestDB(t)
	repos, assets := repository.NewRepoRepo(db), repository.NewAssetRepo(db)
	users := repository.NewUserRepo(db)
	owner, err := users.Create("reserved-owner", "test-hash", "user")
	if err != nil {
		t.Fatal(err)
	}
	another, err := users.Create("another-owner", "test-hash", "user")
	if err != nil {
		t.Fatal(err)
	}
	first, err := repos.Create("first", "raw", "hosted", "private", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repos.Create("second", "raw", "hosted", "private", ""); err != nil {
		t.Fatal(err)
	}
	quota := repository.NewPublishPolicyRepo(db)
	id, err := quota.Reserve(repository.PublishPolicy{UserID: owner, RepositoryID: first, MaxAssetsHour: 2}, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	reservation := (&PublishPolicyService{policies: quota}).CommitReservation(id)
	svc := NewAssetService(repos, assets, blobstore.NewStore(t.TempDir()), nil)
	audit := NewSecurityAuditService(db)
	for _, step := range []struct {
		user       int64
		repo, path string
		accepted   bool
	}{
		{another, "first", "another.txt", false},
		{owner, "second", "another-repo.txt", false},
		{owner, "first", "accepted.txt", true},
		{owner, "first", "second-use.txt", false},
	} {
		_, op, err := svc.PutWithReservedSecurityAudit(context.Background(), audit, reservation, step.user, step.repo, step.path, strings.NewReader("abc"), "text/plain")
		if step.accepted {
			if err != nil || op == "" {
				t.Fatalf("合法单件流式预留未结算：%q / %v", op, err)
			}
		} else if !errors.Is(err, ErrSecurityAuditUnavailable) || op != "" {
			t.Fatalf("预留作用域或已结算检查失败：%q / %v", op, err)
		}
	}
	for _, query := range []string{`SELECT count(*) FROM asset`, `SELECT count(*) FROM replication_operation_outbox`, `SELECT count(*) FROM security_event WHERE event_type='asset.change_committed'`, `SELECT count(*) FROM quota_reservation WHERE settled_at IS NOT NULL AND assets=1 AND bytes=3`} {
		var count int
		if err := db.Get(&count, query); err != nil || count != 1 {
			t.Errorf("应只保留一次提交与结算：%s：%d / %v", query, count, err)
		}
	}
	var count int
	if err := db.Get(&count, `SELECT count(*) FROM quota_usage WHERE user_id=? AND repository_id=? AND used_assets=1 AND used_bytes=3`, owner, first); err != nil || count != 2 {
		t.Fatalf("原范围时日窗口应各只计一次：%d / %v", count, err)
	}
}

func assertRecoveredQuota(t *testing.T, db *persistence.DB, user, repo, id int64, op string, want, size int, pending bool) {
	t.Helper()
	wantReservations := want
	if pending {
		wantReservations++
	}
	for _, check := range []struct {
		query string
		args  []any
		count int
	}{
		{`SELECT count(*) FROM quota_reservation WHERE id=? AND user_id=? AND repository_id=?`, []any{id, user, repo}, wantReservations},
		{`SELECT count(*) FROM quota_reservation WHERE id=? AND user_id=? AND repository_id=? AND settled_at IS NOT NULL AND assets=1 AND bytes=?`, []any{id, user, repo, size}, want},
		{`SELECT count(*) FROM quota_usage WHERE user_id=? AND repository_id=? AND used_assets=1 AND used_bytes=?`, []any{user, repo, size}, want * 2},
		{`SELECT count(*) FROM quota_usage`, nil, want * 2},
		{`SELECT count(*) FROM asset WHERE repository_id=? AND path='recovery.txt'`, []any{repo}, want},
		{`SELECT count(*) FROM asset_path_fts WHERE asset_path_fts MATCH 'recovery'`, nil, want},
		{`SELECT count(*) FROM security_event e JOIN replication_operation_outbox o ON o.operation_id=e.operation_id JOIN asset_mutation m ON m.id=e.operation_id WHERE e.operation_id=? AND e.actor_user_id=? AND e.repository_id=? AND m.status='completed'`, []any{op, user, repo}, want},
	} {
		var count int
		if err := db.Get(&count, check.query, check.args...); err != nil || count != check.count {
			t.Errorf("发布或配额事实：%s：%d 应为 %d / %v", check.query, count, check.count, err)
		}
	}
}
