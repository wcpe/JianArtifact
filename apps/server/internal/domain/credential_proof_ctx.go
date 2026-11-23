package domain

import (
	"context"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
)

// credentialProofKey 把入口已验证的凭据证明带给最终提交事务。
type credentialProofKey struct{}

// WithCredentialProof 把已验证凭据引用随请求上下文传入写路径；
// 最终事务用它在同一事务内重查账号/会话/凭据当前状态。
func WithCredentialProof(ctx context.Context, proof auth.CredentialProof) context.Context {
	return context.WithValue(ctx, credentialProofKey{}, proof)
}

func credentialProofFrom(ctx context.Context) (auth.CredentialProof, bool) {
	proof, ok := ctx.Value(credentialProofKey{}).(auth.CredentialProof)
	return proof, ok
}

// publishPrincipalKey 把入口鉴权时的主体快照带给最终提交事务。
type publishPrincipalKey struct{}

// WithPublishPrincipal 随请求上下文携带已鉴权主体（含角色与机器凭据作用域）；
// 最终事务用它复核「排队时的授权到提交时仍成立」，覆盖凭据状态之外的 ACL 收窄。
func WithPublishPrincipal(ctx context.Context, p *auth.Principal) context.Context {
	if p == nil {
		return ctx
	}
	snapshot := *p
	snapshot.TokenScopes = append([]auth.MachineTokenScope(nil), p.TokenScopes...)
	return context.WithValue(ctx, publishPrincipalKey{}, &snapshot)
}

func publishPrincipalFrom(ctx context.Context) (*auth.Principal, bool) {
	p, ok := ctx.Value(publishPrincipalKey{}).(*auth.Principal)
	return p, ok && p != nil
}
