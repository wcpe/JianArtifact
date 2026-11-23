package domain

import (
	"context"
	"errors"
	"io"

	"github.com/jmoiron/sqlx"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// ErrSecurityAuditUnavailable 表示关键发布无法记录必要的安全事实。
var ErrSecurityAuditUnavailable = errors.New("安全审计暂不可用")

type securityAuditedPublish struct {
	ctx         context.Context
	audit       *SecurityAuditService
	actor       int64
	reservation *PublishCommitReservation
	failed      bool
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
	audited := &securityAuditedPublish{ctx: ctx, audit: audit, actor: actorUserID, reservation: reservation}
	repo, err := s.repos.GetByName(repoName)
	if err != nil {
		return nil, "", audited.mapWriteError(mapNotFound(err))
	}
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

func (a *securityAuditedPublish) bind(operationID string, items []repository.AssetMutationItem, existing repository.MutationCompletionHook) repository.MutationCompletionHook {
	assets := make([]repository.Asset, 0, len(items))
	for _, item := range items {
		assets = append(assets, *item.After)
	}
	commit := a.audit.CommitHook(a.ctx, operationID, a.actor, assets...)
	return func(tx *sqlx.Tx) error {
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
