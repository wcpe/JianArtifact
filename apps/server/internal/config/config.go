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

	defaultDataDir         = "./data"
	defaultHTTPAddr        = ":8080"
	defaultUpstreamTimeout = 30 * time.Second
	defaultSyncInterval    = 5 * time.Second // 复制轮询间隔默认 5s（近实时）
	defaultBlobGCInterval  = 24 * time.Hour  // 孤立 blob 默认每日清理一次
	// defaultOIDCUsernameClaim 是 OIDC 用户名的缺省 claim；各 IdP 常见取值见 OPERATIONS。
	defaultOIDCUsernameClaim = "preferred_username"
	// defaultLDAPUserFilter 与 defaultLDAPEmailAttr 是 LDAP 检索的缺省口径（OpenLDAP 风格 uid）。
	defaultLDAPUserFilter = "(uid={username})"
	defaultLDAPEmailAttr  = "mail"

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
	SyncPeerURL            string        // 遗留复制对端基址；仅为兼容旧配置保留，不驱动主备调度
	SyncInterval           time.Duration // 复制轮询间隔（FR-85）
	PublicURL              string        // 对外基础 URL（FR-87，如 https://repo.example.com）；空则回退请求 Host 推断
	EnabledFormats         formats.Set   // 启动时启用的协议格式（FR-32）
	BlobGCInterval         time.Duration // primary 孤立 blob 定时清理间隔；0 禁用
	TLSAddr                string        // 服务内置 TLS 监听地址（FR-131）；空 = 不启用
	TLSCert                string        // TLS PEM 证书文件路径（FR-131）
	TLSKey                 string        // TLS PEM 私钥文件路径（FR-131）
	OIDC                   OIDCConfig    // FR-34：OIDC 身份源接入配置（Issuer 空 = 不启用）
	LDAP                   LDAPConfig    // FR-35：LDAP 身份源接入配置（URL 空 = 不启用）
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
func Load() (*Config, error) {
	dataDir := envOr(EnvDataDir, defaultDataDir)
	absData, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, fmt.Errorf("解析 data 目录：%w", err)
	}
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
		DataDir:                absData,
		HTTPAddr:               envOr(EnvHTTPAddr, defaultHTTPAddr),
		DBPath:                 filepath.Join(absData, dbFileName),
		BlobDir:                blobDir,
		JWTSecret:              secret,
		MigrationCredentialKey: migrationCredentialKey,
		UpstreamTimeout:        upstreamTimeout(),
		SyncPeerURL:            os.Getenv(EnvSyncPeerURL),
		SyncInterval:           syncInterval(),
		PublicURL:              os.Getenv(EnvPublicURL),
		EnabledFormats:         enabledFormats,
		BlobGCInterval:         blobGCInterval(),
		TLSAddr:                envOr(EnvTLSAddr, ""),
		TLSCert:                os.Getenv(EnvTLSCert),
		TLSKey:                 os.Getenv(EnvTLSKey),
		OIDC:                   oidc,
		LDAP:                   ldap,
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
