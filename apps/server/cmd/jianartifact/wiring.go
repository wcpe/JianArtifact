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
	"github.com/wcpe/jianartifact/apps/server/internal/migration/credential"
	"github.com/wcpe/jianartifact/apps/server/internal/migration/offindex"
	"github.com/wcpe/jianartifact/apps/server/internal/migration/runner"
	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
	"github.com/wcpe/jianartifact/apps/server/internal/upstream"
)

// appServices 汇集装配后的持久化连接、领域服务与鉴权依赖，供 run 及 CLI 子命令复用。
type appServices struct {
	db                  *persistence.DB
	users               *repository.UserRepo
	authSvc             *domain.AuthService
	userSvc             *domain.UserService
	tokenSvc            *domain.TokenService
	repoSvc             *domain.RepositoryService
	cargoSvc            *domain.CargoService
	ociSvc              *domain.OCIService
	formatMetadataSvc   *domain.FormatMetadataService
	publishPolicySvc    *domain.PublishPolicyService
	assetSvc            *domain.AssetService
	migrationSvc        *domain.MigrationService
	settingSvc          *domain.SettingService
	auditLogs           *repository.AuditLogRepo                // FR-38：审计日志
	auditObservability  *repository.AuditObservabilityRepo      // FR-118：统一审计读模型
	operationsMetrics   *repository.OperationsObservabilityRepo // FR-53/120：当前节点业务与主机读模型
	operationsAlertRepo *repository.OperationsAlertRepo         // v0.8.0：运维告警去重持久化
	dashboardSvc        *domain.OperationsDashboardService
	hostMonitoringSvc   *domain.HostMonitoringService
	backupSvc           *domain.BackupService    // FR-132：节点备份包生成与登记
	freeze              *domain.FreezeController // FR-135：全局共享的运行时写入冻结控制器
	restoreSvc          *domain.RestoreService
	backupImports       *domain.BackupImportService // FR-137：导入记录与 URL 拉取
	backupUploads       *domain.BackupUploadService // FR-137：分片上传（Web 第三通道）
	auditAttentionKey   []byte                      // FR-118：由启动密钥派生的关注标识签名输入
	nodeIdentity        *domain.NodeIdentity
	syncTokenSet        bool             // 历史集群状态兼容字段；主备不再以环境令牌配置
	publicURL           string           // FR-115：备用节点从启动配置读取的只读协议回退 URL
	upstreamClient      *upstream.Client // FR-89：回源客户端（web 改回源超时时 SetTimeout）
	store               auth.Store
	jwt                 *auth.JWTManager
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
	formatMetadataRepo := repository.NewFormatMetadataRepo(db)
	publishPolicyRepo := repository.NewPublishPolicyRepo(db)
	migrationTaskRepo := repository.NewMigrationTaskRepo(db)

	jwtMgr := auth.NewJWTManager(cfg.JWTSecret)
	blobs := blobstore.NewStore(cfg.BlobDir)
	upstreamClient := upstream.NewClient(cfg.UpstreamTimeout)
	settingRepo := repository.NewSettingRepo(db)
	settingSvc := domain.NewSettingService(settingRepo)
	repoSvc := domain.NewRepositoryService(repoRepo, aclRepo, assetRepo, settingSvc, userRepo)
	repoSvc.SetEnabledFormats(cfg.EnabledFormats)
	repoSvc.SetFormatMetadataRepo(formatMetadataRepo)
	assetSvc := domain.NewAssetService(repoRepo, assetRepo, blobs, upstreamClient)
	repoSvc.SetMutationCoordinator(assetSvc.MutationCoordinator())
	// FR-135：全局共享的冻结控制器。复制退役后不再有角色派生的静态只读栅栏，
	// 底座传 nil；冻结窗口（搬迁切换用）由该实例统一承载。
	// 所有写栅栏必须装同一个 freeze 实例：只要有一个服务仍挂静态栅栏，冻结期间就
	// 有写路径漏网，"停写" 语义不再成立——而搬迁切换依赖它成立。
	freeze := domain.NewFreezeController(nil)
	settingSvc.SetBusinessWriteGate(freeze)
	repoSvc.SetBusinessWriteGate(freeze)
	assetSvc.SetBusinessWriteGate(freeze)
	// FR-137：导入编排（校验 → 合并 blob → 暂存 db → 写 restore.pending）；
	// 真正的数据库替换由启动期 ApplyPendingRestore 在 persistence.Open 之前完成。
	restoreSvc := domain.NewRestoreService(db, cfg.DataDir, cfg.DBPath, cfg.BlobDir)
	// 复制退役后节点不再是 standby，启动恢复走本地资产操作恢复路径。
	if err := assetSvc.RecoverMutationIntents(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("恢复遗留资产操作：%w", err)
	}
	formatMetadataSvc := domain.NewFormatMetadataService(assetSvc, repoRepo, formatMetadataRepo, upstreamClient)
	formatMetadataSvc.SetBusinessWriteGate(freeze)
	cargoSvc := domain.NewCargoService(assetSvc, repoSvc)
	ociSvc := domain.NewOCIService(assetSvc, repoSvc)
	if err := ociSvc.CleanupUploadTemps(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("清理 OCI 上传暂存：%w", err)
	}
	publishPolicySvc := domain.NewPublishPolicyService(publishPolicyRepo, repoRepo, assetRepo)
	publishPolicySvc.SetBusinessWriteGate(freeze)
	if released, releaseErr := publishPolicyRepo.ReleaseExpired(); releaseErr != nil {
		log.Printf("启动回收过期配额预留失败：%v", releaseErr)
	} else if released > 0 {
		log.Printf("启动回收过期配额预留：%d 条", released)
	}
	userSvc := domain.NewUserService(userRepo)
	tokenSvc := domain.NewTokenService(tokenRepo, userRepo)
	userSvc.SetBusinessWriteGate(freeze)
	tokenSvc.SetBusinessWriteGate(freeze)

	// 复制退役（FR-138）：repl_change 变更日志不再写入，写路径注入 no-op 记录器。
	// 原子 operation 信封不受影响——asset_mutation 的 ApplyWithOutbox* 仍把制品操作
	// 写入 replication_operation_outbox（同一事务），那是制品操作与审计的真源。
	nodeIdentity := domain.NewNodeIdentity(settingRepo)
	assetSvc.SetNodeIdentity(nodeIdentity)
	assetSvc.SetChangeRecorder(domain.NoopChangeRecorder{})
	formatMetadataSvc.SetChangeRecorder(domain.NoopChangeRecorder{})
	repoSvc.SetChangeRecorder(domain.NoopChangeRecorder{})
	userSvc.SetChangeRecorder(domain.NoopChangeRecorder{})
	tokenSvc.SetChangeRecorder(domain.NoopChangeRecorder{})
	settingSvc.SetChangeRecorder(domain.NoopChangeRecorder{})

	// FR-89：环境变量仅在 setting 键不存在时写入初始默认；显式清空必须跨重启保留。
	{
		defaults := []repository.SettingValue{
			{Key: domain.SettingKeyPublicURL, Value: cfg.PublicURL},
			{Key: domain.SettingKeyUpstreamTimeout, Value: strconv.Itoa(int(cfg.UpstreamTimeout.Seconds()))},
			// 同步间隔仍是设置页暴露的四项基础配置之一（FR-89），保留其默认 seed。
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
	}
	auditLogRepo := repository.NewAuditLogRepo(db)
	auditObservabilityRepo := repository.NewAuditObservabilityRepo(db)
	operationsMetrics := repository.NewOperationsObservabilityRepo(db)
	operationsAlertRepo := repository.NewOperationsAlertRepo(db)
	dashboardSvc := domain.NewOperationsDashboardService(operationsMetrics)
	hostMonitoringSvc := domain.NewHostMonitoringService(operationsMetrics, domain.NewHostCollector(cfg.BlobDir, func() bool { return db.Ping() == nil }))

	offlineIndexRepo := repository.NewOfflineIndexRepo(db)
	offlineScanner := offindex.New(offlineIndexRepo)

	migRunner := runner.New(
		runner.TaskStoreAdapter{Repo: migrationTaskRepo},
		runner.AssetServiceAdapter{Assets: assetSvc, Repos: repoRepo, AssetR: assetRepo},
		runner.RepoAdminAdapter{Repos: repoSvc},
	)
	migRunner.SetFormatImporter(domain.NewMigrationFormatImporter(formatMetadataSvc, cargoSvc, ociSvc))
	migRunner.SetAuditLogRepo(auditLogRepo)
	migRunner.SetOfflineIndex(offlineIndexRepo)
	migrationSvc := domain.NewMigrationService(migrationTaskRepo, migRunner)
	migrationCredentialSealer, err := credential.NewSealer(cfg.MigrationCredentialKey)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("初始化迁移凭据加密器：%w", err)
	}
	migRunner.SetCredentialSealer(migrationCredentialSealer)
	migrationSvc.SetCredentialSealer(migrationCredentialSealer)
	migrationSvc.SetOfflineIndex(offlineIndexRepo, offlineScanner)
	migrationSvc.SetBusinessWriteGate(freeze)

	// 进程崩溃回收：残留 running → failed，等人 resume（ADR-0012）。
	if _, err := migrationSvc.FailInterruptedRunning(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("回收中断的迁移任务：%w", err)
	}
	authSvc := domain.NewAuthService(userRepo, revokedRepo, jwtMgr)
	authSvc.SetBusinessWriteGate(freeze)
	authSvc.SetChangeRecorder(domain.NoopChangeRecorder{})

	publicURL := ""

	// FR-137：分片上传服务依赖导入服务（组装完成后走本地导入状态机）。
	backupImportsSvc := domain.NewBackupImportService(repository.NewBackupImportRepo(db), restoreSvc, cfg.DataDir, upstreamClient)

	return &appServices{
		db:                  db,
		users:               userRepo,
		authSvc:             authSvc,
		userSvc:             userSvc,
		tokenSvc:            tokenSvc,
		repoSvc:             repoSvc,
		cargoSvc:            cargoSvc,
		ociSvc:              ociSvc,
		formatMetadataSvc:   formatMetadataSvc,
		publishPolicySvc:    publishPolicySvc,
		assetSvc:            assetSvc,
		migrationSvc:        migrationSvc,
		settingSvc:          settingSvc,
		auditLogs:           auditLogRepo,
		auditObservability:  auditObservabilityRepo,
		operationsMetrics:   operationsMetrics,
		operationsAlertRepo: operationsAlertRepo,
		dashboardSvc:        dashboardSvc,
		hostMonitoringSvc:   hostMonitoringSvc,
		backupSvc:           domain.NewBackupService(db, repository.NewBackupPackageRepo(db), blobs, cfg.DataDir, cfg.DBPath, version, nodeIdentity.NodeID),
		restoreSvc:          restoreSvc,
		backupImports:       backupImportsSvc,
		backupUploads:       domain.NewBackupUploadService(repository.NewBackupUploadRepo(db), backupImportsSvc, cfg.DataDir),
		freeze:              freeze,
		auditAttentionKey:   append([]byte(nil), cfg.JWTSecret...),
		nodeIdentity:        nodeIdentity,
		syncTokenSet:        false,
		publicURL:           publicURL,
		upstreamClient:      upstreamClient,
		store:               domain.NewAuthStore(userRepo, tokenRepo, revokedRepo),
		jwt:                 jwtMgr,
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
		Version:                 version,
		Checks:                  checks,
		Migration:               s.db.CurrentVersion,
		Auth:                    s.authSvc,
		Users:                   s.userSvc,
		Tokens:                  s.tokenSvc,
		Repos:                   s.repoSvc,
		Assets:                  s.assetSvc, // FR-103：制品批量删除
		Migrations:              s.migrationSvc,
		Settings:                s.settingSvc,
		AuditLogs:               s.auditLogs,
		AuditObservability:      s.auditObservability,
		OperationsObservability: s.operationsMetrics,
		OperationsAlerts:        s.operationsAlertRepo,
		AuditAttentionKey:       s.auditAttentionKey,
		AuditSourceNode:         s.nodeIdentity.NodeID(),
		Backups:                 s.backupSvc,
		Freeze:                  s.freeze,
		BackupImports:           s.backupImports,
		BackupUploads:           s.backupUploads,
		BackupLinkKey:           append([]byte(nil), s.auditAttentionKey...),
		ClusterTokenSet:         s.syncTokenSet,
		PublicURL:               s.publicURL,
		EnabledFormats:          s.repoSvc.EnabledFormats(),
		PublishPolicies:         s.publishPolicySvc,
		OnUpstreamTimeoutChange: func(d time.Duration) {
			if s.upstreamClient != nil {
				s.upstreamClient.SetTimeout(d)
			}
		},
	})
}
