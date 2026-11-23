package domain

import (
	"context"
	"errors"
	"io"

	"github.com/jmoiron/sqlx"
	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// ErrSecurityAuditUnavailable 表示关键发布无法记录必要的安全事实。
var ErrSecurityAuditUnavailable = errors.New("安全审计暂不可用")

type securityAuditedPublish struct {
	ctx         context.Context
	audit       *SecurityAuditService
	actor       int64
	reservation *PublishCommitReservation
	// proof 是入口已验证的凭据引用；非空时在完成事务内重查当前状态，
	// 撤销/降代先提交则本次公开事务失败，反之保留原成功。
	proof *auth.CredentialProof
	// principal 是入口鉴权时的主体快照；非空时在完成事务内复核
	// 实时 ACL 与机器凭据作用域仍允许本次公开（排队有权不代表公开有权）。
	principal *auth.Principal
	users     *repository.UserRepo
	repoSvc   *RepositoryService
	repo      *repository.Repository
	failed    bool
}

func (a *securityAuditedPublish) mapWriteError(err error) error {
	if a == nil || err == nil {
		return err
	}
	if errors.Is(err, errAssetCommitUnknown) || a.failed {
		return errors.Join(ErrSecurityAuditUnavailable, err)
	}
	if !errors.Is(err, ErrQuotaExceeded) && !errors.Is(err, ErrValidation) && !errors.Is(err, ErrConflict) && repository.IsSQLiteWriteUnavailable(err) {
		return errors.Join(ErrSecurityAuditUnavailable, err)
	}
	return err
}

// PutWithSecurityAudit 通过现有发布协调器原子提交 Raw 和共用此入口的 Maven 单制品。
// 成功返回原操作号和提交快照；仅结果未决时随错误返回内部可重查的原 intent ID。
func (s *AssetService) PutWithSecurityAudit(ctx context.Context, audit *SecurityAuditService, actorUserID int64, repoName, path string, r io.Reader, contentType string) (*repository.Asset, string, error) {
	return s.PutWithReservedSecurityAudit(ctx, audit, nil, actorUserID, repoName, path, r, contentType)
}

// PutWithReservedSecurityAudit 在同一完成事务中结算已有预留；不创建新预留，也不在失败时自行释放。
func (s *AssetService) PutWithReservedSecurityAudit(ctx context.Context, audit *SecurityAuditService, reservation *PublishCommitReservation, actorUserID int64, repoName, path string, r io.Reader, contentType string) (*repository.Asset, string, error) {
	if audit == nil {
		return nil, "", ErrSecurityAuditUnavailable
	}
	if err := s.requireBusinessWrite(); err != nil {
		return nil, "", err
	}
	audited := &securityAuditedPublish{ctx: ctx, audit: audit, actor: actorUserID, reservation: reservation, users: s.users, repoSvc: s.repoSvc}
	if proof, ok := credentialProofFrom(ctx); ok {
		audited.proof = &proof
	}
	if p, ok := publishPrincipalFrom(ctx); ok {
		audited.principal = p
	}
	repo, err := s.repos.GetByName(repoName)
	if err != nil {
		return nil, "", audited.mapWriteError(mapNotFound(err))
	}
	audited.repo = repo
	if repo.Type != "hosted" {
		return nil, "", ErrConflict
	}
	asset, err := s.StageBlob(r, contentType)
	if err != nil {
		return nil, "", err
	}
	asset.RepositoryID, asset.Path = repo.ID, path
	operationID, err := s.publishAssets(repoName, []*repository.Asset{asset}, nil, audited)
	if err != nil {
		return nil, operationID, s.cleanupFailedWrite(asset.BlobHash, err)
	}
	s.negCache.remove(repo.ID, path)
	return asset, operationID, nil
}

// recheckPublishPermission 在完成事务内按实时 ACL 与机器凭据作用域复核公开权限：
// 入口鉴权后若管理员撤销仓库 ACL 或收窄凭据，在途上传不得继续公开。
// 未装配仓库服务或缺少主体快照时保持既有兼容行为（不降级为拒绝，也不新增放行）。
func (a *securityAuditedPublish) recheckPublishPermission(assets []repository.Asset) error {
	p := a.principal
	if p == nil || a.repoSvc == nil || a.repo == nil {
		return nil
	}
	scoped := p.Kind == auth.KindToken && p.TokenPurpose != auth.TokenPurposeLegacy
	if scoped && !auth.MachinePurposeAllows(p.TokenPurpose, auth.MachineActionPublish) {
		return repository.ErrCredentialRejected
	}
	if p.IsAdmin() && !scoped {
		return nil
	}
	var subject repository.Subject
	if !p.IsAdmin() {
		var err error
		subject, err = a.repoSvc.SubjectFor(p.UserID)
		if err != nil {
			return err
		}
	}
	for _, asset := range assets {
		repo := a.repo
		if asset.RepositoryID != repo.ID {
			resolved, err := a.repoSvc.repos.GetByID(asset.RepositoryID)
			if err != nil || resolved == nil {
				return repository.ErrCredentialRejected
			}
			repo = resolved
		}
		if !p.IsAdmin() {
			allowed, err := a.repoSvc.CanAccessResolved(repo, subject, repository.ActionPublish)
			if err != nil {
				return err
			}
			if !allowed {
				return repository.ErrCredentialRejected
			}
		}
		if scoped && !auth.MachineScopeAllowed(p.TokenScopes, asset.RepositoryID, asset.Path, auth.MachineActionPublish) {
			return repository.ErrCredentialRejected
		}
	}
	return nil
}

func (a *securityAuditedPublish) bind(operationID string, items []repository.AssetMutationItem, existing repository.MutationCompletionHook) repository.MutationCompletionHook {
	assets := make([]repository.Asset, 0, len(items))
	for _, item := range items {
		assets = append(assets, *item.After)
	}
	commit := a.audit.CommitHook(a.ctx, operationID, a.actor, assets...)
	return func(tx *sqlx.Tx) error {
		if a.users != nil && a.proof != nil {
			// 排队有权不代表有公开权：最终事务按同一证明重查凭据当前状态。
			// 此刻事务未提交，拒绝是确定失败而非结果未决，不置 failed。
			if _, err := a.users.RevalidateCredentialTx(a.ctx, tx, *a.proof); err != nil {
				return err
			}
		}
		if err := a.recheckPublishPermission(assets); err != nil {
			// 权限复核同样是确定性拒绝：撤销先行则本次公开确定失败。
			return err
		}
		if existing != nil {
			if err := existing(tx); err != nil {
				return err
			}
		}
		if err := commit(tx); err != nil {
			a.failed = true
			return err
		}
		if a.reservation != nil {
			if len(assets) != 1 {
				a.failed = true
				return ErrSecurityAuditUnavailable
			}
			if err := a.reservation.policies.SettlePublishedAssetTx(tx, a.reservation.id, a.actor, assets[0].RepositoryID, assets[0].Size); err != nil {
				a.failed = true
				return err
			}
		}
		return nil
	}
}
