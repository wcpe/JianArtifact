package domain

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// FR-168：事故处置动作生命周期——预览→确认→执行，幂等、逐项结果、近期强认证由处理层裁决。

const (
	SecurityActionPreviewTTL = 5 * time.Minute
	securityActionMaxTargets = 100
	securityActionMaxReasonB = 512
)

var (
	ErrSecurityTargetInvalid       = errors.New("invalid_security_target")
	ErrSecurityConfirmationStale   = errors.New("security_confirmation_stale")
	ErrSecurityIdempotencyConflict = errors.New("security_idempotency_conflict")
	ErrIncidentWriteLocked         = errors.New("incident_write_locked")
	ErrIncidentRestricted          = errors.New("incident_restricted")
)

type SecurityIncidentService struct {
	repo interface {
		Write(ctx context.Context, fn func(*sqlx.Tx) error) error
		Read(ctx context.Context, fn func(*sqlx.Tx) error) error
	}
	users *repository.UserRepo
	now   func() time.Time
}

func NewSecurityIncidentService(repo interface {
	Write(ctx context.Context, fn func(*sqlx.Tx) error) error
	Read(ctx context.Context, fn func(*sqlx.Tx) error) error
}, users *repository.UserRepo) *SecurityIncidentService {
	return &SecurityIncidentService{repo: repo, users: users, now: time.Now}
}

type SecurityTarget struct {
	Kind   string `json:"kind"`
	Target string `json:"target"`
	Effect string `json:"effect,omitempty"`
}

type SecurityActionPreview struct {
	ActionID     string   `json:"actionId"`
	ActionType   string   `json:"actionType"`
	ActionDigest string   `json:"actionDigest"`
	ExpiresAt    string   `json:"expiresAt"`
	Targets      []string `json:"targets"`
}

type SecurityActionResult struct {
	ActionID string                          `json:"actionId"`
	State    string                          `json:"state"`
	Items    []repository.SecurityActionItem `json:"items"`
}

func securityActionDigest(actionType string, targets []SecurityTarget) string {
	type d struct {
		Type    string           `json:"t"`
		Targets []SecurityTarget `json:"g"`
	}
	b, _ := json.Marshal(d{actionType, targets})
	return hashChallengeToken(string(b))
}

// PreviewSecurityAction 建立预览动作；幂等键冲突且摘要一致时回放既有动作。
func (s *SecurityIncidentService) PreviewSecurityAction(ctx context.Context, actorUserID int64, actionType, reason string, targets []SecurityTarget, idempotencyKey string) (*SecurityActionPreview, error) {
	if reason == "" || len(reason) > securityActionMaxReasonB {
		return nil, ErrValidation
	}
	if len(targets) == 0 || len(targets) > securityActionMaxTargets {
		return nil, ErrSecurityTargetInvalid
	}
	for _, tg := range targets {
		if !validSecurityTarget(actionType, tg) {
			return nil, ErrSecurityTargetInvalid
		}
	}
	digest := securityActionDigest(actionType, targets)
	now := s.now()
	var preview *SecurityActionPreview
	err := s.repo.Write(ctx, func(tx *sqlx.Tx) error {
		if existing, err := repository.SecurityActionByKeyTx(ctx, tx, actorUserID, idempotencyKey); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		} else if err == nil {
			if existing.ActionDigest != digest {
				return ErrSecurityIdempotencyConflict
			}
			items, ierr := repository.SecurityActionItemsTx(ctx, tx, existing.ActionID)
			if ierr != nil {
				return ierr
			}
			tgs := make([]string, 0, len(items))
			for _, it := range items {
				tgs = append(tgs, it.Target)
			}
			preview = &SecurityActionPreview{ActionID: existing.ActionID, ActionType: existing.ActionType,
				ActionDigest: existing.ActionDigest, ExpiresAt: time.Unix(existing.ExpiresAt, 0).UTC().Format(time.RFC3339), Targets: tgs}
			return nil
		}
		a := repository.SecurityAction{
			ActionID:       "sa-" + uuid.NewString(),
			ActionType:     actionType,
			State:          repository.SecurityActionStatePreviewed,
			ActorUserID:    actorUserID,
			Reason:         reason,
			ActionDigest:   digest,
			IdempotencyKey: idempotencyKey,
			ExpiresAt:      now.Add(SecurityActionPreviewTTL).Unix(),
			CreatedAt:      now.Unix(),
		}
		items := make([]repository.SecurityActionItem, 0, len(targets))
		for i, tg := range targets {
			b, _ := json.Marshal(tg)
			items = append(items, repository.SecurityActionItem{ActionID: a.ActionID, Seq: i, Target: string(b), Outcome: "pending"})
		}
		if err := repository.InsertSecurityActionTx(ctx, tx, a, items); err != nil {
			return err
		}
		tgs := make([]string, 0, len(targets))
		for _, it := range items {
			tgs = append(tgs, it.Target)
		}
		preview = &SecurityActionPreview{ActionID: a.ActionID, ActionType: actionType, ActionDigest: digest,
			ExpiresAt: time.Unix(a.ExpiresAt, 0).UTC().Format(time.RFC3339), Targets: tgs}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return preview, nil
}

// ExecuteSecurityAction 确认并执行预览动作；指纹漂移或过期一律拒绝，逐项落结果。
// apply 为每项副作用函数，在同一最终事务内与结果写同事务执行。
func (s *SecurityIncidentService) ExecuteSecurityAction(ctx context.Context, actorUserID int64, actionID, expectedDigest string,
	apply func(tx *sqlx.Tx, target SecurityTarget) (outcome, detail string, err error)) (*SecurityActionResult, error) {
	ctx = context.WithoutCancel(ctx)
	var out *SecurityActionResult
	err := s.repo.Write(ctx, func(tx *sqlx.Tx) error {
		a, err := repository.SecurityActionByIDTx(ctx, tx, actionID)
		if err != nil {
			return ErrSecurityConfirmationStale
		}
		now := s.now().Unix()
		if a.ActorUserID != actorUserID || a.ActionDigest != expectedDigest {
			return ErrSecurityConfirmationStale
		}
		if a.State != repository.SecurityActionStatePreviewed && a.State != repository.SecurityActionStateConfirmed {
			return ErrSecurityConfirmationStale
		}
		if a.ExpiresAt <= now {
			_ = repository.FinalizeSecurityActionTx(ctx, tx, a.ActionID, repository.SecurityActionStateExpired, now)
			return ErrSecurityConfirmationStale
		}
		items, err := repository.SecurityActionItemsTx(ctx, tx, a.ActionID)
		if err != nil {
			return err
		}
		partial := false
		for _, it := range items {
			var tg SecurityTarget
			if err := json.Unmarshal([]byte(it.Target), &tg); err != nil {
				return err
			}
			outcome, detail, aerr := apply(tx, tg)
			if aerr != nil {
				partial = true
				outcome, detail = "rejected", aerr.Error()
			}
			if err := repository.SetSecurityActionItemTx(ctx, tx, a.ActionID, it.Seq, outcome, detail); err != nil {
				return err
			}
		}
		final := repository.SecurityActionStateApplied
		if partial {
			final = repository.SecurityActionStatePartially
		}
		if err := repository.FinalizeSecurityActionTx(ctx, tx, a.ActionID, final, now); err != nil {
			return err
		}
		// 受保护安全状态变更同事务抬水位（恢复核对的可比版本）。
		if _, err := repository.BumpSecurityWatermarkTx(ctx, tx); err != nil {
			return err
		}
		items, err = repository.SecurityActionItemsTx(ctx, tx, a.ActionID)
		if err != nil {
			return err
		}
		out = &SecurityActionResult{ActionID: a.ActionID, State: final, Items: items}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// GetActionType 读动作类型（执行前的路由依据）。
func (s *SecurityIncidentService) GetActionType(ctx context.Context, actionID string) (string, error) {
	var t string
	err := s.repo.Read(ctx, func(tx *sqlx.Tx) error {
		a, err := repository.SecurityActionByIDTx(ctx, tx, actionID)
		if err != nil {
			return err
		}
		t = a.ActionType
		return nil
	})
	return t, err
}

// IncidentVerdict 入口裁决结果：实例写锁或目标命中。
type IncidentVerdict struct {
	Locked     bool
	Restricted bool
}

// BusinessWriteVerdict 入口裁决：实例写锁 + 按仓库/路径前缀命中活跃限制。
func (s *SecurityIncidentService) BusinessWriteVerdict(ctx context.Context, repo, pathPrefix string) (IncidentVerdict, error) {
	var v IncidentVerdict
	err := s.repo.Read(ctx, func(tx *sqlx.Tx) error {
		locked, err := repository.InstanceWriteLockedTx(ctx, tx, s.now().Unix())
		if err != nil {
			return err
		}
		v.Locked = locked
		var rows []repository.IncidentRestriction
		if err := tx.SelectContext(ctx, &rows, `SELECT restriction_id, kind, target, effects, state, version, effective_at, expires_at, action_id, lifted_action, created_at
			FROM incident_restriction WHERE state='active' AND kind IN ('repository','path','version')`); err != nil {
			return err
		}
		for _, r := range rows {
			switch r.Kind {
			case "repository":
				if repo != "" && r.Target == repo {
					v.Restricted = true
				}
			case "path":
				// 目标=仓库名:路径前缀
				if i := strings.Index(r.Target, ":"); i > 0 && repo == r.Target[:i] && pathPrefix != "" && strings.HasPrefix(pathPrefix, r.Target[i+1:]) {
					v.Restricted = true
				}
			case "version":
				if r.Target != "" && repo != "" && pathPrefix != "" && strings.HasPrefix(r.Target, repo+":") && strings.HasPrefix(pathPrefix, strings.SplitN(r.Target, "@", 2)[0][len(repo)+1:]) {
					v.Restricted = true
				}
			}
		}
		return nil
	})
	return v, err
}

// BusinessWriteLocked 入口裁决：是否存在活跃实例业务写锁。
func (s *SecurityIncidentService) BusinessWriteLocked(ctx context.Context) (bool, error) {
	var locked bool
	err := s.repo.Read(ctx, func(tx *sqlx.Tx) error {
		var err error
		locked, err = repository.InstanceWriteLockedTx(ctx, tx, s.now().Unix())
		return err
	})
	return locked, err
}

// ApplyInstanceLock 逐项副作用：为每个目标写活跃实例写锁。
func (s *SecurityIncidentService) ApplyInstanceLock(actionID string) func(*sqlx.Tx, SecurityTarget) (string, string, error) {
	now := s.now()
	return func(tx *sqlx.Tx, tg SecurityTarget) (string, string, error) {
		r := repository.IncidentRestriction{
			RestrictionID: "ir-" + uuid.NewString(),
			Kind:          repository.RestrictionKindInstanceLock,
			State:         repository.RestrictionStateActive,
			Version:       1,
			EffectiveAt:   now.Unix(),
			ActionID:      actionID,
			CreatedAt:     now.Unix(),
		}
		if err := repository.InsertIncidentRestrictionTx(context.Background(), tx, r); err != nil {
			return "", "", fmt.Errorf("写入实例锁：%w", err)
		}
		return "applied", "实例业务写锁已生效", nil
	}
}

// ApplyRestriction 逐项副作用：按目标种类写活跃事故限制。
func (s *SecurityIncidentService) ApplyRestriction(actionID string) func(*sqlx.Tx, SecurityTarget) (string, string, error) {
	now := s.now()
	return func(tx *sqlx.Tx, tg SecurityTarget) (string, string, error) {
		r := repository.IncidentRestriction{
			RestrictionID: "ir-" + uuid.NewString(),
			Kind:          tg.Kind,
			Target:        tg.Target,
			Effects:       tg.Effect,
			State:         repository.RestrictionStateActive,
			Version:       1,
			EffectiveAt:   now.Unix(),
			ActionID:      actionID,
			CreatedAt:     now.Unix(),
		}
		if tg.Kind == repository.RestrictionKindInstanceLock {
			r.Target = ""
		}
		if err := repository.InsertIncidentRestrictionTx(context.Background(), tx, r); err != nil {
			return "", "", fmt.Errorf("写入事故限制：%w", err)
		}
		return "applied", "事故限制已生效", nil
	}
}

// ApplyEvidenceHold 逐项副作用：按摘要登记保全持有；字节状态未知起步，
// 由补录/核对推进 present/metadata_only（本增量不裁定字节）。
func (s *SecurityIncidentService) ApplyEvidenceHold(actionID string) func(*sqlx.Tx, SecurityTarget) (string, string, error) {
	now := s.now()
	return func(tx *sqlx.Tx, tg SecurityTarget) (string, string, error) {
		if existing, err := repository.EvidenceHoldByDigestTx(context.Background(), tx, tg.Target); err != nil {
			return "", "", err
		} else if existing != nil {
			return "skipped", "该摘要已有活跃保全：" + existing.HoldID, nil
		}
		h := repository.EvidenceHold{
			HoldID: "eh-" + uuid.NewString(), ObjectID: tg.Target, Digest: tg.Target,
			ByteState: repository.EvidenceHoldByteUnknown, State: repository.EvidenceHoldStateActive,
			ActionID: actionID, CreatedAt: now.Unix(),
		}
		// 字节状态裁决：摘要被已发布资产引用即 present（字节在内容寻址存储内），否则 metadata_only。
		var cnt int
		if err := tx.GetContext(context.Background(), &cnt,
			`SELECT COUNT(*) FROM asset WHERE blob_hash = ? OR blob_hash = ?`, tg.Target, "sha256:"+tg.Target); err != nil {
			return "", "", fmt.Errorf("裁决字节状态：%w", err)
		}
		if cnt > 0 {
			h.ByteState = repository.EvidenceHoldBytePresent
			var sz *int64
			_ = tx.GetContext(context.Background(), &sz,
				`SELECT MIN(size) FROM asset WHERE blob_hash = ? OR blob_hash = ?`, tg.Target, "sha256:"+tg.Target)
			if sz != nil {
				h.SizeBytes = *sz
			}
		} else {
			h.ByteState = repository.EvidenceHoldByteMetadataOnly
		}
		if err := repository.InsertEvidenceHoldTx(context.Background(), tx, h); err != nil {
			return "", "", fmt.Errorf("登记保全：%w", err)
		}
		return "applied", "保全已登记（byte_state=" + h.ByteState + "）", nil
	}
}

// ApplyReleaseHold 逐项副作用：释放活跃保全。
func (s *SecurityIncidentService) ApplyReleaseHold(actionID string) func(*sqlx.Tx, SecurityTarget) (string, string, error) {
	now := s.now()
	return func(tx *sqlx.Tx, tg SecurityTarget) (string, string, error) {
		if err := repository.ReleaseEvidenceHoldTx(context.Background(), tx, tg.Target, actionID, now.Unix()); err != nil {
			return "", "", fmt.Errorf("释放保全：%w", err)
		}
		return "applied", "保全已释放", nil
	}
}

// ListEvidenceHolds 保全清单（活跃/全部，按创建倒序，上限 200）。
func (s *SecurityIncidentService) ListEvidenceHolds(ctx context.Context) ([]repository.EvidenceHold, error) {
	var out []repository.EvidenceHold
	err := s.repo.Read(ctx, func(tx *sqlx.Tx) error {
		return tx.SelectContext(ctx, &out, `SELECT hold_id, object_id, digest, byte_state, size_bytes, state, action_id, created_at, released_at
			FROM evidence_hold ORDER BY created_at DESC LIMIT 200`)
	})
	return out, err
}

// ApplyLiftInstanceLock 逐项副作用：解除活跃实例写锁。
func (s *SecurityIncidentService) ApplyLiftInstanceLock(actionID string) func(*sqlx.Tx, SecurityTarget) (string, string, error) {
	now := s.now()
	return func(tx *sqlx.Tx, tg SecurityTarget) (string, string, error) {
		var id string
		err := tx.GetContext(context.Background(), &id, `SELECT restriction_id FROM incident_restriction WHERE kind=? AND target=? AND state='active' LIMIT 1`, tg.Kind, tg.Target)
		if err != nil {
			return "", "", err
		}
		if err := repository.LiftIncidentRestrictionTx(context.Background(), tx, id, actionID, now.Unix()); err != nil {
			return "", "", fmt.Errorf("解除事故限制：%w", err)
		}
		return "applied", "事故限制已解除", nil
	}
}

// 合法目标种类：instance_write_lock / repository / path / version / ip_cidr。
// repository 目标=仓库名；path=仓库名:规范路径前缀；version=仓库名:路径@版本；ip_cidr=CIDR+effects 子集。
func validSecurityTarget(actionType string, tg SecurityTarget) bool {
	switch actionType {
	case "restrict":
		switch tg.Kind {
		case "instance_write_lock":
			return true
		case "repository":
			return tg.Target != ""
		case "path", "version":
			return tg.Target != "" && strings.Contains(tg.Target, ":")
		case "ip_cidr":
			if tg.Target == "" {
				return false
			}
			if _, _, err := net.ParseCIDR(tg.Target); err != nil {
				return false
			}
			// 未给 effects 默认 login+publish+read 全集；给了必须是子集。
			for _, e := range strings.Split(tg.Effect, ",") {
				e = strings.TrimSpace(e)
				if e == "" {
					continue
				}
				if e != "login" && e != "publish" && e != "read" {
					return false
				}
			}
			return true
		}
	case "lift_restriction":
		return tg.Kind != "" && tg.Target != "" || tg.Kind == "instance_write_lock"
	case "hold_evidence":
		// 目标=制品摘要（digest 作为唯一物理字节身份）。
		return tg.Kind == "digest" && tg.Target != ""
	case "release_hold":
		return tg.Kind == "hold" && tg.Target != ""
	}
	return false
}

// ── 恢复隔离闸（FR-168 §3.4 最小切片）───────────────────────────────────────

// BeginRestoreIsolation 恢复启动：normal → isolated，记恢复标识与原因。
func (s *SecurityIncidentService) BeginRestoreIsolation(ctx context.Context, restoreID, reason string) error {
	return s.repo.Write(ctx, func(tx *sqlx.Tx) error {
		g, err := repository.RecoveryGateTx(ctx, tx)
		if err != nil {
			return err
		}
		if g.State != repository.RecoveryGateNormal && (g.RestoreID == nil || *g.RestoreID != restoreID) {
			return ErrSecurityConfirmationStale
		}
		g.State = repository.RecoveryGateIsolated
		g.RestoreID = &restoreID
		g.Reason = reason
		g.CheckpointStatus = repository.CheckpointNone
		g.GapStatus = "none"
		g.UpdatedAt = s.now().Unix()
		return repository.SetRecoveryGateTx(ctx, tx, *g)
	})
}

// ReportRestoreCheckpoint 恢复核对：可信检查点 verified 且水位齐 → ready_for_review；
// stale/mismatch/missing 一律回 isolated 并留原因。
func (s *SecurityIncidentService) ReportRestoreCheckpoint(ctx context.Context, restoreID, status string, watermark int64) error {
	return s.repo.Write(ctx, func(tx *sqlx.Tx) error {
		g, err := repository.RecoveryGateTx(ctx, tx)
		if err != nil {
			return err
		}
		if g.RestoreID == nil || *g.RestoreID != restoreID || (g.State != repository.RecoveryGateIsolated && g.State != repository.RecoveryGateReconciling) {
			return ErrSecurityConfirmationStale
		}
		g.State = repository.RecoveryGateReconciling
		g.CheckpointStatus = status
		g.VerifiedWatermark = watermark
		// 水位比对：低于库内当前水位即检查点过时。
		current, err := repository.SecurityWatermarkTx(ctx, tx)
		if err != nil {
			return err
		}
		switch {
		case status == repository.CheckpointVerified && watermark >= current:
			g.State = repository.RecoveryGateReadyForReview
			g.GapStatus = "none"
			g.Reason = ""
		case status == repository.CheckpointVerified && watermark < current:
			g.State = repository.RecoveryGateIsolated
			g.CheckpointStatus = repository.CheckpointStale
			g.GapStatus = "gap"
			g.Reason = "检查点水位低于当前受保护状态版本，视为过时"
		default:
			g.State = repository.RecoveryGateIsolated
			g.GapStatus = "unknown"
			g.Reason = "无可采信检查点（" + status + "），须本机重新认领与核对"
		}
		g.UpdatedAt = s.now().Unix()
		return repository.SetRecoveryGateTx(ctx, tx, *g)
	})
}

// ConfirmRestore 已重新认领的强认证管理员解闸：ready_for_review → normal。
func (s *SecurityIncidentService) ConfirmRestore(ctx context.Context, restoreID string) error {
	return s.repo.Write(ctx, func(tx *sqlx.Tx) error {
		g, err := repository.RecoveryGateTx(ctx, tx)
		if err != nil {
			return err
		}
		if g.RestoreID == nil || *g.RestoreID != restoreID || g.State != repository.RecoveryGateReadyForReview {
			return ErrSecurityConfirmationStale
		}
		g.State = repository.RecoveryGateNormal
		g.RestoreID = nil
		g.SecurityEpoch++
		g.UpdatedAt = s.now().Unix()
		return repository.SetRecoveryGateTx(ctx, tx, *g)
	})
}

// RecoveryGateStatus 恢复闸状态（管理员可读）。
func (s *SecurityIncidentService) RecoveryGateStatus(ctx context.Context) (*repository.RecoveryGate, error) {
	var g *repository.RecoveryGate
	err := s.repo.Read(ctx, func(tx *sqlx.Tx) error {
		var err error
		g, err = repository.RecoveryGateTx(ctx, tx)
		return err
	})
	return g, err
}

// RecoveryIsolated 入口裁决：隔离中（非 normal）一律阻断公网业务。
func (s *SecurityIncidentService) RecoveryIsolated(ctx context.Context) (bool, error) {
	var isolated bool
	err := s.repo.Read(ctx, func(tx *sqlx.Tx) error {
		g, err := repository.RecoveryGateTx(ctx, tx)
		if err != nil {
			return err
		}
		isolated = g.State != repository.RecoveryGateNormal
		return nil
	})
	return isolated, err
}

// ── 风险调查案件（FR-171 §cases）───────────────────────────────────────────

// OpenSecurityCase 建案：管理员人工或规则检测入口共用；title/severity/reason 必填。
func (s *SecurityIncidentService) OpenSecurityCase(ctx context.Context, p *auth.Principal, title, severity, reason string, links []repository.SecurityCaseLinkRow) (*repository.SecurityCaseRow, error) {
	if p == nil || p.UserID <= 0 || p.Role != "admin" {
		return nil, ErrAdmissionForbidden
	}
	if title == "" || reason == "" {
		return nil, ErrValidation
	}
	switch severity {
	case "low", "medium", "high", "critical":
	default:
		return nil, ErrValidation
	}
	now := s.now().Unix()
	row := repository.SecurityCaseRow{CaseID: "case-" + uuid.NewString(), Title: title, Severity: severity,
		State: "open", TargetVersion: 1, OpenedBy: fmt.Sprintf("%d", p.UserID), Reason: reason,
		CreatedUnix: now, UpdatedUnix: now}
	err := s.repo.Write(ctx, func(tx *sqlx.Tx) error {
		if err := repository.InsertSecurityCaseTx(ctx, tx, row); err != nil {
			return err
		}
		for _, l := range links {
			if l.LinkKind != "fact" && l.LinkKind != "candidate" {
				return ErrValidation
			}
			l.CaseID = row.CaseID
			if l.LinkedBy == "" {
				l.LinkedBy = row.OpenedBy
			}
			l.LinkedUnix = now
			if err := repository.LinkSecurityCaseEventTx(ctx, tx, l); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &row, nil
}

// TransitionSecurityCase 认领/结案/重开：绑定 targetVersion，CAS 版本推进；close 需 resolutionCode。
func (s *SecurityIncidentService) TransitionSecurityCase(ctx context.Context, p *auth.Principal, caseID, action, reason, resolution string, expectVersion int64) (*repository.SecurityCaseRow, error) {
	if p == nil || p.UserID <= 0 || p.Role != "admin" {
		return nil, ErrAdmissionForbidden
	}
	var next, res string
	var owner *int64
	var closedAt *int64
	now := s.now().Unix()
	switch action {
	case "claim":
		next, owner = "claimed", &p.UserID
	case "close":
		if reason == "" || resolution == "" {
			return nil, ErrValidation
		}
		next, res, closedAt = "closed", resolution, &now
	case "reopen":
		if reason == "" {
			return nil, ErrValidation
		}
		next = "open"
	default:
		return nil, ErrValidation
	}
	var out *repository.SecurityCaseRow
	err := s.repo.Write(ctx, func(tx *sqlx.Tx) error {
		cur, err := repository.GetSecurityCaseTx(ctx, tx, caseID)
		if err != nil {
			return err
		}
		if cur.TargetVersion != expectVersion {
			return ErrSecurityConfirmationStale
		}
		ok, err := repository.TransitionSecurityCaseTx(ctx, tx, caseID, expectVersion, next, res, owner, closedAt, now)
		if err != nil {
			return err
		}
		if !ok {
			return ErrSecurityConfirmationStale
		}
		out, err = repository.GetSecurityCaseTx(ctx, tx, caseID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ListSecurityCases 案件清单（state 可空=全部，上限由调用方给）。
func (s *SecurityIncidentService) ListSecurityCases(ctx context.Context, state string, limit int) ([]repository.SecurityCaseRow, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	var out []repository.SecurityCaseRow
	err := s.repo.Read(ctx, func(tx *sqlx.Tx) error {
		var e error
		out, e = repository.ListSecurityCasesTx(ctx, tx, state, limit)
		return e
	})
	return out, err
}

// SecurityCaseLinks 案件的关联事件（区分 fact/candidate）。
func (s *SecurityIncidentService) SecurityCaseLinks(ctx context.Context, caseID string) ([]repository.SecurityCaseLinkRow, error) {
	var out []repository.SecurityCaseLinkRow
	err := s.repo.Read(ctx, func(tx *sqlx.Tx) error {
		var e error
		out, e = repository.ListSecurityCaseLinksTx(ctx, tx, caseID)
		return e
	})
	return out, err
}
