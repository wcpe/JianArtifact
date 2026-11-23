package repository

import (
	"crypto/rand"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	securityEventMaxObjects = 256
	securityEventMaxBytes   = 256 * 1024
	securityEventMaxHeader  = 4 * 1024
	securityEventMaxUA      = 256
	securityEventMaxName    = 128
)

var (
	// ErrInvalidSecurityEvent 表示事件违反稳定词表、字段或事务预算。
	ErrInvalidSecurityEvent = errors.New("安全事件字段无效")
	// ErrSecurityEventConflict 表示事件标识或业务成功事实重复；应重查原回执。
	ErrSecurityEventConflict = errors.New("安全事件与已存在事实冲突")
	// ErrSecurityEventUnavailable 不向调用方传播数据库或输入原文。
	ErrSecurityEventUnavailable = errors.New("安全事件存储暂不可用")
	// ErrSecurityEventNotFound 表示找不到相应事件。
	ErrSecurityEventNotFound = errors.New("安全事件不存在")
	// ErrInvalidSecurityEventFilter 表示过滤器、快照锚点或分页边界无效。
	ErrInvalidSecurityEventFilter = errors.New("安全事件查询条件无效")
)

// SecurityEvent 是服务端合成的安全事实。关联 ID 只能使用各自域内已验证的标识，
// 不能使用请求输入、登录令牌、令牌摘要或挑战。发生与入库时间独立于业务回执时间。
// 序号与入库时间由仓储产生；零值事件版本表示首版。
type SecurityEvent struct {
	Sequence           int64
	EventID            string
	EventVersion       int
	EventType          string
	OccurredAt         time.Time
	IngestedAt         time.Time
	RequestID          string
	OriginRequestID    string
	OperationID        string
	TaskID             string
	ActionID           string
	ActorKind          string
	ActorUserID        *int64
	ActorNameSnapshot  string
	CredentialID       string
	SessionID          string
	AuthMethod         string
	AuthStrength       string
	PeerIP             netip.Addr
	ClientIP           netip.Addr
	ProxyTrust         string
	SourceTrustVersion string
	ForwardedChain     []netip.Addr
	UserAgent          string
	RepositoryID       *int64
	Outcome            string
	Committed          string
	HTTPStatus         *int
	ErrorCode          string
}

// SecurityEventObject 保存稳定对象键与最小显示快照。写入时序号按输入顺序从一开始。
type SecurityEventObject struct {
	Ordinal                int    `db:"ordinal"`
	RepositoryID           *int64 `db:"repository_id"`
	CanonicalPath          string `db:"canonical_path"`
	ObjectKind             string `db:"object_kind"`
	ObjectID               string `db:"object_id"`
	Digest                 string `db:"digest"`
	SizeBytes              *int64 `db:"size_bytes"`
	ManifestDigest         string `db:"manifest_digest"`
	TargetVersion          string `db:"target_version"`
	RepositoryNameSnapshot string `db:"repository_name_snapshot"`
}

// NewSecurityEventID 生成服务端随机事件标识，需关联回执时可在事务外调用。
func NewSecurityEventID() string { return rand.Text() }

// SecurityEventUserAgent 保留合规的可选客户端展示值，不合规则省略而不截断。
func SecurityEventUserAgent(value string) string {
	return optionalSecurityEventText(value, securityEventMaxUA)
}

// SecurityEventActorNameSnapshot 保留可靠主体的合规显示名，不合规则省略。
func SecurityEventActorNameSnapshot(value string) string {
	return optionalSecurityEventText(value, securityEventMaxName)
}

// SecurityEventRepositoryNameSnapshot 保留合规的仓库显示名，不合规则省略。
func SecurityEventRepositoryNameSnapshot(value string) string {
	return optionalSecurityEventText(value, securityEventMaxName)
}

func optionalSecurityEventText(value string, maxBytes int) string {
	if !validSecurityEventText(value, maxBytes) {
		return ""
	}
	return value
}

func validateSecurityEvent(event SecurityEvent, objects []SecurityEventObject) error {
	if event.Sequence != 0 || !event.IngestedAt.IsZero() || !validSecurityEventTime(event.OccurredAt) ||
		(event.EventVersion != 0 && event.EventVersion != 1) || !validSecurityEventType(event.EventType) {
		return invalidSecurityEvent("事件头")
	}
	if !securityEventOneOf(event.ActorKind, "user", "anonymous", "unknown", "system", "local_operator") ||
		(event.ActorKind == "user") != (event.ActorUserID != nil) || !validSecurityEventEntity(event.ActorUserID) ||
		(event.ActorKind != "user" && event.ActorNameSnapshot != "") || !validSecurityEventEntity(event.RepositoryID) {
		return invalidSecurityEvent("主体或仓库")
	}
	if !securityEventOneOf(event.AuthMethod, "", "web_jwt", "bearer_token", "basic_token", "cargo_token", "basic", "password", "oidc_callback", "ldap_bind", "totp", "recovery_code", "local_claim", "unknown") ||
		!securityEventOneOf(event.AuthStrength, "", "none", "password", "mfa", "machine", "recovery") ||
		!validSecurityEventPublicID(event.CredentialID) || !validSecurityEventPublicID(event.SessionID) {
		return invalidSecurityEvent("认证词表或非秘密标识")
	}
	for _, id := range []string{event.EventID, event.RequestID, event.OriginRequestID, event.OperationID, event.TaskID, event.ActionID, event.SourceTrustVersion} {
		if !validSecurityEventRef(id) {
			return invalidSecurityEvent("关联标识")
		}
	}
	if !validSecurityEventText(event.ActorNameSnapshot, securityEventMaxName) || !validSecurityEventText(event.UserAgent, securityEventMaxUA) || !validSecurityEventSource(event) {
		return invalidSecurityEvent("来源或显示快照")
	}
	if !validSecurityEventOutcome(event.Outcome) || !securityEventOneOf(event.Committed, "not_applicable", "not_committed", "committed", "unknown") ||
		(event.HTTPStatus != nil && (*event.HTTPStatus < 100 || *event.HTTPStatus > 599)) || !validSecurityEventErrorCode(event.ErrorCode) {
		return invalidSecurityEvent("结果词表")
	}
	if err := validateSecurityEventFact(event, len(objects)); err != nil {
		return err
	}
	if len(objects) > securityEventMaxObjects {
		return invalidSecurityEvent("对象数量")
	}
	// 以持久字段字节计费；固定部分为序号、可空标量、地址和各字段边界留出保守开销。
	headerBytes := 192 + len(event.ForwardedChain)*17 + len(event.EventID) + len(event.EventType) + len(event.RequestID) + len(event.OriginRequestID) +
		len(event.OperationID) + len(event.TaskID) + len(event.ActionID) + len(event.ActorKind) + len(event.ActorNameSnapshot) +
		len(event.CredentialID) + len(event.SessionID) + len(event.AuthMethod) + len(event.AuthStrength) + len(event.ProxyTrust) +
		len(event.SourceTrustVersion) + len(event.UserAgent) + len(event.Outcome) + len(event.Committed) + len(event.ErrorCode)
	if headerBytes > securityEventMaxHeader {
		return invalidSecurityEvent("事件头预算")
	}
	total := headerBytes
	for _, object := range objects {
		if !validSecurityEventObject(object) {
			return invalidSecurityEvent("对象字段")
		}
		total += 64 + len(object.CanonicalPath) + len(object.ObjectKind) + len(object.ObjectID) + len(object.Digest) +
			len(object.ManifestDigest) + len(object.TargetVersion) + len(object.RepositoryNameSnapshot)
		if total > securityEventMaxBytes {
			return invalidSecurityEvent("事件包预算")
		}
	}
	return nil
}

func validateSecurityEventFact(e SecurityEvent, objectCount int) error {
	if e.Committed == "committed" && (e.OperationID == "" || e.Outcome != "succeeded" || e.HTTPStatus != nil || e.ErrorCode != "") {
		return invalidSecurityEvent("业务提交状态")
	}
	switch e.EventType {
	case "asset.change_committed":
		if e.Committed != "committed" || objectCount == 0 {
			return invalidSecurityEvent("制品提交事实")
		}
	case "auth.session_created":
		if e.Committed != "committed" || e.ActorKind != "user" || objectCount != 0 {
			return invalidSecurityEvent("会话提交事实")
		}
	case "asset.response_observed":
		if e.OperationID == "" || e.Committed == "committed" || objectCount != 0 {
			return invalidSecurityEvent("响应观察事实")
		}
	case "auth.attempt":
		if e.Committed != "not_applicable" || objectCount != 0 {
			return invalidSecurityEvent("认证尝试事实")
		}
	case "protocol.write_denied":
		if e.Outcome != "denied" || e.Committed != "not_committed" {
			return invalidSecurityEvent("协议拒绝事实")
		}
	}
	return nil
}

func validSecurityEventSource(e SecurityEvent) bool {
	if e.PeerIP.Zone() != "" || e.ClientIP.Zone() != "" || len(e.ForwardedChain) > 8 {
		return false
	}
	switch e.ProxyTrust {
	case "unknown", "invalid_forwarding":
		return !e.ClientIP.IsValid() && len(e.ForwardedChain) == 0
	case "direct":
		return e.ClientIP.IsValid() && len(e.ForwardedChain) == 0 &&
			(e.PeerIP.Unmap() == e.ClientIP.Unmap() || (!e.PeerIP.IsValid() && e.OriginRequestID != ""))
	case "trusted":
		if !e.ClientIP.IsValid() || e.SourceTrustVersion == "" {
			return false
		}
		if len(e.ForwardedChain) == 0 {
			return e.OriginRequestID != "" && !e.PeerIP.IsValid()
		}
		for _, ip := range e.ForwardedChain {
			if !ip.IsValid() || ip.Zone() != "" {
				return false
			}
		}
		return e.PeerIP.IsValid() && e.ForwardedChain[0].Unmap() == e.ClientIP.Unmap()
	default:
		return false
	}
}

func validSecurityEventObject(o SecurityEventObject) bool {
	if !validSecurityEventEntity(o.RepositoryID) || (o.SizeBytes != nil && *o.SizeBytes < 0) ||
		!securityEventOneOf(o.ObjectKind, "asset", "repository", "package", "version", "manifest", "blob") ||
		!validSecurityEventRef(o.ObjectID) || !validSecurityEventDigest(o.Digest) || !validSecurityEventDigest(o.ManifestDigest) ||
		!validSecurityEventText(o.TargetVersion, 128) || !validSecurityEventText(o.RepositoryNameSnapshot, securityEventMaxName) {
		return false
	}
	if o.CanonicalPath == "" {
		return o.ObjectKind != "asset"
	}
	return validSecurityEventPath(o.CanonicalPath)
}

func validSecurityEventPath(path string) bool {
	// 路径是资产的权威键；只校验路径及文本形态，不用展示词表改写或拒绝其身份。
	if !validSecurityEventTextShape(path, 1024) || strings.ContainsAny(path, "?#\\") {
		return false
	}
	for segment := range strings.SplitSeq(path, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

func validSecurityEventText(text string, maxBytes int) bool {
	if !validSecurityEventTextShape(text, maxBytes) {
		return false
	}
	// 展示字段不接收裸凭据、原始认证头或常见凭据载体；调用方仍必须只传对应显示事实。
	lower := strings.ToLower(text)
	for _, marker := range []string{"authorization:", "cookie:", "bearer ", "basic ", "jat_", "eyj"} {
		if strings.Contains(lower, marker) {
			return false
		}
	}
	return true
}

func validSecurityEventTextShape(text string, maxBytes int) bool {
	if len(text) > maxBytes || !utf8.ValidString(text) {
		return false
	}
	for _, ch := range text {
		if unicode.IsControl(ch) || unicode.In(ch, unicode.Cf, unicode.Zl, unicode.Zp) {
			return false
		}
	}
	return true
}

func validSecurityEventRef(id string) bool {
	if len(id) > 128 || strings.HasPrefix(strings.ToLower(id), "jat_") {
		return false
	}
	for _, c := range id {
		if !('a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func validSecurityEventPublicID(id string) bool {
	if id == "" {
		return true
	}
	if len(id) == 26 {
		for _, c := range id {
			if !('A' <= c && c <= 'Z' || '2' <= c && c <= '7') {
				return false
			}
		}
		return true
	}
	if len(id) != 36 || id[14] != '4' || !strings.ContainsRune("89ab", rune(id[19])) {
		return false
	}
	for i, c := range id {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
		} else if !securityEventHex(c) {
			return false
		}
	}
	return true
}

func validSecurityEventDigest(digest string) bool {
	if digest == "" {
		return true
	}
	algorithm, value, ok := strings.Cut(digest, ":")
	if !ok || !((algorithm == "sha256" && len(value) == 64) || (algorithm == "sha512" && len(value) == 128)) {
		return false
	}
	for _, c := range value {
		if !securityEventHex(c) {
			return false
		}
	}
	return true
}

func securityEventHex(c rune) bool { return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' }

func validSecurityEventEntity(id *int64) bool { return id == nil || *id > 0 }

func validSecurityEventTime(value time.Time) bool {
	return !value.IsZero() && value.Year() >= 1 && value.Year() <= 9999
}

func validSecurityEventType(value string) bool {
	return securityEventOneOf(value, "auth.attempt", "auth.session_created", "protocol.write_denied", "asset.change_committed", "asset.response_observed")
}

func validSecurityEventOutcome(value string) bool {
	return securityEventOneOf(value, "succeeded", "denied", "failed", "pending", "unknown")
}

func validSecurityEventErrorCode(value string) bool {
	return securityEventOneOf(value, "", "invalid_credentials", "unauthorized", "forbidden", "bad_request", "rate_limited",
		"not_found", "invalid_forwarding", "permission_denied", "quota_exceeded", "size_limit_exceeded",
		"security_audit_unavailable", "security_idempotency_conflict", "internal_error")
}

func securityEventOneOf(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if candidate == value {
			return true
		}
	}
	return false
}

func invalidSecurityEvent(field string) error {
	return fmt.Errorf("%w：%s", ErrInvalidSecurityEvent, field)
}
