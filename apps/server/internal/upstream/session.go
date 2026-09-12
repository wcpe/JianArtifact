package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// ErrRequestFailed 表示受控会话的上游请求失败，避免把目标地址或凭据暴露给调用方。
var ErrRequestFailed = errors.New("upstream: 上游请求失败")

// Session 把一次上游读取绑定到配置的 origin 与可选凭据引用。
// 凭据只会发送到该 origin，绝对地址读取可安全复用既有的出站校验和重定向策略。
type Session struct {
	client        *Client
	origin        *url.URL
	credentialRef string
}

// NewSession 创建受控上游读取会话。configuredURL 必须是无用户信息的绝对 HTTP 地址；
// 配置的查询参数和片段不属于 origin，因而不接受。
func (c *Client) NewSession(configuredURL, credentialRef string) (*Session, error) {
	origin, err := parseSessionOrigin(configuredURL)
	if err != nil {
		return nil, ErrUnsafeURL
	}
	if credentialRef != "" {
		if _, err := ResolveCredential(credentialRef); err != nil {
			return nil, err
		}
	}
	return &Session{client: c, origin: origin, credentialRef: credentialRef}, nil
}

// ReadPath 读取配置地址下的相对路径。凭据仅会发往配置 origin。
func (s *Session) ReadPath(ctx context.Context, path string) (io.ReadCloser, http.Header, error) {
	return s.readPath(ctx, path, "")
}

// ReadPathWithAccept 读取配置地址下的相对路径，并声明协议可接受的内容类型。
// 认证头仍只由 Session 按同源规则注入，调用方无法通过此入口覆盖凭据边界。
func (s *Session) ReadPathWithAccept(ctx context.Context, path, accept string) (io.ReadCloser, http.Header, error) {
	return s.readPath(ctx, path, accept)
}

func (s *Session) readPath(ctx context.Context, path, accept string) (io.ReadCloser, http.Header, error) {
	if s == nil || s.client == nil || s.origin == nil {
		return nil, nil, ErrRequestFailed
	}
	target, err := joinURL(s.origin.String(), path)
	if err != nil {
		return nil, nil, ErrUnsafeURL
	}
	parsed, err := url.Parse(target)
	if err != nil {
		return nil, nil, ErrUnsafeURL
	}
	return s.read(ctx, parsed, accept)
}

// ReadAbsolute 读取绝对 HTTP 地址。若目标不是配置 origin，静态凭据不会随请求发送；
// 重定向过程继续由 Client 的统一策略逐跳校验并在跨 origin 时剥离认证头。
func (s *Session) ReadAbsolute(ctx context.Context, absoluteURL string) (io.ReadCloser, http.Header, error) {
	if s == nil || s.client == nil || s.origin == nil {
		return nil, nil, ErrRequestFailed
	}
	target, err := parseAbsoluteSessionURL(absoluteURL)
	if err != nil {
		return nil, nil, ErrUnsafeURL
	}
	return s.read(ctx, target, "")
}

// ReadOCIPath 读取 OCI Distribution 资源。遇到 Bearer 质询时，仅向安全 realm
// 请求目标镜像的 pull 令牌；配置的静态凭据不会随跨 origin 的 token 请求转发。
func (s *Session) ReadOCIPath(ctx context.Context, path, pullScope string) (io.ReadCloser, http.Header, error) {
	if s == nil || s.client == nil || s.origin == nil || !validOCIPullScope(pullScope) {
		return nil, nil, ErrRequestFailed
	}
	target, err := joinURL(s.origin.String(), path)
	if err != nil {
		return nil, nil, ErrUnsafeURL
	}
	parsed, err := url.Parse(target)
	if err != nil {
		return nil, nil, ErrUnsafeURL
	}
	response, err := s.request(ctx, parsed, "", true, "")
	if err != nil {
		return nil, nil, err
	}
	if response.StatusCode != http.StatusUnauthorized {
		return readResponse(response)
	}
	challenge, err := parseOCIBearerChallenge(response.Header.Values("WWW-Authenticate"), pullScope)
	_ = response.Body.Close()
	if err != nil {
		return nil, nil, ErrRequestFailed
	}
	token, err := s.ociToken(ctx, challenge, pullScope)
	if err != nil {
		return nil, nil, err
	}
	response, err = s.request(ctx, parsed, token, false, "")
	if err != nil {
		return nil, nil, err
	}
	return readResponse(response)
}

func (s *Session) read(ctx context.Context, target *url.URL, accept string) (io.ReadCloser, http.Header, error) {
	resp, err := s.request(ctx, target, "", true, accept)
	if err != nil {
		return nil, nil, err
	}
	return readResponse(resp)
}

func (s *Session) request(ctx context.Context, target *url.URL, bearer string, allowStaticCredential bool, accept string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, ErrUnsafeURL
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	} else if allowStaticCredential && s.credentialRef != "" && sameOrigin(s.origin, target) {
		credential, credentialErr := ResolveCredential(s.credentialRef)
		if credentialErr != nil {
			return nil, credentialErr
		}
		applyCredential(req, credential)
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		if errors.Is(err, ErrUnsafeURL) {
			return nil, ErrUnsafeURL
		}
		if IsTimeout(err) {
			return nil, err
		}
		return nil, ErrRequestFailed
	}
	return resp, nil
}

type ociBearerChallenge struct {
	realm   *url.URL
	service string
}

func (s *Session) ociToken(ctx context.Context, challenge ociBearerChallenge, pullScope string) (string, error) {
	target := *challenge.realm
	query := target.Query()
	if challenge.service != "" {
		query.Set("service", challenge.service)
	}
	query.Set("scope", pullScope)
	target.RawQuery = query.Encode()
	response, err := s.request(ctx, &target, "", true, "")
	if err != nil {
		return "", err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return "", ErrRequestFailed
	}
	var payload struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 1<<20))
	if err := decoder.Decode(&payload); err != nil {
		return "", ErrRequestFailed
	}
	token := payload.Token
	if token == "" {
		token = payload.AccessToken
	}
	if !validOCIBearerToken(token) {
		return "", ErrRequestFailed
	}
	return token, nil
}

func parseOCIBearerChallenge(values []string, pullScope string) (ociBearerChallenge, error) {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if len(value) < len("Bearer ") || !strings.EqualFold(value[:len("Bearer")], "Bearer") || (len(value) > len("Bearer") && value[len("Bearer")] != ' ') {
			continue
		}
		params, err := parseOCIChallengeParameters(strings.TrimSpace(value[len("Bearer"):]))
		if err != nil {
			return ociBearerChallenge{}, err
		}
		realm, ok := params["realm"]
		if !ok || realm == "" {
			return ociBearerChallenge{}, errors.New("缺少 realm")
		}
		parsed, err := parseAbsoluteSessionURL(realm)
		if err != nil {
			return ociBearerChallenge{}, err
		}
		if offered := params["scope"]; offered != "" && !ociScopeAllowsPull(offered, pullScope) {
			return ociBearerChallenge{}, errors.New("上游 scope 不允许 pull")
		}
		if service := params["service"]; len(service) > 512 || strings.ContainsAny(service, "\r\n") {
			return ociBearerChallenge{}, errors.New("service 非法")
		} else {
			return ociBearerChallenge{realm: parsed, service: service}, nil
		}
	}
	return ociBearerChallenge{}, errors.New("缺少 Bearer 质询")
}

func parseOCIChallengeParameters(value string) (map[string]string, error) {
	params := make(map[string]string)
	for value != "" {
		value = strings.TrimSpace(value)
		keyEnd := strings.IndexByte(value, '=')
		if keyEnd <= 0 {
			return nil, errors.New("质询参数非法")
		}
		key := strings.ToLower(strings.TrimSpace(value[:keyEnd]))
		value = strings.TrimSpace(value[keyEnd+1:])
		if value == "" || value[0] != '"' {
			return nil, errors.New("质询参数必须加引号")
		}
		value = value[1:]
		end := strings.IndexByte(value, '"')
		if end < 0 {
			return nil, errors.New("质询参数未闭合")
		}
		if _, exists := params[key]; exists {
			return nil, errors.New("质询参数重复")
		}
		params[key] = value[:end]
		value = strings.TrimSpace(value[end+1:])
		if value == "" {
			break
		}
		if value[0] != ',' {
			return nil, errors.New("质询参数分隔符非法")
		}
		value = value[1:]
	}
	return params, nil
}

func validOCIPullScope(scope string) bool {
	parts := strings.Split(scope, ":")
	return len(parts) == 3 && parts[0] == "repository" && parts[1] != "" && parts[2] == "pull" && !strings.ContainsAny(scope, " \r\n\\")
}

func ociScopeAllowsPull(offered, required string) bool {
	parts := strings.Split(offered, ":")
	requiredParts := strings.Split(required, ":")
	if len(parts) != 3 || len(requiredParts) != 3 || parts[0] != "repository" || parts[1] != requiredParts[1] {
		return false
	}
	for _, action := range strings.Split(parts[2], ",") {
		if action == "pull" {
			return true
		}
	}
	return false
}

func validOCIBearerToken(token string) bool {
	return token != "" && len(token) <= 16384 && !strings.ContainsAny(token, " \r\n")
}

func parseSessionOrigin(rawURL string) (*url.URL, error) {
	target, err := parseAbsoluteSessionURL(rawURL)
	if err != nil || target.RawQuery != "" || target.Fragment != "" {
		return nil, ErrUnsafeURL
	}
	return target, nil
}

func parseAbsoluteSessionURL(rawURL string) (*url.URL, error) {
	target, err := url.ParseRequestURI(rawURL)
	if err != nil || target == nil || target.Scheme == "" || target.Host == "" || target.User != nil {
		return nil, ErrUnsafeURL
	}
	if target.Scheme != "http" && target.Scheme != "https" {
		return nil, ErrUnsafeURL
	}
	return target, nil
}

func readResponse(resp *http.Response) (io.ReadCloser, http.Header, error) {
	if resp.StatusCode == http.StatusNotFound {
		_ = resp.Body.Close()
		return nil, nil, ErrNotFound
	}
	if resp.StatusCode == http.StatusGone {
		_ = resp.Body.Close()
		return nil, nil, ErrGone
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		_ = resp.Body.Close()
		return nil, nil, &StatusError{Code: resp.StatusCode}
	}
	return resp.Body, resp.Header, nil
}
