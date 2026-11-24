package domain

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"

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

// ApplyLiftInstanceLock 逐项副作用：解除活跃实例写锁。
func (s *SecurityIncidentService) ApplyLiftInstanceLock(actionID string) func(*sqlx.Tx, SecurityTarget) (string, string, error) {
	now := s.now()
	return func(tx *sqlx.Tx, tg SecurityTarget) (string, string, error) {
		var id string
		if err := tx.GetContext(context.Background(), &id, `SELECT restriction_id FROM incident_restriction WHERE kind='instance_write_lock' AND state='active' LIMIT 1`); err != nil {
			return "", "", err
		}
		if err := repository.LiftIncidentRestrictionTx(context.Background(), tx, id, actionID, now.Unix()); err != nil {
			return "", "", fmt.Errorf("解除实例锁：%w", err)
		}
		return "applied", "实例业务写锁已解除", nil
	}
}

func validSecurityTarget(actionType string, tg SecurityTarget) bool {
	switch actionType {
	case "restrict":
		return tg.Kind == "instance_write_lock"
	case "lift_restriction":
		return tg.Kind == "instance_write_lock"
	}
	return false
}
