package auditctx

import (
	"context"
	"net/http"
)

// ClientIP 返回可归因的客户端地址。来源未知或转发链无效时返回空值，不使用连接对端补齐。
func ClientIP(ctx context.Context) string {
	source, ok := SourceFrom(ctx)
	if !ok || (source.ProxyTrust != ProxyTrustDirect && source.ProxyTrust != ProxyTrustTrusted) ||
		!source.ClientIP.IsValid() || source.ClientIP.Zone() != "" || source.ClientIP.Is4In6() {
		return ""
	}
	return source.ClientIP.String()
}

// RequestOrigin 以请求 Host 生成回退基址。协议只来自可信来源或实际传输，不直接读取转发头。
func RequestOrigin(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	} else if source, ok := SourceFrom(r.Context()); ok && source.TrustedScheme == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}
