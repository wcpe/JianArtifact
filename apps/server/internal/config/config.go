// Package config 加载与校验 JianArtifact 后端的运行配置。
//
// 横切层（见 docs/ARCHITECTURE.md 分层）：供各层读取，不反向依赖业务层。
// 全部配置项经环境变量注入；凭据类（JWT 密钥）引用外部值或本地持久化，不写入元数据库、不进日志。
package config

import (
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
)

// 默认值与环境变量名。
const (
	EnvDataDir   = "JIAN_DATA_DIR"
	EnvHTTPAddr  = "JIAN_HTTP_ADDR"
	EnvJWTSecret = "JIAN_JWT_SECRET"

	defaultDataDir  = "./data"
	defaultHTTPAddr = ":8080"

	dbFileName     = "jianartifact.db"
	blobDirName    = "blobs"
	secretFileName = "jwt.secret"
)

// Config 是解析后的运行配置。路径均为绝对化后的结果，便于日志与探错一致。
type Config struct {
	DataDir   string // 数据根目录（SQLite 与 blob 存放）
	HTTPAddr  string // HTTP 监听地址
	DBPath    string // SQLite 文件路径（DataDir/jianartifact.db）
	BlobDir   string // blob 存储目录（DataDir/blobs）
	JWTSecret []byte // JWT HS256 签名密钥（不入库、不打印）
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
	for _, dir := range []string{absData, blobDir} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("创建目录 %s：%w", dir, err)
		}
	}

	secret, err := loadOrCreateSecret(absData)
	if err != nil {
		return nil, err
	}

	return &Config{
		DataDir:   absData,
		HTTPAddr:  envOr(EnvHTTPAddr, defaultHTTPAddr),
		DBPath:    filepath.Join(absData, dbFileName),
		BlobDir:   blobDir,
		JWTSecret: secret,
	}, nil
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
