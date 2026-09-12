package discover

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/wcpe/jianartifact/apps/server/internal/migration/credential"
	"github.com/wcpe/jianartifact/apps/server/internal/upstream"
)

// OnlineREST 通过 Nexus REST API 发现仓库与资产规模估算。
type OnlineREST struct {
	HTTP *upstream.Client
}

// NewOnlineREST 构造在线 Nexus 发现器；所有来源访问均经统一安全出站客户端。
// client 为 nil 时延迟构建：Discover/ListRemoteRepositoriesWithAuth 按本次请求的 allowPrivateSource
// 决定私网放行策略，确保默认仍执行 SSRF 防护。
func NewOnlineREST(client *upstream.Client) *OnlineREST {
	return &OnlineREST{HTTP: client}
}

type nexusRepo struct {
	Name   string `json:"name"`
	Format string `json:"format"`
	Type   string `json:"type"`
}

type nexusAssetsPage struct {
	Items             []json.RawMessage `json:"items"`
	ContinuationToken string            `json:"continuationToken"`
}

// Discover 实现 Source：列仓库 + 每仓有限页资产计数（estimated）。
func (s *OnlineREST) Discover(ctx context.Context, cfg Config) (Plan, error) {
	if err := requireURL(cfg.URL); err != nil {
		return Plan{}, err
	}
	if s.HTTP == nil {
		// 仅在未注入显式客户端时按来源声明决定私网放行策略，确保 SSRF 防护默认生效。
		s.HTTP = upstream.NewClientWithPolicy(upstream.DefaultTimeout, cfg.AllowPrivateSource)
	}
	base := strings.TrimRight(cfg.URL, "/")
	auth := cfg.auth()
	maxPages := cfg.MaxAssetPages
	if maxPages <= 0 {
		maxPages = 5
	}

	repos, err := s.listRepositories(ctx, base, auth)
	if err != nil {
		return Plan{}, err
	}

	allow := map[string]bool{}
	for _, n := range cfg.IncludeRepositories {
		if n != "" {
			allow[n] = true
		}
	}
	filterOn := len(allow) > 0

	plan := emptyPlan()
	plan.Estimated = true
	plan.SourceRef = cfg.SourceRef
	// 未指定 include 时只列仓、不逐仓拉资产（全量计数极易卡住 UI）；
	// 指定 include 后才对命中仓做有限页估算。
	countAssets := filterOn
	if !countAssets {
		plan.Warnings = append(plan.Warnings, "未指定 includeRepositories：仅列出可迁移仓库，资产数为 0（可在预览勾选后开始迁移；需要估算请在发现前填写仓库名）")
	}
	for _, r := range repos {
		if err := ctx.Err(); err != nil {
			return Plan{}, err
		}
		if filterOn && !allow[r.Name] {
			continue
		}
		mapped, ok := mapNexusFormat(strings.ToLower(r.Format))
		if !ok || !supportedFormat(mapped) {
			plan.Warnings = append(plan.Warnings, "跳过不支持的 format: "+r.Name+" ("+r.Format+")")
			continue
		}
		typ := normalizeRepoType(r.Type)
		mode := migrationMode(mapped, typ)
		if mode == "unsupported" {
			plan.Warnings = append(plan.Warnings, "跳过不支持的仓库类型："+r.Name+"（Go modules 仅允许 proxy）")
			continue
		}
		config, configMode, configWarning := s.repositoryConfig(ctx, base, r, typ, auth)
		if configWarning != "" {
			plan.Warnings = append(plan.Warnings, r.Name+": "+configWarning)
		}
		if configMode == "unsupported" {
			mode = configMode
		}
		var count int64
		if countAssets && mode == "assets" {
			n, truncated, err := s.countAssets(ctx, base, r.Name, auth, maxPages)
			if err != nil {
				plan.Warnings = append(plan.Warnings, r.Name+": 资产枚举失败")
				count = 0
			} else {
				count = n
				if truncated {
					plan.Warnings = append(plan.Warnings, r.Name+": 资产数达分页上限，为估算值")
				}
			}
		}
		plan.Repositories = append(plan.Repositories, PlanRepository{
			Name:            r.Name,
			Format:          mapped,
			Type:            typ,
			EstimatedAssets: count,
			Config:          config,
			MigrationMode:   mode,
		})
	}
	if filterOn && len(plan.Repositories) == 0 {
		plan.Warnings = append(plan.Warnings, "includeRepositories 未匹配到任何可迁移仓库")
	}
	return finalizePlan(plan), nil
}

func (s *OnlineREST) repositoryConfig(ctx context.Context, base string, repo nexusRepo, typ string, auth credential.SourceAuth) (map[string]any, string, string) {
	if typ == "hosted" {
		return map[string]any{}, "assets", ""
	}
	endpoint := base + "/service/rest/v1/repositories/" + url.PathEscape(repo.Format) + "/" + url.PathEscape(typ) + "/" + url.PathEscape(repo.Name)
	body, status, err := s.get(ctx, endpoint, auth)
	if err != nil || status < http.StatusOK || status >= http.StatusMultipleChoices {
		return map[string]any{}, "unsupported", "无法读取安全的仓库配置"
	}
	var payload struct {
		Proxy struct {
			RemoteURL string `json:"remoteUrl"`
		} `json:"proxy"`
		Group struct {
			MemberNames []string `json:"memberNames"`
		} `json:"group"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return map[string]any{}, "unsupported", "仓库配置解析失败"
	}
	source := TargetRepositoryConfig{RemoteURL: payload.Proxy.RemoteURL, Members: payload.Group.MemberNames}
	config, mode, warning := migrationConfigSummary(typ, source)
	return config, mode, warning
}

// RemoteRepository 是 Nexus REST 列出的远程仓库摘要（仅索引，无资产枚举）。
type RemoteRepository struct {
	Name   string `json:"name"`
	Format string `json:"format"`
	Type   string `json:"type"`
}

// ListRemoteRepositories 仅调用 /service/rest/v1/repositories，不落库、不扫资产。
// 供离线 blob 迁移前勾选 includeRepositories，避免全盘 Walk。
// supportedOnly=true 时仅返回本产品可迁移 format（maven/npm/raw）。
func (s *OnlineREST) ListRemoteRepositories(ctx context.Context, baseURL, rawCredential string, supportedOnly bool) ([]RemoteRepository, error) {
	return s.ListRemoteRepositoriesWithAuth(ctx, baseURL, credential.FromLegacy(rawCredential), supportedOnly, false)
}

// ListRemoteRepositoriesWithAuth 以显式认证仅拉取 Nexus 仓库索引，不落库。
// allowPrivateSource=true 时放行回环/私网来源地址（仅当用户在迁移来源中显式声明）。
func (s *OnlineREST) ListRemoteRepositoriesWithAuth(ctx context.Context, baseURL string, auth credential.SourceAuth, supportedOnly bool, allowPrivateSource bool) ([]RemoteRepository, error) {
	if err := requireURL(baseURL); err != nil {
		return nil, err
	}
	if s.HTTP == nil {
		s.HTTP = upstream.NewClientWithPolicy(upstream.DefaultTimeout, allowPrivateSource)
	}
	base := strings.TrimRight(baseURL, "/")
	repos, err := s.listRepositories(ctx, base, auth)
	if err != nil {
		return nil, err
	}
	out := make([]RemoteRepository, 0, len(repos))
	for _, r := range repos {
		mapped, ok := mapNexusFormat(strings.ToLower(r.Format))
		if supportedOnly && (!ok || !supportedFormat(mapped)) {
			continue
		}
		format := r.Format
		if ok {
			format = mapped
		}
		typ := strings.ToLower(r.Type)
		if typ == "" {
			typ = "hosted"
		}
		out = append(out, RemoteRepository{
			Name:   r.Name,
			Format: format,
			Type:   typ,
		})
	}
	return out, nil
}

func (s *OnlineREST) listRepositories(ctx context.Context, base string, auth credential.SourceAuth) ([]nexusRepo, error) {
	u := base + "/service/rest/v1/repositories"
	body, status, err := s.get(ctx, u, auth)
	if err != nil {
		return nil, outboundError("无法连接 Nexus REST", err)
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return nil, &ErrAuth{Msg: "Nexus 认证失败"}
	}
	if status < 200 || status >= 300 {
		return nil, &ErrUpstream{Msg: fmt.Sprintf("Nexus 仓库列表返回 %d", status)}
	}
	var repos []nexusRepo
	if err := json.Unmarshal(body, &repos); err != nil {
		return nil, &ErrUpstream{Msg: "Nexus 仓库列表解析失败"}
	}
	return repos, nil
}

func (s *OnlineREST) countAssets(ctx context.Context, base, repo string, auth credential.SourceAuth, maxPages int) (int64, bool, error) {
	var total int64
	token := ""
	truncated := false
	for page := 0; page < maxPages; page++ {
		q := url.Values{}
		q.Set("repository", repo)
		if token != "" {
			q.Set("continuationToken", token)
		}
		u := base + "/service/rest/v1/assets?" + q.Encode()
		body, status, err := s.get(ctx, u, auth)
		if err != nil {
			return total, truncated, outboundError("资产列表请求失败", err)
		}
		if status == http.StatusUnauthorized || status == http.StatusForbidden {
			return 0, false, &ErrAuth{Msg: "Nexus 认证失败"}
		}
		if status == http.StatusNotFound {
			return 0, false, nil
		}
		if status < 200 || status >= 300 {
			return total, truncated, &ErrUpstream{Msg: fmt.Sprintf("资产列表返回 %d", status)}
		}
		var pageData nexusAssetsPage
		if err := json.Unmarshal(body, &pageData); err != nil {
			return total, truncated, &ErrUpstream{Msg: "资产列表解析失败"}
		}
		total += int64(len(pageData.Items))
		if pageData.ContinuationToken == "" {
			return total, false, nil
		}
		token = pageData.ContinuationToken
		if page == maxPages-1 {
			truncated = true
		}
	}
	return total, truncated, nil
}

func (s *OnlineREST) get(ctx context.Context, rawURL string, auth credential.SourceAuth) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, 0, err
	}
	auth.Apply(req)
	resp, err := s.HTTP.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return body, resp.StatusCode, nil
}

func outboundError(message string, err error) *ErrUpstream {
	if errors.Is(err, upstream.ErrUnsafeURL) {
		return &ErrUpstream{Msg: "Nexus 来源被出站安全策略拒绝：该地址可能是本机或内网地址，若确认可信请在迁移任务勾选「来源是本机/内网地址（允许私网回源）」后重试", Cause: upstream.ErrUnsafeURL}
	}
	return &ErrUpstream{Msg: message}
}
