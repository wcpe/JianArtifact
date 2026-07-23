package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/migration/discover"
)

// onlineAsset 是 Nexus assets API 中与下载相关的字段（容错解析）。
type onlineAsset struct {
	Path        string `json:"path"`
	DownloadURL string `json:"downloadUrl"`
	ContentType string `json:"contentType"`
}

type onlineAssetsPage struct {
	Items             []onlineAsset `json:"items"`
	ContinuationToken string        `json:"continuationToken"`
}

// enumerateOnlineREST 枚举 plan 中各仓库的资产，Open 时流式 HTTP GET downloadUrl。
// cred 为已解析的凭据明文（user:pass 或 token）；空表示匿名。
func enumerateOnlineREST(baseURL, cred string, plan discover.Plan) ([]sourceItem, error) {
	if baseURL == "" {
		return nil, fmt.Errorf("sourceConfig.url 为空")
	}
	base := strings.TrimRight(baseURL, "/")
	client := &http.Client{Timeout: 0} // 单请求不整体超时；依赖 ctx（Open 时用 Background 短超时客户端）
	// 下载用独立超时客户端
	dlClient := &http.Client{Timeout: 5 * time.Minute}
	listClient := &http.Client{Timeout: 60 * time.Second}
	_ = client

	var items []sourceItem
	for _, repo := range plan.Repositories {
		format := repo.Format
		if format == "" {
			format = "raw"
		}
		assets, err := listAllAssets(listClient, base, repo.Name, cred)
		if err != nil {
			return nil, fmt.Errorf("枚举仓库 %s：%w", repo.Name, err)
		}
		for _, a := range assets {
			if a.Path == "" || a.DownloadURL == "" {
				continue
			}
			// 跳过目录占位
			if strings.HasSuffix(a.Path, "/") {
				continue
			}
			path := strings.TrimPrefix(a.Path, "/")
			dlURL := a.DownloadURL
			ct := a.ContentType
			repoName := repo.Name
			items = append(items, sourceItem{
				Repo:   repoName,
				Path:   path,
				Format: format,
				Open: func() (io.ReadCloser, error) {
					return openDownload(dlClient, dlURL, cred, ct)
				},
			})
		}
	}
	return items, nil
}

func listAllAssets(client *http.Client, base, repo, cred string) ([]onlineAsset, error) {
	var all []onlineAsset
	token := ""
	// 防护：单仓最多 10000 页 × 默认页大小，避免失控
	const maxPages = 10000
	for page := 0; page < maxPages; page++ {
		q := url.Values{}
		q.Set("repository", repo)
		if token != "" {
			q.Set("continuationToken", token)
		}
		u := base + "/service/rest/v1/assets?" + q.Encode()
		body, status, err := httpGet(client, u, cred)
		if err != nil {
			return nil, err
		}
		if status == http.StatusUnauthorized || status == http.StatusForbidden {
			return nil, fmt.Errorf("Nexus 认证失败")
		}
		if status == http.StatusNotFound {
			return all, nil
		}
		if status < 200 || status >= 300 {
			return nil, fmt.Errorf("资产列表返回 %d", status)
		}
		var pg onlineAssetsPage
		if err := json.Unmarshal(body, &pg); err != nil {
			return nil, fmt.Errorf("解析资产列表：%w", err)
		}
		all = append(all, pg.Items...)
		if pg.ContinuationToken == "" {
			break
		}
		token = pg.ContinuationToken
	}
	return all, nil
}

func openDownload(client *http.Client, downloadURL, cred, contentType string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, downloadURL, nil)
	if err != nil {
		return nil, err
	}
	applyCredential(req, cred)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("下载返回 %d", resp.StatusCode)
	}
	// 流式返回 body，调用方 Close
	_ = contentType
	return resp.Body, nil
}

func httpGet(client *http.Client, rawURL, cred string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, 0, err
	}
	applyCredential(req, cred)
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	// 列表响应限制 32MB
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return body, resp.StatusCode, nil
}

func applyCredential(req *http.Request, cred string) {
	if cred == "" {
		return
	}
	if strings.Contains(cred, ":") {
		parts := strings.SplitN(cred, ":", 2)
		req.SetBasicAuth(parts[0], parts[1])
		return
	}
	req.Header.Set("Authorization", "Bearer "+cred)
}

// resolveCred 从 credential_ref 读环境变量；空 ref 返回空串。
func resolveCred(ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", nil
	}
	return domain.ResolveCredential(ref)
}
