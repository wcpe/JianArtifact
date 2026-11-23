package domain

import (
	"context"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
)

// credentialProofKey 把入口已验证的凭据证明带给最终提交事务。
type credentialProofKey struct{}

// WithCredentialProof 把已验证凭据引用随请求上下文传入写路径；
// 最终事务用它在同一事务内重查账号/会话/凭据当前状态（FR-166 §3.3）。
func WithCredentialProof(ctx context.Context, proof auth.CredentialProof) context.Context {
	return context.WithValue(ctx, credentialProofKey{}, proof)
}

func credentialProofFrom(ctx context.Context) (auth.CredentialProof, bool) {
	proof, ok := ctx.Value(credentialProofKey{}).(auth.CredentialProof)
	return proof, ok
}
