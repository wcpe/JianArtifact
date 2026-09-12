// Package discover 实现 Nexus 三来源发现，产出统一 MigrationPlan。
// 分层：被 domain.MigrationService 调用；不写 blob、不改任务状态以外的持久化（落库在 domain）。
package discover

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/wcpe/jianartifact/apps/server/internal/migration/credential"
)

// 支持纳入 plan 的 format。
const (
	FormatRaw    = "raw"
	FormatMaven  = "maven"
	FormatNPM    = "npm"
	FormatDocker = "docker"
	FormatCargo  = "cargo"
	FormatPyPI   = "pypi"
	FormatGoMod  = "gomod"
	FormatNuGet  = "nuget"
)

// PlanRepository 是计划中的一条仓库建议。
type PlanRepository struct {
	Name            string `json:"name"`
	Format          string `json:"format"`
	Type            string `json:"type,omitempty"`
	EstimatedAssets int64  `json:"estimatedAssets,omitempty"`
	// Config 是不含来源地址和凭据的目标配置摘要。
	Config map[string]any `json:"config,omitempty"`
	// MigrationMode 表示资产导入、代理配置、分组配置或不可执行。
	MigrationMode string `json:"migrationMode,omitempty"`
	// Warnings 是仅影响当前仓库的结构化前置告警。
	Warnings []string `json:"warnings,omitempty"`
}

// TargetRepositoryConfig 是迁移来源已验证的目标仓库配置摘要。
// 仅允许不含凭据的上游基址与 group 成员顺序进入迁移计划。
type TargetRepositoryConfig struct {
	RemoteURL string
	Members   []string
}

// Plan 是统一发现输出。
type Plan struct {
	Repositories []PlanRepository `json:"repositories"`
	Warnings     []string         `json:"warnings"`
	Stats        map[string]any   `json:"stats"`
	Estimated    bool             `json:"estimated,omitempty"`
	// SourceRef 是在线来源的逻辑引用，不包含地址或主机名。
	SourceRef string `json:"sourceRef,omitempty"`
}

// Config 是发现入参（无密钥明文；凭据值由 domain 解析后可选传入 Credential）。
type Config struct {
	// SourceRef 是 online_rest 的部署来源逻辑引用。
	SourceRef string
	// URL 是由受限 sourceRef 在运行时解析出的 online_rest 基址，不来自 sourceConfig。
	URL string
	// Path 用于 offline_dir / offline_bundle 本地路径。
	Path string
	// Credential 是旧 credentialRef 解析结果，保留冒号推断兼容。
	// 不得写回 plan 或日志。
	Credential string
	// SourceAuth 是直接提交或已转换的显式来源认证。
	SourceAuth credential.SourceAuth
	// MaxAssetPages 在线资产分页上限（每仓）；0 表示默认 5。
	MaxAssetPages int
	// IncludeRepositories 非空时仅把名单内仓库纳入 plan（真机小范围验收）。
	IncludeRepositories []string
	// RepositoryFormats/Types 是离线原生目录的可信导出映射。
	RepositoryFormats map[string]string
	RepositoryTypes   map[string]string
	// RepositoryConfigs 是离线来源提供的可信非敏感配置映射。
	RepositoryConfigs map[string]TargetRepositoryConfig
	// AllowPrivateSource 为 true 时，本发现使用的出站客户端放行回环/私网地址。
	// 仅当用户在迁移来源中显式声明（allowPrivateSource）时置位；默认 false 保持 SSRF 防护。
	AllowPrivateSource bool
}

func (c Config) auth() credential.SourceAuth {
	if c.SourceAuth.Type != "" {
		return c.SourceAuth
	}
	return credential.FromLegacy(c.Credential)
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
	Msg   string
	Cause error
}

func (e *ErrUpstream) Error() string { return e.Msg }

func (e *ErrUpstream) Unwrap() error { return e.Cause }

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
	case FormatRaw, FormatMaven, FormatNPM, FormatDocker, FormatCargo, FormatPyPI, FormatGoMod, FormatNuGet:
		return true
	default:
		return false
	}
}

func mapNexusFormat(format string) (string, bool) {
	format = strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(format, "-hosted"), "-proxy"), "-group")
	switch format {
	case "raw", "maven2", "maven", "npm", "docker", "cargo", "pypi", "pypi-proxy", "go", "gomod", "nuget":
		if format == "maven2" {
			return FormatMaven, true
		}
		if format == "maven" {
			return FormatMaven, true
		}
		if format == "pypi-proxy" {
			return FormatPyPI, true
		}
		if format == "go" {
			return FormatGoMod, true
		}
		return format, true
	default:
		return format, false
	}
}

// MapNexusFormatForIndex 为离线索引计划提供受控格式映射，不暴露来源地址或凭据。
func MapNexusFormatForIndex(format string) (string, bool) {
	return mapNexusFormat(strings.ToLower(strings.TrimSpace(format)))
}

func normalizeRepoType(typ string) string {
	switch strings.ToLower(strings.TrimSpace(typ)) {
	case "hosted", "proxy", "group":
		return strings.ToLower(strings.TrimSpace(typ))
	default:
		return "hosted"
	}
}

func migrationMode(format, typ string) string {
	if format == FormatGoMod && typ != "proxy" {
		return "unsupported"
	}
	switch typ {
	case "proxy":
		return "proxy_config"
	case "group":
		return "group_config"
	default:
		return "assets"
	}
}

func migrationConfigSummary(typ string, source TargetRepositoryConfig) (map[string]any, string, string) {
	config := map[string]any{}
	mode := "assets"
	if typ == "proxy" {
		mode = "proxy_config"
		if _, err := validateSourceURL(source.RemoteURL); err != nil {
			return config, "unsupported", "proxy 缺少或包含无效上游配置"
		}
		config["remoteUrl"] = strings.TrimRight(strings.TrimSpace(source.RemoteURL), "/")
		return config, mode, ""
	}
	if typ == "group" {
		mode = "group_config"
		if len(source.Members) == 0 {
			return config, "unsupported", "group 缺少成员"
		}
		config["members"] = append([]string(nil), source.Members...)
	}
	return config, mode, ""
}

func validateSourceURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", &ErrInvalidConfig{Msg: "online_rest 来源地址无效"}
	}
	return strings.TrimRight(u.String(), "/"), nil
}

func requirePath(path string) error {
	if path == "" {
		return &ErrInvalidConfig{Msg: "sourceConfig.path 不能为空"}
	}
	return nil
}

func requireURL(u string) error {
	if u == "" {
		return &ErrInvalidConfig{Msg: "online_rest 来源基址为空"}
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
