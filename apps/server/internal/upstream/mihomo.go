package upstream

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/goccy/go-yaml"
)

// allowedMihomoProxyTypes 是允许在受管 Mihomo 配置中使用的代理协议白名单。
// 仅放行节点出站协议，严禁用户注入外部控制器、TUN、规则提供商等顶层配置。
var allowedMihomoProxyTypes = map[string]bool{
	"ss":        true,
	"ssr":       true,
	"vmess":     true,
	"vless":     true,
	"trojan":    true,
	"hysteria":  true,
	"hysteria2": true,
	"tuic":      true,
	"anytls":    true,
	"wireguard": true,
	"ssh":       true,
	"socks5":    true,
	"http":      true,
	"snell":     true,
	"mieru":     true,
	"sudoku":    true,
}

// ParseMihomoConfig 解析用户提交的 Mihomo 节点配置。
// 支持两类输入：
//  1. 常见单节点分享链接：vless://、vmess://、trojan://、ss://、hysteria2://、hy2://、tuic://、anytls://、socks5://、http://、https://
//  2. Clash / Mihomo YAML 节点定义（单节点对象，或包含 proxies 列表的 YAML 片段，默认取首个节点）
//
// 返回标准化后的单节点 map（name 统一改写为 "upstream-node"）与不含密钥的脱敏预览串。
func ParseMihomoConfig(raw string) (map[string]any, string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, "", errors.New("Mihomo 节点配置不能为空")
	}
	if len(trimmed) > 64*1024 {
		return nil, "", errors.New("Mihomo 节点配置过长（上限 64 KiB）")
	}
	var node map[string]any
	var err error
	if isMihomoShareURI(trimmed) {
		node, err = parseMihomoShareURI(trimmed)
	} else {
		node, err = parseMihomoYAMLNode(trimmed)
	}
	if err != nil {
		return nil, "", err
	}
	return validateAndNormalizeMihomoNode(node)
}

func isMihomoShareURI(s string) bool {
	if strings.ContainsAny(s, "\r\n") {
		return false
	}
	lower := strings.ToLower(s)
	for _, prefix := range []string{
		"vless://", "vmess://", "trojan://", "ss://",
		"hysteria2://", "hy2://", "tuic://", "anytls://",
		"socks5://", "socks5h://", "http://", "https://",
	} {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}

func parseMihomoYAMLNode(raw string) (map[string]any, error) {
	// 兼容从 Clash 配置里直接复制带 `- ` 列表前缀的单节点片段。
	var list []map[string]any
	if strings.HasPrefix(raw, "-") {
		if err := yaml.Unmarshal([]byte(raw), &list); err == nil && len(list) > 0 {
			return list[0], nil
		}
	}
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(raw), &doc); err != nil {
		return nil, fmt.Errorf("解析 Mihomo YAML 失败：%w", err)
	}
	if len(doc) == 0 {
		return nil, errors.New("Mihomo YAML 节点为空")
	}
	if proxiesVal, ok := doc["proxies"]; ok {
		items, ok := proxiesVal.([]any)
		if !ok || len(items) == 0 {
			return nil, errors.New("Mihomo YAML 中的 proxies 列表为空")
		}
		first, ok := items[0].(map[string]any)
		if !ok {
			return nil, errors.New("Mihomo YAML 中的 proxies 首项格式非法")
		}
		return first, nil
	}
	return doc, nil
}

func validateAndNormalizeMihomoNode(in map[string]any) (map[string]any, string, error) {
	if len(in) == 0 {
		return nil, "", errors.New("Mihomo 节点配置为空")
	}
	node := make(map[string]any, len(in)+1)
	for k, v := range in {
		node[k] = v
	}
	typ := strings.ToLower(strings.TrimSpace(asString(node["type"])))
	if typ == "hy2" {
		typ = "hysteria2"
	}
	if !allowedMihomoProxyTypes[typ] {
		return nil, "", fmt.Errorf("不支持的 Mihomo 节点协议类型 %q", typ)
	}
	node["type"] = typ

	server := strings.TrimSpace(asString(node["server"]))
	if server == "" || strings.ContainsAny(server, " \t\r\n/@") {
		return nil, "", errors.New("Mihomo 节点缺少合法的 server 地址")
	}
	node["server"] = server

	port, ok := asPort(node["port"])
	if !ok {
		return nil, "", errors.New("Mihomo 节点缺少合法的 port（须为 1-65535）")
	}
	node["port"] = port

	// 强制改写节点名为固定受控值，避免任意字符进入代理组引用。
	node["name"] = "upstream-node"

	// 生成脱敏预览：仅包含协议、服务器、端口与非敏感传输/安全特征，绝不回显密码/UUID/密钥。
	preview := typ + "://" + net.JoinHostPort(server, strconv.Itoa(port))
	var tags []string
	if netType := strings.TrimSpace(asString(node["network"])); netType != "" && netType != "tcp" {
		tags = append(tags, netType)
	}
	if _, hasReality := node["reality-opts"]; hasReality {
		tags = append(tags, "reality")
	} else if tlsEnabled, ok := node["tls"].(bool); ok && tlsEnabled {
		tags = append(tags, "tls")
	}
	if len(tags) > 0 {
		preview += " (" + strings.Join(tags, "+") + ")"
	}
	return node, preview, nil
}

// RenderMihomoRuntimeYAML 生成最小化、沙箱化的 Mihomo 运行配置：
// 仅监听 127.0.0.1:<listenPort>、关闭 allow-lan、不暴露 external-controller/tun，
// 并绑定随机鉴权凭据，防止宿主机上其它非授权进程借用该本地代理端口。
func RenderMihomoRuntimeYAML(node map[string]any, listenPort int, authUser, authPass string) ([]byte, error) {
	if listenPort < 1 || listenPort > 65535 {
		return nil, errors.New("监听端口非法")
	}
	normalized, _, err := validateAndNormalizeMihomoNode(node)
	if err != nil {
		return nil, err
	}
	cfg := map[string]any{
		"mixed-port":   listenPort,
		"bind-address": "127.0.0.1",
		"allow-lan":    false,
		"mode":         "global",
		"log-level":    "warning",
		"ipv6":         false,
		"authentication": []string{
			authUser + ":" + authPass,
		},
		"proxies": []map[string]any{normalized},
		"proxy-groups": []map[string]any{
			{
				"name":    "GLOBAL",
				"type":    "select",
				"proxies": []string{"upstream-node"},
			},
		},
	}
	return yaml.Marshal(cfg)
}

func parseMihomoShareURI(raw string) (map[string]any, error) {
	lower := strings.ToLower(raw)
	switch {
	case strings.HasPrefix(lower, "vless://"):
		return parseVLESSURI(raw)
	case strings.HasPrefix(lower, "vmess://"):
		return parseVMessURI(raw)
	case strings.HasPrefix(lower, "trojan://"):
		return parseTrojanURI(raw)
	case strings.HasPrefix(lower, "ss://"):
		return parseShadowsocksURI(raw)
	case strings.HasPrefix(lower, "hysteria2://"), strings.HasPrefix(lower, "hy2://"):
		return parseHysteria2URI(raw)
	case strings.HasPrefix(lower, "tuic://"):
		return parseTUICURI(raw)
	case strings.HasPrefix(lower, "anytls://"):
		return parseAnyTLSURI(raw)
	case strings.HasPrefix(lower, "socks5://"), strings.HasPrefix(lower, "socks5h://"):
		return parseBasicProxyURI("socks5", raw)
	case strings.HasPrefix(lower, "http://"), strings.HasPrefix(lower, "https://"):
		return parseBasicProxyURI("http", raw)
	default:
		return nil, errors.New("不支持的分享链接协议")
	}
}

func parseVLESSURI(raw string) (map[string]any, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User == nil || u.User.Username() == "" {
		return nil, errors.New("VLESS 链接格式非法")
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 {
		return nil, errors.New("VLESS 端口非法")
	}
	q := u.Query()
	node := map[string]any{
		"type":   "vless",
		"server": u.Hostname(),
		"port":   port,
		"uuid":   u.User.Username(),
		"udp":    true,
	}
	if flow := strings.TrimSpace(q.Get("flow")); flow != "" {
		node["flow"] = flow
	}
	if enc := strings.TrimSpace(q.Get("encryption")); enc != "" && enc != "none" {
		node["encryption"] = enc
	}
	security := strings.ToLower(strings.TrimSpace(q.Get("security")))
	switch security {
	case "tls":
		node["tls"] = true
	case "reality":
		node["tls"] = true
		reality := map[string]any{}
		if pbk := strings.TrimSpace(q.Get("pbk")); pbk != "" {
			reality["public-key"] = pbk
		}
		if sid := strings.TrimSpace(q.Get("sid")); sid != "" {
			reality["short-id"] = sid
		}
		if len(reality) > 0 {
			node["reality-opts"] = reality
		}
	}
	if sni := firstNonEmpty(q.Get("sni"), q.Get("servername")); sni != "" {
		node["servername"] = sni
	}
	if fp := strings.TrimSpace(q.Get("fp")); fp != "" {
		node["client-fingerprint"] = fp
	}
	if alpn := splitCSV(q.Get("alpn")); len(alpn) > 0 {
		node["alpn"] = alpn
	}
	if isTruthy(q.Get("allowInsecure")) || isTruthy(q.Get("insecure")) {
		node["skip-cert-verify"] = true
	}
	applyURITransport(node, q)
	return node, nil
}

func parseVMessURI(raw string) (map[string]any, error) {
	encoded := strings.TrimSpace(raw[len("vmess://"):])
	if idx := strings.IndexByte(encoded, '#'); idx >= 0 {
		encoded = encoded[:idx]
	}
	decoded, err := decodeFlexibleBase64(encoded)
	if err != nil {
		return nil, errors.New("VMess 链接 Base64 解码失败")
	}
	var payload map[string]any
	if err := json.Unmarshal(decoded, &payload); err != nil {
		return nil, errors.New("VMess 链接 JSON 格式非法")
	}
	server := strings.TrimSpace(asString(payload["add"]))
	port, ok := asPort(payload["port"])
	uuid := strings.TrimSpace(asString(payload["id"]))
	if server == "" || !ok || uuid == "" {
		return nil, errors.New("VMess 链接缺少服务器、端口或 UUID")
	}
	aid, _ := asInt(payload["aid"])
	cipher := strings.TrimSpace(asString(payload["scy"]))
	if cipher == "" {
		cipher = "auto"
	}
	node := map[string]any{
		"type":    "vmess",
		"server":  server,
		"port":    port,
		"uuid":    uuid,
		"alterId": aid,
		"cipher":  cipher,
		"udp":     true,
	}
	if strings.EqualFold(strings.TrimSpace(asString(payload["tls"])), "tls") {
		node["tls"] = true
	}
	if sni := firstNonEmpty(asString(payload["sni"]), asString(payload["host"])); sni != "" {
		node["servername"] = sni
	}
	if fp := strings.TrimSpace(asString(payload["fp"])); fp != "" {
		node["client-fingerprint"] = fp
	}
	if alpn := splitCSV(asString(payload["alpn"])); len(alpn) > 0 {
		node["alpn"] = alpn
	}
	if isTruthy(asString(payload["skip-cert-verify"])) {
		node["skip-cert-verify"] = true
	}
	netType := strings.ToLower(strings.TrimSpace(asString(payload["net"])))
	host := strings.TrimSpace(asString(payload["host"]))
	path := strings.TrimSpace(asString(payload["path"]))
	switch netType {
	case "ws":
		node["network"] = "ws"
		wsOpts := map[string]any{}
		if path != "" {
			wsOpts["path"] = path
		}
		if host != "" {
			wsOpts["headers"] = map[string]string{"Host": host}
		}
		if len(wsOpts) > 0 {
			node["ws-opts"] = wsOpts
		}
	case "grpc":
		node["network"] = "grpc"
		if path != "" {
			node["grpc-opts"] = map[string]any{"grpc-service-name": path}
		}
	case "h2":
		node["network"] = "h2"
		h2Opts := map[string]any{}
		if path != "" {
			h2Opts["path"] = path
		}
		if host != "" {
			h2Opts["host"] = []string{host}
		}
		if len(h2Opts) > 0 {
			node["h2-opts"] = h2Opts
		}
	}
	return node, nil
}

func parseTrojanURI(raw string) (map[string]any, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User == nil || u.User.Username() == "" {
		return nil, errors.New("Trojan 链接格式非法")
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 {
		return nil, errors.New("Trojan 端口非法")
	}
	q := u.Query()
	node := map[string]any{
		"type":     "trojan",
		"server":   u.Hostname(),
		"port":     port,
		"password": u.User.Username(),
		"udp":      true,
	}
	if sni := firstNonEmpty(q.Get("sni"), q.Get("peer")); sni != "" {
		node["sni"] = sni
	}
	if fp := strings.TrimSpace(q.Get("fp")); fp != "" {
		node["client-fingerprint"] = fp
	}
	if alpn := splitCSV(q.Get("alpn")); len(alpn) > 0 {
		node["alpn"] = alpn
	}
	if isTruthy(q.Get("allowInsecure")) || isTruthy(q.Get("insecure")) {
		node["skip-cert-verify"] = true
	}
	applyURITransport(node, q)
	return node, nil
}

func parseShadowsocksURI(raw string) (map[string]any, error) {
	body := raw[len("ss://"):]
	if idx := strings.IndexByte(body, '#'); idx >= 0 {
		body = body[:idx]
	}
	var query url.Values
	if idx := strings.IndexByte(body, '?'); idx >= 0 {
		q, err := url.ParseQuery(body[idx+1:])
		if err != nil {
			return nil, errors.New("Shadowsocks 链接查询参数非法")
		}
		query = q
		body = body[:idx]
	}
	var method, password, server string
	var port int
	if at := strings.LastIndexByte(body, '@'); at >= 0 {
		userPart := body[:at]
		hostPort := body[at+1:]
		if unesc, err := url.PathUnescape(userPart); err == nil {
			userPart = unesc
		}
		if decoded, err := decodeFlexibleBase64(userPart); err == nil && strings.Contains(string(decoded), ":") {
			userPart = string(decoded)
		}
		m, p, ok := strings.Cut(userPart, ":")
		if !ok || m == "" || p == "" {
			return nil, errors.New("Shadowsocks 链接加密方式或密码缺失")
		}
		method, password = m, p
		h, portStr, err := net.SplitHostPort(hostPort)
		if err != nil {
			return nil, errors.New("Shadowsocks 服务器地址格式非法")
		}
		pNum, err := strconv.Atoi(portStr)
		if err != nil || pNum < 1 || pNum > 65535 {
			return nil, errors.New("Shadowsocks 端口非法")
		}
		server, port = h, pNum
	} else {
		decoded, err := decodeFlexibleBase64(body)
		if err != nil {
			return nil, errors.New("Shadowsocks 链接解码失败")
		}
		full := string(decoded)
		at = strings.LastIndexByte(full, '@')
		if at < 0 {
			return nil, errors.New("Shadowsocks 链接格式非法")
		}
		m, p, ok := strings.Cut(full[:at], ":")
		if !ok || m == "" || p == "" {
			return nil, errors.New("Shadowsocks 链接加密方式或密码缺失")
		}
		h, portStr, err := net.SplitHostPort(full[at+1:])
		if err != nil {
			return nil, errors.New("Shadowsocks 服务器地址格式非法")
		}
		pNum, err := strconv.Atoi(portStr)
		if err != nil || pNum < 1 || pNum > 65535 {
			return nil, errors.New("Shadowsocks 端口非法")
		}
		method, password, server, port = m, p, h, pNum
	}
	node := map[string]any{
		"type":     "ss",
		"server":   server,
		"port":     port,
		"cipher":   method,
		"password": password,
		"udp":      true,
	}
	if pluginRaw := strings.TrimSpace(query.Get("plugin")); pluginRaw != "" {
		parts := strings.Split(pluginRaw, ";")
		pluginName := strings.TrimSpace(parts[0])
		if pluginName == "simple-obfs" || pluginName == "obfs-local" {
			pluginName = "obfs"
		}
		if pluginName != "" {
			node["plugin"] = pluginName
			opts := map[string]any{}
			for _, kv := range parts[1:] {
				k, v, ok := strings.Cut(kv, "=")
				if !ok {
					if strings.TrimSpace(kv) == "tls" {
						opts["tls"] = true
					}
					continue
				}
				switch k {
				case "obfs":
					opts["mode"] = v
				case "obfs-host":
					opts["host"] = v
				default:
					opts[k] = v
				}
			}
			if len(opts) > 0 {
				node["plugin-opts"] = opts
			}
		}
	}
	return node, nil
}

func parseHysteria2URI(raw string) (map[string]any, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User == nil {
		return nil, errors.New("Hysteria2 链接格式非法")
	}
	password := u.User.Username()
	if pwd, hasPwd := u.User.Password(); hasPwd {
		password = password + ":" + pwd
	}
	if password == "" {
		return nil, errors.New("Hysteria2 缺少认证凭据")
	}
	port := 443
	if u.Port() != "" {
		p, convErr := strconv.Atoi(u.Port())
		if convErr != nil || p < 1 || p > 65535 {
			return nil, errors.New("Hysteria2 端口非法")
		}
		port = p
	}
	q := u.Query()
	node := map[string]any{
		"type":     "hysteria2",
		"server":   u.Hostname(),
		"port":     port,
		"password": password,
	}
	if sni := strings.TrimSpace(q.Get("sni")); sni != "" {
		node["sni"] = sni
	}
	if obfs := strings.TrimSpace(q.Get("obfs")); obfs != "" {
		node["obfs"] = obfs
		if obfsPwd := strings.TrimSpace(q.Get("obfs-password")); obfsPwd != "" {
			node["obfs-password"] = obfsPwd
		}
	}
	if pin := strings.TrimSpace(q.Get("pinSHA256")); pin != "" {
		node["fingerprint"] = pin
	}
	if alpn := splitCSV(q.Get("alpn")); len(alpn) > 0 {
		node["alpn"] = alpn
	}
	if isTruthy(q.Get("insecure")) || isTruthy(q.Get("allowInsecure")) {
		node["skip-cert-verify"] = true
	}
	return node, nil
}

func parseTUICURI(raw string) (map[string]any, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User == nil {
		return nil, errors.New("TUIC 链接格式非法")
	}
	uuid := u.User.Username()
	password, _ := u.User.Password()
	if uuid == "" {
		return nil, errors.New("TUIC 缺少 UUID")
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 {
		return nil, errors.New("TUIC 端口非法")
	}
	q := u.Query()
	node := map[string]any{
		"type":     "tuic",
		"server":   u.Hostname(),
		"port":     port,
		"uuid":     uuid,
		"password": password,
	}
	if sni := strings.TrimSpace(q.Get("sni")); sni != "" {
		node["sni"] = sni
	}
	if alpn := splitCSV(q.Get("alpn")); len(alpn) > 0 {
		node["alpn"] = alpn
	}
	if cc := strings.TrimSpace(q.Get("congestion_control")); cc != "" {
		node["congestion-controller"] = cc
	}
	if mode := strings.TrimSpace(q.Get("udp_relay_mode")); mode != "" {
		node["udp-relay-mode"] = mode
	}
	if isTruthy(q.Get("allow_insecure")) || isTruthy(q.Get("insecure")) {
		node["skip-cert-verify"] = true
	}
	return node, nil
}

func parseAnyTLSURI(raw string) (map[string]any, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User == nil || u.User.Username() == "" {
		return nil, errors.New("AnyTLS 链接格式非法")
	}
	port := 443
	if u.Port() != "" {
		p, convErr := strconv.Atoi(u.Port())
		if convErr != nil || p < 1 || p > 65535 {
			return nil, errors.New("AnyTLS 端口非法")
		}
		port = p
	}
	q := u.Query()
	node := map[string]any{
		"type":     "anytls",
		"server":   u.Hostname(),
		"port":     port,
		"password": u.User.Username(),
		"udp":      true,
	}
	if sni := strings.TrimSpace(q.Get("sni")); sni != "" {
		node["sni"] = sni
	}
	if fp := strings.TrimSpace(q.Get("fp")); fp != "" {
		node["client-fingerprint"] = fp
	}
	if alpn := splitCSV(q.Get("alpn")); len(alpn) > 0 {
		node["alpn"] = alpn
	}
	if isTruthy(q.Get("insecure")) || isTruthy(q.Get("allowInsecure")) {
		node["skip-cert-verify"] = true
	}
	return node, nil
}

func parseBasicProxyURI(kind, raw string) (map[string]any, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return nil, errors.New("代理链接格式非法")
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 {
		if kind == "http" && strings.EqualFold(u.Scheme, "https") {
			port = 443
		} else if kind == "http" {
			port = 80
		} else {
			return nil, errors.New("代理端口非法")
		}
	}
	node := map[string]any{
		"type":   kind,
		"server": u.Hostname(),
		"port":   port,
	}
	if kind == "http" && strings.EqualFold(u.Scheme, "https") {
		node["tls"] = true
	}
	if u.User != nil {
		if user := u.User.Username(); user != "" {
			node["username"] = user
		}
		if pass, ok := u.User.Password(); ok && pass != "" {
			node["password"] = pass
		}
	}
	return node, nil
}

func applyURITransport(node map[string]any, q url.Values) {
	netType := strings.ToLower(strings.TrimSpace(q.Get("type")))
	switch netType {
	case "ws":
		node["network"] = "ws"
		wsOpts := map[string]any{}
		if p := strings.TrimSpace(q.Get("path")); p != "" {
			wsOpts["path"] = p
		}
		if host := strings.TrimSpace(q.Get("host")); host != "" {
			wsOpts["headers"] = map[string]string{"Host": host}
		}
		if len(wsOpts) > 0 {
			node["ws-opts"] = wsOpts
		}
	case "grpc":
		node["network"] = "grpc"
		if svc := strings.TrimSpace(q.Get("serviceName")); svc != "" {
			node["grpc-opts"] = map[string]any{"grpc-service-name": svc}
		}
	case "h2", "http":
		node["network"] = "h2"
		h2Opts := map[string]any{}
		if p := strings.TrimSpace(q.Get("path")); p != "" {
			h2Opts["path"] = p
		}
		if host := strings.TrimSpace(q.Get("host")); host != "" {
			h2Opts["host"] = []string{host}
		}
		if len(h2Opts) > 0 {
			node["h2-opts"] = h2Opts
		}
	}
}

func decodeFlexibleBase64(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding,
		base64.RawStdEncoding,
		base64.URLEncoding,
		base64.RawURLEncoding,
	} {
		if b, err := enc.DecodeString(s); err == nil {
			return b, nil
		}
	}
	return nil, errors.New("base64 decode failed")
}

func asString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case fmt.Stringer:
		return t.String()
	default:
		return ""
	}
}

func asInt(v any) (int, bool) {
	switch t := v.(type) {
	case int:
		return t, true
	case int64:
		return int(t), true
	case uint64:
		return int(t), true
	case float64:
		return int(t), true
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(t))
		return n, err == nil
	default:
		return 0, false
	}
}

func asPort(v any) (int, bool) {
	p, ok := asInt(v)
	return p, ok && p >= 1 && p <= 65535
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if trimmed := strings.TrimSpace(v); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func splitCSV(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func isTruthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}
