package domain

import (
	"context"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/wcpe/jianartifact/apps/server/internal/auditctx"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// SecurityAuditIdentity 只记录调用方已经验证的身份事实，绝不参与授权。
// 凭据和会话标识只接受专门的非秘密关联 ID，不使用令牌、挑战或它们的摘要代替。
type SecurityAuditIdentity struct {
	ActorUserID              int64
	AuthMethod, AuthStrength string
	CredentialID, SessionID  string
}

type securityAuditIdentityKey struct{}

// WithSecurityAuditIdentity 保存已验证主体的可选审计事实，不替代权限、来源或信任代次判断。
func WithSecurityAuditIdentity(ctx context.Context, identity SecurityAuditIdentity) context.Context {
	return context.WithValue(ctx, securityAuditIdentityKey{}, identity)
}

// SecurityAuditDenied 只允许已裁决的拒绝事实。规范路径必须来自服务端对象解析，而不是 URL 或请求体。
// ActorUserID 仅可用于已有真实身份的协议拒绝；认证失败不记录账号 ID。
type SecurityAuditDenied struct {
	EventType, ErrorCode, AuthMethod, CanonicalPath string
	ActorUserID, RepositoryID                       int64
	HTTPStatus                                      int
}

// SecurityAuditResponse 区分观察到的响应与实际业务提交，不证明客户端收到响应。
type SecurityAuditResponse struct {
	OperationID, Outcome, ErrorCode string
	ActorUserID, RepositoryID       int64
	HTTPStatus                      int
}

// RecordDenied 尝试采集已作出的拒绝；没有返回值，采集故障不能改变裁决。
func (s *SecurityAuditService) RecordDenied(ctx context.Context, fact SecurityAuditDenied) {
	e := securityAuditEvent(ctx, fact.ActorUserID)
	e.EventType, e.ErrorCode, e.Outcome = fact.EventType, fact.ErrorCode, "denied"
	e.HTTPStatus, e.RepositoryID = auditOptionalStatus(fact.HTTPStatus), auditOptionalID(fact.RepositoryID)
	observation := repository.SecurityAuditObservation{}
	switch fact.EventType {
	case "auth.attempt":
		e.ActorKind, e.ActorUserID = "unknown", nil
		e.CredentialID, e.SessionID, e.AuthStrength = "", "", ""
		e.AuthMethod, e.Committed = fact.AuthMethod, "not_applicable"
	case "protocol.write_denied":
		e.Committed = "not_committed"
		if fact.CanonicalPath != "" {
			observation.Object = &repository.SecurityEventObject{RepositoryID: e.RepositoryID, ObjectKind: "asset", CanonicalPath: fact.CanonicalPath}
		}
	}
	observation.Event = e
	s.enqueue(observation)
}

// RecordAuthAttemptSucceeded 记录认证核验成功，不声明持久会话已经建立。
func (s *SecurityAuditService) RecordAuthAttemptSucceeded(ctx context.Context, actorUserID int64, authMethod string, httpStatus int) {
	e := securityAuditEvent(ctx, actorUserID)
	e.EventType, e.Outcome, e.Committed = "auth.attempt", "succeeded", "not_applicable"
	e.HTTPStatus = auditOptionalStatus(httpStatus)
	if authMethod != "" {
		e.AuthMethod = authMethod
	}
	s.enqueue(repository.SecurityAuditObservation{Event: e})
}

// RecordAuthAttemptFailed 记录认证服务故障，不将故障当作凭据拒绝或已验证身份。
func (s *SecurityAuditService) RecordAuthAttemptFailed(ctx context.Context, authMethod string, httpStatus int) {
	e := securityAuditEvent(ctx, 0)
	e.EventType, e.Outcome, e.Committed, e.ErrorCode = "auth.attempt", "failed", "not_applicable", "internal_error"
	e.AuthMethod, e.HTTPStatus = authMethod, auditOptionalStatus(httpStatus)
	s.enqueue(repository.SecurityAuditObservation{Event: e})
}

// RecordAuthAttemptUnknown 记录未能判因的认证失败，不归因于凭据或基础设施。
func (s *SecurityAuditService) RecordAuthAttemptUnknown(ctx context.Context, authMethod string, httpStatus int) {
	e := securityAuditEvent(ctx, 0)
	e.EventType, e.Outcome, e.Committed = "auth.attempt", "unknown", "not_applicable"
	e.AuthMethod, e.HTTPStatus = authMethod, auditOptionalStatus(httpStatus)
	s.enqueue(repository.SecurityAuditObservation{Event: e})
}

// RecordResponse 追加非关键响应观察，不重复创建业务成功事实。
func (s *SecurityAuditService) RecordResponse(ctx context.Context, fact SecurityAuditResponse) {
	e := securityAuditEvent(ctx, fact.ActorUserID)
	e.EventType, e.Outcome, e.Committed = "asset.response_observed", fact.Outcome, "not_applicable"
	e.OperationID, e.ErrorCode, e.HTTPStatus = fact.OperationID, fact.ErrorCode, auditOptionalStatus(fact.HTTPStatus)
	e.RepositoryID = auditOptionalID(fact.RepositoryID)
	s.enqueue(repository.SecurityAuditObservation{Event: e})
}

func securityAuditEvent(ctx context.Context, actorUserID int64) repository.SecurityEvent {
	source, _ := auditctx.SourceFrom(ctx)
	e := repository.SecurityEvent{RequestID: source.RequestID, OriginRequestID: source.OriginRequestID,
		PeerIP: source.PeerIP.Unmap(), ProxyTrust: string(auditctx.ProxyTrustUnknown), SourceTrustVersion: source.SourceTrustVersion,
		ActorKind: "unknown", ActorUserID: auditOptionalID(actorUserID)}
	if e.PeerIP.Zone() != "" {
		e.PeerIP = netip.Addr{}
	}
	if source.ProxyTrust == auditctx.ProxyTrustInvalidForwarding {
		e.ProxyTrust = string(auditctx.ProxyTrustInvalidForwarding)
	} else if source.ClientIP.IsValid() && source.ClientIP.Zone() == "" {
		switch {
		case source.ProxyTrust == auditctx.ProxyTrustDirect && e.PeerIP == source.ClientIP.Unmap():
			e.ClientIP, e.ProxyTrust = source.ClientIP.Unmap(), string(source.ProxyTrust)
		case source.ProxyTrust == auditctx.ProxyTrustTrusted && source.SourceTrustVersion != "" && e.PeerIP.IsValid() &&
			len(source.ForwardedChain) > 0 && len(source.ForwardedChain) <= 8 && source.ForwardedChain[0].Unmap() == source.ClientIP.Unmap():
			e.ClientIP, e.ProxyTrust, e.ForwardedChain = source.ClientIP.Unmap(), string(source.ProxyTrust), source.ForwardedChain
		}
	}
	if e.ActorUserID != nil {
		e.ActorKind = "user"
		if identity, ok := ctx.Value(securityAuditIdentityKey{}).(SecurityAuditIdentity); ok && identity.ActorUserID == actorUserID {
			e.AuthMethod, e.AuthStrength = identity.AuthMethod, identity.AuthStrength
			e.CredentialID, e.SessionID = identity.CredentialID, identity.SessionID
		}
	}
	return e
}

// CommitHook 绑定调用方已有的真实操作标识与已验证对象，关键事实只写传入事务。
func (s *SecurityAuditService) CommitHook(ctx context.Context, operationID string, actorUserID int64, assets ...repository.Asset) repository.MutationCompletionHook {
	return s.assetCommitHook(ctx, securityAuditEvent(ctx, actorUserID), operationID, assets)
}

// CommitHookFromSnapshot 仅记录已固化的发起来源；后台连接不补齐缺失或失效快照。
// 最终来源、代次及授权仍由调用方裁决。
func (s *SecurityAuditService) CommitHookFromSnapshot(ctx context.Context, source auditctx.SourceSnapshot, operationID string, actorUserID int64, assets ...repository.Asset) repository.MutationCompletionHook {
	e := securityAuditEvent(ctx, actorUserID)
	e.RequestID, e.OriginRequestID, e.SourceTrustVersion = "", "", ""
	e.PeerIP, e.ClientIP, e.ForwardedChain, e.ProxyTrust = netip.Addr{}, netip.Addr{}, nil, string(auditctx.ProxyTrustUnknown)
	if source.Valid() {
		e.OriginRequestID, e.SourceTrustVersion = source.OriginRequestID, source.SourceTrustVersion
		e.ClientIP, e.ProxyTrust = source.ClientIP, string(source.ProxyTrust)
	}
	return s.assetCommitHook(ctx, e, operationID, assets)
}

func (s *SecurityAuditService) assetCommitHook(ctx context.Context, e repository.SecurityEvent, operationID string, assets []repository.Asset) repository.MutationCompletionHook {
	if len(assets) == 0 || len(assets) > 256 || operationID == "" {
		return func(*sqlx.Tx) error { return repository.ErrInvalidSecurityEvent }
	}
	e.EventType, e.Outcome, e.Committed, e.OperationID = "asset.change_committed", "succeeded", "committed", operationID
	e.RepositoryID = auditOptionalID(assets[0].RepositoryID)
	objects := make([]repository.SecurityEventObject, 0, len(assets))
	for _, asset := range assets {
		if asset.RepositoryID <= 0 || asset.BlobHash == "" {
			return func(*sqlx.Tx) error { return repository.ErrInvalidSecurityEvent }
		}
		if asset.RepositoryID != assets[0].RepositoryID {
			e.RepositoryID = nil
		}
		object := repository.SecurityEventObject{RepositoryID: auditOptionalID(asset.RepositoryID), CanonicalPath: asset.Path,
			ObjectKind: "asset", Digest: "sha256:" + asset.BlobHash, SizeBytes: &asset.Size}
		if asset.ID > 0 {
			object.ObjectID = strconv.FormatInt(asset.ID, 10)
		}
		objects = append(objects, object)
	}
	// 队列和请求连接断开均不参与已经进入协调器完成事务的记账。
	writeCtx := context.WithoutCancel(ctx)
	return func(tx *sqlx.Tx) error {
		event := e
		event.OccurredAt = time.Now().UTC()
		return s.events.InsertTx(writeCtx, tx, event, objects)
	}
}

func auditOptionalID(id int64) *int64 {
	if id == 0 {
		return nil
	}
	return &id
}

func auditOptionalStatus(status int) *int {
	if status == 0 {
		return nil
	}
	return &status
}

// detachSecurityAuditObservation 脱离调用方字符串的背后缓冲，避免小字段留住大请求。
func detachSecurityAuditObservation(o repository.SecurityAuditObservation) (repository.SecurityAuditObservation, int) {
	e := &o.Event
	size := 640 + len(e.ForwardedChain)*32
	fields := []*string{&e.EventID, &e.EventType, &e.RequestID, &e.OriginRequestID, &e.OperationID,
		&e.ActorKind, &e.AuthMethod, &e.AuthStrength, &e.CredentialID, &e.SessionID, &e.ProxyTrust,
		&e.SourceTrustVersion, &e.Outcome, &e.Committed, &e.ErrorCode}
	if o.Object != nil {
		size += 192
		fields = append(fields, &o.Object.ObjectKind, &o.Object.CanonicalPath)
	}
	for _, field := range fields {
		size += len(*field)
		*field = strings.Clone(*field)
	}
	return o, size
}
