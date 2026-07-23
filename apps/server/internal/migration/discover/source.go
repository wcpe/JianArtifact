// Package discover 实现 Nexus 三来源发现，产出统一 MigrationPlan。
// 分层：被 domain.MigrationService 调用；不写 blob、不改任务状态以外的持久化（落库在 domain）。
package discover

import (
	"context"
	"fmt"
)

// 支持纳入 plan 的 format。
const (
	FormatRaw   = "raw"
	FormatMaven = "maven"
	FormatNPM   = "npm"
)

// PlanRepository 是计划中的一条仓库建议。
type PlanRepository struct {
	Name            string `json:"name"`
	Format          string `json:"format"`
	Type            string `json:"type,omitempty"`
	EstimatedAssets int64  `json:"estimatedAssets,omitempty"`
}

// Plan 是统一发现输出。
type Plan struct {
	Repositories []PlanRepository `json:"repositories"`
	Warnings     []string         `json:"warnings"`
	Stats        map[string]any   `json:"stats"`
	Estimated    bool             `json:"estimated,omitempty"`
}

// Config 是发现入参（无密钥明文；凭据值由 domain 解析后可选传入 Credential）。
type Config struct {
	// URL 用于 online_rest 基址（如 http://nexus:8081）。
	URL string
	// Path 用于 offline_dir / offline_bundle 本地路径。
	Path string
	// Credential 已解析的凭据明文（Basic user:pass 或 token）；可空。
	// 不得写回 plan 或日志。
	Credential string
	// MaxAssetPages 在线资产分页上限（每仓）；0 表示默认 5。
	MaxAssetPages int
	// IncludeRepositories 非空时仅把名单内仓库纳入 plan（真机小范围验收）。
	IncludeRepositories []string
}

// Source 三来源统一接口。
type Source interface {
	Discover(ctx context.Context, cfg Config) (Plan, error)
}

// ErrInvalidConfig 配置/路径错误（映射 400）。
type ErrInvalidConfig struct {
	Msg string
}

func (e *ErrInvalidConfig) Error() string { return e.Msg }

// ErrUpstream 上游不可达或错误（映射 502）。
type ErrUpstream struct {
	Msg string
}

func (e *ErrUpstream) Error() string { return e.Msg }

// ErrAuth 源认证失败（映射 validation_error，避免与会话 401 混淆）。
type ErrAuth struct {
	Msg string
}

func (e *ErrAuth) Error() string { return e.Msg }

func emptyPlan() Plan {
	return Plan{
		Repositories: []PlanRepository{},
		Warnings:     []string{},
		Stats:        map[string]any{},
	}
}

func finalizePlan(p Plan) Plan {
	if p.Repositories == nil {
		p.Repositories = []PlanRepository{}
	}
	if p.Warnings == nil {
		p.Warnings = []string{}
	}
	if p.Stats == nil {
		p.Stats = map[string]any{}
	}
	var totalAssets int64
	for _, r := range p.Repositories {
		totalAssets += r.EstimatedAssets
	}
	p.Stats["repositoryCount"] = len(p.Repositories)
	p.Stats["estimatedAssets"] = totalAssets
	return p
}

func supportedFormat(format string) bool {
	switch format {
	case FormatRaw, FormatMaven, FormatNPM:
		return true
	default:
		return false
	}
}

func mapNexusFormat(format string) (string, bool) {
	switch format {
	case "raw", "maven2", "maven", "npm":
		if format == "maven2" {
			return FormatMaven, true
		}
		if format == "maven" {
			return FormatMaven, true
		}
		return format, true
	default:
		return format, false
	}
}

func requirePath(path string) error {
	if path == "" {
		return &ErrInvalidConfig{Msg: "sourceConfig.path 不能为空"}
	}
	return nil
}

func requireURL(u string) error {
	if u == "" {
		return &ErrInvalidConfig{Msg: "sourceConfig.url 不能为空"}
	}
	return nil
}

func includeSet(names []string) map[string]bool {
	if len(names) == 0 {
		return nil
	}
	m := make(map[string]bool, len(names))
	for _, n := range names {
		if n != "" {
			m[n] = true
		}
	}
	if len(m) == 0 {
		return nil
	}
	return m
}

// NewSource 按 sourceType 构造 Source。
func NewSource(sourceType string) (Source, error) {
	switch sourceType {
	case "online_rest":
		return NewOnlineREST(nil), nil
	case "offline_dir":
		return OfflineDir{}, nil
	case "offline_bundle":
		return OfflineBundle{}, nil
	default:
		return nil, fmt.Errorf("不支持的 sourceType %q", sourceType)
	}
}
