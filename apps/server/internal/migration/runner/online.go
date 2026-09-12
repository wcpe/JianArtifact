package runner

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/migration/credential"
	"github.com/wcpe/jianartifact/apps/server/internal/migration/discover"
	"github.com/wcpe/jianartifact/apps/server/internal/upstream"
)

// OnlineAsset 是 Nexus assets API 中资产元数据字段（含创建/更新时间，供迁移与时间回填复用）。
type OnlineAsset struct {
	Path         string `json:"path"`
	DownloadURL  string `json:"downloadUrl"`
	ContentType  string `json:"contentType"`
	BlobCreated  string `json:"blobCreated"`
	LastModified string `json:"lastModified"`
}

type onlineAssetsPage struct {
	Items             []OnlineAsset `json:"items"`
	ContinuationToken string        `json:"continuationToken"`
}

// enumerateOnlineREST 枚举 plan 中各仓库的资产，Open 时流式 HTTP GET downloadUrl。
// auth 为已解析的显式来源认证；不得写入任务、报告或日志。
func enumerateOnlineREST(ctx context.Context, client *upstream.Client, baseURL string, auth credential.SourceAuth, plan discover.Plan, onProg discover.EnumProgress) ([]sourceItem, error) {
	if baseURL == "" {
		return nil, fmt.Errorf("online_rest 来源基址为空")
	}
	if client == nil {
		client = upstream.NewClient(5 * time.Minute)
	}
	base := strings.TrimRight(baseURL, "/")

	var items []sourceItem
	for _, repo := range plan.Repositories {
		if err := ctx.Err(); err != nil {
			return nil, ctx.Err()
		}
		if repo.MigrationMode != "" && repo.MigrationMode != "assets" {
			continue
		}
		format := repo.Format
		if format == "" {
			format = "raw"
		}
		assets, err := listAllAssets(ctx, client, base, repo.Name, auth, onProg)
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
			if skipOnlineMigrationAsset(format, path) {
				continue
			}
			dlURL := a.DownloadURL
			ct := a.ContentType
			repoName := repo.Name
			items = append(items, sourceItem{
				Repo:           repoName,
				Path:           path,
				Format:         format,
				SourceModified: parseNexusTime(a.LastModified, a.BlobCreated),
				Open: func() (io.ReadCloser, error) {
					return openDownload(ctx, client, base, dlURL, auth, ct)
				},
			})
		}
	}
	sort.SliceStable(items, func(i, j int) bool {
		return onlineMigrationAssetRank(items[i]) < onlineMigrationAssetRank(items[j])
	})
	return items, nil
}

func skipOnlineMigrationAsset(format, path string) bool {
	return format == "pypi" && strings.HasSuffix(path, ".metadata") ||
		format == "cargo" && (strings.Trim(path, "/") == "config.json" || !isCargoCrateAsset(path))
}

func isCargoCrateAsset(path string) bool {
	path = strings.Trim(path, "/")
	return strings.HasPrefix(path, "api/v1/crates/") ||
		strings.HasPrefix(path, "crates/") ||
		strings.HasPrefix(path, "cargo/crates/")
}

func onlineMigrationAssetRank(item sourceItem) int {
	if item.Format != "cargo" {
		return 0
	}
	path := strings.Trim(item.Path, "/")
	if strings.HasPrefix(path, "api/v1/crates/") || strings.HasPrefix(path, "cargo/crates/") {
		return 0
	}
	return 1
}

// ListAllAssets 分页拉取源 Nexus 仓库的全部资产元数据（含 blobCreated/lastModified），
// 供迁移枚举与时间回填（admin backfill-times）复用。保留旧字符串凭据兼容。
func ListAllAssets(ctx context.Context, base, repo, cred string) ([]OnlineAsset, error) {
	client := upstream.NewClient(60 * time.Second)
	return listAllAssets(ctx, client, base, repo, credential.FromLegacy(cred), nil)
}

func listAllAssets(ctx context.Context, client *upstream.Client, base, repo string, auth credential.SourceAuth, onProg discover.EnumProgress) ([]OnlineAsset, error) {
	var all []OnlineAsset
	token := ""
	// 防护：单仓最多 10000 页 × 默认页大小，避免失控
	const maxPages = 10000
	for page := 0; page < maxPages; page++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		q := url.Values{}
		q.Set("repository", repo)
		if token != "" {
			q.Set("continuationToken", token)
		}
		u := base + "/service/rest/v1/assets?" + q.Encode()
		body, status, err := httpGet(ctx, client, u, auth)
		if err != nil {
			return nil, onlineRequestError(err)
		}
		if status == http.StatusUnauthorized || status == http.StatusForbidden {
			return nil, fmt.Errorf("向 Nexus 认证失败")
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
		if onProg != nil {
			onProg(int64(len(all)), repo)
		}
		if pg.ContinuationToken == "" {
			break
		}
		token = pg.ContinuationToken
	}
	return all, nil
}

func openDownload(ctx context.Context, client *upstream.Client, sourceBase, downloadURL string, auth credential.SourceAuth, contentType string) (io.ReadCloser, error) {
	if !sameOrigin(sourceBase, downloadURL) {
		return nil, onlineRequestError(upstream.ErrUnsafeURL)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return nil, onlineRequestError(err)
	}
	applyCredential(req, auth)
	resp, err := client.Do(req)
	if err != nil {
		return nil, onlineRequestError(err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("下载返回 %d", resp.StatusCode)
	}
	// 流式返回 body，调用方 Close
	_ = contentType
	return resp.Body, nil
}

// sameOrigin 仅允许 Nexus 资产下载回到同一来源，避免将来源凭据发送给资产列表返回的跨站地址。
func sameOrigin(sourceBase, downloadURL string) bool {
	source, err := url.Parse(sourceBase)
	if err != nil {
		return false
	}
	download, err := url.Parse(downloadURL)
	if err != nil {
		return false
	}
	return strings.EqualFold(source.Scheme, download.Scheme) &&
		strings.EqualFold(source.Hostname(), download.Hostname()) &&
		effectivePort(source) == effectivePort(download)
}

func effectivePort(u *url.URL) string {
	if port := u.Port(); port != "" {
		return port
	}
	switch strings.ToLower(u.Scheme) {
	case "http":
		return "80"
	case "https":
		return "443"
	default:
		return ""
	}
}

func httpGet(ctx context.Context, client *upstream.Client, rawURL string, auth credential.SourceAuth) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, 0, onlineRequestError(err)
	}
	applyCredential(req, auth)
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	// 列表响应限制 32MB
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return body, resp.StatusCode, nil
}

func onlineRequestError(err error) error {
	if errors.Is(err, upstream.ErrUnsafeURL) {
		return fmt.Errorf("出站安全策略拒绝 Nexus 来源：%w", upstream.ErrUnsafeURL)
	}
	if upstream.IsTimeout(err) {
		return errors.New("Nexus 来源连接超时")
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return errors.New("Nexus 来源域名解析失败")
	}
	var tlsRecordErr *tls.RecordHeaderError
	var tlsVerifyErr *tls.CertificateVerificationError
	var unknownAuthority x509.UnknownAuthorityError
	var invalidCertificate x509.CertificateInvalidError
	if errors.As(err, &tlsRecordErr) || errors.As(err, &tlsVerifyErr) ||
		errors.As(err, &unknownAuthority) || errors.As(err, &invalidCertificate) {
		return errors.New("Nexus 来源 TLS 握手失败")
	}
	var netErr *net.OpError
	if errors.As(err, &netErr) {
		return errors.New("Nexus 来源网络连接失败")
	}
	return errors.New("Nexus 来源网络请求失败")
}

func applyCredential(req *http.Request, auth credential.SourceAuth) {
	auth.Apply(req)
}

// parseNexusTime 从 Nexus 资产元数据的 lastModified / blobCreated（RFC3339 字符串）解析源端时间，
// 优先 lastModified，缺失或解析失败则回退 blobCreated。两者皆空或解析失败返回零值，
// 调用方据此回退到 Put 的本地时间语义。
func parseNexusTime(lastModified, blobCreated string) time.Time {
	for _, s := range []string{lastModified, blobCreated} {
		if s == "" {
			continue
		}
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// resolveCred 从 credential_ref 读环境变量；空 ref 返回空串。
func resolveCred(ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", nil
	}
	return domain.ResolveCredential(ref)
}
