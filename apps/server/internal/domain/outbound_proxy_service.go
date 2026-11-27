package domain

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/repository"
	"github.com/wcpe/jianartifact/apps/server/internal/upstream"
)

// defaultProxyTestTargetURL 是代理连通性测试缺省探测地址：
// 使用 MC 常见上游之一，用户测试时可按需覆盖为任意公网地址（如 https://libraries.minecraft.net/）。
const defaultProxyTestTargetURL = "https://libraries.minecraft.net/"

// outboundProxySecret 是加密落库的配置载荷（仅在内存中解密使用，绝不回显或写日志）。
type outboundProxySecret struct {
	ProxyURL     string `json:"proxyUrl,omitempty"`
	MihomoConfig string `json:"mihomoConfig,omitempty"`
}

// OutboundProxyView 是管理端可见的出站代理视图（不含密文与明文凭据）。
type OutboundProxyView struct {
	ID                int64
	Name              string
	Kind              string
	Description       string
	EndpointPreview   string
	Enabled           bool
	BoundRepositories []string
	CreatedAt         string
	UpdatedAt         string
}

// CreateOutboundProxyInput 创建出站代理入参。
type CreateOutboundProxyInput struct {
	Name        string
	Kind        string
	Description string
	Config      string // HTTP/SOCKS5 代理 URL 或 Mihomo YAML/分享链接
	Enabled     bool
}

// UpdateOutboundProxyInput 更新出站代理入参；指针为 nil 表示保持原值。
type UpdateOutboundProxyInput struct {
	Name        *string
	Kind        *string
	Description *string
	Config      *string // 非空时重新校验并替换加密配置；空串/nil 且类型未变时保留原加密配置
	Enabled     *bool
}

// OutboundProxyTestResult 表示一次出站代理连通性探测的结果。
type OutboundProxyTestResult struct {
	OK         bool
	StatusCode int
	LatencyMs  int64
	Message    string
}

type cachedProxyTransport struct {
	hash      string
	transport *http.Transport
}

// OutboundProxyService 管理全局出站代理池、凭据加解密与运行期传输层缓存。
type OutboundProxyService struct {
	proxies   *repository.OutboundProxyRepo
	repos     *repository.RepoRepo
	aead      cipher.AEAD
	mihomo    *upstream.MihomoManager
	writeGate BusinessWriteGate

	mu             sync.Mutex
	transportCache map[int64]cachedProxyTransport
}

// NewOutboundProxyService 构造 OutboundProxyService；key 必须为 32 字节（AES-256-GCM）。
func NewOutboundProxyService(
	proxies *repository.OutboundProxyRepo,
	repos *repository.RepoRepo,
	key []byte,
	mihomo *upstream.MihomoManager,
) (*OutboundProxyService, error) {
	if len(key) != 32 {
		return nil, errors.New("出站代理加密密钥必须为 32 字节")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("初始化出站代理加密：%w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("初始化出站代理认证加密：%w", err)
	}
	return &OutboundProxyService{
		proxies:        proxies,
		repos:          repos,
		aead:           aead,
		mihomo:         mihomo,
		transportCache: make(map[int64]cachedProxyTransport),
	}, nil
}

// SetBusinessWriteGate 注入业务写栅栏（写入冻结期间拒绝修改代理配置）。
func (s *OutboundProxyService) SetBusinessWriteGate(gate BusinessWriteGate) { s.writeGate = gate }

// Capabilities 返回本机 Mihomo 运行时可用能力。
func (s *OutboundProxyService) Capabilities(ctx context.Context) upstream.MihomoCapabilities {
	if s.mihomo == nil {
		return upstream.MihomoCapabilities{Available: false}
	}
	return s.mihomo.Capabilities(ctx)
}

// List 返回全部出站代理及其绑定的仓库名列表。
func (s *OutboundProxyService) List() ([]OutboundProxyView, error) {
	items, err := s.proxies.List()
	if err != nil {
		return nil, err
	}
	boundMap := s.boundRepositoriesByProxy()
	out := make([]OutboundProxyView, 0, len(items))
	for i := range items {
		out = append(out, toOutboundProxyView(&items[i], boundMap[items[i].ID]))
	}
	return out, nil
}

// Get 按 ID 返回单个出站代理视图。
func (s *OutboundProxyService) Get(id int64) (*OutboundProxyView, error) {
	p, err := s.proxies.GetByID(id)
	if err != nil {
		return nil, mapNotFound(err)
	}
	boundMap := s.boundRepositoriesByProxy()
	view := toOutboundProxyView(p, boundMap[p.ID])
	return &view, nil
}

// Exists 检查出站代理 ID 是否存在（供 RepositoryService 校验引用合法性）。
func (s *OutboundProxyService) Exists(id int64) (bool, error) {
	if id <= 0 {
		return false, nil
	}
	_, err := s.proxies.GetByID(id)
	if errors.Is(err, repository.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// NameByID 返回代理名称映射（供仓库列表批量补全 outboundProxyName）。
func (s *OutboundProxyService) NameMap() (map[int64]string, error) {
	items, err := s.proxies.List()
	if err != nil {
		return nil, err
	}
	m := make(map[int64]string, len(items))
	for i := range items {
		m[items[i].ID] = items[i].Name
	}
	return m, nil
}

// Create 新增全局出站代理：校验并规范化协议配置、生成脱敏预览、AES-256-GCM 密封落库。
func (s *OutboundProxyService) Create(in CreateOutboundProxyInput) (*OutboundProxyView, error) {
	if err := requireBusinessWrite(s.writeGate); err != nil {
		return nil, err
	}
	name := strings.TrimSpace(in.Name)
	if name == "" || len(name) > 64 {
		return nil, fmt.Errorf("%w: 代理名称须为 1-64 个字符", ErrValidation)
	}
	kind := strings.ToLower(strings.TrimSpace(in.Kind))
	preview, secret, err := validateAndBuildProxySecret(kind, in.Config)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrValidation, err)
	}
	ciphertext, err := s.sealSecret(kind, secret)
	if err != nil {
		return nil, err
	}
	row := &repository.OutboundProxy{
		Name:             name,
		Kind:             kind,
		Description:      strings.TrimSpace(in.Description),
		EndpointPreview:  preview,
		ConfigCiphertext: ciphertext,
		Enabled:          in.Enabled,
	}
	id, err := s.proxies.Create(row)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, fmt.Errorf("%w: 代理名称 %q 已存在", ErrConflict, name)
		}
		return nil, err
	}
	return s.Get(id)
}

// Update 更新全局出站代理；若变更了配置或停用代理，立即失效运行期缓存与受管 Mihomo 子进程。
func (s *OutboundProxyService) Update(id int64, in UpdateOutboundProxyInput) (*OutboundProxyView, error) {
	if err := requireBusinessWrite(s.writeGate); err != nil {
		return nil, err
	}
	existing, err := s.proxies.GetByID(id)
	if err != nil {
		return nil, mapNotFound(err)
	}
	next := *existing
	if in.Name != nil {
		name := strings.TrimSpace(*in.Name)
		if name == "" || len(name) > 64 {
			return nil, fmt.Errorf("%w: 代理名称须为 1-64 个字符", ErrValidation)
		}
		next.Name = name
	}
	if in.Description != nil {
		next.Description = strings.TrimSpace(*in.Description)
	}
	if in.Enabled != nil {
		next.Enabled = *in.Enabled
	}
	kindChanged := false
	if in.Kind != nil {
		k := strings.ToLower(strings.TrimSpace(*in.Kind))
		if k != existing.Kind {
			kindChanged = true
		}
		next.Kind = k
	}
	rawCfg := ""
	if in.Config != nil {
		rawCfg = strings.TrimSpace(*in.Config)
	}
	if kindChanged && rawCfg == "" {
		return nil, fmt.Errorf("%w: 切换代理协议类型时必须提供新的代理配置", ErrValidation)
	}
	if rawCfg != "" {
		preview, secret, valErr := validateAndBuildProxySecret(next.Kind, rawCfg)
		if valErr != nil {
			return nil, fmt.Errorf("%w: %v", ErrValidation, valErr)
		}
		ciphertext, sealErr := s.sealSecret(next.Kind, secret)
		if sealErr != nil {
			return nil, sealErr
		}
		next.EndpointPreview = preview
		next.ConfigCiphertext = ciphertext
	}
	if err := s.proxies.Update(&next); err != nil {
		if isUniqueViolation(err) {
			return nil, fmt.Errorf("%w: 代理名称 %q 已存在", ErrConflict, next.Name)
		}
		return nil, mapNotFound(err)
	}
	s.invalidateTransport(id)
	return s.Get(id)
}

// Delete 删除出站代理；若仍有 proxy 仓库挂载该代理则拒绝（409 Conflict）并指明仓库名。
func (s *OutboundProxyService) Delete(id int64) error {
	if err := requireBusinessWrite(s.writeGate); err != nil {
		return err
	}
	if _, err := s.proxies.GetByID(id); err != nil {
		return mapNotFound(err)
	}
	bound := s.boundRepositoriesByProxy()[id]
	if len(bound) > 0 {
		return fmt.Errorf("%w: 代理仍被以下仓库使用，请先解除绑定：%s", ErrConflict, strings.Join(bound, ", "))
	}
	if err := s.proxies.Delete(id); err != nil {
		return mapNotFound(err)
	}
	s.invalidateTransport(id)
	return nil
}

// ResolveTransport 按代理 ID 返回可用传输层，实现 upstream.ProxyTransportResolver。
func (s *OutboundProxyService) ResolveTransport(ctx context.Context, proxyID int64) (http.RoundTripper, error) {
	if s == nil || proxyID <= 0 {
		return nil, upstream.ErrProxyUnavailable
	}
	row, err := s.proxies.GetByID(proxyID)
	if err != nil || !row.Enabled {
		return nil, upstream.ErrProxyUnavailable
	}
	secret, err := s.openSecret(row.Kind, row.ConfigCiphertext)
	if err != nil {
		return nil, fmt.Errorf("%w: 解密代理配置失败", upstream.ErrProxyUnavailable)
	}
	sum := sha256.Sum256(row.ConfigCiphertext)
	hashHex := hex.EncodeToString(sum[:])

	switch row.Kind {
	case repository.OutboundProxyKindHTTP, repository.OutboundProxyKindSOCKS5:
		s.mu.Lock()
		if cached, ok := s.transportCache[proxyID]; ok && cached.hash == hashHex && cached.transport != nil {
			s.mu.Unlock()
			return cached.transport, nil
		}
		s.mu.Unlock()

		tr, buildErr := upstream.NewProxyTransport(row.Kind, secret.ProxyURL)
		if buildErr != nil {
			return nil, fmt.Errorf("%w: %v", upstream.ErrProxyUnavailable, buildErr)
		}
		s.mu.Lock()
		if old, ok := s.transportCache[proxyID]; ok && old.transport != nil {
			old.transport.CloseIdleConnections()
		}
		s.transportCache[proxyID] = cachedProxyTransport{hash: hashHex, transport: tr}
		s.mu.Unlock()
		return tr, nil

	case repository.OutboundProxyKindMihomo:
		if s.mihomo == nil {
			return nil, upstream.ErrMihomoNotInstalled
		}
		node, _, parseErr := upstream.ParseMihomoConfig(secret.MihomoConfig)
		if parseErr != nil {
			return nil, fmt.Errorf("%w: %v", upstream.ErrProxyUnavailable, parseErr)
		}
		return s.mihomo.TransportFor(ctx, proxyID, hashHex, node)

	default:
		return nil, upstream.ErrProxyUnavailable
	}
}

// TestConnectivity 通过指定出站代理向目标公网地址发起一次连通性探测，返回状态码与耗时。
func (s *OutboundProxyService) TestConnectivity(ctx context.Context, proxyID int64, targetURL string, client *upstream.Client) (OutboundProxyTestResult, error) {
	if _, err := s.proxies.GetByID(proxyID); err != nil {
		return OutboundProxyTestResult{}, mapNotFound(err)
	}
	target := strings.TrimSpace(targetURL)
	if target == "" {
		target = defaultProxyTestTargetURL
	}
	u, err := url.Parse(target)
	if err != nil || !u.IsAbs() || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return OutboundProxyTestResult{}, fmt.Errorf("%w: 测试目标地址必须为合法的 http/https URL", ErrValidation)
	}
	if client == nil {
		return OutboundProxyTestResult{}, fmt.Errorf("%w: 回源客户端未就绪", ErrUpstream)
	}
	probeCtx, cancel := context.WithTimeout(upstream.WithOutboundProxyID(ctx, proxyID), 12*time.Second)
	defer cancel()

	started := time.Now()
	status, probeErr := client.ProbeWithCredential(probeCtx, target, "")
	latencyMs := time.Since(started).Milliseconds()
	if probeErr != nil {
		return OutboundProxyTestResult{
			OK:        false,
			LatencyMs: latencyMs,
			Message:   probeErr.Error(),
		}, nil
	}
	// 上游返回 2xx/3xx/4xx（如 S3/CloudFront 根路径常返回 403/404）均证明代理隧道已贯通目标上游。
	ok := status > 0 && status < 500
	msg := fmt.Sprintf("上游响应 HTTP %d", status)
	if !ok {
		msg = fmt.Sprintf("上游返回服务端错误 HTTP %d", status)
	}
	return OutboundProxyTestResult{
		OK:         ok,
		StatusCode: status,
		LatencyMs:  latencyMs,
		Message:    msg,
	}, nil
}

// Close 释放所有缓存连接并终止全部受管 Mihomo 子进程（进程停机时调用）。
func (s *OutboundProxyService) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	for id, cached := range s.transportCache {
		if cached.transport != nil {
			cached.transport.CloseIdleConnections()
		}
		delete(s.transportCache, id)
	}
	s.mu.Unlock()
	if s.mihomo != nil {
		s.mihomo.Close()
	}
}

func (s *OutboundProxyService) invalidateTransport(proxyID int64) {
	s.mu.Lock()
	if cached, ok := s.transportCache[proxyID]; ok {
		if cached.transport != nil {
			cached.transport.CloseIdleConnections()
		}
		delete(s.transportCache, proxyID)
	}
	s.mu.Unlock()
	if s.mihomo != nil {
		s.mihomo.Invalidate(proxyID)
	}
}

func (s *OutboundProxyService) boundRepositoriesByProxy() map[int64][]string {
	out := make(map[int64][]string)
	if s.repos == nil {
		return out
	}
	const pageSize = 200
	for offset := 0; ; offset += pageSize {
		repos, err := s.repos.List(pageSize, offset)
		if err != nil {
			break
		}
		for i := range repos {
			if repos[i].Type != "proxy" {
				continue
			}
			cfg, err := repos[i].DecodeConfig()
			if err != nil || cfg.OutboundProxyID <= 0 {
				continue
			}
			out[cfg.OutboundProxyID] = append(out[cfg.OutboundProxyID], repos[i].Name)
		}
		if len(repos) < pageSize {
			break
		}
	}
	for id := range out {
		sort.Strings(out[id])
	}
	return out
}

func validateAndBuildProxySecret(kind, raw string) (string, outboundProxySecret, error) {
	switch kind {
	case repository.OutboundProxyKindHTTP, repository.OutboundProxyKindSOCKS5:
		preview, normalized, err := upstream.ValidateStandardProxyURL(kind, raw)
		if err != nil {
			return "", outboundProxySecret{}, err
		}
		return preview, outboundProxySecret{ProxyURL: normalized}, nil
	case repository.OutboundProxyKindMihomo:
		_, preview, err := upstream.ParseMihomoConfig(raw)
		if err != nil {
			return "", outboundProxySecret{}, err
		}
		return preview, outboundProxySecret{MihomoConfig: strings.TrimSpace(raw)}, nil
	default:
		return "", outboundProxySecret{}, fmt.Errorf("不支持的代理协议类型 %q（可选 http / socks5 / mihomo）", kind)
	}
}

func (s *OutboundProxyService) sealSecret(kind string, secret outboundProxySecret) ([]byte, error) {
	payload, err := json.Marshal(secret)
	if err != nil {
		return nil, fmt.Errorf("序列化出站代理配置：%w", err)
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("生成加密随机数：%w", err)
	}
	return s.aead.Seal(nonce, nonce, payload, []byte("outbound-proxy:"+kind)), nil
}

func (s *OutboundProxyService) openSecret(kind string, ciphertext []byte) (outboundProxySecret, error) {
	if len(ciphertext) < s.aead.NonceSize()+s.aead.Overhead() {
		return outboundProxySecret{}, errors.New("密文格式非法")
	}
	nonce := ciphertext[:s.aead.NonceSize()]
	plain, err := s.aead.Open(nil, nonce, ciphertext[s.aead.NonceSize():], []byte("outbound-proxy:"+kind))
	if err != nil {
		return outboundProxySecret{}, errors.New("密文解密失败")
	}
	var secret outboundProxySecret
	if err := json.Unmarshal(plain, &secret); err != nil {
		return outboundProxySecret{}, errors.New("代理配置反序列化失败")
	}
	return secret, nil
}

func toOutboundProxyView(row *repository.OutboundProxy, bound []string) OutboundProxyView {
	if bound == nil {
		bound = []string{}
	}
	return OutboundProxyView{
		ID:                row.ID,
		Name:              row.Name,
		Kind:              row.Kind,
		Description:       row.Description,
		EndpointPreview:   row.EndpointPreview,
		Enabled:           row.Enabled,
		BoundRepositories: bound,
		CreatedAt:         row.CreatedAt,
		UpdatedAt:         row.UpdatedAt,
	}
}
