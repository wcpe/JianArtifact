package domain

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"github.com/wcpe/jianartifact/apps/server/internal/auditctx"
	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/blobstore"
	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

const (
	admissionMaxBytes        int64 = 64 << 20
	admissionLifetime              = 7 * 24 * time.Hour
	admissionIntegrityFailed       = "admission_integrity_failed"
	// FR-169 私有暂存容量上限（拟议预算）：超上限返回 admission_limit。
	admissionMaxRepoPendingBytes  int64 = 4 << 30
	admissionMaxTotalPendingBytes int64 = 32 << 30
	admissionMaxRepoPendingCount  int64 = 256
	admissionMaxTotalPendingCount int64 = 4096
	// FR-170 扫描任务：内置完整性检测版本号与租约。
	admissionScanEngine        = "builtin-integrity"
	admissionScanEngineVersion = "1"
	admissionScanRulesVersion  = "1"
	admissionScanParserProfile = "generic/1"
	admissionScanLease         = 60 * time.Second
)

var (
	ErrAdmissionUnauthenticated = errors.New("候选请求没有有效身份")
	ErrAdmissionForbidden       = errors.New("候选仅允许交互会话")
	ErrAdmissionUnavailable     = errors.New("候选状态暂不可确认")
	// ErrAdmissionLimit 私有暂存超出容量或条数上限；映射 413 admission_limit。
	ErrAdmissionLimit = errors.New("私有暂存容量已满")
)

// AdmissionStatus 是公开状态适配器可使用的脱敏投影，不含内部文件键、源 IP 或路径。
type AdmissionStatus struct {
	OperationID      string
	RepositoryID     int64
	State            string
	TargetVersion    int64
	ManifestVersion  *int64
	ManifestDigest   *string
	ExpiresAt        time.Time
	ErrorCode        string
	PublishedAssetID *int64
}

// admissionStatus 只投影白名单字段。

// AdmissionService 登记和追踪私有候选；批准后把字节提升为公开资产（FR-169）。
type AdmissionService struct {
	rows         *repository.AdmissionCandidateRepo
	jobs         *repository.AdmissionScanJobRepo
	repos        *repository.RepoRepo
	users        *repository.UserRepo
	events       *repository.SecurityEventRepo
	blobs        *blobstore.Store
	freeze       *FreezeController
	assets       *AssetService
	bootID       string
	receiving    sync.Map
	intake       chan struct{}
	verification chan struct{}
}

// SetAssetService 注入提升发布所需的资产服务（在首个请求前调用）。
func (s *AdmissionService) SetAssetService(assets *AssetService) { s.assets = assets }

func NewAdmissionService(db *persistence.DB, blobs *blobstore.Store, users *repository.UserRepo, repos *repository.RepoRepo, freeze *FreezeController) *AdmissionService {
	return &AdmissionService{rows: repository.NewAdmissionCandidateRepo(db), jobs: repository.NewAdmissionScanJobRepo(), users: users, repos: repos, events: repository.NewSecurityEventRepo(db), blobs: blobs, freeze: freeze, bootID: rand.Text(), intake: make(chan struct{}, 4), verification: make(chan struct{}, 2)}
}

// Prepare 接收内部调用方交来的已验证会话和 HTTP 来源。registered 只在确认登记后通知操作号，
// 即使之后接收中断也能追踪；返回 staged 则一定已有确认的私有封存记录。
func (s *AdmissionService) Prepare(ctx context.Context, principal *auth.Principal, repositoryID int64, canonicalPath, contentType string, body io.Reader, registered func(string)) (AdmissionStatus, error) {
	if err := checkAdmissionPrincipal(principal); err != nil {
		return AdmissionStatus{}, err
	}
	verified := *principal
	principal = &verified
	source, _ := auditctx.SourceFrom(ctx)
	snapshot, ok := source.Snapshot()
	if !ok {
		return AdmissionStatus{}, ErrAdmissionUnavailable
	}
	if repositoryID <= 0 || body == nil || !validAdmissionPath(canonicalPath) {
		return AdmissionStatus{}, ErrValidation
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	media, params, err := mime.ParseMediaType(contentType)
	if err != nil || len(contentType) > 255 {
		return AdmissionStatus{}, ErrValidation
	}
	contentType = mime.FormatMediaType(media, params)
	if contentType == "" {
		return AdmissionStatus{}, ErrValidation
	}
	if s.freeze == nil {
		return AdmissionStatus{}, ErrAdmissionUnavailable
	}
	if err := s.freeze.RequireBusinessWrite(); err != nil {
		return AdmissionStatus{}, err
	}
	select {
	case s.intake <- struct{}{}:
		defer func() { <-s.intake }()
	default:
		return AdmissionStatus{}, ErrAdmissionUnavailable
	}
	op, err := uuid.NewRandom()
	if err != nil {
		return AdmissionStatus{}, ErrAdmissionUnavailable
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return AdmissionStatus{}, ErrAdmissionUnavailable
	}
	// 会话位：交互会话用 session_id，机器凭据用凭据号占位（CHECK 要求非空）。
	sessionID := principal.SessionID
	if sessionID == "" && principal.Kind == auth.KindToken {
		sessionID = "token:" + strconv.FormatInt(principal.TokenID, 10)
	}
	row := repository.AdmissionCandidateRow{OperationID: op.String(), ObjectKey: blobstore.NewRegisteredPrivateCandidateKey(random), BootID: s.bootID, RepositoryID: repositoryID,
		ActorUserID: principal.UserID, SessionID: sessionID, AccountEpoch: principal.AccountEpoch, AuthMethod: principal.AuthSource, OriginRequestID: snapshot.OriginRequestID, ClientIP: snapshot.ClientIP.String(),
		ProxyTrust: string(snapshot.ProxyTrust), SourceTrustVersion: snapshot.SourceTrustVersion, CanonicalPath: canonicalPath, ContentType: contentType, State: "receiving", TargetVersion: 1,
		ExpiresUnix: time.Now().UTC().Add(admissionLifetime).Unix()}
	s.receiving.Store(row.OperationID, struct{}{})
	defer s.receiving.Delete(row.OperationID)
	err = s.rows.Write(ctx, func(tx *sqlx.Tx) error {
		_, _, err := s.authorize(ctx, tx, principal, row.RepositoryID)
		if err != nil {
			return err
		}
		return s.rows.InsertTx(ctx, tx, row)
	}, s.commitWhenWritable)
	if errors.Is(err, repository.ErrAdmissionCommitUnknown) {
		observed, readErr := s.observeCommit(ctx, row)
		if readErr != nil || observed.State != "receiving" || observed.StagedEventID != nil {
			return AdmissionStatus{}, ErrAdmissionUnavailable
		}
		err = nil
	}
	if err != nil {
		return AdmissionStatus{}, admissionServiceError(err)
	}
	if registered != nil {
		registered(row.OperationID)
	}
	// 已登记的文件即使接收失败也不作为成功；不尝试删除已封存或结果未知的文件。
	object, err := s.blobs.StageRegisteredPrivateCandidate(ctx, row.ObjectKey, body, admissionMaxBytes)
	if err != nil {
		return AdmissionStatus{OperationID: row.OperationID}, err
	}
	digest, size := object.SHA256(), object.Size()
	repo, repoErr := s.repoByID(ctx, row.RepositoryID)
	if repoErr != nil {
		return AdmissionStatus{OperationID: row.OperationID}, ErrAdmissionUnavailable
	}
	manifest := admissionManifestDigest(row, repo.Format, digest, size)
	eventID := repository.NewSecurityEventID()
	row.ManifestDigest, row.BlobHash, row.SizeBytes, row.StagedEventID = &manifest, &digest, &size, &eventID
	err = s.rows.Write(ctx, func(tx *sqlx.Tx) error {
		user, repo, err := s.authorize(ctx, tx, principal, row.RepositoryID)
		if err != nil {
			return err
		}
		if time.Now().Unix() >= row.ExpiresUnix {
			return ErrAdmissionUnavailable
		}
		repoBytes, totalBytes, repoCount, totalCount, sumErr := s.rows.SumPendingTx(ctx, tx, row.RepositoryID)
		if sumErr != nil {
			return sumErr
		}
		// 自身 receiving 行不计入既有占用；超任一上限拒绝封存。
		if repoBytes+size > admissionMaxRepoPendingBytes || totalBytes+size > admissionMaxTotalPendingBytes ||
			repoCount > admissionMaxRepoPendingCount || totalCount > admissionMaxTotalPendingCount {
			return ErrAdmissionLimit
		}
		if err := s.rows.SealTx(ctx, tx, row); err != nil {
			return err
		}
		event := securityAuditEvent(ctx, row.ActorUserID)
		event.EventID, event.EventVersion, event.EventType = eventID, 1, repository.SecurityEventAdmissionCandidateStaged
		event.OccurredAt = time.Now().UTC()
		event.ActorNameSnapshot, event.AuthMethod = repository.SecurityEventActorNameSnapshot(user.Username), row.AuthMethod
		// 持久凭据层的会话引用留在候选登记中，不假定其编码满足审计对外词表。
		event.CredentialID, event.SessionID = "", ""
		event.RepositoryID, event.OperationID = &row.RepositoryID, row.OperationID
		event.Outcome, event.Committed = "succeeded", "committed"
		return s.events.InsertTx(ctx, tx, event, []repository.SecurityEventObject{{Ordinal: 1, RepositoryID: &row.RepositoryID, CanonicalPath: row.CanonicalPath,
			ObjectKind: "asset", Digest: "sha256:" + digest, SizeBytes: &size, ManifestDigest: manifest, RepositoryNameSnapshot: repo.Name}})
	}, s.commitWhenWritable)
	if errors.Is(err, repository.ErrAdmissionCommitUnknown) {
		observed, readErr := s.observeCommit(ctx, row)
		if readErr == nil && observed.State == "staged" && observed.StagedEventID != nil && *observed.StagedEventID == eventID && observed.ManifestDigest != nil && *observed.ManifestDigest == manifest {
			return admissionStatus(observed), nil
		}
	}
	if err != nil {
		return AdmissionStatus{OperationID: row.OperationID}, admissionServiceError(err)
	}
	version := int64(1)
	row.State, row.TargetVersion, row.ManifestVersion = "staged", 2, &version
	return admissionStatus(row), nil
}

// Status 先复核归属和当前发布权限，再做有界单对象对账；不在启动扫描所有文件。
func (s *AdmissionService) Status(ctx context.Context, principal *auth.Principal, operationID string) (AdmissionStatus, error) {
	if err := checkAdmissionPrincipal(principal); err != nil {
		return AdmissionStatus{}, err
	}
	row, err := s.authorizedRow(ctx, principal, operationID)
	if err != nil {
		return AdmissionStatus{}, admissionServiceError(err)
	}
	switch row.State {
	case "receiving":
		if _, active := s.receiving.Load(operationID); row.BootID == s.bootID && active {
			return AdmissionStatus{}, ErrAdmissionUnavailable
		}
		// 单实例重启或同实例接收失败：不能由遗留文件推导接收成功。
		return s.transitionStatus(ctx, principal, row, "rejected", "")
	case "staged":
		repo, repoErr := s.repoByID(ctx, row.RepositoryID)
		if repoErr != nil || row.BlobHash == nil || row.SizeBytes == nil || *row.SizeBytes > admissionMaxBytes || row.ManifestVersion == nil || *row.ManifestVersion != 1 ||
			row.ManifestDigest == nil || *row.ManifestDigest != admissionManifestDigest(row, repo.Format, *row.BlobHash, *row.SizeBytes) {
			return AdmissionStatus{}, ErrAdmissionUnavailable
		}
		// FR-170：检测经持久 scan_job 认领；租约带递增代次与墙钟。
		workerID := "main-" + s.bootID
		var job repository.AdmissionScanJobRow
		var claimed bool
		now := time.Now().UTC()
		err = s.rows.Write(ctx, func(tx *sqlx.Tx) error {
			latest, e := s.authorizedRowTx(ctx, tx, principal, operationID)
			if e != nil || latest.State != "staged" || latest.TargetVersion != row.TargetVersion {
				return ErrAdmissionUnavailable
			}
			if _, e = s.jobs.EnqueueTx(ctx, tx, repository.AdmissionScanJobRow{
				JobID: uuid.NewString(), OperationID: operationID, ManifestVersion: *row.ManifestVersion,
				Engine: admissionScanEngine, EngineVersion: admissionScanEngineVersion,
				RulesVersion: admissionScanRulesVersion, ParserProfileVersion: admissionScanParserProfile,
				NextRunAtUnix: now.Unix(), CreatedUnix: now.Unix(), UpdatedUnix: now.Unix(),
			}); e != nil {
				return e
			}
			var e2 error
			job, claimed, e2 = s.jobs.ClaimTx(ctx, tx, operationID, *row.ManifestVersion, workerID, admissionScanLease, now)
			return e2
		}, s.commitWhenWritable)
		if err != nil {
			return AdmissionStatus{}, admissionServiceError(err)
		}
		if !claimed {
			// 他人持有有效租约或尝试预算耗尽：无公开结果。
			return AdmissionStatus{}, ErrAdmissionUnavailable
		}
		select {
		case s.verification <- struct{}{}:
		default:
			_ = s.rows.Write(ctx, func(tx *sqlx.Tx) error {
				_, e := s.jobs.RequeueTx(ctx, tx, job.JobID, job.LeaseGeneration, time.Now().UTC().Add(5*time.Second), "scan_busy", time.Now().UTC())
				return e
			}, s.commitWhenWritable)
			return AdmissionStatus{}, ErrAdmissionUnavailable
		}
		verifyErr := s.blobs.VerifyRegisteredPrivateCandidate(ctx, row.ObjectKey, *row.BlobHash, *row.SizeBytes)
		<-s.verification
		// 完成事务：租约代次栅栏 + 事实追加 + 候选转态（FR-170 §3.2）。
		verdict, integrity, errCode := "approved", "passed", ""
		switch {
		case errors.Is(verifyErr, blobstore.ErrPrivateCandidateIntegrity):
			verdict, integrity, errCode = "rejected", "failed", admissionIntegrityFailed
		case verifyErr != nil:
			verdict, integrity = "", "unknown" // 可重试故障退回 queued
		case time.Now().Unix() >= row.ExpiresUnix:
			verdict = "" // 到期走候选 expired 路径
		}
		var outcome AdmissionStatus
		werr := s.rows.Write(ctx, func(tx *sqlx.Tx) error {
			if integrity != "unknown" {
				factOrdinal, e := s.jobs.CountFactsTx(ctx, tx, job.JobID)
				if e != nil {
					return e
				}
				completed, missing := int64(1), "[]"
				if integrity != "passed" {
					completed, missing = 0, `["integrity"]`
				}
				fv := verdict
				if fv == "" {
					fv = "rejected"
				}
				if e = s.jobs.InsertFactTx(ctx, tx, repository.AdmissionScanFactRow{
					JobID: job.JobID, Ordinal: factOrdinal + 1, LeaseGeneration: job.LeaseGeneration,
					ObjectDigest: *row.BlobHash, CoverageRequired: 1,
					CoverageCompleted: completed, CoverageMissing: missing, Integrity: integrity,
					Verdict: fv, Findings: "[]", WorkerID: workerID, ObservedAtUnix: time.Now().UTC().Unix(),
				}); e != nil {
					return e
				}
			}
			if verdict == "" {
				// 可重试故障或到期：退回队列（到期由下方候选转态处理）。
				ok, e := s.jobs.RequeueTx(ctx, tx, job.JobID, job.LeaseGeneration, time.Now().UTC(), "retryable", time.Now().UTC())
				if e != nil {
					return e
				}
				if !ok {
					return ErrAdmissionUnavailable // 代次不匹配：迟到结果不生效
				}
				return nil
			}
			ok, e := s.jobs.CompleteTx(ctx, tx, job.JobID, job.LeaseGeneration, verdict, errCode, time.Now().UTC())
			if e != nil {
				return e
			}
			if !ok {
				return ErrAdmissionUnavailable // 租约被接管：本次结果作废
			}
			if verdict == "rejected" {
				outcome, e = s.transitionStatusTx(ctx, tx, principal, row, "rejected", admissionIntegrityFailed)
				return e
			}
			outcome, e = s.transitionStatusTx(ctx, tx, principal, row, "awaiting_review", "")
			return e
		}, s.commitWhenWritable)
		if werr != nil {
			if verifyErr != nil && !errors.Is(verifyErr, blobstore.ErrPrivateCandidateIntegrity) {
				return AdmissionStatus{}, ErrAdmissionUnavailable
			}
			return AdmissionStatus{}, admissionServiceError(werr)
		}
		if verifyErr != nil && !errors.Is(verifyErr, blobstore.ErrPrivateCandidateIntegrity) {
			return AdmissionStatus{}, ErrAdmissionUnavailable
		}
		if time.Now().Unix() >= row.ExpiresUnix && verdict == "" {
			return s.transitionStatus(ctx, principal, row, "expired", "")
		}
		return outcome, nil
	case "awaiting_review":
		if time.Now().Unix() >= row.ExpiresUnix {
			return s.transitionStatus(ctx, principal, row, "expired", "")
		}
		return admissionStatus(row), nil
	case "rejected", "expired", "published":
		return admissionStatus(row), nil
	default:
		return AdmissionStatus{}, ErrAdmissionUnavailable
	}
}

func (s *AdmissionService) authorizedRow(ctx context.Context, p *auth.Principal, operationID string) (row repository.AdmissionCandidateRow, err error) {
	err = s.rows.Read(ctx, func(tx *sqlx.Tx) error {
		var readErr error
		row, readErr = s.authorizedRowTx(ctx, tx, p, operationID)
		return readErr
	})
	return row, err
}

func (s *AdmissionService) authorizedRowTx(ctx context.Context, tx *sqlx.Tx, p *auth.Principal, operationID string) (repository.AdmissionCandidateRow, error) {
	// 凭据复验先于格式和归属裁决，且凭据、候选和发布权限使用同一事务视图。
	user, err := s.revalidatePrincipal(ctx, tx, p)
	if err != nil {
		return repository.AdmissionCandidateRow{}, err
	}
	id, err := uuid.Parse(operationID)
	if err != nil || id.Version() != 4 || id.String() != operationID {
		return repository.AdmissionCandidateRow{}, repository.ErrNotFound
	}
	row, err := s.rows.GetTx(ctx, tx, operationID, user.ID)
	if err != nil {
		return repository.AdmissionCandidateRow{}, err
	}
	if _, err := s.authorizeRepository(ctx, tx, user, row.RepositoryID); err != nil {
		return repository.AdmissionCandidateRow{}, err
	}
	return row, nil
}

func (s *AdmissionService) authorize(ctx context.Context, tx *sqlx.Tx, p *auth.Principal, repoID int64) (*repository.User, *repository.Repository, error) {
	user, err := s.revalidatePrincipal(ctx, tx, p)
	if err != nil {
		return nil, nil, err
	}
	repo, err := s.authorizeRepository(ctx, tx, user, repoID)
	return user, repo, err
}

func (s *AdmissionService) revalidatePrincipal(ctx context.Context, tx *sqlx.Tx, p *auth.Principal) (*repository.User, error) {
	if err := checkAdmissionPrincipal(p); err != nil {
		return nil, err
	}
	user, err := s.users.RevalidateCredentialTx(ctx, tx, p.CredentialProof())
	if errors.Is(err, repository.ErrCredentialRejected) {
		return nil, ErrAdmissionUnauthenticated
	}
	if err != nil {
		return nil, err
	}
	if user.ID != p.UserID || user.WebLoginDisabled {
		return nil, ErrAdmissionForbidden
	}
	return user, nil
}

func (s *AdmissionService) authorizeRepository(ctx context.Context, tx *sqlx.Tx, user *repository.User, repoID int64) (*repository.Repository, error) {
	repo, err := s.repos.GetByIDTx(tx, repoID)
	if err != nil {
		return nil, err
	}
	if repo.Type != "hosted" || !admissionFormatAllowed(repo.Format) || !repo.Online {
		return nil, repository.ErrNotFound
	}
	if user.Role != "admin" {
		allowed, err := repository.AdmissionPublishGrantTx(ctx, tx, repo.ID, user.ID)
		if err != nil {
			return nil, err
		}
		if !allowed {
			return nil, repository.ErrNotFound
		}
	}
	return repo, nil
}

func (s *AdmissionService) transitionStatus(ctx context.Context, p *auth.Principal, row repository.AdmissionCandidateRow, state, code string) (AdmissionStatus, error) {
	err := s.rows.Write(ctx, func(tx *sqlx.Tx) error {
		current, err := s.authorizedRowTx(ctx, tx, p, row.OperationID)
		if err != nil {
			return err
		}
		if current.RepositoryID != row.RepositoryID || current.TargetVersion != row.TargetVersion || current.State != row.State {
			return repository.ErrNotFound
		}
		return s.rows.TransitionTx(ctx, tx, row, state, code)
	}, s.commitWhenWritable)
	if errors.Is(err, repository.ErrAdmissionCommitUnknown) || errors.Is(err, repository.ErrNotFound) {
		observed, readErr := s.authorizedRow(ctx, p, row.OperationID)
		if readErr != nil {
			return AdmissionStatus{}, admissionServiceError(readErr)
		}
		if observed.State == state && observed.ErrorCode == code && observed.TargetVersion == row.TargetVersion+1 {
			return admissionStatus(observed), nil
		}
		return AdmissionStatus{}, ErrAdmissionUnavailable
	}
	if err != nil {
		return AdmissionStatus{}, admissionServiceError(err)
	}
	row.State, row.ErrorCode, row.TargetVersion = state, code, row.TargetVersion+1
	return admissionStatus(row), nil
}

// transitionStatusTx 在既有写事务内复核归属/版本/当前态后转态（FR-170 扫描完成同事务）。
func (s *AdmissionService) transitionStatusTx(ctx context.Context, tx *sqlx.Tx, p *auth.Principal, row repository.AdmissionCandidateRow, state, code string) (AdmissionStatus, error) {
	current, err := s.authorizedRowTx(ctx, tx, p, row.OperationID)
	if err != nil {
		return AdmissionStatus{}, err
	}
	if current.RepositoryID != row.RepositoryID || current.TargetVersion != row.TargetVersion || current.State != row.State {
		return AdmissionStatus{}, repository.ErrNotFound
	}
	if err := s.rows.TransitionTx(ctx, tx, row, state, code); err != nil {
		return AdmissionStatus{}, err
	}
	row.State, row.ErrorCode, row.TargetVersion = state, code, row.TargetVersion+1
	return admissionStatus(row), nil
}

// transitionStatusAdmin 与 transitionStatus 同构，但按操作号取行（管理员可处置他人候选）。
func (s *AdmissionService) transitionStatusAdmin(ctx context.Context, row repository.AdmissionCandidateRow, state, code string) (AdmissionStatus, error) {
	err := s.rows.Write(ctx, func(tx *sqlx.Tx) error {
		current, err := s.rows.GetAnyTx(ctx, tx, row.OperationID)
		if err != nil {
			return err
		}
		if current.RepositoryID != row.RepositoryID || current.TargetVersion != row.TargetVersion || current.State != row.State {
			return repository.ErrNotFound
		}
		return s.rows.TransitionTx(ctx, tx, row, state, code)
	}, s.commitWhenWritable)
	if errors.Is(err, repository.ErrAdmissionCommitUnknown) || errors.Is(err, repository.ErrNotFound) {
		var observed repository.AdmissionCandidateRow
		readErr := s.rows.Read(ctx, func(tx *sqlx.Tx) error {
			r, err := s.rows.GetAnyTx(ctx, tx, row.OperationID)
			if err == nil {
				observed = r
			}
			return err
		})
		if readErr != nil {
			return AdmissionStatus{}, admissionServiceError(readErr)
		}
		if observed.State == state && observed.ErrorCode == code && observed.TargetVersion == row.TargetVersion+1 {
			return admissionStatus(observed), nil
		}
		return AdmissionStatus{}, ErrAdmissionUnavailable
	}
	if err != nil {
		return AdmissionStatus{}, admissionServiceError(err)
	}
	row.State, row.ErrorCode, row.TargetVersion = state, code, row.TargetVersion+1
	return admissionStatus(row), nil
}

func (s *AdmissionService) observeCommit(ctx context.Context, row repository.AdmissionCandidateRow) (observed repository.AdmissionCandidateRow, err error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	err = s.rows.Read(ctx, func(tx *sqlx.Tx) error {
		var readErr error
		observed, readErr = s.rows.GetTx(ctx, tx, row.OperationID, row.ActorUserID)
		if readErr == nil && (observed.ObjectKey != row.ObjectKey || observed.RepositoryID != row.RepositoryID || observed.BootID != row.BootID) {
			return ErrAdmissionUnavailable
		}
		return readErr
	})
	return observed, err
}

func (s *AdmissionService) commitWhenWritable(commit func() error) error {
	if s.freeze == nil {
		return ErrAdmissionUnavailable
	}
	// 只把短数据库提交与冻结线性排序，不在此边界内接收/校验文件。
	s.freeze.mu.RLock()
	defer s.freeze.mu.RUnlock()
	if s.freeze.snapshotLocked().Frozen {
		return ErrWriteFrozen
	}
	if s.freeze.base != nil {
		if err := s.freeze.base.RequireBusinessWrite(); err != nil {
			return err
		}
	}
	return commit()
}

func checkAdmissionPrincipal(p *auth.Principal) error {
	if p == nil || p.UserID <= 0 {
		return ErrAdmissionUnauthenticated
	}
	switch {
	case p.Kind == auth.KindSession && p.AuthSource == auth.AuthSourceWebJWT:
		if p.WebLoginDisabled {
			return ErrAdmissionForbidden
		}
		if p.JTI == "" || p.SessionID == "" {
			return ErrAdmissionUnauthenticated
		}
	case p.Kind == auth.KindToken && p.TokenID > 0:
		// 机器凭据：凭据位由 TokenID 承载，协议写入口允许提交候选。
	case p.Kind == auth.KindInternal:
		// 内部批写入口（迁移导入）：身份绑定任务发起人，
		// 每次接收仍按当前 ACL 复核发布权。
	default:
		return ErrAdmissionForbidden
	}
	return nil
}

func admissionServiceError(err error) error {
	for _, known := range []error{ErrAdmissionUnauthenticated, ErrAdmissionForbidden, ErrAdmissionUnavailable, ErrAdmissionLimit, repository.ErrNotFound, ErrWriteFrozen, context.Canceled, context.DeadlineExceeded} {
		if errors.Is(err, known) {
			return known
		}
	}
	return ErrAdmissionUnavailable
}

func admissionStatus(r repository.AdmissionCandidateRow) AdmissionStatus {
	return AdmissionStatus{OperationID: r.OperationID, RepositoryID: r.RepositoryID, State: r.State, TargetVersion: r.TargetVersion, ManifestVersion: r.ManifestVersion,
		ManifestDigest: r.ManifestDigest, ExpiresAt: time.Unix(r.ExpiresUnix, 0).UTC(), ErrorCode: r.ErrorCode, PublishedAssetID: r.PublishedAssetID}
}

// 准入候选的规范路径比审计事件路径更严：审计须覆盖制品键的全部合法字符（含
// `#` 这类文件名），候选登记则不接受 `#`——其在若干协议坐标中另有含义，
// 入选范围按规范收窄而不是随制品键全集放行。
func validAdmissionPath(p string) bool {
	return repository.ValidSecurityEventPath(p) && !strings.Contains(p, "#") && path.Clean(p) == p
}

func admissionManifestDigest(r repository.AdmissionCandidateRow, format, hash string, size int64) string {
	// 固定顺序、类型与编码版本；输入摘要不会被误作完整清单摘要。
	data, _ := json.Marshal([]any{"jianartifact.private-raw-manifest.v1", r.RepositoryID, format, "asset.put", r.CanonicalPath, "input", r.ContentType, "sha256:" + hash, size})
	return fmt.Sprintf("sha256:%x", sha256.Sum256(data))
}

// ── FR-169 最小准入流水线：检测结论 + 管理员审批 + 提升发布 ─────────────────

var ErrAdmissionDecisionRejected = errors.New("准入裁决被拒")

// Decide 管理员处置候选：awaiting_review → approve（提升字节为公开资产）或 reject。
// reason 必填；批准在最终事务内先复核仓库与权限，再把状态与资产引用同事务裁决。
func (s *AdmissionService) Decide(ctx context.Context, p *auth.Principal, operationID, action, reason string) (AdmissionStatus, error) {
	if p == nil || p.UserID <= 0 || p.Role != "admin" {
		return AdmissionStatus{}, ErrAdmissionForbidden
	}
	if reason == "" || (action != "approve" && action != "reject") {
		return AdmissionStatus{}, ErrValidation
	}
	var row repository.AdmissionCandidateRow
	err := s.rows.Read(ctx, func(tx *sqlx.Tx) error {
		r, err := s.rows.GetAnyTx(ctx, tx, operationID)
		if err != nil {
			return err
		}
		row = r
		return nil
	})
	if err != nil {
		return AdmissionStatus{}, repository.ErrNotFound
	}
	if row.State != "awaiting_review" {
		return AdmissionStatus{}, ErrAdmissionDecisionRejected
	}
	if row.BlobHash == nil || row.SizeBytes == nil {
		return AdmissionStatus{}, ErrAdmissionUnavailable
	}
	if action == "reject" {
		return s.transitionStatusAdmin(ctx, row, "rejected", "manual_reject")
	}
	// approve：隔离区外先做字节搬运（私有→内容寻址活动存储），再同事务标 published。
	if s.assets == nil {
		return AdmissionStatus{}, ErrAdmissionUnavailable
	}
	if err := s.blobs.VerifyRegisteredPrivateCandidate(ctx, row.ObjectKey, *row.BlobHash, *row.SizeBytes); err != nil {
		return AdmissionStatus{}, admissionServiceError(err)
	}
	rc, size, err := s.blobs.OpenRegisteredPrivateCandidate(ctx, row.ObjectKey)
	if err != nil {
		return AdmissionStatus{}, ErrAdmissionUnavailable
	}
	defer rc.Close()
	repo, err := s.repoByID(ctx, row.RepositoryID)
	if err != nil {
		return AdmissionStatus{}, ErrAdmissionUnavailable
	}
	asset, err := s.assets.Put(repo.Name, row.CanonicalPath, io.LimitReader(rc, size), row.ContentType)
	if err != nil {
		return AdmissionStatus{}, admissionServiceError(err)
	}
	row.PublishedAssetID = &asset.ID
	return s.transitionStatusAdmin(ctx, row, "published", "")
}

func (s *AdmissionService) repoByID(ctx context.Context, id int64) (*repository.Repository, error) {
	return s.repos.GetByID(id)
}

// ErrAdmissionIntegrityRejected 表示隔离检测裁定硬完整性失败；上传方得到非成功响应。
var ErrAdmissionIntegrityRejected = errors.New("候选完整性裁定失败")

// IntakeCandidate 是 Raw hosted 写入的准入入口：私有接收→隔离完整性裁定。
// 返回处于 awaiting_review 的候选行；受损内容落 rejected 且不返回字节。
func (s *AdmissionService) IntakeCandidate(ctx context.Context, principal *auth.Principal, repositoryID int64, canonicalPath, contentType string, body io.Reader) (repository.AdmissionCandidateRow, error) {
	status, err := s.Prepare(ctx, principal, repositoryID, canonicalPath, contentType, body, nil)
	if err != nil {
		return repository.AdmissionCandidateRow{}, err
	}
	status, err = s.Status(ctx, principal, status.OperationID)
	if err != nil {
		return repository.AdmissionCandidateRow{}, err
	}
	switch status.State {
	case "awaiting_review":
	case "rejected":
		return repository.AdmissionCandidateRow{}, ErrAdmissionIntegrityRejected
	default:
		return repository.AdmissionCandidateRow{}, ErrAdmissionUnavailable
	}
	row, err := s.authorizedRow(ctx, principal, status.OperationID)
	if err != nil {
		return repository.AdmissionCandidateRow{}, admissionServiceError(err)
	}
	return row, nil
}

// IntakeInternalCandidate 是内部批写入口的接收变体：HTTP 上下文没有来源快照时，
// 由调用方以任务记录的发起快照显式注入（迁移导入等后台写入口，FR-169 §3.6）。
func (s *AdmissionService) IntakeInternalCandidate(ctx context.Context, principal *auth.Principal, source auditctx.Source, repositoryID int64, canonicalPath, contentType string, body io.Reader) (repository.AdmissionCandidateRow, error) {
	return s.IntakeCandidate(auditctx.WithSource(ctx, source), principal, repositoryID, canonicalPath, contentType, body)
}

// PromoteBytes 复核完整性后打开封存字节供提升读取；调用方负责发布及 MarkPublished。
func (s *AdmissionService) PromoteBytes(ctx context.Context, row repository.AdmissionCandidateRow) (io.ReadCloser, int64, error) {
	if row.State != "awaiting_review" || row.BlobHash == nil || row.SizeBytes == nil {
		return nil, 0, ErrAdmissionUnavailable
	}
	if err := s.blobs.VerifyRegisteredPrivateCandidate(ctx, row.ObjectKey, *row.BlobHash, *row.SizeBytes); err != nil {
		return nil, 0, ErrAdmissionUnavailable
	}
	rc, size, err := s.blobs.OpenRegisteredPrivateCandidate(ctx, row.ObjectKey)
	if err != nil {
		return nil, 0, ErrAdmissionUnavailable
	}
	return rc, size, nil
}

// MarkPublished 在提升字节已被发布链提交后同事务置 published 并持久化资产号。
func (s *AdmissionService) MarkPublished(ctx context.Context, principal *auth.Principal, row repository.AdmissionCandidateRow, assetID int64) (AdmissionStatus, error) {
	row.PublishedAssetID = &assetID
	return s.transitionStatus(ctx, principal, row, "published", "")
}

// admissionFormatAllowed 是获准走私有候选准入的协议格式集合（FR-169 已接入口径）。
func admissionFormatAllowed(format string) bool { return AdmissionFormatAllowed(format) }

// AdmissionFormatAllowed 是获准走私有候选准入的协议格式集合（FR-169 已接入口径）。
func AdmissionFormatAllowed(format string) bool {
	switch format {
	case "raw", "maven", "cargo", "npm", "docker", "nuget", "pypi":
		return true
	}
	return false
}
