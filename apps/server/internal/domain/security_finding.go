package domain

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/netip"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// FR-171 规则检测投影：security_event を sequence 順に消費し、版付き規則で命中を
// 冪等登録 → 同主体の未決案件に集約、無ければ自動建案 + 站内/SMTP 通知。
// 規則は検出と通知のみ（自動封禁・自動結案・弱线索による并案はしない）。
// 命中 ID は規則・版・主体・窓の決定的ハッシュ：同じ再計算は同じ ID を出し、
// INSERT OR IGNORE で二重登録・二重建案を防ぐ。

const (
	findingBatchSize      = 100
	ruleDenialBurstID     = "subject_denial_burst"
	ruleDeniedThenCommit  = "denied_then_committed"
	ruleSensitiveNewSrcID = "sensitive_change_new_source"
	findingRuleVersion    = 1
	// denialBurstThreshold/Window：同主体 5 分 10 回拒否（規格 §3.2 初期規則）。
	denialBurstThreshold = 10
	denialBurstWindow    = 300
	// deniedThenCommitWindow：拒否後 10 分以内に別来源で已提交（規格 §3.2）。
	deniedThenCommitWindow = 600
)

// SecurityFindingService は規則投影 worker（単一実行前提；二重起動でも
// 水位・冪等 ID により行は増えないが処理は重複し得る→调度器は単一实例）。
type SecurityFindingService struct {
	repo interface {
		Write(ctx context.Context, fn func(*sqlx.Tx) error) error
		Read(ctx context.Context, fn func(*sqlx.Tx) error) error
	}
	now func() time.Time
}

func NewSecurityFindingService(repo interface {
	Write(ctx context.Context, fn func(*sqlx.Tx) error) error
	Read(ctx context.Context, fn func(*sqlx.Tx) error) error
}) *SecurityFindingService {
	return &SecurityFindingService{repo: repo, now: time.Now}
}

// findingSubject は評価対象の安定錨点（user → credential → client_ip の順で代表）。
type findingSubject struct {
	Kind, Key, Column string
	Value             any
}

func findingSubjects(e repository.SecurityFindingEventRow) []findingSubject {
	var out []findingSubject
	if e.ActorUserID != nil && *e.ActorUserID > 0 {
		out = append(out, findingSubject{"user", fmt.Sprintf("user:%d", *e.ActorUserID), "actor_user_id", *e.ActorUserID})
	}
	if e.CredentialID != "" {
		out = append(out, findingSubject{"cred", "cred:" + e.CredentialID, "credential_id", e.CredentialID})
	}
	if e.ClientIPFamily != 0 && len(e.ClientIP) > 0 {
		if ip, ok := netip.AddrFromSlice(e.ClientIP); ok {
			out = append(out, findingSubject{"ip", "ip:" + ip.String(), "client_ip", e.ClientIP})
		}
	}
	return out
}

// findingIDFor は決定的命中 ID（重複計算で再発しない冪等键）。
func findingIDFor(ruleID string, version int, subjectKey string, bucket int64) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s|%d|%s|%d", ruleID, version, subjectKey, bucket)))
	return "fnd-" + hex.EncodeToString(h[:16])
}

// RunBatch は水位以降の最大 100 件を消費し、命中→案件集約/建案→水位前進までを
// 単一写トランザクションで行う。命中件数を返す。
func (s *SecurityFindingService) RunBatch(ctx context.Context) (int, error) {
	newFindings := 0
	err := s.repo.Write(ctx, func(tx *sqlx.Tx) error {
		if err := repository.InitFindingWatermarkTx(ctx, tx); err != nil {
			return err
		}
		wm, err := repository.FindingWatermarkTx(ctx, tx)
		if err != nil {
			return err
		}
		events, err := repository.EventsAfterSeqTx(ctx, tx, wm, findingBatchSize)
		if err != nil {
			return err
		}
		if len(events) == 0 {
			return nil
		}
		now := s.now()
		for _, ev := range events {
			n, err := s.evaluate(ctx, tx, ev, now)
			if err != nil {
				return err
			}
			newFindings += n
		}
		return repository.SetFindingWatermarkTx(ctx, tx, events[len(events)-1].Sequence, now.Unix())
	})
	return newFindings, err
}

// evaluate は 1 イベントに全規則を適用する。
func (s *SecurityFindingService) evaluate(ctx context.Context, tx *sqlx.Tx, ev repository.SecurityFindingEventRow, now time.Time) (int, error) {
	subjects := findingSubjects(ev)
	if len(subjects) == 0 {
		return 0, nil
	}
	count := 0
	for _, sub := range subjects {
		// 規則 subject_denial_burst：同主体 5 分 10 回拒否。
		if ev.Outcome == "denied" {
			n, err := repository.CountDeniedByActorTx(ctx, tx, sub.Column, sub.Value, ev.OccurredAt-denialBurstWindow, ev.OccurredAt)
			if err != nil {
				return count, err
			}
			if n >= denialBurstThreshold {
				new, err := s.record(ctx, tx, repository.SecurityFindingRow{
					FindingID: findingIDFor(ruleDenialBurstID, findingRuleVersion, sub.Key, ev.OccurredAt/denialBurstWindow),
					RuleID:    ruleDenialBurstID, RuleVersion: findingRuleVersion,
					Severity: "medium", Confidence: "high", SubjectKey: sub.Key,
					WindowFrom: ev.OccurredAt - denialBurstWindow, WindowTo: ev.OccurredAt,
					Summary: fmt.Sprintf("主体 %s が 5 分以内に %d 回拒否（阈值 %d）", sub.Key, n, denialBurstThreshold),
				}, ev, "同主体の連続拒否", now)
				if err != nil {
					return count, err
				}
				count += new
			}
		}
		// 規則 denied_then_committed：拒否の後、別来源から已提交が成功。
		// ip: 主体は来源そのものなので「新来源」は成立せず評価しない。
		if ev.EventType == "asset.change_committed" && ev.Committed == "committed" && sub.Kind != "ip" {
			n, err := repository.DeniedFromOtherIPTx(ctx, tx, sub.Column, sub.Value,
				ev.OccurredAt-deniedThenCommitWindow, ev.OccurredAt, ev.ClientIPFamily, ev.ClientIP)
			if err != nil {
				return count, err
			}
			if n > 0 {
				new, err := s.record(ctx, tx, repository.SecurityFindingRow{
					FindingID: findingIDFor(ruleDeniedThenCommit, findingRuleVersion, sub.Key, ev.OccurredAt/deniedThenCommitWindow),
					RuleID:    ruleDeniedThenCommit, RuleVersion: findingRuleVersion,
					Severity: "high", Confidence: "medium", SubjectKey: sub.Key,
					WindowFrom: ev.OccurredAt - deniedThenCommitWindow, WindowTo: ev.OccurredAt,
					Summary: fmt.Sprintf("主体 %s が拒否後 10 分以内に新しい来源から已提交を成功（拒否 %d 件）", sub.Key, n),
				}, ev, "拒否後の新来源での成功", now)
				if err != nil {
					return count, err
				}
				count += new
			}
		}
		// 規則 sensitive_change_new_source：新来源での機微操作（権限/凭据変更）。
		if ev.EventType == "auth.account_access_changed" && len(ev.ClientIP) > 0 {
			n, err := repository.SubjectSeenFromIPTx(ctx, tx, sub.Column, sub.Value,
				ev.OccurredAt, ev.ClientIPFamily, ev.ClientIP)
			if err != nil {
				return count, err
			}
			if n == 0 {
				new, err := s.record(ctx, tx, repository.SecurityFindingRow{
					FindingID: findingIDFor(ruleSensitiveNewSrcID, findingRuleVersion, sub.Key, ev.Sequence),
					RuleID:    ruleSensitiveNewSrcID, RuleVersion: findingRuleVersion,
					Severity: "high", Confidence: "medium", SubjectKey: sub.Key,
					WindowFrom: ev.OccurredAt, WindowTo: ev.OccurredAt,
					Summary: fmt.Sprintf("主体 %s が初めて観測する来源から権限/凭据変更（event %s）", sub.Key, ev.EventID),
				}, ev, "新来源からの機微操作", now)
				if err != nil {
					return count, err
				}
				count += new
			}
		}
	}
	return count, nil
}

// record は命中を冪等登録し、新規なら未決案件集約または自動建案 + 通知を行う。
// 戻り値は新規登録した命中数（0/1）。
func (s *SecurityFindingService) record(ctx context.Context, tx *sqlx.Tx, f repository.SecurityFindingRow,
	ev repository.SecurityFindingEventRow, caseTitle string, now time.Time) (int, error) {
	f.CaseID = ""
	f.CreatedUnix = now.Unix()
	inserted, err := repository.InsertFindingTx(ctx, tx, f)
	if err != nil {
		return 0, err
	}
	if !inserted {
		return 0, nil // 既出命中：再計算・旧イベント重放は無視（建案も読取もしない）
	}
	if err := repository.LinkFindingEventTx(ctx, tx, f.FindingID, ev.Sequence); err != nil {
		return 0, err
	}
	// 同主体の未決案件に集約；無ければ自動建案（opened_by=rule-engine）。
	caseID, err := repository.OpenCaseForSubjectTx(ctx, tx, f.SubjectKey)
	if err == sql.ErrNoRows {
		caseID = ""
	} else if err != nil {
		return 0, err
	}
	if caseID == "" {
		row := repository.SecurityCaseRow{
			CaseID: "case-" + uuid.NewString(), Title: caseTitle, Severity: f.Severity,
			State: "open", TargetVersion: 1, OpenedBy: "rule-engine",
			Reason:      fmt.Sprintf("規則 %s v%d 命中（%s）", f.RuleID, f.RuleVersion, f.Summary),
			CreatedUnix: now.Unix(), UpdatedUnix: now.Unix()}
		if err := repository.InsertSecurityCaseTx(ctx, tx, row); err != nil {
			return 0, err
		}
		caseID = row.CaseID
		if _, err := EmitSecurityNotificationTx(ctx, tx, "security.case_opened", row.Title,
			fmt.Sprintf(`{"caseId":%q,"severity":%q,"ruleId":%q,"auto":true}`, row.CaseID, row.Severity, f.RuleID), now); err != nil {
			return 0, err
		}
	}
	if err := repository.AttachFindingCaseTx(ctx, tx, f.FindingID, caseID); err != nil {
		return 0, err
	}
	// 命中の証拠イベントを案件に fact 関連付け。
	if err := repository.LinkSecurityCaseEventTx(ctx, tx, repository.SecurityCaseLinkRow{
		CaseID: caseID, EventID: ev.EventID, LinkKind: "fact", LinkedBy: "rule-engine", LinkedUnix: now.Unix(),
	}); err != nil {
		return 0, err
	}
	return 1, nil
}

// ListFindings は命中を新しい順で返す（管理員限定は呼び出し側で裁决済み）。
func (s *SecurityFindingService) ListFindings(ctx context.Context, p *auth.Principal, limit int) ([]repository.SecurityFindingRow, error) {
	if p == nil || p.UserID <= 0 || p.Role != "admin" {
		return nil, ErrAdmissionForbidden
	}
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	var out []repository.SecurityFindingRow
	err := s.repo.Read(ctx, func(tx *sqlx.Tx) error {
		var e error
		out, e = repository.ListFindingsTx(ctx, tx, limit)
		return e
	})
	return out, err
}
