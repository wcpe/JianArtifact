package domain

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// assetDownloadRetention 明细保留期，与观测域 operationsRetention 一致（30 天）。
const assetDownloadRetention = 30 * 24 * time.Hour

// assetDownloadPrefix 制品协议路由前缀：仅该前缀下的 GET 才可能是制品下载。
const assetDownloadPrefix = "/repository/"

type assetDownloadKey struct {
	BucketStart string
	Repo        string
	AssetPath   string
	ClientIP    string
	UAFamily    string
}

// AssetDownloadService 累积「完整传输」的制品下载（GET + 200），按分钟桶在内存合并、
// 周期批量落库——下载路径不逐请求写 SQLite（与观测域同一设计原则）；Range 分段（206）
// 不计，避免断点续传与重试虚高。设计见 docs/specs/download-metrics.md（FR-142）。
type AssetDownloadService struct {
	repo    *repository.AssetDownloadRepo
	mu      sync.Mutex
	pending map[assetDownloadKey]int64
}

func NewAssetDownloadService(repo *repository.AssetDownloadRepo) *AssetDownloadService {
	return &AssetDownloadService{repo: repo, pending: make(map[assetDownloadKey]int64)}
}

// ParseRepositoryAssetPath 从 /repository/{repo}/{path...} 解析仓库名与制品路径；
// 前缀不符、缺仓库或路径时 ok=false。制品路径按 URL 解码（协议层可能传编码路径）。
func ParseRepositoryAssetPath(requestPath string) (repo, assetPath string, ok bool) {
	if !strings.HasPrefix(requestPath, assetDownloadPrefix) {
		return "", "", false
	}
	rest := strings.TrimPrefix(requestPath, assetDownloadPrefix)
	repo, assetPath, found := strings.Cut(rest, "/")
	if !found || repo == "" || assetPath == "" {
		return "", "", false
	}
	if decoded, err := url.PathUnescape(assetPath); err == nil {
		assetPath = decoded
	}
	return repo, assetPath, true
}

// RecordDownload 记录一次下载尝试；只有「完整传输」（GET + 200，且路径命中制品协议前缀）
// 才计入。返回是否计入——调用方与测试据此判断，不做静默分支。
func (s *AssetDownloadService) RecordDownload(method string, status int, requestPath, clientIP, userAgent string, at time.Time) bool {
	if s == nil || s.repo == nil {
		return false
	}
	if method != http.MethodGet || status != http.StatusOK {
		return false
	}
	repo, assetPath, ok := ParseRepositoryAssetPath(requestPath)
	if !ok {
		return false
	}
	key := assetDownloadKey{
		BucketStart: at.UTC().Truncate(time.Minute).Format(time.RFC3339Nano),
		Repo:        repo,
		AssetPath:   assetPath,
		ClientIP:    clientIP,
		UAFamily:    classifyUAFamily(userAgent),
	}
	s.mu.Lock()
	s.pending[key]++
	s.mu.Unlock()
	return true
}

// Flush 把不晚于当前分钟的累计批量落库；失败时把本批计数放回缓冲等待下一次重试
// （与 OperationsDashboardService.Flush 同语义）。
func (s *AssetDownloadService) Flush(now time.Time) error {
	if s == nil || s.repo == nil {
		return nil
	}
	cutoff := now.UTC().Truncate(time.Minute).Format(time.RFC3339Nano)
	s.mu.Lock()
	items := make([]repository.AssetDownloadMinute, 0, len(s.pending))
	for key, count := range s.pending {
		if key.BucketStart <= cutoff {
			items = append(items, repository.AssetDownloadMinute{
				BucketStart:   key.BucketStart,
				Repo:          key.Repo,
				AssetPath:     key.AssetPath,
				ClientIP:      key.ClientIP,
				UAFamily:      key.UAFamily,
				DownloadCount: count,
			})
			delete(s.pending, key)
		}
	}
	s.mu.Unlock()
	if err := s.repo.AddMinutes(items); err != nil {
		s.mu.Lock()
		for _, item := range items {
			s.pending[assetDownloadKey{
				BucketStart: item.BucketStart,
				Repo:        item.Repo,
				AssetPath:   item.AssetPath,
				ClientIP:    item.ClientIP,
				UAFamily:    item.UAFamily,
			}] += item.DownloadCount
		}
		s.mu.Unlock()
		return err
	}
	return nil
}

// Start 启动周期任务：每分钟 flush；每 24 小时清理超过保留期的明细。
func (s *AssetDownloadService) Start(ctx context.Context, now func() time.Time) {
	if s == nil || s.repo == nil {
		return
	}
	go func() {
		minute := time.NewTicker(time.Minute)
		day := time.NewTicker(24 * time.Hour)
		defer minute.Stop()
		defer day.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-minute.C:
				_ = s.Flush(now())
			case <-day.C:
				_, _ = s.repo.PurgeBefore(now().UTC().Add(-assetDownloadRetention))
			}
		}
	}()
}

// classifyUAFamily 把 User-Agent 归类为有限族（见 spec 附录；按序首中即停）。
// 归类在采集时完成——原始 UA 串不落库（隐私边界，比审计域更保守）。
func classifyUAFamily(userAgent string) string {
	ua := strings.ToLower(userAgent)
	switch {
	case strings.Contains(ua, "apache-maven"), strings.Contains(ua, "maven"):
		return "maven"
	case strings.Contains(ua, "gradle"):
		return "gradle"
	case strings.Contains(ua, "npm/"), strings.Contains(ua, "node-fetch"), strings.Contains(ua, "node"):
		return "npm"
	case strings.Contains(ua, "curl"):
		return "curl"
	case strings.Contains(ua, "mozilla"):
		return "browser"
	default:
		return "other"
	}
}
