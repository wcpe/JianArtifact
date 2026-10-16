package main

import (
	"context"
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
	"github.com/wcpe/jianartifact/apps/server/internal/metrics"
	"github.com/wcpe/jianartifact/apps/server/internal/migration/credential"
	"github.com/wcpe/jianartifact/apps/server/internal/migration/offindex"
	"github.com/wcpe/jianartifact/apps/server/internal/migration/runner"
	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
	"github.com/wcpe/jianartifact/apps/server/internal/scheduler"
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
	userGroupSvc        *domain.UserGroupService // FR-36：用户组（授权主体之一）的 CRUD 与成员维护
	assetSvc            *domain.AssetService
	migrationSvc        *domain.MigrationService
	settingSvc          *domain.SettingService
	auditLogs           *repository.AuditLogRepo                // FR-38：审计日志
	auditObservability  *repository.AuditObservabilityRepo      // FR-118：统一审计读模型
	operationsMetrics   *repository.OperationsObservabilityRepo // FR-53/120：当前节点业务与主机读模型
	operationsAlertRepo *repository.OperationsAlertRepo         // v0.8.0：运维告警去重持久化
	assetDownloadRepo   *repository.AssetDownloadRepo           // FR-142：制品下载计量明细
	pinnedRepo          *repository.PinnedRepoRepo              // 置顶仓库（用户级 + 全局兜底）
	dashboardSvc        *domain.OperationsDashboardService
	assetDownloadSvc    *domain.AssetDownloadService
	hostMonitoringSvc   *domain.HostMonitoringService
	backupSvc           *domain.BackupService      // FR-132：节点备份包生成与登记
	backupDrillSvc      *domain.BackupDrillService // FR-40：恢复演练与校验报告
	freeze              *domain.FreezeController   // FR-135：全局共享的运行时写入冻结控制器
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
	metricsReg          *metrics.Registry        // FR-39：进程内指标登记（协议请求计数）
	metricsExp          *metrics.Exposition      // FR-39：/metrics 文本暴露渲染器
	scheduler           *scheduler.Scheduler     // FR-44：维护类周期作业调度器
	restartSvc          *domain.RestartService   // FR-40 追加七：一键重启（能力预检 + 延迟触发）
	restartCh           chan struct{}            // FR-40 追加七：重启请求信号，由 main 的 select 接收
	quotaSvc            *domain.RepoQuotaService // FR-41：仓库存储配额（预检早拒 + 提交点权威复检）
	oidcDeps            *api.OIDCDeps            // FR-34：OIDC 登录端点依赖（nil = 未启用）
	ldapAuth            api.LDAPAuthenticator    // FR-35：LDAP 目录侧校验（nil = 未启用）
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
	userGroupRepo := repository.NewUserGroupRepo(db) // FR-36：用户组（授权主体之一）
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
	// FR-36：授权判定据此展开主体的组归属；不注入则退化成「仅按用户自身 ACL」。
	repoSvc.SetUserGroupRepo(userGroupRepo)
	assetSvc := domain.NewAssetService(repoRepo, assetRepo, blobs, upstreamClient)
	// FR-43：auto-block 退避起始时长来自 JIAN_AUTO_BLOCK_BASE_SECONDS（缺省/非法回落 40s）；
	// 未注入时 domain 侧以 autoBlockInitial 常量兜底（两者必须同值）。
	assetSvc.SetAutoBlockBase(cfg.AutoBlockBaseSeconds)
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
	// FR-36：用户组管理（组的 CRUD 与成员维护）。ACL 仓储供删组时清理组主体条目。
	userGroupSvc := domain.NewUserGroupService(userGroupRepo, aclRepo, userRepo)
	userGroupSvc.SetBusinessWriteGate(freeze)
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
	// FR-40 追加：自动备份三项配置的初始值来自环境变量，此后以 setting 表为准
	// （运行时可经界面修改并热生效）。只播种整组都缺失的情况，理由见函数注释。
	if err := seedBackupSettings(settingSvc, settingRepo, cfg); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("初始化自动备份设置：%w", err)
	}
	auditLogRepo := repository.NewAuditLogRepo(db)
	auditObservabilityRepo := repository.NewAuditObservabilityRepo(db)
	operationsMetrics := repository.NewOperationsObservabilityRepo(db)
	operationsAlertRepo := repository.NewOperationsAlertRepo(db)
	dashboardSvc := domain.NewOperationsDashboardService(operationsMetrics)
	assetDownloadRepo := repository.NewAssetDownloadRepo(db)
	pinnedRepo := repository.NewPinnedRepoRepo(db)
	assetDownloadSvc := domain.NewAssetDownloadService(assetDownloadRepo)
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

	// FR-132 / FR-40：备份服务是生成、保留策略与演练三者共用的同一实例——
	// 保留策略的候选集查询与删除都经它走同一条已装配的连接与生成槽位，
	// 避免"策略看到的包"与"生成写下的包"来自两个视图。
	backupRepo := repository.NewBackupPackageRepo(db)
	backupSvc := domain.NewBackupService(db, backupRepo, blobs, cfg.DataDir, cfg.DBPath, version, nodeIdentity.NodeID)

	// FR-44：维护类周期作业统一经调度器驱动（间隔 0 禁用；启动不立即执行）。
	schedulerSvc := scheduler.New()
	// FR-40 追加七：一键重启。单元名与作用域只来自启动配置（环境变量），
	// 请求体不参与命令构造——这是防命令注入的第一道边界（见 domain/restart_service.go）。
	//
	// 执行路径是**进程自替换**（syscall.Exec 换新映像），不执行任何外部命令：
	// 本进程本就有权执行自己的二进制（systemd 的 ExecStart 就是它），因此不引入任何新权限，
	// NoNewPrivileges / polkit / sudoers 这些提权路径上的门全都绕开。
	// 域层只负责"延迟多久触发"，动作（优雅关停 + Exec）由 main 的 select 完成——
	// 关停哪些监听器是本进程的编排知识，见 RestartTrigger 的说明。
	restartSvc := domain.NewRestartService(cfg.RestartUnit, cfg.RestartScope)
	// restartCh 把域层的"该重启了"送到 main 的 select。
	//
	// 缓冲 1：域层只发一次（RestartAfter 是单次排定），缓冲 1 保证那次发送永不阻塞，
	// 不给即将关停的进程留一个卡在发送上的 goroutine；重复触发直接丢弃
	// （同一件事不必排队两次，重复只会重复关停与 Exec）。
	//
	// CLI 子命令（backup / admin / status）也走本函数，它们的 restartCh 无人接收——
	// 这是无害的：那些子命令不提供 HTTP 端点，没有调用方能看到 supported 判据。
	restartCh := make(chan struct{}, 1)
	restartSvc.SetTrigger(func() {
		select {
		case restartCh <- struct{}{}:
		default:
		}
	})
	registerBlobGCJob(schedulerSvc, cfg.BlobGCInterval, assetSvc.CleanupUnreferencedBlobs)
	// FR-41：存储治理清理（隔离区空目录 / 过期 OCI 上传临时文件 / 终态元数据裁剪 / 代理缓存保留）。
	// 只做「不触碰任何在途文件」的四类清理；pending_gc 滞留件回收留待后续增量
	// （前置能力缺口：操作引擎没有「在写门下判定是否在途」的入口）。
	storageCleanupSvc := domain.NewStorageCleanupService(
		repository.NewStorageCleanupRepo(db),
		assetSvc,
		blobs,
		domain.StorageCleanupOptions{
			MetadataRetention: cfg.StorageMetadataRetention, // JIAN_STORAGE_METADATA_RETENTION_DAYS（天），0 禁用
			TempMaxAge:        cfg.StorageTempMaxAge,        // JIAN_STORAGE_TEMP_MAX_AGE_HOURS（小时），0 禁用
		},
	)
	registerStorageCleanupJob(schedulerSvc, cfg.StorageCleanupInterval, storageCleanupSvc.Run)
	// FR-40：周期化自动备份 + 保留策略。间隔的当前值取自 setting 表
	// （初值由环境变量 JIAN_BACKUP_AUTO_INTERVAL 播种），因此界面上改完下一轮即生效；
	// 间隔为 0 表示关闭（作业仍在心跳，但每轮跳过）。
	backupRetentionSvc := domain.NewBackupRetentionService(backupSvc, backupRepo, domain.BackupRetentionOptions{
		KeepLast:      settingSvc.BackupKeepLast(),      // 初值来自 JIAN_BACKUP_KEEP_LAST（份），0 = 不按份数限制
		RetentionDays: settingSvc.BackupRetentionDays(), // 初值来自 JIAN_BACKUP_RETENTION_DAYS（天），0 = 不限天数
	})
	registerAutoBackupJob(schedulerSvc, autoBackupHeartbeat, func() autoBackupSettings {
		// 每轮现读：这三个值就是"界面上改完即生效"的全部来源。
		return autoBackupSettings{
			IntervalSecs:  settingSvc.BackupAutoIntervalSecs(),
			KeepLast:      settingSvc.BackupKeepLast(),
			RetentionDays: settingSvc.BackupRetentionDays(),
		}
	}, func(ctx context.Context, settings autoBackupSettings) (autoBackupRun, error) {
		return runAutoBackup(ctx, backupSvc, backupRetentionSvc, settings)
	})
	// FR-40：恢复演练服务（CLI 与 API 共用同一实现；API 端点在后续阶段接入）。
	backupDrillSvc := domain.NewBackupDrillService(backupSvc, repository.NewBackupVerificationRepo(db), cfg.DataDir)
	// FR-39：指标登记与 /metrics 渲染器；协议请求计数由 HTTP 层钩子（run）累加。
	metricsReg := metrics.New()
	metricsExp := metrics.NewExposition(metricsReg, schedulerJobStatuses(schedulerSvc))
	// FR-43：上游断路器指标。阻止事件计数按闭集 reason 分区（probe_failed / upstream_error，
	// 由 domain 定义、装配层注入）；阻止态仓库数在每次抓取时实时取数（只读内存态，
	// 不查库、不访问网络，符合 /metrics 的抓取路径约定）。
	assetSvc.SetUpstreamBlockRecorder(metricsReg)
	metricsExp.SetBlockedRepositories(assetSvc.BlockedUpstreamCount)
	// FR-41：仓库级存储配额判定。预检（已知长度/流式）走 Admission，提交点权威复检走 CheckCommit；
	// 拒绝原因以闭集枚举累加进 jianartifact_publish_rejections_total{reason="quota"}。
	quotaSvc := domain.NewRepoQuotaService(repoRepo, assetRepo)
	quotaSvc.SetRejectionRecorder(metricsReg)
	assetSvc.SetStorageQuotaCheck(quotaSvc.CheckCommit)

	// FR-34：OIDC 登录端点依赖；未配置 issuer 时保持 nil，端点返回 404（存在即启用）。
	var oidcDeps *api.OIDCDeps
	if cfg.OIDC.Enabled() {
		oidcDeps = &api.OIDCDeps{
			Verifier:       auth.NewOIDCVerifier(cfg.OIDC),
			FlowSigner:     auth.NewOIDCFlowSigner(cfg.JWTSecret),
			AllowedDomains: cfg.OIDC.AllowedDomains,
			// 回跳直达业务页：/login 在前端会经一次客户端跳转，而客户端跳转会丢掉
			// 承载令牌的 URL 片段（实机验收发现），故不能借道 /login。
			RedirectAfter: "/repositories",
		}
	}

	// FR-35：LDAP 目录侧校验；未配置 URL 时保持 nil，登录不回退目录。
	var ldapAuth api.LDAPAuthenticator
	if cfg.LDAP.Enabled() {
		ldapVerifier, err := auth.NewLDAPVerifier(cfg.LDAP)
		if err != nil {
			return nil, fmt.Errorf("初始化 LDAP 校验器：%w", err)
		}
		ldapAuth = ldapVerifier.Authenticate
	}

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
		userGroupSvc:        userGroupSvc,
		quotaSvc:            quotaSvc,
		assetSvc:            assetSvc,
		migrationSvc:        migrationSvc,
		settingSvc:          settingSvc,
		auditLogs:           auditLogRepo,
		auditObservability:  auditObservabilityRepo,
		operationsMetrics:   operationsMetrics,
		operationsAlertRepo: operationsAlertRepo,
		dashboardSvc:        dashboardSvc,
		assetDownloadSvc:    assetDownloadSvc,
		assetDownloadRepo:   assetDownloadRepo,
		pinnedRepo:          pinnedRepo,
		hostMonitoringSvc:   hostMonitoringSvc,
		backupSvc:           backupSvc,
		backupDrillSvc:      backupDrillSvc,
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
		metricsReg:          metricsReg,
		metricsExp:          metricsExp,
		scheduler:           schedulerSvc,
		restartSvc:          restartSvc,
		restartCh:           restartCh,
		oidcDeps:            oidcDeps,
		ldapAuth:            ldapAuth,
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

// seedBackupSettings 把环境变量里的自动备份三项配置作为**初值**播种进 setting 表。
//
// 与 seedSettingDefault 的两点不同：
//
//   - **0 是有意义的值**（关闭 / 不限制），因此不能用"非空才写"的判据——三项一律写入；
//   - **整组一起判断**：只要表里已存在任意一项，就认为本节点已由界面管过这组配置，
//     三项都不再播种。否则会出现"运维在界面把间隔改成 60、重启后却被环境变量里的
//     30 覆盖回去"这种最令人困惑的行为（因为份数/天数键也在，逐键播种就会各自为政）。
//
// 环境变量在播种后即完成使命：此后以 setting 表为准，界面改动不再受 env 干扰。
func seedBackupSettings(settingSvc *domain.SettingService, settings *repository.SettingRepo, cfg *config.Config) error {
	if settingSvc.HasBackupSettings() {
		return nil
	}
	values := []repository.SettingValue{
		{Key: domain.SettingKeyBackupAutoInterval, Value: strconv.Itoa(int(cfg.BackupAutoInterval.Seconds()))},
		{Key: domain.SettingKeyBackupKeepLast, Value: strconv.Itoa(cfg.BackupKeepLast)},
		{Key: domain.SettingKeyBackupRetentionDays, Value: strconv.Itoa(cfg.BackupRetentionDays)},
	}
	for _, value := range values {
		if err := settings.Set(value.Key, value.Value); err != nil {
			return err
		}
	}
	return nil
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
		AssetDownloads:          s.assetDownloadRepo,
		Pinned:                  s.pinnedRepo,
		AuditAttentionKey:       s.auditAttentionKey,
		AuditSourceNode:         s.nodeIdentity.NodeID(),
		Backups:                 s.backupSvc,
		BackupDrill:             s.backupDrillSvc, // FR-40：恢复演练端点（202 受理 + 报告查询）
		Freeze:                  s.freeze,
		BackupImports:           s.backupImports,
		BackupUploads:           s.backupUploads,
		Metrics:                 s.metricsExp,
		OIDC:                    s.oidcDeps,
		LDAPAuth:                s.ldapAuth,
		BackupLinkKey:           append([]byte(nil), s.auditAttentionKey...),
		ClusterTokenSet:         s.syncTokenSet,
		PublicURL:               s.publicURL,
		EnabledFormats:          s.repoSvc.EnabledFormats(),
		PublishPolicies:         s.publishPolicySvc,
		UserGroups:              s.userGroupSvc, // FR-36：用户组管理端点（仅管理员）
		MaintenanceJobs:         s.scheduler,    // FR-41：维护作业清单与手动触发
		Restart:                 s.restartSvc,   // FR-40 追加七：一键重启（先回响应，再重启）
		OnUpstreamTimeoutChange: func(d time.Duration) {
			if s.upstreamClient != nil {
				s.upstreamClient.SetTimeout(d)
			}
		},
	})
}

// schedulerJobStatuses 把调度器作业状态适配为指标渲染快照（FR-39）；
// 由渲染器在每次抓取时调用，不缓存。
func schedulerJobStatuses(s *scheduler.Scheduler) func() []metrics.JobStatus {
	return func() []metrics.JobStatus {
		statuses := s.Status()
		out := make([]metrics.JobStatus, 0, len(statuses))
		for _, st := range statuses {
			out = append(out, metrics.JobStatus{
				Name:     st.Name,
				Runs:     st.Runs,
				Failures: st.Failures,
				Running:  st.Running,
				LastRun:  st.LastFinish,
				Failed:   st.LastError != "",
			})
		}
		return out
	}
}
