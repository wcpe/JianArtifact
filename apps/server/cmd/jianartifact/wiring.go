package main

import (
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
	db             *persistence.DB
	users          *repository.UserRepo
	authSvc        *domain.AuthService
	userSvc        *domain.UserService
	tokenSvc       *domain.TokenService
	repoSvc        *domain.RepositoryService
	assetSvc       *domain.AssetService
	migrationSvc   *domain.MigrationService
	settingSvc     *domain.SettingService
	replSvc        *domain.ReplicationService
	scheduler      *domain.ReplicationScheduler // FR-85：配置了对端 URL 时非 nil
	syncLogs       *repository.SyncLogRepo      // FR-88：同步历史日志
	auditLogs      *repository.AuditLogRepo     // FR-38：审计日志
	peerURL        string                       // FR-86：复制对端基址（cfg.SyncPeerURL）
	syncTokenSet   bool                         // FR-86：同步令牌是否已配置（不暴露明文）
	publicURL      string                       // FR-87：对外基础 URL（cfg.PublicURL，CDN 域名）
	upstreamClient *upstream.Client             // FR-89：回源客户端（web 改回源超时时 SetTimeout）
	store          auth.Store
	jwt            *auth.JWTManager
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
	settingSvc := domain.NewSettingService(repository.NewSettingRepo(db))
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

	// FR-88：对端配置从 setting 读取（web 可配置），调度器常驻。
	// 环境变量作为初始默认：setting 中不存在或值为空时写入（可被 web 覆盖）。
	// 空字符串不代表有效配置（web 保存对端 URL 时可能留下空令牌），需被环境变量兜底。
	settingRepo := repository.NewSettingRepo(db)
	if cfg.SyncPeerURL != "" {
		if v, err := settingRepo.Get(domain.SettingKeyReplPeerURL); err != nil || v == "" {
			_ = settingRepo.Set(domain.SettingKeyReplPeerURL, cfg.SyncPeerURL)
		}
	}
	if cfg.SyncToken != "" {
		if v, err := settingRepo.Get(domain.SettingKeyReplPeerToken); err != nil || v == "" {
			_ = settingRepo.Set(domain.SettingKeyReplPeerToken, cfg.SyncToken)
		}
	}
	// FR-89：基础配置 env 兜底写 setting（web 可运行时覆盖），缺省由读取侧回退。
	// 仅当 setting 缺失或为空时写入，web 显式配置优先（与对端 URL/令牌同待遇）。
	if v, err := settingRepo.Get(domain.SettingKeyPublicURL); err != nil || v == "" {
		_ = settingRepo.Set(domain.SettingKeyPublicURL, cfg.PublicURL)
	}
	if v, err := settingRepo.Get(domain.SettingKeyUpstreamTimeout); err != nil || v == "" {
		_ = settingRepo.Set(domain.SettingKeyUpstreamTimeout, strconv.Itoa(int(cfg.UpstreamTimeout.Seconds())))
	}
	if v, err := settingRepo.Get(domain.SettingKeyReplSyncInterval); err != nil || v == "" {
		_ = settingRepo.Set(domain.SettingKeyReplSyncInterval, strconv.Itoa(int(cfg.SyncInterval.Seconds())))
	}
	client := domain.NewReplicationClient(cfg.SyncPeerURL, cfg.SyncToken, replSvc, blobs)
	syncLogRepo := repository.NewSyncLogRepo(db)
	auditLogRepo := repository.NewAuditLogRepo(db)
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
		db:             db,
		users:          userRepo,
		authSvc:        domain.NewAuthService(userRepo, revokedRepo, jwtMgr),
		userSvc:        userSvc,
		tokenSvc:       tokenSvc,
		repoSvc:        repoSvc,
		assetSvc:       assetSvc,
		migrationSvc:   migrationSvc,
		settingSvc:     settingSvc,
		replSvc:        replSvc,
		scheduler:      scheduler,
		syncLogs:       syncLogRepo,
		auditLogs:      auditLogRepo,
		peerURL:        cfg.SyncPeerURL,
		syncTokenSet:   cfg.SyncToken != "",
		publicURL:      cfg.PublicURL,
		upstreamClient: upstreamClient,
		store:          domain.NewAuthStore(userRepo, tokenRepo, revokedRepo),
		jwt:            jwtMgr,
	}, nil
}

// handlers 用给定版本与就绪检查构造 api.Handlers。
func (s *appServices) handlers(version string, checks []func() error) *api.Handlers {
	return api.NewHandlers(api.Deps{
		Version:          version,
		Checks:           checks,
		Migration:        s.db.CurrentVersion,
		Auth:             s.authSvc,
		Users:            s.userSvc,
		Tokens:           s.tokenSvc,
		Repos:            s.repoSvc,
		Migrations:       s.migrationSvc,
		Settings:         s.settingSvc,
		Replication:      s.replSvc,
		ReplicationSched: s.scheduler,
		SyncLogs:         s.syncLogs,
		AuditLogs:        s.auditLogs,
		ClusterPeerURL:   s.peerURL,
		ClusterTokenSet:  s.syncTokenSet,
		PublicURL:        s.publicURL,
		OnUpstreamTimeoutChange: func(d time.Duration) {
			if s.upstreamClient != nil {
				s.upstreamClient.SetTimeout(d)
			}
		},
	})
}
