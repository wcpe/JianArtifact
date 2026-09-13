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
	EnvUpstreamTimeout        = "JIAN_UPSTREAM_TIMEOUT" // proxy 回源整体超时，单位秒
	EnvSyncPeerURL            = "JIAN_SYNC_PEER_URL"    // 遗留复制对端基址；主备角色模式下不参与调度
	EnvSyncInterval           = "JIAN_SYNC_INTERVAL"    // 同步轮询间隔（FR-85），单位秒
	EnvPublicURL              = "JIAN_PUBLIC_URL"       // 对外基础 URL（FR-87）；CDN 域名，隐藏源站 IP
	EnvEnabledFormats         = "JIAN_ENABLED_FORMATS"  // 启用的协议格式，逗号分隔（FR-32）
	EnvBlobGCInterval         = "JIAN_BLOB_GC_INTERVAL" // 孤立 blob 定时清理间隔，单位秒；0 禁用
	EnvTLSAddr                = "JIAN_TLS_ADDR"         // 服务内置 TLS 监听地址（FR-131）；空 = 不启用
	EnvTLSCert                = "JIAN_TLS_CERT"         // TLS PEM 证书文件路径（FR-131）
	EnvTLSKey                 = "JIAN_TLS_KEY"          // TLS PEM 私钥文件路径（FR-131）

	defaultDataDir         = "./data"
	defaultHTTPAddr        = ":8080"
	defaultUpstreamTimeout = 30 * time.Second
	defaultSyncInterval    = 5 * time.Second // 复制轮询间隔默认 5s（近实时）
	defaultBlobGCInterval  = 24 * time.Hour  // 孤立 blob 默认每日清理一次

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
}

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
