// Package config 加载与校验 JianArtifact 后端的运行配置。
//
// 横切层（见 docs/ARCHITECTURE.md 分层）：供各层读取，不反向依赖业务层。
// 全部配置项经环境变量注入；凭据类（JWT 密钥）引用外部值或本地持久化，不写入元数据库、不进日志。
package config

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/formats"
)

// 默认值与环境变量名。
const (
	EnvDataDir                = "JIAN_DATA_DIR"
	EnvHTTPAddr               = "JIAN_HTTP_ADDR"
	EnvJWTSecret              = "JIAN_JWT_SECRET"
	EnvMigrationCredentialKey = "JIAN_MIGRATION_CREDENTIAL_KEY"
	EnvUpstreamTimeout        = "JIAN_UPSTREAM_TIMEOUT"     // proxy 回源整体超时，单位秒
	EnvSyncPeerURL            = "JIAN_SYNC_PEER_URL"        // 遗留复制对端基址；主备角色模式下不参与调度
	EnvSyncInterval           = "JIAN_SYNC_INTERVAL"        // 同步轮询间隔（FR-85），单位秒
	EnvPublicURL              = "JIAN_PUBLIC_URL"           // 对外基础 URL（FR-87）；CDN 域名，隐藏源站 IP
	EnvEnabledFormats         = "JIAN_ENABLED_FORMATS"      // 启用的协议格式，逗号分隔（FR-32）
	EnvBlobGCInterval         = "JIAN_BLOB_GC_INTERVAL"     // 孤立 blob 定时清理间隔，单位秒；0 禁用
	EnvTLSAddr                = "JIAN_TLS_ADDR"             // 服务内置 TLS 监听地址（FR-131）；空 = 不启用
	EnvTLSCert                = "JIAN_TLS_CERT"             // TLS PEM 证书文件路径（FR-131）
	EnvTLSKey                 = "JIAN_TLS_KEY"              // TLS PEM 私钥文件路径（FR-131）
	EnvOIDCIssuer             = "JIAN_OIDC_ISSUER"          // OIDC 身份提供方 issuer（FR-34）；空 = 不启用 OIDC 登录
	EnvOIDCClientID           = "JIAN_OIDC_CLIENT_ID"       // OIDC 客户端 ID（FR-34）
	EnvOIDCClientSecret       = "JIAN_OIDC_CLIENT_SECRET"   // OIDC 客户端密钥（FR-34）；只从环境变量读取，不入库不打印
	EnvOIDCRedirectURL        = "JIAN_OIDC_REDIRECT_URL"    // OIDC 回调地址（FR-34），须与 IdP 侧注册一致
	EnvOIDCUsernameClaim      = "JIAN_OIDC_USERNAME_CLAIM"  // 用户名 claim（FR-34）；缺省 preferred_username
	EnvOIDCAllowedDomains     = "JIAN_OIDC_ALLOWED_DOMAINS" // 允许自动建号的邮箱域名（FR-34），逗号分隔；空 = 不限制
	EnvLDAPURL                = "JIAN_LDAP_URL"             // LDAP 目录地址（FR-35，ldap:// 或 ldaps://）；空 = 不启用
	EnvLDAPBindDN             = "JIAN_LDAP_BIND_DN"         // 检索用服务账号 DN（FR-35）；空 = 改用 DN 模板直接绑定
	EnvLDAPBindPassword       = "JIAN_LDAP_BIND_PASSWORD"   // 服务账号口令（FR-35）；只从环境变量读取，不入库不打印
	EnvLDAPBaseDN             = "JIAN_LDAP_BASE_DN"         // 用户检索基准 DN（FR-35）
	EnvLDAPUserFilter         = "JIAN_LDAP_USER_FILTER"     // 用户检索过滤器（FR-35），必须含 {username} 占位
	EnvLDAPEmailAttr          = "JIAN_LDAP_EMAIL_ATTR"      // 邮箱属性名（FR-35）；缺省 mail
	EnvLDAPStartTLS           = "JIAN_LDAP_STARTTLS"        // 对 ldap:// 是否使用 StartTLS（FR-35）；缺省关闭
	EnvLDAPCAFile             = "JIAN_LDAP_CA_FILE"         // 自定义 CA（PEM）路径（FR-35）；不提供跳过证书校验的开关

	// FR-41 存储治理清理作业的开关（单独成组：名字更长，并入上一组会只为对齐而重排全部既有行）。
	EnvStorageCleanupInterval   = "JIAN_STORAGE_CLEANUP_INTERVAL"        // 存储治理清理作业间隔，单位秒；0 禁用
	EnvStorageMetadataRetention = "JIAN_STORAGE_METADATA_RETENTION_DAYS" // 终态操作与隔离元数据保留天数；0 禁用
	EnvStorageTempMaxAge        = "JIAN_STORAGE_TEMP_MAX_AGE_HOURS"      // 过期上传临时文件阈值，单位小时；0 禁用

	// FR-40 周期化自动备份与保留策略（单独成组，理由同上）。
	EnvBackupAutoInterval  = "JIAN_BACKUP_AUTO_INTERVAL"  // 自动备份作业间隔，单位秒；0 禁用
	EnvBackupKeepLast      = "JIAN_BACKUP_KEEP_LAST"      // 保留最近 N 份自动备份；0 = 不按份数限制
	EnvBackupRetentionDays = "JIAN_BACKUP_RETENTION_DAYS" // 自动备份保留天数；0 = 不限天数

	// FR-40 追加八 备份孤儿回收（单独成组，理由同上）。
	EnvBackupOrphanCleanupInterval = "JIAN_BACKUP_ORPHAN_CLEANUP_INTERVAL" // 孤儿备份包回收间隔，单位秒；0 禁用
	EnvBackupOrphanGraceHours      = "JIAN_BACKUP_ORPHAN_GRACE_HOURS"      // 回收宽限期，单位小时；0 取默认 1

	// FR-43 上游断路器补强：auto-block 退避起始时长，单位秒。
	EnvAutoBlockBaseSeconds = "JIAN_AUTO_BLOCK_BASE_SECONDS" // proxy 上游首次失败的阻止窗口时长；非正数回落默认

	// FR-40 追加七：一键重启的能力配置（单独成组，理由同上）。
	// 单元名只从环境变量读取，**绝不**接受请求体输入——见 domain/restart_service.go 的注入防护说明。
	EnvRestartUnit  = "JIAN_RESTART_UNIT"  // 重启目标单元名；缺省 jianartifact
	EnvRestartScope = "JIAN_RESTART_SCOPE" // 作用域 system / systemd-user / docker；非法值回落 system

	// FR-149 搜索性能：搜索链路（网页搜索 / npm 搜索 / suggest）的总预算，单位秒。
	EnvSearchTimeoutSeconds = "JIAN_SEARCH_TIMEOUT_SECONDS" // 一次搜索的总预算上限；非正数回落默认（无禁用语义）

	// 仪表盘容量聚合结果的缓存有效期，单位秒；0 禁用缓存（每次请求实时聚合）。
	EnvCapacityCacheTTLSeconds = "JIAN_CAPACITY_CACHE_TTL_SECONDS"

	// SQLite 连接池上限。WAL 允许「多读 + 单写」，把池放宽才能真正吃到并发读的收益；
	// 非正数与非法值回落默认（池上限取 0 等于禁用连接池，不是有意义的配置，故无禁用语义）。
	EnvSQLiteMaxConns = "JIAN_SQLITE_MAX_CONNS"

	defaultDataDir         = "./data"
	defaultHTTPAddr        = ":8080"
	defaultUpstreamTimeout = 30 * time.Second
	defaultSyncInterval    = 5 * time.Second // 复制轮询间隔默认 5s（近实时）
	defaultBlobGCInterval  = 24 * time.Hour  // 孤立 blob 默认每日清理一次
	// FR-41 存储治理清理的默认阈值：每日一轮、终态元数据保留 7 天、临时文件 24 小时。
	// 隔离区空目录的宽限期不设开关（见 domain/storage_cleanup.go 的常量与理由）。
	defaultStorageCleanupInterval   = 24 * time.Hour
	defaultStorageMetadataRetention = 7 * 24 * time.Hour
	defaultStorageTempMaxAge        = 24 * time.Hour
	// FR-40 自动备份与保留策略的默认值。
	//
	// 间隔默认 0 = **关闭**。这一条是**有意**的，不是遗漏：
	// 备份是**全量**包（每份 ≈ 数据目录大小），因此默认开启会在用户毫无预期的情况下
	// 持续消耗磁盘。实测存在"20GB 数据 / 40GB 磁盘"这类配置——默认开几天就能把盘写满，
	// 而盘满会连带把服务写挂，代价远高于"少一份备份"。
	// 反面（默认关闭、出事时没备份）可以由运维事后补配置修复；盘被写满则是运行故障。
	// 两害相权，默认关闭。
	//
	// 保留份数与天数同样只是**上限**，不构成"占用量可控"的保证：
	// 7 份 × 20GB = 140GB，仍可能远超可用磁盘。因此开启前必须自己算一遍
	// 份数 × 数据量 ≤ 可用空间，见 docs/OPERATIONS.md「备份占用的容量规划」。
	defaultBackupAutoInterval  = 0
	defaultBackupKeepLast      = 7
	defaultBackupRetentionDays = 30
	// FR-40 追加八 孤儿备份包回收的默认值：每日一轮、宽限期 1 小时。
	// 间隔默认 24h 而非 0（与自动备份相反）：孤儿文件只会占盘不会自行消失，
	// 回收是纯粹的兜底动作，开启它不存在"悄悄占磁盘"的风险。
	// 宽限期保证"拷入 → 界面导入"的操作窗口（见 domain/backup_orphan_cleanup.go）。
	defaultBackupOrphanCleanupInterval = 24 * time.Hour
	defaultBackupOrphanGrace           = time.Hour
	// defaultAutoBlockBase 是 auto-block 退避起始时长（FR-43）。
	// 与 domain 侧兜底常量 autoBlockInitial（proxy_health.go）保持同值：
	// 装配层始终注入本值，domain 常量只在未装配/测试构造时兜底。
	defaultAutoBlockBase = 40 * time.Second
	// defaultSearchTimeout 是搜索链路总预算的默认值（FR-149 §3.4）。
	// 与 domain 侧兜底常量 searchTimeoutDefault（repository_service.go）保持同值：
	// 装配层始终注入本值，domain 常量只在未装配/测试构造时兜底。
	defaultSearchTimeout = 10 * time.Second
	// defaultCapacityCacheTTL 是仪表盘容量聚合缓存有效期的默认值（JIAN_CAPACITY_CACHE_TTL_SECONDS）。
	// 与 repository 侧兜底常量 defaultCapacityCacheTTL（operations_observability_repo.go）保持同值：
	// 装配层始终注入本值，repository 常量只在未装配 / 测试构造时兜底。
	// 取 30 秒：容量聚合要遍历整张 asset 表（线上约 8.6 GB），而仪表盘是常用落地页；
	// 30 秒的滞后对运维展示无影响，却能把重复加载的聚合次数压到「每分钟最多两次」。
	defaultCapacityCacheTTL = 30 * time.Second
	// defaultSQLiteMaxConns 是 SQLite 连接池上限的默认值（JIAN_SQLITE_MAX_CONNS）。
	// 与 persistence 侧兜底常量 persistence.DefaultMaxOpenConns 保持同值：
	// 装配层始终注入本值，Open 在未显式给值时用同一个数兜底。
	// 取 8：WAL 下读并发收益在 8 条连接上已基本吃满，再放宽只是增加 SQLite 侧的
	// 页面缓存副本与文件句柄，收益递减。
	defaultSQLiteMaxConns = 8
	// defaultOIDCUsernameClaim 是 OIDC 用户名的缺省 claim；各 IdP 常见取值见 OPERATIONS。
	defaultOIDCUsernameClaim = "preferred_username"
	// defaultLDAPUserFilter 与 defaultLDAPEmailAttr 是 LDAP 检索的缺省口径（OpenLDAP 风格 uid）。
	defaultLDAPUserFilter = "(uid={username})"
	defaultLDAPEmailAttr  = "mail"
	// FR-40 追加七：一键重启的默认单元名与默认作用域。
	// 单元名默认 jianartifact（与部署脚本里的 systemd 单元同名），作用域默认 system（systemctl）。
	defaultRestartUnit  = "jianartifact"
	defaultRestartScope = "system"

	dbFileName                       = "jianartifact.db"
	blobDirName                      = "blobs"
	secretFileName                   = "jwt.secret"
	migrationCredentialKeyFileName   = "migration-credential.key"
	replicationCredentialKeyFileName = "replication-credential.key"
)

// Config 是解析后的运行配置。路径均为绝对化后的结果，便于日志与探错一致。
type Config struct {
	DataDir                string        // 数据根目录（SQLite 与 blob 存放）
	HTTPAddr               string        // HTTP 监听地址
	DBPath                 string        // SQLite 文件路径（DataDir/jianartifact.db）
	BlobDir                string        // blob 存储目录（DataDir/blobs）
	JWTSecret              []byte        // JWT HS256 签名密钥（不入库、不打印）
	MigrationCredentialKey []byte        // 在线迁移凭据 AES-256-GCM 专用密钥（不入库、不打印）
	UpstreamTimeout        time.Duration // proxy 回源整体超时
	// AutoBlockBaseSeconds 是 proxy 上游 auto-block 退避起始时长（FR-43）：
	// 首次失败的阻止窗口，之后每档翻倍。非正数（含非法值）已在解析期回落默认值。
	AutoBlockBaseSeconds   time.Duration
	SyncPeerURL            string        // 遗留复制对端基址；仅为兼容旧配置保留，不驱动主备调度
	SyncInterval           time.Duration // 复制轮询间隔（FR-85）
	PublicURL              string        // 对外基础 URL（FR-87，如 https://repo.example.com）；空则回退请求 Host 推断
	EnabledFormats         formats.Set   // 启动时启用的协议格式（FR-32）
	BlobGCInterval         time.Duration // primary 孤立 blob 定时清理间隔；0 禁用
	StorageCleanupInterval time.Duration // FR-41 存储治理清理作业间隔；0 禁用
	// StorageMetadataRetention 是终态资产操作与隔离元数据的保留期（FR-41）；0 禁用裁剪。
	StorageMetadataRetention time.Duration
	// StorageTempMaxAge 是过期上传临时文件（<BlobDir>/tmp/oci-upload）的最长滞留时长（FR-41）；0 禁用。
	StorageTempMaxAge time.Duration
	// BackupAutoInterval 是周期化自动备份作业的间隔（FR-40）；0 禁用（作业不注册）。
	// 调度间隔在启动时固定、不支持运行时热改——改它需要重启进程（见 docs/OPERATIONS.md）。
	BackupAutoInterval time.Duration
	// BackupKeepLast 是自动备份按份数保留的上限（FR-40）；0 = 不按份数限制。
	BackupKeepLast int
	// BackupRetentionDays 是自动备份按时间的保留上限（FR-40）；0 = 不限天数。
	BackupRetentionDays int
	// BackupOrphanCleanupInterval 是孤儿备份包回收作业的间隔（FR-40 追加八）；0 禁用。
	BackupOrphanCleanupInterval time.Duration
	// BackupOrphanGrace 是孤儿回收的宽限期：mtime 晚于此窗口的文件一律保留，
	// 给"拷入 → 界面导入"留出操作窗口（FR-40 追加八）。
	BackupOrphanGrace time.Duration
	// RestartUnit 是一键重启的目标单元名（FR-40 追加七）；仅来自环境变量，
	// 绝不从请求体读取——这是防命令注入的第一道边界（见 domain/restart_service.go）。
	RestartUnit string
	// RestartScope 是一键重启的作用域（FR-40 追加七）：system / systemd-user / docker；
	// 非法值已在解析期回落 system，装配层可放心使用。
	RestartScope string
	// SearchTimeout 是搜索链路（网页搜索 / npm 搜索 / suggest）的总预算上限（FR-149 §3.4）；
	// 非正数（含非法值）已在解析期回落默认值，**无禁用语义**。
	SearchTimeout time.Duration
	// CapacityCacheTTL 是仪表盘容量聚合结果的缓存有效期（JIAN_CAPACITY_CACHE_TTL_SECONDS）。
	// 0 = 禁用缓存（每次请求实时聚合），非数字 / 负数已在解析期回落默认 30s。
	CapacityCacheTTL time.Duration
	// SQLiteMaxConns 是 SQLite 连接池上限（JIAN_SQLITE_MAX_CONNS）。
	// 非正数（含非法值）已在解析期回落默认 defaultSQLiteMaxConns，**无禁用语义**。
	SQLiteMaxConns int
	TLSAddr        string     // 服务内置 TLS 监听地址（FR-131）；空 = 不启用
	TLSCert        string     // TLS PEM 证书文件路径（FR-131）
	TLSKey         string     // TLS PEM 私钥文件路径（FR-131）
	OIDC           OIDCConfig // FR-34：OIDC 身份源接入配置（Issuer 空 = 不启用）
	LDAP           LDAPConfig // FR-35：LDAP 身份源接入配置（URL 空 = 不启用）
}

// LDAPConfig 是 LDAP 目录接入配置（FR-35，见 ADR-0029）；
// 服务账号口令只从环境变量解析，不落库、不打印、不进审计。
type LDAPConfig struct {
	URL          string // 目录地址（ldap:// 或 ldaps://）
	BindDN       string // 检索用服务账号 DN；空 = 改用 DN 模板直接绑定
	BindPassword string // 服务账号口令（仅环境变量）
	BaseDN       string // 用户检索基准 DN
	UserFilter   string // 用户检索过滤器，含 {username} 占位
	EmailAttr    string // 邮箱属性名
	StartTLS     bool   // 对 ldap:// 使用 StartTLS
	CAFile       string // 自定义 CA（PEM）路径；留空用系统根证书
}

// Enabled 报告是否启用 LDAP 登录（配置了 URL 即启用）。
func (c LDAPConfig) Enabled() bool { return c.URL != "" }

// OIDCConfig 是 OIDC 身份源接入配置（FR-34，见 ADR-0029）；
// 凭据只从环境变量解析，不落库、不打印、不进审计。
type OIDCConfig struct {
	Issuer         string   // 身份提供方 issuer（discovery 基址）
	ClientID       string   // 客户端 ID
	ClientSecret   string   // 客户端密钥（仅环境变量）
	RedirectURL    string   // 回调地址（须与 IdP 注册一致）
	UsernameClaim  string   // 用户名 claim
	AllowedDomains []string // 允许自动建号的邮箱域名；空 = 不限制
}

// Enabled 报告是否启用 OIDC 登录（配置了 issuer 即启用）。
func (c OIDCConfig) Enabled() bool { return c.Issuer != "" }

// Load 从环境变量解析配置并确保 data / blob 目录存在。
// JWT 密钥优先取 JIAN_JWT_SECRET；缺省时从 data 目录的 jwt.secret 读取，
// 仍无则生成随机密钥并持久化（0600），同时向 stderr 告警一次。
//
// 数据目录的解析交给 ResolveDataDir（见 datadir.go）：显式设置优先，未设置时尝试
// 自动发现正在运行的实例，最后才回落默认值——并且**每次都会说明依据**。
func Load() (*Config, error) {
	resolved, err := ResolveDataDir(os.Getenv(EnvDataDir))
	if err != nil {
		return nil, err
	}
	// 歧义（发现多个候选）时不能猜：猜错就是操作到另一个库，代价远高于让用户显式指定一次。
	if len(resolved.Ambiguous) > 0 {
		return nil, fmt.Errorf(
			"未设置 %s，且发现多个不同的数据目录，无法确定该用哪一个：\n    %s\n"+
				"    请显式设置 %s 指定要操作的实例",
			EnvDataDir, strings.Join(resolved.Ambiguous, "\n    "), EnvDataDir)
	}
	if resolved.Warning != "" {
		// 走 stderr：不能污染 --json 的 stdout（命令的输出要能被管道消费）。
		fmt.Fprintln(os.Stderr, resolved.Warning)
	}
	// 自动发现到实例时，**整份采纳**该实例的 env 文件（只补当前进程未设置的键）。
	//
	// 只取目录是不够的：那样该实例的其余配置仍缺失，CLI 会以为 JWT 密钥"未设置"，
	// 于是在服务的数据目录里现场生成一份，并对 `backup list` 这种只读命令打印误导性警告。
	// 采纳后 CLI 的行为与服务一致，且不产生任何多余写入。显式设置的环境变量优先级更高——
	// 它们是调用方刻意的覆盖意图。
	if resolved.EnvFileContent != "" {
		applyEnvFileDefaults(resolved.EnvFileContent)
	}
	// 采纳之后重新解析目录，因为 env 文件本身可能就带 JIAN_DATA_DIR（此时它同样生效）。
	if v := os.Getenv(EnvDataDir); strings.TrimSpace(v) != "" {
		abs, err := absDataDir(v)
		if err != nil {
			return nil, err
		}
		resolved.Dir = abs
	}
	absData := resolved.Dir
	blobDir := filepath.Join(absData, blobDirName)
	for _, dir := range []string{absData, blobDir, filepath.Join(blobDir, "tmp")} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("创建目录 %s：%w", dir, err)
		}
	}

	secret, err := loadOrCreateSecret(absData)
	if err != nil {
		return nil, err
	}
	migrationCredentialKey, err := loadOrCreateMigrationCredentialKey(absData)
	if err != nil {
		return nil, err
	}
	enabledFormats, err := enabledFormats()
	if err != nil {
		return nil, err
	}

	oidc, err := oidcConfig()
	if err != nil {
		return nil, err
	}
	ldap, err := ldapConfig()
	if err != nil {
		return nil, err
	}

	return &Config{
		DataDir:                     absData,
		HTTPAddr:                    envOr(EnvHTTPAddr, defaultHTTPAddr),
		DBPath:                      filepath.Join(absData, dbFileName),
		BlobDir:                     blobDir,
		JWTSecret:                   secret,
		MigrationCredentialKey:      migrationCredentialKey,
		UpstreamTimeout:             upstreamTimeout(),
		AutoBlockBaseSeconds:        autoBlockBase(),
		SyncPeerURL:                 os.Getenv(EnvSyncPeerURL),
		SyncInterval:                syncInterval(),
		PublicURL:                   os.Getenv(EnvPublicURL),
		EnabledFormats:              enabledFormats,
		BlobGCInterval:              blobGCInterval(),
		StorageCleanupInterval:      storageCleanupInterval(),
		StorageMetadataRetention:    storageMetadataRetention(),
		StorageTempMaxAge:           storageTempMaxAge(),
		BackupAutoInterval:          backupAutoInterval(),
		BackupKeepLast:              backupKeepLast(),
		BackupRetentionDays:         backupRetentionDays(),
		BackupOrphanCleanupInterval: backupOrphanCleanupInterval(),
		BackupOrphanGrace:           backupOrphanGrace(),
		SearchTimeout:               searchTimeout(),
		CapacityCacheTTL:            capacityCacheTTL(),
		SQLiteMaxConns:              sqliteMaxConns(),
		TLSAddr:                     envOr(EnvTLSAddr, ""),
		TLSCert:                     os.Getenv(EnvTLSCert),
		TLSKey:                      os.Getenv(EnvTLSKey),
		OIDC:                        oidc,
		LDAP:                        ldap,
		RestartUnit:                 envOr(EnvRestartUnit, defaultRestartUnit),
		RestartScope:                restartScope(),
	}, nil
}

// loadOrCreateMigrationCredentialKey 解析在线迁移凭据的独立 AES-256 密钥。
// 环境变量使用 base64 编码的 32 字节值；未配置时仅在 data 目录持久化随机密钥。
func loadOrCreateMigrationCredentialKey(dataDir string) ([]byte, error) {
	if raw := strings.TrimSpace(os.Getenv(EnvMigrationCredentialKey)); raw != "" {
		key, err := base64.StdEncoding.DecodeString(raw)
		if err != nil || len(key) != 32 {
			return nil, fmt.Errorf("解析 %s：必须为 base64 编码的 32 字节密钥", EnvMigrationCredentialKey)
		}
		return key, nil
	}
	keyPath := filepath.Join(dataDir, migrationCredentialKeyFileName)
	if key, err := os.ReadFile(keyPath); err == nil {
		if len(key) != 32 {
			return nil, fmt.Errorf("读取迁移凭据密钥：长度必须为 32 字节")
		}
		return key, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("读取迁移凭据密钥：%w", err)
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("生成迁移凭据密钥：%w", err)
	}
	if err := os.WriteFile(keyPath, key, 0o600); err != nil {
		return nil, fmt.Errorf("持久化迁移凭据密钥：%w", err)
	}
	fmt.Fprintf(os.Stderr, "警告：未设置 %s，已在 %s 生成并持久化随机迁移凭据密钥；生产环境建议显式配置。\n", EnvMigrationCredentialKey, keyPath)
	return key, nil
}

// enabledFormats 解析格式开关。环境变量缺失时保持旧版本的三格式默认值，
// 显式空字符串表示关闭全部格式。
func enabledFormats() (formats.Set, error) {
	raw, ok := os.LookupEnv(EnvEnabledFormats)
	if !ok {
		return formats.Default(), nil
	}
	set, err := formats.Parse(raw)
	if err != nil {
		return formats.Set{}, fmt.Errorf("解析 %s：%w", EnvEnabledFormats, err)
	}
	return set, nil
}

// syncInterval 解析 JIAN_SYNC_INTERVAL（秒）；缺省或非法（<=0）时取默认值。
func syncInterval() time.Duration {
	v := os.Getenv(EnvSyncInterval)
	if v == "" {
		return defaultSyncInterval
	}
	secs, err := strconv.Atoi(v)
	if err != nil || secs <= 0 {
		return defaultSyncInterval
	}
	return time.Duration(secs) * time.Second
}

// blobGCInterval 解析 JIAN_BLOB_GC_INTERVAL（秒）；缺省或非法值取每日一次，显式 0 禁用。
func blobGCInterval() time.Duration {
	v := os.Getenv(EnvBlobGCInterval)
	if v == "" {
		return defaultBlobGCInterval
	}
	secs, err := strconv.Atoi(v)
	if err != nil || secs < 0 {
		return defaultBlobGCInterval
	}
	return time.Duration(secs) * time.Second
}

// storageCleanupInterval 解析 JIAN_STORAGE_CLEANUP_INTERVAL（秒）；
// 缺省或非法值取每日一次，显式 0 禁用（与 blobGCInterval 同构）。
func storageCleanupInterval() time.Duration {
	return envDurationOr(EnvStorageCleanupInterval, defaultStorageCleanupInterval, time.Second)
}

// storageMetadataRetention 解析 JIAN_STORAGE_METADATA_RETENTION_DAYS（天）；
// 缺省或非法值取 7 天，显式 0 禁用元数据裁剪。
func storageMetadataRetention() time.Duration {
	return envDurationOr(EnvStorageMetadataRetention, defaultStorageMetadataRetention, 24*time.Hour)
}

// storageTempMaxAge 解析 JIAN_STORAGE_TEMP_MAX_AGE_HOURS（小时）；
// 缺省或非法值取 24 小时，显式 0 禁用过期上传临时文件清理。
func storageTempMaxAge() time.Duration {
	return envDurationOr(EnvStorageTempMaxAge, defaultStorageTempMaxAge, time.Hour)
}

// backupAutoInterval 解析 JIAN_BACKUP_AUTO_INTERVAL（秒，FR-40）：周期化自动备份作业的间隔。
// 缺省、非法与负数取默认值 0（= **关闭**），显式 0 同样是关闭——本项默认关闭，
// 因为自动备份会在无人确认的情况下持续占用磁盘，必须是运维显式开启的能力。
// 与 storageCleanupInterval 的「默认开、0 关」不同，但「0 = 不注册作业」的机制完全一致。
func backupAutoInterval() time.Duration {
	return envDurationOr(EnvBackupAutoInterval, defaultBackupAutoInterval, time.Second)
}

// backupKeepLast 解析 JIAN_BACKUP_KEEP_LAST（份，FR-40）：保留最近 N 份**自动**备份。
// 缺省或非法值取 7；显式 0 = **不按份数限制**（只受天数约束），而不是「一份都不留」——
// 把 0 解释成"全删"会让一个笔误直接清空备份目录，而"不限制"永远只导致多留。
// 负数与非法值回落默认 7（不采用"负数为不限制"的隐式写法，避免与 0 的语义重叠）。
func backupKeepLast() int {
	return envNonNegativeIntOr(EnvBackupKeepLast, defaultBackupKeepLast)
}

// backupRetentionDays 解析 JIAN_BACKUP_RETENTION_DAYS（天，FR-40）：自动备份的保留天数。
// 缺省或非法值取 30；显式 0 = 不限天数（只受份数约束）。0 语义与 backupKeepLast 同构。
func backupRetentionDays() int {
	return envNonNegativeIntOr(EnvBackupRetentionDays, defaultBackupRetentionDays)
}

// backupOrphanCleanupInterval 解析 JIAN_BACKUP_ORPHAN_CLEANUP_INTERVAL（秒，FR-40 追加八）：
// 孤儿备份包回收作业的间隔。缺省或非法值取 24 小时，显式 0 禁用（不注册作业）。
//
// 默认开：孤儿文件只占磁盘、不会自行消失，回收是纯兜底动作，不存在"意外删数据"的
// 默认风险——真正的护栏是宽限期与身份校验（见 domain/backup_orphan_cleanup.go）。
func backupOrphanCleanupInterval() time.Duration {
	return envDurationOr(EnvBackupOrphanCleanupInterval, defaultBackupOrphanCleanupInterval, time.Second)
}

// backupOrphanGrace 解析 JIAN_BACKUP_ORPHAN_GRACE_HOURS（小时，FR-40 追加八）：
// 孤儿回收的宽限期。缺省或非法值取 1 小时；显式 0 同样回落 1 小时——
// **不允许零宽限**：那会让"刚拷进来、还没来得及在界面导入"的包在下一轮就被删掉。
func backupOrphanGrace() time.Duration {
	v := os.Getenv(EnvBackupOrphanGraceHours)
	if v == "" {
		return defaultBackupOrphanGrace
	}
	hours, err := strconv.Atoi(v)
	if err != nil || hours <= 0 {
		return defaultBackupOrphanGrace
	}
	return time.Duration(hours) * time.Hour
}

// envNonNegativeIntOr 解析环境变量为整数：缺省、非法与负数取 fallback，显式 0 原样返回。
// 0 的具体含义由调用方定义并注释（保留策略里是"不限制"），本函数只负责区分
// 「显式 0」与「未配置 / 配错」这两种在解析层就该分开的情况。
func envNonNegativeIntOr(key string, fallback int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return fallback
	}
	return n
}

// autoBlockBase 解析 JIAN_AUTO_BLOCK_BASE_SECONDS（秒，FR-43）：proxy 上游首次失败的
// 阻止窗口时长，之后每档翻倍。缺省、非数字、负数与显式 0 都回落默认 40s——
// 退避起始为 0 等于「失败不阻止」，不是有意义的配置，故本项**无禁用语义**
// （口径同 upstreamTimeout，而非 storageCleanupInterval 的 0 禁用）。
func autoBlockBase() time.Duration {
	return envPositiveDurationOr(EnvAutoBlockBaseSeconds, defaultAutoBlockBase, time.Second)
}

// searchTimeout 解析 JIAN_SEARCH_TIMEOUT_SECONDS（秒，FR-149 §3.4）：搜索链路
// （网页搜索 / npm 搜索 / 后续 suggest）的**总预算**，三条语句共享同一个 deadline。
// 缺省、非数字、负数与显式 0 都回落默认 10s——本项**无禁用语义**：搜索是无界查询，
// 关掉超时等于开放一个可以长时间占住数据库连接（主库池见 db.go 的 OpenWithMaxConns）
// 的入口。这与 JIAN_STORAGE_CLEANUP_INTERVAL 的「0 = 禁用」形态不同，口径同 upstreamTimeout /
// autoBlockBase（取 0 无意义）。
func searchTimeout() time.Duration {
	return envPositiveDurationOr(EnvSearchTimeoutSeconds, defaultSearchTimeout, time.Second)
}

// capacityCacheTTL 解析 JIAN_CAPACITY_CACHE_TTL_SECONDS（秒）：仪表盘容量聚合结果的缓存有效期。
// 缺省、非数字与负数回落默认 30s；**显式 0 = 禁用缓存**（每次请求都重新聚合）。
//
// 为什么本项**有**禁用语义（与 searchTimeout / autoBlockBase 的「0 也回落默认」相反）：
// 判据不是「取 0 是否常见」，而是「取 0 是否会放开一个无界的、可能伤害系统的操作」。
//   - 容量缓存不是授权数据：鉴权与 ACL 判定仍逐请求读库，组归属与仓库可见性变更即时生效，
//     与本缓存无关；缓存的只是「仓库数 / 资产数 / 逻辑字节数」这类展示口径。
//   - 禁用它的代价是**每次多跑一次有界聚合**（两条单表语句，最坏是整表扫描），而不是放开
//     一个可以无限占住唯一数据库连接的入口——这正是 JIAN_SEARCH_TIMEOUT_SECONDS 拒绝提供
//     禁用语义的理由（关闭超时 = 无界查询），本项不具备该性质。
//   - 反过来，「要实时数字」确是真实的运维需求：批量导入 / 迁移核对期间，运维需要仪表盘
//     立刻反映真实占用；重启进程虽也能清掉缓存，但那不是一个可用于日常运维的手段。
//
// 因此这里沿用 JIAN_STORAGE_CLEANUP_INTERVAL / JIAN_BLOB_GC_INTERVAL 的「0 = 禁用」形态：
// 把「关掉一个纯粹的性能优化」当作正当配置，而不是当成非法值。
//
// 不设上限夹取：本项只影响展示数字的新鲜度，超长 TTL 不破坏任何安全或正确性属性（进程重启
// 即自愈，运维看到数字滞后可直接调小），而**静默改写运维显式给出的值**是更糟的语义——口径
// 同 FR-43 拒绝给 auto-block 退避加上限的既有取舍：配置值应当可预期，不做隐藏的二次加工。
func capacityCacheTTL() time.Duration {
	return envDurationOr(EnvCapacityCacheTTLSeconds, defaultCapacityCacheTTL, time.Second)
}

// sqliteMaxConns 解析 JIAN_SQLITE_MAX_CONNS（连接条数，纯数量而非时长）：
// SQLite 主库连接池上限。缺省、非数字、负数与显式 0 都回落默认 8——本项**无禁用语义**，
// 池上限为 0 在 database/sql 里表示「无上限」，既不是想要的语义，也会让 SQLite 侧的
// 页面缓存副本与文件句柄随负载无界增长，因此与非正数一并按非法值处理。
func sqliteMaxConns() int {
	return envPositiveIntOr(EnvSQLiteMaxConns, defaultSQLiteMaxConns)
}

// envDurationOr 按给定单位解析环境变量为时长：缺省、非法与负数取 fallback，显式 0 原样返回（禁用语义）。
func envDurationOr(key string, fallback, unit time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return fallback
	}
	return time.Duration(n) * unit
}

// envPositiveDurationOr 按给定单位解析环境变量为时长：缺省、非法、负数与显式 0 都取 fallback。
// 与 envDurationOr 的区别是**没有禁用语义**——用于「取 0 无意义」的时长配置
// （如 auto-block 退避起始：0 窗口等于不阻止），对齐 upstreamTimeout 的正数口径。
func envPositiveDurationOr(key string, fallback, unit time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return fallback
	}
	return time.Duration(n) * unit
}

// envPositiveIntOr 解析环境变量为纯数量（非时长）：缺省、非法与 <=0 都取 fallback。
// 语义对齐 envPositiveDurationOr 的**无禁用语义**口径，只是不做单位换算
// （连接条数这类配置不存在「取 0 = 禁用」的合理解释）。
func envPositiveIntOr(key string, fallback int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}

// loadOrCreateSecret 解析 JWT 签名密钥：环境变量 > 本地持久化文件 > 新生成并落盘。
func loadOrCreateSecret(dataDir string) ([]byte, error) {
	if v := os.Getenv(EnvJWTSecret); v != "" {
		return []byte(v), nil
	}
	secretPath := filepath.Join(dataDir, secretFileName)
	if b, err := os.ReadFile(secretPath); err == nil && len(b) > 0 {
		return b, nil
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, fmt.Errorf("生成 JWT 密钥：%w", err)
	}
	if err := os.WriteFile(secretPath, secret, 0o600); err != nil {
		return nil, fmt.Errorf("持久化 JWT 密钥：%w", err)
	}
	fmt.Fprintf(os.Stderr, "警告：未设置 %s，已在 %s 生成并持久化随机 JWT 密钥；生产环境建议显式配置。\n", EnvJWTSecret, secretPath)
	return secret, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// restartScope 解析 JIAN_RESTART_SCOPE（FR-40 追加七）。
//
// 非法值**回落默认而不是报错**：作用域是"怎么重启"的实现细节，写错了应当退化成
// 最保守的 system 再交给同步预检（Probe）判定可用性——由预检返回 supported:false +
// reason，运维在界面上看到的是"这台机器没法自动重启，请手动执行 xxx"，
// 比服务因一个拼错的枚举值拒绝启动要好得多。
//
// 三个取值与 domain.RestartScope*（restart_service.go）**必须同值**：这里是字面量而非
// 引用，因为 config 是横切层、不得反向依赖业务层（见 internal/doc.go 的分层约定）。
// 与 defaultAutoBlockBase 镜像 domain 常量的处理同一口径。
func restartScope() string {
	switch v := strings.TrimSpace(os.Getenv(EnvRestartScope)); v {
	case "system", "systemd-user", "docker":
		return v
	default:
		return defaultRestartScope
	}
}

// upstreamTimeout 解析 JIAN_UPSTREAM_TIMEOUT（秒）；缺省或非法（<=0）时取默认值。
func upstreamTimeout() time.Duration {
	v := os.Getenv(EnvUpstreamTimeout)
	if v == "" {
		return defaultUpstreamTimeout
	}
	secs, err := strconv.Atoi(v)
	if err != nil || secs <= 0 {
		return defaultUpstreamTimeout
	}
	return time.Duration(secs) * time.Second
}

// oidcConfig 解析 OIDC 接入配置（FR-34）。未配置 issuer 时整组不启用；
// 半配置（缺客户端 ID / 密钥 / 回调地址）或非法 URL 直接报错，
// 避免"以为启用了实际没有"的静默半启用（对齐 FR-131 的防静默降级口径）。
func oidcConfig() (OIDCConfig, error) {
	cfg := OIDCConfig{
		Issuer:        strings.TrimSpace(os.Getenv(EnvOIDCIssuer)),
		ClientID:      strings.TrimSpace(os.Getenv(EnvOIDCClientID)),
		ClientSecret:  os.Getenv(EnvOIDCClientSecret),
		RedirectURL:   strings.TrimSpace(os.Getenv(EnvOIDCRedirectURL)),
		UsernameClaim: envOr(EnvOIDCUsernameClaim, defaultOIDCUsernameClaim),
	}
	if cfg.Issuer == "" {
		return OIDCConfig{}, nil
	}
	var missing []string
	if cfg.ClientID == "" {
		missing = append(missing, EnvOIDCClientID)
	}
	if cfg.ClientSecret == "" {
		missing = append(missing, EnvOIDCClientSecret)
	}
	if cfg.RedirectURL == "" {
		missing = append(missing, EnvOIDCRedirectURL)
	}
	if len(missing) > 0 {
		return OIDCConfig{}, fmt.Errorf("启用 %s 时必须同时配置：%s", EnvOIDCIssuer, strings.Join(missing, "、"))
	}
	if err := requireHTTPURL(EnvOIDCIssuer, cfg.Issuer); err != nil {
		return OIDCConfig{}, err
	}
	if err := requireHTTPURL(EnvOIDCRedirectURL, cfg.RedirectURL); err != nil {
		return OIDCConfig{}, err
	}
	for _, part := range strings.Split(os.Getenv(EnvOIDCAllowedDomains), ",") {
		if domain := strings.ToLower(strings.TrimSpace(part)); domain != "" {
			cfg.AllowedDomains = append(cfg.AllowedDomains, domain)
		}
	}
	return cfg, nil
}

// requireHTTPURL 校验配置值为 http(s) 绝对 URL；配置错误在启动期早失败。
func requireHTTPURL(env, value string) error {
	u, err := url.Parse(value)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("解析 %s：须为 http(s) 绝对 URL", env)
	}
	return nil
}

// ldapConfig 解析 LDAP 接入配置（FR-35）。未配置 URL 时整组不启用；
// 半配置、过滤器缺占位、StartTLS 与 ldaps 冲突、CA 文件不可读都在启动期直接报错，
// 避免"以为启用了实际没有"（对齐 OIDC 与 FR-131 的防静默降级口径）。
func ldapConfig() (LDAPConfig, error) {
	startTLS := os.Getenv(EnvLDAPStartTLS)
	cfg := LDAPConfig{
		URL:          strings.TrimSpace(os.Getenv(EnvLDAPURL)),
		BindDN:       strings.TrimSpace(os.Getenv(EnvLDAPBindDN)),
		BindPassword: os.Getenv(EnvLDAPBindPassword),
		BaseDN:       strings.TrimSpace(os.Getenv(EnvLDAPBaseDN)),
		UserFilter:   envOr(EnvLDAPUserFilter, defaultLDAPUserFilter),
		EmailAttr:    envOr(EnvLDAPEmailAttr, defaultLDAPEmailAttr),
		StartTLS:     startTLS == "1" || strings.EqualFold(startTLS, "true"),
		CAFile:       strings.TrimSpace(os.Getenv(EnvLDAPCAFile)),
	}
	if cfg.URL == "" {
		return LDAPConfig{}, nil
	}
	u, err := url.Parse(cfg.URL)
	if err != nil || (u.Scheme != "ldap" && u.Scheme != "ldaps") || u.Host == "" {
		return LDAPConfig{}, fmt.Errorf("解析 %s：须为 ldap:// 或 ldaps:// 地址", EnvLDAPURL)
	}
	if cfg.BaseDN == "" {
		return LDAPConfig{}, fmt.Errorf("启用 %s 时必须同时配置：%s", EnvLDAPURL, EnvLDAPBaseDN)
	}
	if !strings.Contains(cfg.UserFilter, "{username}") {
		return LDAPConfig{}, fmt.Errorf("解析 %s：过滤器必须包含 {username} 占位", EnvLDAPUserFilter)
	}
	if cfg.BindDN != "" && cfg.BindPassword == "" {
		return LDAPConfig{}, fmt.Errorf("配置了 %s 时必须同时配置：%s", EnvLDAPBindDN, EnvLDAPBindPassword)
	}
	if cfg.StartTLS && u.Scheme == "ldaps" {
		return LDAPConfig{}, fmt.Errorf("%s 与 ldaps:// 冲突：ldaps 已全程加密", EnvLDAPStartTLS)
	}
	if cfg.CAFile != "" {
		if _, err := os.Stat(cfg.CAFile); err != nil {
			return LDAPConfig{}, fmt.Errorf("读取 %s 指向的 CA 文件：%w", EnvLDAPCAFile, err)
		}
	}
	return cfg, nil
}
