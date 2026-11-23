package repository

import (
	"context"
	"database/sql"
	"errors"
	"net/netip"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
)

// SecurityEventRepo 提供事务内追加及有界读取，不更新历史事实。
type SecurityEventRepo struct{ db *persistence.DB }

// NewSecurityEventRepo 构建安全事件仓储。
func NewSecurityEventRepo(db *persistence.DB) *SecurityEventRepo {
	return &SecurityEventRepo{db: db}
}

const securityEventColumns = `sequence,event_id,event_version,event_type,occurred_at,ingested_at,
	request_id,origin_request_id,operation_id,task_id,action_id,actor_kind,actor_user_id,actor_name_snapshot,
	credential_id,session_id,auth_method,auth_strength,peer_ip_family,peer_ip,client_ip_family,client_ip,
	proxy_trust,source_trust_version,forwarded_chain,user_agent,repository_id,outcome,committed,http_status,error_code`

const insertSecurityEventSQL = `INSERT INTO security_event (` + securityEventColumns + `)
	VALUES (NULL,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
	ON CONFLICT(event_id) DO NOTHING
	ON CONFLICT(operation_id,event_type) WHERE committed = 'committed' DO NOTHING
	RETURNING sequence`

const securityEventObjectColumns = `ordinal,repository_id,canonical_path,object_kind,object_id,digest,
	size_bytes,manifest_digest,target_version,repository_name_snapshot`

// InsertTx 在调用方事务内追加事件包。任何错误均要求调用方回滚业务事务；
// 保存点只隔离事件头与明细，不提交或替换调用方事务。
func (r *SecurityEventRepo) InsertTx(ctx context.Context, tx *sqlx.Tx, event SecurityEvent, objects []SecurityEventObject) error {
	if event.EventID == "" {
		event.EventID = NewSecurityEventID()
	}
	if err := validateSecurityEvent(event, objects); err != nil {
		return err
	}
	if tx == nil {
		return ErrSecurityEventUnavailable
	}
	const savepoint = `security_event_insert`
	if _, err := tx.ExecContext(ctx, `SAVEPOINT `+savepoint); err != nil {
		return ErrSecurityEventUnavailable
	}
	finished := false
	defer func() {
		if !finished {
			// 即使操作上下文已取消也尝试撤销事件包；外层仍须整体回滚。
			cleanup := context.WithoutCancel(ctx)
			if _, err := tx.ExecContext(cleanup, `ROLLBACK TO `+savepoint); err == nil {
				_, _ = tx.ExecContext(cleanup, `RELEASE `+savepoint)
			}
		}
	}()
	peerFamily, peer := securityEventIPKey(event.PeerIP)
	clientFamily, client := securityEventIPKey(event.ClientIP)
	var sequence int64
	err := tx.GetContext(ctx, &sequence, insertSecurityEventSQL,
		event.EventID, 1, event.EventType, event.OccurredAt.UnixMilli(), time.Now().UnixMilli(),
		event.RequestID, event.OriginRequestID, event.OperationID, event.TaskID, event.ActionID,
		event.ActorKind, event.ActorUserID, event.ActorNameSnapshot, event.CredentialID, event.SessionID, event.AuthMethod, event.AuthStrength,
		peerFamily, peer, clientFamily, client, event.ProxyTrust, event.SourceTrustVersion, encodeSecurityEventChain(event.ForwardedChain),
		event.UserAgent, event.RepositoryID, event.Outcome, event.Committed, event.HTTPStatus, event.ErrorCode)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrSecurityEventConflict
	}
	if err != nil {
		return ErrSecurityEventUnavailable
	}
	if len(objects) != 0 {
		if err := insertSecurityEventObjects(ctx, tx, sequence, objects); err != nil {
			return ErrSecurityEventUnavailable
		}
	}
	if _, err := tx.ExecContext(ctx, `RELEASE `+savepoint); err != nil {
		return ErrSecurityEventUnavailable
	}
	finished = true
	return nil
}

func insertSecurityEventObjects(ctx context.Context, tx *sqlx.Tx, sequence int64, objects []SecurityEventObject) error {
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO security_event_object (event_sequence,`+securityEventObjectColumns+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer func() { _ = stmt.Close() }()
	for i, o := range objects {
		result, err := stmt.ExecContext(ctx, sequence, i+1, o.RepositoryID, o.CanonicalPath, o.ObjectKind, o.ObjectID,
			o.Digest, o.SizeBytes, o.ManifestDigest, o.TargetVersion, o.RepositoryNameSnapshot)
		if err != nil {
			return err
		}
		if n, err := result.RowsAffected(); err != nil || n != 1 {
			return ErrSecurityEventUnavailable
		}
	}
	return nil
}

func securityEventIPKey(ip netip.Addr) (int, []byte) {
	ip = ip.Unmap()
	if !ip.IsValid() {
		return 0, []byte{}
	}
	if ip.Is4() {
		return 4, ip.AsSlice()
	}
	return 6, ip.AsSlice()
}

func decodeSecurityEventIP(family int, key []byte) (netip.Addr, bool) {
	if family == 0 && len(key) == 0 {
		return netip.Addr{}, true
	}
	ip, ok := netip.AddrFromSlice(key)
	return ip, ok && ((family == 4 && ip.Is4()) || (family == 6 && ip.Is6() && !ip.Is4In6()))
}

func encodeSecurityEventChain(chain []netip.Addr) []byte {
	result := make([]byte, 17*len(chain))
	for i, ip := range chain {
		ip = ip.Unmap()
		address := ip.As16()
		result[17*i] = 6
		if ip.Is4() {
			result[17*i] = 4
		}
		copy(result[17*i+1:17*i+17], address[:])
	}
	return result
}

func decodeSecurityEventChain(encoded []byte) ([]netip.Addr, bool) {
	if len(encoded) == 0 {
		return nil, true
	}
	if len(encoded)%17 != 0 || len(encoded) > 8*17 {
		return nil, false
	}
	result := make([]netip.Addr, 0, len(encoded)/17)
	for i := 0; i < len(encoded); i += 17 {
		ip := netip.AddrFrom16([16]byte(encoded[i+1 : i+17]))
		if (encoded[i] == 4 && !ip.Is4In6()) || (encoded[i] == 6 && ip.Is4In6()) || (encoded[i] != 4 && encoded[i] != 6) {
			return nil, false
		}
		result = append(result, ip.Unmap())
	}
	return result, true
}
