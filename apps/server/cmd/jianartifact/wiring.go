package main

import (
	"errors"
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/api"
	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/blobstore"
	"github.com/wcpe/jianartifact/apps/server/internal/config"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/migration/offindex"
	"github.com/wcpe/jianartifact/apps/server/internal/migration/runner"
	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
	"github.com/wcpe/jianartifact/apps/server/internal/upstream"
)

// appServices 汇集装配后的持久化连接、领域服务与鉴权依赖，供 run 及 CLI 子命令复用。
type appServices struct {
	db                   *persistence.DB
	users                *repository.UserRepo
	authSvc              *domain.AuthService
	userSvc              *domain.UserService
	tokenSvc             *domain.TokenService
	repoSvc              *domain.RepositoryService
	assetSvc             *domain.AssetService
	migrationSvc         *domain.MigrationService
	settingSvc           *domain.SettingService
	replSvc              *domain.ReplicationService
	scheduler            *domain.ReplicationScheduler        // FR-85：配置了对端 URL 时非 nil
	syncLogs             *repository.SyncLogRepo             // FR-88：同步历史日志
	replicationApplyLogs *repository.ReplicationApplyLogRepo // 复制接收审计
	auditLogs            *repository.AuditLogRepo            // FR-38：审计日志
	peerURL              string                              // FR-86：复制对端基址（cfg.SyncPeerURL）
	syncTokenSet         bool                                // FR-86：同步令牌是否已配置（不暴露明文）
	publicURL            string                              // FR-89：静态回退保持空，统一由 setting 动态读取
	upstreamClient       *upstream.Client                    // FR-89：回源客户端（web 改回源超时时 SetTimeout）
	store                auth.Store
	jwt                  *auth.JWTManager
}

// openServices 打开数据库、执行迁移并装配领域服务。调用方负责在返回的 db 上 Close。
func openServices(cfg *config.Config) (*appServices, error) {
	db, err := persistence.Open(cfg.DBPath)
	if err != nil {
		return nil, err
	}
	if err := db.Migrate(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("应用数据库迁移：%w", err)
	}

	userRepo := repository.NewUserRepo(db)
	tokenRepo := repository.NewTokenRepo(db)
	revokedRepo := repository.NewRevokedRepo(db)
	repoRepo := repository.NewRepoRepo(db)
	aclRepo := repository.NewAclRepo(db)
	assetRepo := repository.NewAssetRepo(db)
	migrationTaskRepo := repository.NewMigrationTaskRepo(db)

	jwtMgr := auth.NewJWTManager(cfg.JWTSecret)
	blobs := blobstore.NewStore(cfg.BlobDir)
	upstreamClient := upstream.NewClient(cfg.UpstreamTimeout)
	settingRepo := repository.NewSettingRepo(db)
	settingSvc := domain.NewSettingService(settingRepo)
	repoSvc := domain.NewRepositoryService(repoRepo, aclRepo, assetRepo, settingSvc, userRepo)
	assetSvc := domain.NewAssetService(repoRepo, assetRepo, blobs, upstreamClient)
	userSvc := domain.NewUserService(userRepo)
	tokenSvc := domain.NewTokenService(tokenRepo, userRepo)

	// FR-83：复制变更日志。ReplicationService 作为 ChangeRecorder 注入各写路径 service。
	replSvc := domain.NewReplicationService(
		repository.NewReplChangeRepo(db), assetRepo, repoRepo, aclRepo, userRepo, tokenRepo, repository.NewSettingRepo(db), blobs,
	)
	assetSvc.SetChangeRecorder(replSvc)
	repoSvc.SetChangeRecorder(replSvc)
	userSvc.SetChangeRecorder(replSvc)
	tokenSvc.SetChangeRecorder(replSvc)
	settingSvc.SetChangeRecorder(replSvc)

	// FR-88/89：环境变量仅在 setting 键不存在时写入初始默认；显式清空必须跨重启保留。
	defaults := []repository.SettingValue{
		{Key: domain.SettingKeyReplPeerURL, Value: cfg.SyncPeerURL},
		{Key: domain.SettingKeyReplPeerToken, Value: cfg.SyncToken},
		{Key: domain.SettingKeyPublicURL, Value: cfg.PublicURL},
		{Key: domain.SettingKeyUpstreamTimeout, Value: strconv.Itoa(int(cfg.UpstreamTimeout.Seconds()))},
		{Key: domain.SettingKeyReplSyncInterval, Value: strconv.Itoa(int(cfg.SyncInterval.Seconds()))},
	}
	for _, value := range defaults {
		if err := seedSettingDefault(settingRepo, value.Key, value.Value); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("初始化设置 %s：%w", value.Key, err)
		}
	}
	if secs := settingSvc.UpstreamTimeoutSecs(); secs > 0 {
		upstreamClient.SetTimeout(time.Duration(secs) * time.Second)
	}
	syncLogRepo := repository.NewSyncLogRepo(db)
	replicationApplyLogRepo := repository.NewReplicationApplyLogRepo(db)
	auditLogRepo := repository.NewAuditLogRepo(db)
	client := domain.NewReplicationClient(cfg.SyncPeerURL, cfg.SyncToken, replSvc, blobs, replicationApplyLogRepo)
	scheduler := domain.NewReplicationScheduler(client, settingRepo, syncLogRepo, cfg.SyncInterval)

	// 历史数据全量回填（全量对齐）：为迁移 0010 之前写入的存量实体生成变更日志，
	// 使对端经 since=0 全量拉取即可对齐历史数据。幂等（repl:backfill_done），
	// 失败不阻塞启动（对端重复应用由 LWW 幂等兜底），下次重启重试。
	if err := replSvc.BackfillHistory(); err != nil {
		log.Printf("复制历史回填失败（下次重启重试）：%v", err)
	}

	offlineIndexRepo := repository.NewOfflineIndexRepo(db)
	offlineScanner := offindex.New(offlineIndexRepo)

	migRunner := runner.New(
		runner.TaskStoreAdapter{Repo: migrationTaskRepo},
		runner.AssetServiceAdapter{Assets: assetSvc, Repos: repoRepo, AssetR: assetRepo},
		runner.RepoAdminAdapter{Repos: repoSvc},
	)
	migRunner.SetOfflineIndex(offlineIndexRepo)
	migrationSvc := domain.NewMigrationService(migrationTaskRepo, migRunner)
	migrationSvc.SetOfflineIndex(offlineIndexRepo, offlineScanner)

	// 进程崩溃回收：残留 running → failed，等人 resume（ADR-0012）。
	if _, err := migrationSvc.FailInterruptedRunning(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("回收中断的迁移任务：%w", err)
	}

	return &appServices{
		db:                   db,
		users:                userRepo,
		authSvc:              domain.NewAuthService(userRepo, revokedRepo, jwtMgr),
		userSvc:              userSvc,
		tokenSvc:             tokenSvc,
		repoSvc:              repoSvc,
		assetSvc:             assetSvc,
		migrationSvc:         migrationSvc,
		settingSvc:           settingSvc,
		replSvc:              replSvc,
		scheduler:            scheduler,
		syncLogs:             syncLogRepo,
		replicationApplyLogs: replicationApplyLogRepo,
		auditLogs:            auditLogRepo,
		peerURL:              cfg.SyncPeerURL,
		syncTokenSet:         cfg.SyncToken != "",
		publicURL:            "",
		upstreamClient:       upstreamClient,
		store:                domain.NewAuthStore(userRepo, tokenRepo, revokedRepo),
		jwt:                  jwtMgr,
	}, nil
}

// seedSettingDefault 仅在键缺失且默认值非空时写入，显式空串配置保持不变。
func seedSettingDefault(settings *repository.SettingRepo, key, value string) error {
	if value == "" {
		return nil
	}
	if _, err := settings.Get(key); err == nil {
		return nil
	} else if !errors.Is(err, repository.ErrNotFound) {
		return err
	}
	return settings.Set(key, value)
}

// handlers 用给定版本与就绪检查构造 api.Handlers。
func (s *appServices) handlers(version string, checks []func() error) *api.Handlers {
	return api.NewHandlers(api.Deps{
		Version:              version,
		Checks:               checks,
		Migration:            s.db.CurrentVersion,
		Auth:                 s.authSvc,
		Users:                s.userSvc,
		Tokens:               s.tokenSvc,
		Repos:                s.repoSvc,
		Assets:               s.assetSvc, // FR-103：制品批量删除
		Migrations:           s.migrationSvc,
		Settings:             s.settingSvc,
		Replication:          s.replSvc,
		ReplicationSched:     s.scheduler,
		SyncLogs:             s.syncLogs,
		ReplicationApplyLogs: s.replicationApplyLogs,
		AuditLogs:            s.auditLogs,
		ClusterPeerURL:       s.peerURL,
		ClusterTokenSet:      s.syncTokenSet,
		PublicURL:            s.publicURL,
		OnUpstreamTimeoutChange: func(d time.Duration) {
			if s.upstreamClient != nil {
				s.upstreamClient.SetTimeout(d)
			}
		},
	})
}
