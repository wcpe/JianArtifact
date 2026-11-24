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

// 安全治理规则检测投影：security_event 按 sequence 顺序消费，命中经带版本规则
// 幂等登记 → 聚合到同主体未决案件，无则自动建案 + 站内/SMTP 通知。
// 规则只做检测与通知（不自动封禁、不自动结案、不按弱线索并案）。
// 命中 ID 是规则·版本·主体·窗口的确定性哈希：相同重算产出相同 ID，
// 以 INSERT OR IGNORE 防止重复登记与重复建案。

const (
	findingBatchSize      = 100
	ruleDenialBurstID     = "subject_denial_burst"
	ruleDeniedThenCommit  = "denied_then_committed"
	ruleSensitiveNewSrcID = "sensitive_change_new_source"
	findingRuleVersion    = 1
	// denialBurstThreshold/Window：同主体 5 分 10 回拒否（規格 §3.2 初期規則）。
	denialBurstThreshold = 10
	denialBurstWindow    = 300
	// deniedThenCommitWindow：拒绝后 10 分钟内另一来源已提交（规范 §3.2）。
	deniedThenCommitWindow = 600
)

// SecurityFindingService 是规则投影 worker（单实例前提；即使重复启动，
// 水位与幂等 ID 保证行不重复，但处理可能重复→调度器保持单实例）。
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

// findingSubject 是评估对象的稳定锚点（按 user → credential → client_ip 顺序取代表）。
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

// findingIDFor 生成确定性命中 ID（重复计算不再新发的幂等键）。
func findingIDFor(ruleID string, version int, subjectKey string, bucket int64) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s|%d|%s|%d", ruleID, version, subjectKey, bucket)))
	return "fnd-" + hex.EncodeToString(h[:16])
}

// RunBatch 消费水位后至多 100 件事件，命中→案件聚合/建案→水位前进，
// 全部在单一写事务内完成。返回命中件数。
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

// evaluate 对单个事件应用全部规则。
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
					Summary: fmt.Sprintf("主体 %s 在 5 分钟内被拒绝 %d 次（阈值 %d）", sub.Key, n, denialBurstThreshold),
				}, ev, "同主体连续拒绝", now)
				if err != nil {
					return count, err
				}
				count += new
			}
		}
		// 规则 denied_then_committed：拒绝之后，另一来源已提交成功。
		// ip 主体即来源本身，「新来源」不成立故不评估。
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
					Summary: fmt.Sprintf("主体 %s 在拒绝后 10 分钟内从新来源成功已提交（拒绝 %d 件）", sub.Key, n),
				}, ev, "拒绝后新来源成功", now)
				if err != nil {
					return count, err
				}
				count += new
			}
		}
		// 规则 sensitive_change_new_source：新来源的敏感操作（权限/凭据变更）。
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
					Summary: fmt.Sprintf("主体 %s 首次观测的来源执行权限/凭据变更（event %s）", sub.Key, ev.EventID),
				}, ev, "新来源的敏感操作", now)
				if err != nil {
					return count, err
				}
				count += new
			}
		}
	}
	return count, nil
}

// record 幂等登记命中；新命中则聚合到未决案件或自动建案 + 通知。
// 返回新登记命中数（0/1）。
func (s *SecurityFindingService) record(ctx context.Context, tx *sqlx.Tx, f repository.SecurityFindingRow,
	ev repository.SecurityFindingEventRow, caseTitle string, now time.Time) (int, error) {
	f.CaseID = ""
	f.CreatedUnix = now.Unix()
	inserted, err := repository.InsertFindingTx(ctx, tx, f)
	if err != nil {
		return 0, err
	}
	if !inserted {
		return 0, nil // 已登记命中：重算与旧事件重放一律忽略（不建案也不读）
	}
	if err := repository.LinkFindingEventTx(ctx, tx, f.FindingID, ev.Sequence); err != nil {
		return 0, err
	}
	// 聚合到同主体未决案件；无则自动建案（opened_by=rule-engine）。
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
	// 命中的证据事件以 fact 关联到案件。
	if err := repository.LinkSecurityCaseEventTx(ctx, tx, repository.SecurityCaseLinkRow{
		CaseID: caseID, EventID: ev.EventID, LinkKind: "fact", LinkedBy: "rule-engine", LinkedUnix: now.Unix(),
	}); err != nil {
		return 0, err
	}
	return 1, nil
}

// ListFindings 按新到旧返回命中（管理员限定由调用侧裁决）。
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
