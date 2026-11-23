package auth

import "errors"

var (
	// ErrExternalAuthUnavailable 表示已证实的外部认证基础设施故障。
	ErrExternalAuthUnavailable = errors.New("外部认证服务不可用")
	// ErrExternalIdentityRejected 表示已确定不满足认证约束。
	ErrExternalIdentityRejected = errors.New("外部身份核验被拒绝")
	// errExternalAuthIndeterminate 表示核验未能完成且原因不可判定。
	errExternalAuthIndeterminate = errors.New("外部身份核验原因未知")
)

// ProviderAuthOutcome 是认证失败后的审计归因，不用于放行认证。
type ProviderAuthOutcome uint8

const (
	ProviderAuthUnknown ProviderAuthOutcome = iota
	ProviderAuthDenied
	ProviderAuthFailed
)

// ClassifyProviderAuthFailure 只使用类型信息；无法归因时不把失败归咎于用户。
func ClassifyProviderAuthFailure(err error) ProviderAuthOutcome {
	switch {
	case errors.Is(err, ErrExternalAuthUnavailable):
		return ProviderAuthFailed
	case errors.Is(err, errExternalAuthIndeterminate):
		return ProviderAuthUnknown
	case errors.Is(err, ErrExternalIdentityRejected), errors.Is(err, ErrLDAPAuthFailed):
		return ProviderAuthDenied
	default:
		return ProviderAuthUnknown
	}
}
