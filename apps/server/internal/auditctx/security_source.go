package auditctx

import (
	"context"
	"net/netip"
	"slices"
)

// ProxyTrust 标记客户端地址依据，不能通过连接对端补齐无效的转发来源。
type ProxyTrust string

const (
	ProxyTrustDirect            ProxyTrust = "direct"
	ProxyTrustTrusted           ProxyTrust = "trusted"
	ProxyTrustInvalidForwarding ProxyTrust = "invalid_forwarding"
	ProxyTrustUnknown           ProxyTrust = "unknown"
)

// Source 是服务端采集的请求来源；无效的地址或空协议表示未判定。
type Source struct {
	RequestID          string
	OriginRequestID    string
	PeerIP             netip.Addr
	ClientIP           netip.Addr
	ProxyTrust         ProxyTrust
	ForwardedChain     []netip.Addr // 从客户端到最近的转发节点，不含连接对端。
	UserAgent          string       // 有界的客户端自述，不参与信任裁决。
	SourceTrustVersion string
	TrustedScheme      string
}

// SourceSnapshot 是请求发起时的来源快照，不含后台执行者或执行时的连接事实。
type SourceSnapshot struct {
	OriginRequestID    string
	ClientIP           netip.Addr
	ProxyTrust         ProxyTrust
	SourceTrustVersion string
}

// Valid 校验快照结构；最终裁决仍须按当前信任版本及授权校验。
func (s SourceSnapshot) Valid() bool {
	return s.OriginRequestID != "" && s.SourceTrustVersion != "" &&
		s.ClientIP.IsValid() && s.ClientIP.Zone() == "" && !s.ClientIP.Is4In6() &&
		(s.ProxyTrust == ProxyTrustDirect || s.ProxyTrust == ProxyTrustTrusted)
}

// Snapshot 仅从真实 HTTP 请求的可信来源生成发起快照。
func (s Source) Snapshot() (SourceSnapshot, bool) {
	snapshot := SourceSnapshot{
		OriginRequestID: s.RequestID, ClientIP: s.ClientIP,
		ProxyTrust: s.ProxyTrust, SourceTrustVersion: s.SourceTrustVersion,
	}
	if !snapshot.Valid() {
		return SourceSnapshot{}, false
	}
	return snapshot, true
}

type sourceContextKey struct{}

// WithSource 把服务端已采集的来源写入标准上下文，不保留可变的链地址切片。
func WithSource(ctx context.Context, source Source) context.Context {
	source.ForwardedChain = slices.Clone(source.ForwardedChain)
	return context.WithValue(ctx, sourceContextKey{}, source)
}

// SourceFrom 读取来源副本；不存在时返回未知来源而非猜测客户端地址。
func SourceFrom(ctx context.Context) (Source, bool) {
	source, ok := ctx.Value(sourceContextKey{}).(Source)
	if !ok {
		return Source{ProxyTrust: ProxyTrustUnknown}, false
	}
	source.ForwardedChain = slices.Clone(source.ForwardedChain)
	return source, true
}
