package auditctx

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	forwardedForHeader   = "X-Forwarded-For"
	forwardedProtoHeader = "X-Forwarded-Proto"
	maxForwardedBytes    = 1024
	maxForwardedHops     = 8
	maxUserAgentBytes    = 256
	policyVersionDomain  = "xff-xfp-v1"
)

// Policy 是不可变的代理信任配置；零值不信任任何代理。
type Policy struct {
	trusted []netip.Prefix
	version string
}

// NewPolicy 解析逗号分隔的可信 CIDR；空值关闭代理信任，无效条目不是空配置。
func NewPolicy(raw string) (Policy, error) {
	if strings.TrimSpace(raw) == "" {
		return Policy{}, nil
	}
	var prefixes []netip.Prefix
	for index, item := range strings.Split(raw, ",") {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(item))
		if err != nil {
			return Policy{}, fmt.Errorf("第 %d 项不是有效的 IP CIDR", index+1)
		}
		if prefix.Addr().Is4In6() {
			if prefix.Bits() < 96 {
				return Policy{}, fmt.Errorf("第 %d 项的 IPv4 映射 CIDR 超出映射地址范围", index+1)
			}
			prefix = netip.PrefixFrom(prefix.Addr().Unmap(), prefix.Bits()-96)
		}
		prefixes = append(prefixes, prefix.Masked())
	}
	prefixes = canonicalPrefixes(prefixes)
	var versionInput strings.Builder
	versionInput.WriteString(policyVersionDomain)
	for _, prefix := range prefixes {
		versionInput.WriteByte('\n')
		versionInput.WriteString(prefix.String())
	}
	return Policy{trusted: prefixes, version: policyVersion(versionInput.String())}, nil
}

func canonicalPrefixes(prefixes []netip.Prefix) []netip.Prefix {
	slices.SortFunc(prefixes, func(a, b netip.Prefix) int {
		if order := a.Addr().Compare(b.Addr()); order != 0 {
			return order
		}
		return a.Bits() - b.Bits()
	})
	// 已保留的网段彼此不相交；能覆盖当前前缀的网段只可能紧邻它。
	canonical := prefixes[:0]
	for _, prefix := range prefixes {
		if len(canonical) == 0 || !canonical[len(canonical)-1].Contains(prefix.Addr()) {
			canonical = append(canonical, prefix)
		}
	}
	return canonical
}

func policyVersion(canonical string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(canonical)))
}

// Version 返回由固定头约定和规范可信网段生成的版本，不含任何请求输入。
func (p Policy) Version() string {
	if p.version == "" {
		return policyVersion(policyVersionDomain)
	}
	return p.version
}

// Resolve 从服务端连接和获准转发尾段采集来源，不修改 HTTP 请求或其正文。
func (p Policy) Resolve(r *http.Request, requestID string) Source {
	source := Source{
		RequestID: requestID, PeerIP: parseSourceAddr(r.RemoteAddr), ProxyTrust: ProxyTrustUnknown,
		UserAgent: safeUserAgent(r.UserAgent()), SourceTrustVersion: p.Version(),
	}
	if r.TLS != nil {
		source.TrustedScheme = "https"
	}
	if !source.PeerIP.IsValid() {
		return source
	}
	if !p.trusts(source.PeerIP) {
		source.ClientIP = source.PeerIP
		source.ProxyTrust = ProxyTrustDirect
		if source.TrustedScheme == "" {
			source.TrustedScheme = "http"
		}
		return source
	}

	chain, scheme, ok := p.forwarding(r.Header)
	if !ok {
		source.ProxyTrust = ProxyTrustInvalidForwarding
		return source
	}
	source.ClientIP = chain[0]
	source.ProxyTrust = ProxyTrustTrusted
	source.ForwardedChain = chain
	if source.TrustedScheme == "" {
		source.TrustedScheme = scheme
	}
	return source
}

func (p Policy) forwarding(header http.Header) ([]netip.Addr, string, bool) {
	forwardedFor := header.Values(forwardedForHeader)
	forwardedProto := header.Values(forwardedProtoHeader)
	if len(forwardedFor) != 1 || len(forwardedProto) > 1 || len(forwardedFor[0]) > maxForwardedBytes {
		return nil, "", false
	}
	var scheme string
	if len(forwardedProto) == 1 {
		if len(forwardedProto[0]) > maxForwardedBytes-len(forwardedFor[0]) {
			return nil, "", false
		}
		scheme = strings.ToLower(strings.Trim(forwardedProto[0], " \t"))
		if scheme != "http" && scheme != "https" {
			return nil, "", false
		}
	}
	parts := strings.SplitN(forwardedFor[0], ",", maxForwardedHops+1)
	if len(parts) > maxForwardedHops {
		return nil, "", false
	}
	var chain [maxForwardedHops]netip.Addr
	for i, part := range parts {
		chain[i] = parseSourceAddr(part)
		if !chain[i].IsValid() {
			return nil, "", false
		}
	}
	for i := len(parts) - 1; i >= 0; i-- {
		if !p.trusts(chain[i]) {
			// 新切片只含获准尾段，左侧原始声明不进入上下文。
			return slices.Clone(chain[i:len(parts)]), scheme, true
		}
	}
	return nil, "", false
}

func (p Policy) trusts(addr netip.Addr) bool {
	for _, net := range p.trusted {
		if net.Contains(addr) {
			return true
		}
	}
	return false
}

func parseSourceAddr(raw string) netip.Addr {
	raw = strings.Trim(raw, " \t")
	addr, err := netip.ParseAddr(raw)
	if err != nil {
		addrPort, err := netip.ParseAddrPort(raw)
		if err != nil {
			return netip.Addr{}
		}
		addr = addrPort.Addr()
	}
	if addr.Zone() != "" {
		return netip.Addr{}
	}
	return addr.Unmap()
}

func safeUserAgent(raw string) string {
	var value strings.Builder
	raw = raw[:min(len(raw), maxUserAgentBytes)]
	for len(raw) > 0 {
		r, n := utf8.DecodeRuneInString(raw)
		if !(r == utf8.RuneError && n == 1) && !unicode.IsControl(r) {
			value.WriteString(raw[:n])
		}
		raw = raw[n:]
	}
	return value.String()
}
