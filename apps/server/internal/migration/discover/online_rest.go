package discover

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// OnlineREST 通过 Nexus REST API 发现仓库与资产规模估算。
type OnlineREST struct {
	HTTP *http.Client
}

// NewOnlineREST 构造；client 为 nil 时用 30s 超时默认客户端。
func NewOnlineREST(client *http.Client) *OnlineREST {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
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
	base := strings.TrimRight(cfg.URL, "/")
	maxPages := cfg.MaxAssetPages
	if maxPages <= 0 {
		maxPages = 5
	}

	repos, err := s.listRepositories(ctx, base, cfg.Credential)
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
		var count int64
		if countAssets {
			n, truncated, err := s.countAssets(ctx, base, r.Name, cfg.Credential, maxPages)
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
		typ := strings.ToLower(r.Type)
		if typ == "" {
			typ = "hosted"
		}
		plan.Repositories = append(plan.Repositories, PlanRepository{
			Name:            r.Name,
			Format:          mapped,
			Type:            typ,
			EstimatedAssets: count,
		})
	}
	if filterOn && len(plan.Repositories) == 0 {
		plan.Warnings = append(plan.Warnings, "includeRepositories 未匹配到任何可迁移仓库")
	}
	return finalizePlan(plan), nil
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
func (s *OnlineREST) ListRemoteRepositories(ctx context.Context, baseURL, credential string, supportedOnly bool) ([]RemoteRepository, error) {
	if err := requireURL(baseURL); err != nil {
		return nil, err
	}
	base := strings.TrimRight(baseURL, "/")
	repos, err := s.listRepositories(ctx, base, credential)
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

func (s *OnlineREST) listRepositories(ctx context.Context, base, credential string) ([]nexusRepo, error) {
	u := base + "/service/rest/v1/repositories"
	body, status, err := s.get(ctx, u, credential)
	if err != nil {
		return nil, &ErrUpstream{Msg: "无法连接 Nexus REST"}
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

func (s *OnlineREST) countAssets(ctx context.Context, base, repo, credential string, maxPages int) (int64, bool, error) {
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
		body, status, err := s.get(ctx, u, credential)
		if err != nil {
			return total, truncated, &ErrUpstream{Msg: "资产列表请求失败"}
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

func (s *OnlineREST) get(ctx context.Context, rawURL, credential string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, 0, err
	}
	if credential != "" {
		// 含 ":" 视为 Basic user:pass；否则 Bearer token
		if strings.Contains(credential, ":") {
			parts := strings.SplitN(credential, ":", 2)
			req.SetBasicAuth(parts[0], parts[1])
		} else {
			req.Header.Set("Authorization", "Bearer "+credential)
		}
	}
	resp, err := s.HTTP.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return body, resp.StatusCode, nil
}
