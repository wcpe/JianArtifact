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
	userGroupSvc        *domain.UserGroupService     // 用户组（授权主体之一）的 CRUD 与成员维护
	searchIndexSvc      *domain.SearchIndexService   // 搜索索引后台回填与全量重建
	storageHealthSvc    *domain.StorageHealthService // 存储体检（只读，不做清理）
	cleanupSvc          *domain.CleanupService       // 定向清理（移入回收站）
	cleanTaskRepo       *repository.CleanTaskRepo    // 清理任务队列
	trashSvc            *domain.TrashService         // 回收站（恢复 / 彻底删除 / 清空）
	assetSvc            *domain.AssetService
	migrationSvc        *domain.MigrationService
	settingSvc          *domain.SettingService
	auditLogs           *repository.AuditLogRepo                // 审计日志
	auditObservability  *repository.AuditObservabilityRepo      // 统一审计读模型
	operationsMetrics   *repository.OperationsObservabilityRepo // 当前节点业务与主机读模型
	operationsAlertRepo *repository.OperationsAlertRepo         // v0.8.0：运维告警去重持久化
	assetDownloadRepo   *repository.AssetDownloadRepo           // 制品下载计量明细
	pinnedRepo          *repository.PinnedRepoRepo              // 置顶仓库（用户级 + 全局兜底）
	dashboardSvc        *domain.OperationsDashboardService
	assetDownloadSvc    *domain.AssetDownloadService
	hostMonitoringSvc   *domain.HostMonitoringService
	backupSvc           *domain.BackupService      // 节点备份包生成与登记
	backupDrillSvc      *domain.BackupDrillService // 恢复演练与校验报告
	freeze              *domain.FreezeController   // 全局共享的运行时写入冻结控制器
	restoreSvc          *domain.RestoreService
	backupImports       *domain.BackupImportService // 导入记录与 URL 拉取
	backupUploads       *domain.BackupUploadService // 分片上传（Web 第三通道）
	backupAdopt         *domain.BackupAdoptService  // 未管理备份包的扫描与导入
	auditAttentionKey   []byte                      // 由启动密钥派生的关注标识签名输入
	nodeIdentity        *domain.NodeIdentity
	syncTokenSet        bool             // 历史集群状态兼容字段；主备不再以环境令牌配置
	publicURL           string           // 备用节点从启动配置读取的只读协议回退 URL
	upstreamClient      *upstream.Client // 回源客户端（web 改回源超时时 SetTimeout）
	store               auth.Store
	jwt                 *auth.JWTManager
	metricsReg          *metrics.Registry              // 进程内指标登记（协议请求计数）
	metricsExp          *metrics.Exposition            // /metrics 文本暴露渲染器
	scheduler           *scheduler.Scheduler           // 维护类周期作业调度器
	restartSvc          *domain.RestartService         // 一键重启（能力预检 + 延迟触发）
	restartCh           chan struct{}                  // 重启请求信号，由 main 的 select 接收
	quotaSvc            *domain.RepoQuotaService       // 仓库存储配额（预检早拒 + 提交点权威复检）
	upstreamHealthSvc   *domain.UpstreamHealthService  // 上游健康历史采样
	upstreamHealthRepo  *repository.UpstreamHealthRepo // 上游健康历史读写（查询端点 + 保留清理）
	upstreamProbeSvc    *domain.UpstreamProbeService   // upstream-probe 调度作业
	oidcDeps            *api.OIDCDeps                  // OIDC 登录端点依赖（nil = 未启用）
	ldapAuth            api.LDAPAuthenticator          // LDAP 目录侧校验（nil = 未启用）
}

// openServices 打开数据库、执行迁移并装配领域服务。调用方负责在返回的 db 上 Close。
func openServices(cfg *config.Config) (*appServices, error) {
	// SQLite 连接池上限来自 JIAN_SQLITE_MAX_CONNS（缺省/非正回落 8）。
	// WAL 支持「多读 + 单写」，池放宽后读侧才真正并发；写仍由 SQLite 自身串行化。
	db, err := persistence.OpenWithMaxConns(cfg.DBPath, cfg.SQLiteMaxConns)
	if err != nil {
		return nil, err
	}
	// fts5 与 trigram 是 SQLite 的**构建期**能力，缺能力时搜索索引（asset_path_fts，迁移 0049）
	// 根本无法建立。自检必须跑在 Migrate **之前**：0049 的第一条语句就是
	// `CREATE VIRTUAL TABLE asset_path_fts USING fts5(...)`，一旦放在迁移之后，首次升级时
	// 缺能力的部署会先撞上迁移抛出的那句泛化 SQL logic error——正是本自检要消灭的场景。
	// 放在前面是安全的：自检只在独立的内存库上建/删探测表，既不依赖主库 schema（此时主库可能
	// 还没建表），也不与主库连接池争用（见 persistence.CheckFTS5Support）。
	if err := persistence.CheckFTS5Support(); err != nil {
		_ = db.Close()
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
	userGroupRepo := repository.NewUserGroupRepo(db) // 用户组（授权主体之一）
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
	// 授权判定据此展开主体的组归属；不注入则退化成「仅按用户自身 ACL」。
	repoSvc.SetUserGroupRepo(userGroupRepo)
	// 搜索链路总预算来自 JIAN_SEARCH_TIMEOUT_SECONDS（缺省/非法回落 10s）；
	// 未注入时 domain 侧以 searchTimeoutDefault 常量兜底（两者必须同值）。
	repoSvc.SetSearchTimeout(cfg.SearchTimeout)
	assetSvc := domain.NewAssetService(repoRepo, assetRepo, blobs, upstreamClient)
	// auto-block 退避起始时长来自 JIAN_AUTO_BLOCK_BASE_SECONDS（缺省/非法回落 40s）；
	// 未注入时 domain 侧以 autoBlockInitial 常量兜底（两者必须同值）。
	assetSvc.SetAutoBlockBase(cfg.AutoBlockBaseSeconds)
	// group 成员**确认阶段**预算来自 JIAN_GROUP_MEMBER_TIMEOUT（缺省/非法/非正数回落 3s，
	// 上限 60s 超限夹取）：只约束「该成员是否及时给出上游响应头」，落盘预算仍由
	// JIAN_UPSTREAM_TIMEOUT 承担（见 domain 侧 groupGet 的首字节看门狗）。
	// 未注入时 domain 侧以 groupMemberTimeoutDefault 常量兜底（两者必须同值）。
	assetSvc.SetGroupMemberTimeout(cfg.GroupMemberTimeout)
	// 404 负缓存 TTL 来自 JIAN_NEGATIVE_CACHE_TTL（缺省/非法回落 60s，显式 0 = 关闭负缓存）。
	// 注入点必须在 NewAssetService 之后、第一个请求之前：该 setter 此前只有定义、
	// 装配层从未调用，负缓存 TTL 实际是一个运行时改不了的常量，这里是它第一次真正接线。
	assetSvc.SetNegativeCacheTTL(cfg.NegativeCacheTTL)
	repoSvc.SetMutationCoordinator(assetSvc.MutationCoordinator())
	// 全局共享的冻结控制器。复制退役后不再有角色派生的静态只读栅栏，
	// 底座传 nil；冻结窗口（搬迁切换用）由该实例统一承载。
	// 所有写栅栏必须装同一个 freeze 实例：只要有一个服务仍挂静态栅栏，冻结期间就
	// 有写路径漏网，"停写" 语义不再成立——而搬迁切换依赖它成立。
	freeze := domain.NewFreezeController(nil)
	settingSvc.SetBusinessWriteGate(freeze)
	repoSvc.SetBusinessWriteGate(freeze)
	assetSvc.SetBusinessWriteGate(freeze)
	// 导入编排（校验 → 合并 blob → 暂存 db → 写 restore.pending）；
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
	// 用户组管理（组的 CRUD 与成员维护）。ACL 仓储供删组时清理组主体条目。
	userGroupSvc := domain.NewUserGroupService(userGroupRepo, aclRepo, userRepo)
	userGroupSvc.SetBusinessWriteGate(freeze)
	userSvc.SetBusinessWriteGate(freeze)
	tokenSvc.SetBusinessWriteGate(freeze)

	// 复制退役：repl_change 变更日志不再写入，写路径注入 no-op 记录器。
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

	// 环境变量仅在 setting 键不存在时写入初始默认；显式清空必须跨重启保留。
	{
		defaults := []repository.SettingValue{
			{Key: domain.SettingKeyPublicURL, Value: cfg.PublicURL},
			{Key: domain.SettingKeyUpstreamTimeout, Value: strconv.Itoa(int(cfg.UpstreamTimeout.Seconds()))},
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
	// 自动备份三项配置的初始值来自环境变量，此后以 setting 表为准
	// （运行时可经界面修改并热生效）。只播种整组都缺失的情况，理由见函数注释。
	if err := seedBackupSettings(settingSvc, settingRepo, cfg); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("初始化自动备份设置：%w", err)
	}
	// proxy/group 调优两项同理：环境变量只是首次播种的初值，此后以 setting 表为准。
	if err := seedProxyTuningSettings(settingRepo, cfg); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("初始化代理调优设置：%w", err)
	}
	// 运行期策略 13 项同理：作业间隔 / 保留策略 / 容量与阈值。
	if err := seedRuntimePolicySettings(settingRepo, cfg); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("初始化运行期策略设置：%w", err)
	}
	// 播种之后按**生效值**再注入一次：上面两处 setter 用的是环境变量解析结果，
	// 而 setting 表可能已被界面改过且跨重启保留（环境变量此时已不是真源）。
	applyProxyTuningSettings(settingSvc, assetSvc)
	// 同理，运行期策略里三项"请求路径按需读取"的阈值也按生效值再注入一次：
	// 上面两处 setter 用的是环境变量解析结果，而 setting 表可能已被界面改过且跨重启保留。
	applyRuntimePolicySettings(settingSvc, cfg, repoSvc, assetSvc)
	auditLogRepo := repository.NewAuditLogRepo(db)
	auditObservabilityRepo := repository.NewAuditObservabilityRepo(db)
	operationsMetrics := repository.NewOperationsObservabilityRepo(db)
	// 仪表盘容量聚合的缓存有效期来自 JIAN_CAPACITY_CACHE_TTL_SECONDS
	// （缺省/非法回落 30s，显式 0 = 禁用缓存、每次请求实时聚合）；
	// 未注入时 repository 侧以 defaultCapacityCacheTTL 常量兜底（两者必须同值）。
	// 这里取 setting 表的生效值：初值由环境变量播种，之后可在设置页改并热生效。
	operationsMetrics.SetCapacityCacheTTL(
		effectivePolicyDuration(settingSvc.CapacityCacheTTLSeconds, cfg.CapacityCacheTTL))
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

	// 分片上传服务依赖导入服务（组装完成后走本地导入状态机）。
	backupImportsSvc := domain.NewBackupImportService(repository.NewBackupImportRepo(db), restoreSvc, cfg.DataDir, upstreamClient)

	// 备份服务是生成、保留策略与演练三者共用的同一实例——
	// 保留策略的候选集查询与删除都经它走同一条已装配的连接与生成槽位，
	// 避免"策略看到的包"与"生成写下的包"来自两个视图。
	backupRepo := repository.NewBackupPackageRepo(db)
	backupSvc := domain.NewBackupService(db, backupRepo, blobs, cfg.DataDir, cfg.DBPath, version, nodeIdentity.NodeID)

	// 搜索索引的后台异步回填服务。只做编排（分批推进水位 / 可中断续跑 / 可重建），
	// 索引行与状态表的维护原语都在 repository 层；真正的启动点在各子命令就绪后调用 Start。
	searchIndexSvc := domain.NewSearchIndexService(repository.NewSearchIndexStateRepo(db))
	// 存储体检：只统计「确定可回收」的类别（孤儿 blob、隔离区、空目录、暂存目录），
	// 不清理任何东西——仓库删除与 blob 回收都是不可逆的运维决策，自动清理会把一次误判
	// 直接变成数据丢失。清理动作仍由既有作业按明确间隔执行。
	storageHealthSvc := domain.NewStorageHealthService(cfg.DataDir, blobs, assetRepo.ListReferencedBlobHashes)
	// 定向清理：把四类可回收垃圾移入回收站（不是物理删除）。过期上传会话复用既有
	// uploadTTL 口径（BackupUploadService 的 expires_at 字段），不另造 idle 常量。
	cleanTaskRepo := repository.NewCleanTaskRepo(db)
	cleanupSvc := domain.NewCleanupService(cfg.DataDir, cleanTaskRepo)
	trashSvc := domain.NewTrashService(cfg.DataDir, cleanTaskRepo)
	// 回收站容量护栏：超上限时拒绝新的清理（而不是自动淘汰最旧条目——静默删除等于
	// 绕过用户确认）。0 = 不限制。初值来自 JIAN_TRASH_MAX_BYTES 播种，此后以 setting 表为准，
	// 保存时经 OnTrashCapacityChange 热注入（下一次清理的前置检查即生效）。
	cleanupSvc.SetTrashCapacity(effectiveTrashMaxBytes(settingSvc, cfg))
	// 过期上传会话复用既有 uploadTTL 口径（backup_upload.expires_at），不另造 idle 常量。
	backupUploadRepoForCleanup := repository.NewBackupUploadRepo(db)
	cleanupSvc.SetBackupUploadExpiredSource(func() ([]string, error) {
		expired, err := backupUploadRepoForCleanup.ListExpired(time.Now().UTC().Format(time.RFC3339Nano))
		if err != nil {
			return nil, err
		}
		names := make([]string, 0, len(expired))
		for _, rec := range expired {
			names = append(names, rec.UploadID)
		}
		return names, nil
	})
	// 隔离区候选判据：隔离目录的一级子目录名是**资产变更操作的 operationID**
	// （见 asset_mutation.go 的 blobs.Stage(hash, id)），不是恢复操作的标识。
	// 所以「是否被待生效操作引用」要问 blob_quarantine：该操作仍有 staged 记录时，
	// 隔离副本是那次变更的唯一回滚依据，绝不能当垃圾收走。
	//
	// 不接这一条会让 collectQuarantine 的跳过条件恒假，隔离区被全量回收——
	// 与规格要求的「宁可少收」正好相反。
	mutationRepoForCleanup := repository.NewAssetMutationRepo(db)
	cleanupSvc.SetPendingRestoreChecker(func(operationID string) bool {
		pending, err := mutationRepoForCleanup.HasPendingQuarantine(operationID)
		if err != nil {
			// 查不到就当作「仍在引用」：清理是破坏性动作，无法确认时必须保守。
			return true
		}
		return pending
	})

	// 上游健康历史：分钟桶采样 + 后台定时探测。
	//
	// 采样分两路写同一张表、用 source 区分（request 反映真实可用性，probe 补无流量时的空窗），
	// 绝不混在一行——混了就看不出「探测 100% 成功但真实请求 60%」这一关键分化。
	// 真实请求的采样走进程内小批量合并后刷盘（每 5s 一次 + 关停前 flush），
	// 因此回源主路径不承担写库成本；探测在后台作业里，天然不阻塞任何请求。
	upstreamHealthRepo := repository.NewUpstreamHealthRepo(db)
	upstreamHealthSvc := domain.NewUpstreamHealthService(upstreamHealthRepo)
	assetSvc.SetUpstreamHealthService(upstreamHealthSvc)
	// 维护类周期作业统一经调度器驱动（间隔 0 禁用；启动不立即执行）。
	schedulerSvc := scheduler.New()
	// 一键重启。单元名与作用域只来自启动配置（环境变量），
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
	// 五个周期作业的**初始**间隔取自 setting 表（初值由环境变量播种），此后可在设置页
	// 运行期改（经 PutSettings → OnRuntimePolicyChange → scheduler.SetInterval）。
	//
	// 0 仍然表示「不注册」：调度器的 Register 对 interval <= 0 是静默跳过。
	// 而**启动之后**把间隔改成 0 由作业自己的每轮检查接管（见 jobEnabled）：
	// 作业仍注册、每轮空转，这样"关掉一个跑着的清理作业"不需要重启。
	registerBlobGCJob(schedulerSvc,
		effectivePolicyDuration(settingSvc.BlobGCIntervalSecs, cfg.BlobGCInterval),
		func() (int, error) {
			if !jobEnabled(settingSvc.BlobGCIntervalSecs) {
				return 0, nil
			}
			return assetSvc.CleanupUnreferencedBlobs()
		})
	// 存储治理清理（隔离区空目录 / 过期 OCI 上传临时文件 / 终态元数据裁剪 / 代理缓存保留）。
	// 只做「不触碰任何在途文件」的四类清理；pending_gc 滞留件回收留待后续增量
	// （前置能力缺口：操作引擎没有「在写门下判定是否在途」的入口）。
	storageCleanupSvc := domain.NewStorageCleanupService(
		repository.NewStorageCleanupRepo(db),
		assetSvc,
		blobs,
		effectiveStorageCleanupOptions(settingSvc, cfg),
	)
	// 上游健康历史的 30 天保留由 storage-cleanup **顺带清理**（规格 §2.2 明确不新增作业）。
	// 只清理 upstream_health_minute 一张表，不波及 host_metric_minute 等同类分钟表。
	storageCleanupSvc.SetUpstreamHealthRepo(upstreamHealthRepo)
	registerStorageCleanupJob(schedulerSvc,
		effectivePolicyDuration(settingSvc.StorageCleanupIntervalSecs, cfg.StorageCleanupInterval),
		func() (domain.StorageCleanupResult, error) {
			// 间隔改成 0 表示禁用（下一轮起空转），见 jobEnabled 的说明。
			if !jobEnabled(settingSvc.StorageCleanupIntervalSecs) {
				return domain.StorageCleanupResult{}, nil
			}
			// 保留策略在**每轮开始时**现读 setting 表：这是"界面改完下一轮即生效"的全部来源，
			// 也是本作业唯一需要热生效的两个阈值。
			storageCleanupSvc.SetRetentionOptions(effectiveStorageCleanupOptions(settingSvc, cfg))
			return storageCleanupSvc.Run()
		})
	// 上游健康定时探测：只探 type=proxy 且 online 的仓库，绝不改断路器状态。
	upstreamProbeSvc := domain.NewUpstreamProbeService(repoRepo, assetSvc, upstreamHealthSvc)
	registerUpstreamProbeJob(schedulerSvc,
		effectivePolicyDuration(settingSvc.UpstreamProbeIntervalSecs, cfg.UpstreamProbeInterval),
		func(ctx context.Context) (domain.UpstreamProbeResult, error) {
			// 0 = 关闭定时探测（真实请求采样照常积累，只是无流量时不再补空窗）。
			if !jobEnabled(settingSvc.UpstreamProbeIntervalSecs) {
				return domain.UpstreamProbeResult{}, nil
			}
			return upstreamProbeSvc.Run(ctx)
		})
	// 周期化自动备份 + 保留策略。间隔的当前值取自 setting 表
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
	// 恢复演练服务（CLI 与 API 共用同一实现；API 端点在后续阶段接入）。
	backupDrillSvc := domain.NewBackupDrillService(backupSvc, repository.NewBackupVerificationRepo(db), cfg.DataDir)
	// 未管理备份包（磁盘有包体、登记无记录）的扫描与导入。
	// 与孤儿回收共用同一份差集口径，因此两者都由 adopt 服务给出候选。
	backupAdoptSvc := domain.NewBackupAdoptService(backupSvc, backupRepo)
	// 孤儿备份包回收。宽限期保证"拷入 → 界面导入"的窗口，
	// 身份校验保证不误删用户放错的其他归档（见 domain/backup_orphan_cleanup.go）。
	backupOrphanSvc := domain.NewBackupOrphanCleanupService(backupSvc, backupAdoptSvc,
		domain.BackupOrphanCleanupOptions{
			Grace: effectivePositiveHours(settingSvc.BackupOrphanGraceHours, cfg.BackupOrphanGrace),
		})
	registerBackupOrphanCleanupJob(schedulerSvc,
		effectivePolicyDuration(settingSvc.BackupOrphanCleanupIntervalSecs, cfg.BackupOrphanCleanupInterval),
		func() (domain.BackupOrphanCleanupResult, error) {
			// 间隔改成 0 表示禁用（下一轮起空转），见 jobEnabled 的说明。
			if !jobEnabled(settingSvc.BackupOrphanCleanupIntervalSecs) {
				return domain.BackupOrphanCleanupResult{}, nil
			}
			// 宽限期每轮现读：它在 Run 里决定"mtime 早于 now-grace 才算孤儿"，
			// 现读才能让界面改完下一轮即生效。
			backupOrphanSvc.SetGrace(effectivePositiveHours(settingSvc.BackupOrphanGraceHours, cfg.BackupOrphanGrace))
			return backupOrphanSvc.Run()
		})
	// 回收站保留期回收：只删回收站副本，不触碰 original_path（那是 restore 的职责）。
	// 保留期用闭包每轮现读（0 = 不自动清理），而不是在注册时固定：
	// 这正是「下一次作业运行生效」这条生效时机的实现方式。
	registerTrashGCJob(schedulerSvc,
		effectivePolicyDuration(settingSvc.TrashGCIntervalSecs, cfg.TrashGCInterval),
		func() time.Duration { return effectiveTrashRetention(settingSvc, cfg) },
		func(retention time.Duration) (domain.TrashPurgeResult, error) {
			// 间隔改成 0 表示禁用（下一轮起空转），见 jobEnabled 的说明。
			if !jobEnabled(settingSvc.TrashGCIntervalSecs) {
				return domain.TrashPurgeResult{}, nil
			}
			// 保留期 <= 0（不自动清理）由 ReconcileExpired 自己空转，无需在此重复判断。
			return trashSvc.ReconcileExpired(retention)
		})
	// 指标登记与 /metrics 渲染器；协议请求计数由 HTTP 层钩子（run）累加。
	metricsReg := metrics.New()
	metricsExp := metrics.NewExposition(metricsReg, schedulerJobStatuses(schedulerSvc))
	// 上游断路器指标。阻止事件计数按闭集 reason 分区（probe_failed / upstream_error，
	// 由 domain 定义、装配层注入）；阻止态仓库数在每次抓取时实时取数（只读内存态，
	// 不查库、不访问网络，符合 /metrics 的抓取路径约定）。
	assetSvc.SetUpstreamBlockRecorder(metricsReg)
	metricsExp.SetBlockedRepositories(assetSvc.BlockedUpstreamCount)
	// 解析结果指标。与上面的阻断事件**不是同一维度**：这里记的是「请求解析的结论」
	// （confirmed / uncertain），由顶层解析出口读回既有判定后累加一次；采样点唯一性
	// 由 domain 侧保证（不在成员层累加，否则一次 group 请求会被计成多次）。
	// 负缓存条目数在每次抓取时实时取数（只读内存态并顺带清理过期条目，不查库、不访问网络）。
	assetSvc.SetResolveFailureRecorder(metricsReg)
	metricsExp.SetNegativeCacheEntries(assetSvc.NegativeCacheEntriesCount)
	// 仓库级存储配额判定。预检（已知长度/流式）走 Admission，提交点权威复检走 CheckCommit；
	// 拒绝原因以闭集枚举累加进 jianartifact_publish_rejections_total{reason="quota"}。
	quotaSvc := domain.NewRepoQuotaService(repoRepo, assetRepo)
	quotaSvc.SetRejectionRecorder(metricsReg)
	assetSvc.SetStorageQuotaCheck(quotaSvc.CheckCommit)

	// OIDC 登录端点依赖；未配置 issuer 时保持 nil，端点返回 404（存在即启用）。
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

	// LDAP 目录侧校验；未配置 URL 时保持 nil，登录不回退目录。
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
		searchIndexSvc:      searchIndexSvc,
		storageHealthSvc:    storageHealthSvc,
		cleanupSvc:          cleanupSvc,
		cleanTaskRepo:       cleanTaskRepo,
		trashSvc:            trashSvc,
		quotaSvc:            quotaSvc,
		upstreamHealthSvc:   upstreamHealthSvc,
		upstreamHealthRepo:  upstreamHealthRepo,
		upstreamProbeSvc:    upstreamProbeSvc,
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
		backupUploads:       domain.NewBackupUploadService(repository.NewBackupUploadRepo(db), backupAdoptSvc, cfg.DataDir),
		backupAdopt:         backupAdoptSvc,
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

// seedProxyTuningSettings 把环境变量里的两项 proxy/group 调优作为**初值**播种进 setting 表。
//
// 与 seedBackupSettings 的差别只有一点，但这点必须说清：**逐键播**，不做整组判断。
// 备份三项在语义上是一组（"这本盘怎么留备份"），任何一项被界面改过就说明整组已由界面管过；
// 而成员确认预算与负缓存 TTL 是两个互不相干的旋钮——运维只想调负缓存、从没碰过成员预算，
// 整组判断会让成员预算永远拿不到环境变量里的初值，而它又确实被配在了环境变量里。
//
// 复用 seedSettingDefault 而不是无条件 Set：两项的初值都不可能是空串（秒数字面量），
// 因此"非空才写 + 键缺失才写"正好就是"缺键时用环境变量播种、已有值一律不动"。
// 显式 0（关闭负缓存）也照常播种：它是合法初值，不能被当成"未配置"跳过。
func seedProxyTuningSettings(settings *repository.SettingRepo, cfg *config.Config) error {
	defaults := []repository.SettingValue{
		{Key: domain.SettingKeyGroupMemberTimeout, Value: strconv.Itoa(int(cfg.GroupMemberTimeout.Seconds()))},
		{Key: domain.SettingKeyNegativeCacheTTL, Value: strconv.Itoa(int(cfg.NegativeCacheTTL.Seconds()))},
	}
	for _, value := range defaults {
		if err := seedSettingDefault(settings, value.Key, value.Value); err != nil {
			return err
		}
	}
	return nil
}

// applyProxyTuningSettings 把 setting 表里的两项 proxy/group 调优注入 AssetService。
//
// 两次调用的差异不是笔误，而是两项的语义不同：
//   - 成员确认预算**无禁用语义**（0 秒预算 = 每个成员立刻超时），0/缺失都表示"沿用当前值"，
//     因此只在 secs > 0 时应用；
//   - 负缓存 TTL 的 0 是"关闭"的显式表达，必须用 (secs, ok) 判定：ok=true 且 secs==0
//     要照实注入（真的关掉），ok=false 才回退——否则一次播种失败或 DB 抖动就会静默
//     关掉负缓存，把每个缺失路径都变成真实回源。
func applyProxyTuningSettings(settingSvc *domain.SettingService, assetSvc *domain.AssetService) {
	if secs := settingSvc.GroupMemberTimeoutSecs(); secs > 0 {
		assetSvc.SetGroupMemberTimeout(time.Duration(secs) * time.Second)
	}
	if secs, ok := settingSvc.NegativeCacheTTLSecs(); ok {
		assetSvc.SetNegativeCacheTTL(time.Duration(secs) * time.Second)
	}
}

// seedRuntimePolicySettings 把运行期策略 13 项的环境变量值作为**初值**播种进 setting 表。
//
// # 为什么逐键播、不做整组判断
//
// 与 seedProxyTuningSettings 同一理由，而且这里更明显：13 项分属四组、彼此不相干
// （改过回收站上限不代表运维碰过 blob-gc 间隔）。整组判断会让「运维只调了搜索预算」
// 变成「其余 12 项永远拿不到环境变量里的初值」，而它们确实被配在环境变量里。
//
// # 为什么复用 seedSettingDefault
//
// 它的语义正是本函数需要的：**键缺失才写**。这条例行是硬要求——每次重启都无条件写
// 会把运维在界面上改过的值覆盖回环境变量值，症状是「我明明改成 6 小时了，重启又变回
// 一天」这种最难排查的形态。
//
// 0 值也照常播种：本函数把 0 格式化成 "0"（非空串），seedSettingDefault 的「非空才写」
// 判据因此不会误跳——而 0 在 10 项上是**合法且有意义**的初值（禁用作业 / 不限制），
// 跳过它等于让「环境变量里特意关掉的作业」在重启后被默认值重新打开。
func seedRuntimePolicySettings(settings *repository.SettingRepo, cfg *config.Config) error {
	defaults := []repository.SettingValue{
		// 作业间隔（秒）；0 = 禁用。
		{Key: domain.SettingKeyBlobGCInterval, Value: strconv.Itoa(int(cfg.BlobGCInterval.Seconds()))},
		{Key: domain.SettingKeyStorageCleanupInterval, Value: strconv.Itoa(int(cfg.StorageCleanupInterval.Seconds()))},
		{Key: domain.SettingKeyBackupOrphanCleanupInterval, Value: strconv.Itoa(int(cfg.BackupOrphanCleanupInterval.Seconds()))},
		{Key: domain.SettingKeyTrashGCInterval, Value: strconv.Itoa(int(cfg.TrashGCInterval.Seconds()))},
		{Key: domain.SettingKeyUpstreamProbeInterval, Value: strconv.Itoa(int(cfg.UpstreamProbeInterval.Seconds()))},
		// 保留策略：宽限期按小时、两个存储阈值分别按小时与天、回收站保留按天。
		{Key: domain.SettingKeyBackupOrphanGraceHours, Value: strconv.Itoa(int(cfg.BackupOrphanGrace.Hours()))},
		{Key: domain.SettingKeyStorageMetadataRetentionDays, Value: strconv.Itoa(int(cfg.StorageMetadataRetention.Hours() / 24))},
		{Key: domain.SettingKeyStorageTempMaxAgeHours, Value: strconv.Itoa(int(cfg.StorageTempMaxAge.Hours()))},
		{Key: domain.SettingKeyTrashRetentionDays, Value: strconv.Itoa(cfg.TrashRetentionDays)},
		// 容量与阈值。
		{Key: domain.SettingKeyTrashMaxBytes, Value: strconv.FormatInt(cfg.TrashMaxBytes, 10)},
		{Key: domain.SettingKeyAutoBlockBaseSeconds, Value: strconv.Itoa(int(cfg.AutoBlockBaseSeconds.Seconds()))},
		{Key: domain.SettingKeySearchTimeoutSeconds, Value: strconv.Itoa(int(cfg.SearchTimeout.Seconds()))},
		{Key: domain.SettingKeyCapacityCacheTTLSeconds, Value: strconv.Itoa(int(cfg.CapacityCacheTTL.Seconds()))},
	}
	for _, value := range defaults {
		if err := seedSettingDefault(settings, value.Key, value.Value); err != nil {
			return err
		}
	}
	return nil
}

// effectivePolicyDuration 返回一项「0 有语义」的时长类策略的**当前生效值**。
//
// 关键口径：ok=true 且值为 0 表示运维显式禁用，必须返回 0（作业不注册 / 服务关掉缓存），
// 不得回退 fallback；只有 ok=false（键缺失或值非法）才回退——那是「尚未播种」或
// 「播种失败」的情况，此时环境变量解析结果就是唯一可信的初值。
//
// 这与既有 applyProxyTuningSettings 对负缓存 TTL 的处理逐字同构，理由也一样：
// 若把 0 当成「未配置」，运维在界面上关掉的作业会在下次重启时自己回来。
func effectivePolicyDuration(get func() (int, bool), fallback time.Duration) time.Duration {
	if secs, ok := get(); ok {
		return time.Duration(secs) * time.Second
	}
	return fallback
}

// effectivePositiveDuration 返回一项「0 无意义」的时长类策略的当前生效值。
//
// 与上面的差别只有一点：本类配置的 0 不是禁用，而是等价于「未配置」，故只在 > 0 时采用，
// 否则回退启动配置（口径同 domain 侧 SetSearchTimeout / SetAutoBlockBase 的守卫）。
func effectivePositiveDuration(get func() int, fallback time.Duration) time.Duration {
	if secs := get(); secs > 0 {
		return time.Duration(secs) * time.Second
	}
	return fallback
}

// effectivePositiveHours 是「0 无意义」的小时量纲版本（孤儿包回收宽限期）。
func effectivePositiveHours(get func() int, fallback time.Duration) time.Duration {
	if hours := get(); hours > 0 {
		return time.Duration(hours) * time.Hour
	}
	return fallback
}

// effectiveStorageCleanupOptions 返回本轮生效的存储治理清理阈值（每轮现读）。
func effectiveStorageCleanupOptions(settingSvc *domain.SettingService, cfg *config.Config) domain.StorageCleanupOptions {
	opts := domain.StorageCleanupOptions{}
	// 两个 0 都表示禁用对应子步骤，必须用 (值, ok) 区分「显式禁用」与「未配置」。
	if days, ok := settingSvc.StorageMetadataRetentionDays(); ok {
		opts.MetadataRetention = time.Duration(days) * 24 * time.Hour
	} else {
		opts.MetadataRetention = cfg.StorageMetadataRetention
	}
	if hours, ok := settingSvc.StorageTempMaxAgeHours(); ok {
		opts.TempMaxAge = time.Duration(hours) * time.Hour
	} else {
		opts.TempMaxAge = cfg.StorageTempMaxAge
	}
	return opts
}

// effectiveTrashRetention 返回本轮生效的回收站保留期（0 = 不自动清理）。
func effectiveTrashRetention(settingSvc *domain.SettingService, cfg *config.Config) time.Duration {
	if days, ok := settingSvc.TrashRetentionDays(); ok {
		return time.Duration(days) * 24 * time.Hour
	}
	return time.Duration(cfg.TrashRetentionDays) * 24 * time.Hour
}

// effectiveTrashMaxBytes 返回回收站容量上限的生效值（0 = 不限制）。
func effectiveTrashMaxBytes(settingSvc *domain.SettingService, cfg *config.Config) int64 {
	if n, ok := settingSvc.TrashMaxBytes(); ok {
		return n
	}
	return cfg.TrashMaxBytes
}

// applyRuntimePolicySettings 把「请求路径按需读取」的三项阈值按 setting 表现值注入一次。
//
// 为什么启动时还要再注入一次：上方构造服务时用的是环境变量解析结果，而 setting 表里的值
// 可能已被界面改过并跨重启保留——不补这一次，重启后运行时会退回环境变量值，
// 症状是"我昨天改的值今天又变回去了"（与 applyProxyTuningSettings 同一理由）。
//
// 三项的口径各不相同，因此不能共用一个读取函数：
//   - 搜索预算与断路器退避**无禁用语义**（0 会让搜索变成无界查询、退避变成不阻止），
//     故只在 > 0 时注入；
//   - 容量缓存 TTL 的 0 是合法的"禁用缓存"，必须用 (值, ok) 判定，否则关不掉。
func applyRuntimePolicySettings(settingSvc *domain.SettingService, cfg *config.Config, repoSvc *domain.RepositoryService, assetSvc *domain.AssetService) {
	if repoSvc != nil {
		repoSvc.SetSearchTimeout(effectivePositiveDuration(settingSvc.SearchTimeoutSeconds, cfg.SearchTimeout))
	}
	if assetSvc != nil {
		assetSvc.SetAutoBlockBase(effectivePositiveDuration(settingSvc.AutoBlockBaseSeconds, cfg.AutoBlockBaseSeconds))
	}
}

// runtimePolicyJobNames 把「设置键」映射到**调度器里实际注册的作业名**。
//
// 这张表必须与各 registerXxxJob 里写死的名字逐字一致（blob-gc / storage-cleanup /
// backup-orphan-cleanup / trash-gc / upstream-probe）。名字写错不会报错——
// Scheduler.SetInterval 对未知作业名只返回 false（与 Register「静默跳过」同口径）——
// 所以这里的错误只能靠本表 + 单测来保证，不能靠"看起来对"。
var runtimePolicyJobNames = map[string]string{
	domain.SettingKeyBlobGCInterval:              "blob-gc",
	domain.SettingKeyStorageCleanupInterval:      "storage-cleanup",
	domain.SettingKeyBackupOrphanCleanupInterval: "backup-orphan-cleanup",
	domain.SettingKeyTrashGCInterval:             "trash-gc",
	domain.SettingKeyUpstreamProbeInterval:       "upstream-probe",
}

// jobEnabled 报告某个「0 = 禁用」的作业在当前配置下是否应当真正执行。
//
// # 为什么需要这个每轮检查
//
// Scheduler.SetInterval 刻意拒绝 d <= 0（那会让 loop 进入 time.NewTimer(0) 的忙等），
// 而「0 表示禁用」是本批作业间隔的核心语义。若只靠 SetInterval，界面上把间隔改成 0
// 会**什么都不发生**——作业继续按旧间隔跑，运维却以为已经关了。这是最坏的一类缺陷：
// 关不掉，界面还显示"已关闭"。
//
// 因此这里采用与本仓 backup-auto 作业相同的做法：作业保持注册，每轮开始时现读配置，
// 0 就空转跳过。代价（操作文档需写清）：作业仍出现在「维护作业」清单里、仍可手动触发，
// 「看到它」不代表「它开着」；重启之后间隔为 0 的作业才彻底不注册。
func jobEnabled(get func() (int, bool)) bool {
	secs, ok := get()
	return !ok || secs > 0
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
		Assets:                  s.assetSvc, // 制品批量删除
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
		BackupDrill:             s.backupDrillSvc, // 恢复演练端点（202 受理 + 报告查询）
		Freeze:                  s.freeze,
		BackupImports:           s.backupImports,
		BackupUploads:           s.backupUploads,
		BackupAdopt:             s.backupAdopt, // 未管理备份包的扫描与导入
		Metrics:                 s.metricsExp,
		OIDC:                    s.oidcDeps,
		LDAPAuth:                s.ldapAuth,
		BackupLinkKey:           append([]byte(nil), s.auditAttentionKey...),
		ClusterTokenSet:         s.syncTokenSet,
		PublicURL:               s.publicURL,
		EnabledFormats:          s.repoSvc.EnabledFormats(),
		PublishPolicies:         s.publishPolicySvc,
		UserGroups:              s.userGroupSvc,       // 用户组管理端点（仅管理员）
		SearchIndex:             s.searchIndexSvc,     // 搜索索引状态与重建端点（仅管理员）
		StorageHealth:           s.storageHealthSvc,   // 存储体检快照（只读，仅管理员）
		Cleanup:                 s.cleanupSvc,         // 定向清理（移入回收站）
		CleanTasks:              s.cleanTaskRepo,      // 清理任务队列只读查询
		Trash:                   s.trashSvc,           // 回收站（恢复 / 彻底删除 / 清空）
		UpstreamHealth:          s.upstreamHealthRepo, // 上游健康历史查询
		MaintenanceJobs:         s.scheduler,          // 维护作业清单与手动触发
		Restart:                 s.restartSvc,         // 一键重启（先回响应，再重启）
		OnUpstreamTimeoutChange: func(d time.Duration) {
			if s.upstreamClient != nil {
				s.upstreamClient.SetTimeout(d)
			}
		},
		// 两项 proxy/group 调优的热生效链路：设置页保存 → PutSettings 回调 →
		// AssetService 的 setter（内部有锁保护，与在途 group 读并发安全）。
		// 注入点必须在 NewAssetService 之后、第一个请求之前，而 handlers 是在
		// 服务启动装配的最后一步构造的，早于任何请求。
		OnGroupMemberTimeoutChange: func(d time.Duration) {
			if s.assetSvc != nil {
				s.assetSvc.SetGroupMemberTimeout(d)
			}
		},
		OnNegativeCacheTTLChange: func(d time.Duration) {
			if s.assetSvc != nil {
				// 0 = 关闭负缓存，必须原样传给 setter（setter 的守卫只忽略负数）。
				s.assetSvc.SetNegativeCacheTTL(d)
			}
		},
		// 运行期策略里时长类的 8 项热生效链路：设置页保存 → PutSettings → 这里按 key 分发。
		//
		// 为什么把分发放在装配层而不是 api 层：key → 目标的映射是装配知识
		// （作业在调度器里叫什么名字、哪个服务持有该阈值）。放在 api 层会让作业改名时
		// 静默失配——SetInterval 对未知作业名只返回 false，不报错。
		OnRuntimePolicyChange: func(key string, d time.Duration) {
			s.applyRuntimePolicyChange(key, d)
		},
		// 回收站容量上限（字节，0 = 不限制）。量纲不是时长，故单独一条回调。
		OnTrashCapacityChange: func(maxBytes int64) {
			if s.cleanupSvc != nil {
				s.cleanupSvc.SetTrashCapacity(maxBytes)
			}
		},
	})
}

// applyRuntimePolicyChange 把一项时长类运行期策略推给持有它的运行时状态。
//
// 五个作业间隔走 Scheduler.SetInterval：从**下一轮**起生效，当轮不打断
// （当轮可能正在写盘，强行中断会留下半成品）。
//
// 间隔被改成 0（禁用）时**不调用** SetInterval：它会拒绝 d <= 0（0 会让 loop 进入
// time.NewTimer(0) 忙等）。真正的禁用由作业自己的每轮检查完成（见 jobEnabled）——
// 下一轮起空转跳过。作业仍留在「维护作业」清单里，重启后才彻底不注册。
//
// 未注册的作业名会返回 false 且不报错（与 Register「静默跳过」同口径）。这里**必须
// 打一条警告**：那个 false 的真实含义是「这个作业在启动时间隔就是 0，因此没有注册，
// 你的新间隔不会生效，需要重启一次」——静默吞掉它，运维会以为改成功了。
func (s *appServices) applyRuntimePolicyChange(key string, d time.Duration) {
	if jobName, ok := runtimePolicyJobNames[key]; ok {
		if s.scheduler == nil {
			return
		}
		if d <= 0 {
			log.Printf("提示：作业 %s 的间隔已设为 0（禁用），将从下一轮起跳过执行；"+
				"作业仍保留在维护作业清单里，重启后不再注册。", jobName)
			return
		}
		if !s.scheduler.SetInterval(jobName, d) {
			log.Printf("警告：作业 %s 未注册，间隔 %s 未生效。"+
				"该作业可能因**启动时**间隔为 0 而被禁用，需重启进程后才会按新值注册。",
				jobName, d)
		}
		return
	}
	switch key {
	case domain.SettingKeySearchTimeoutSeconds:
		if s.repoSvc != nil {
			s.repoSvc.SetSearchTimeout(d)
		}
	case domain.SettingKeyAutoBlockBaseSeconds:
		if s.assetSvc != nil {
			s.assetSvc.SetAutoBlockBase(d)
		}
	case domain.SettingKeyCapacityCacheTTLSeconds:
		if s.operationsMetrics != nil {
			// 0 = 禁用缓存，必须原样传入（setter 内部会顺带让既有缓存立即失效）。
			s.operationsMetrics.SetCapacityCacheTTL(d)
		}
	}
}

// schedulerJobStatuses 把调度器作业状态适配为指标渲染快照；
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
