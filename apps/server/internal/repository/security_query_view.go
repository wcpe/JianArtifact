package repository

import (
	"context"
	"database/sql/driver"
	"errors"
	"strconv"

	"github.com/jmoiron/sqlx"
)

// ErrSecurityEventSnapshotExpired 表示已验证的续页快照因删除、缺失或恢复不能继续。
var ErrSecurityEventSnapshotExpired = errors.New("安全事件快照已失效")

// SecurityQueryCoverage 表示请求窗口的采集覆盖，不表示投影是否可读。
type SecurityQueryCoverage string

const (
	SecurityQueryCoverageComplete SecurityQueryCoverage = "complete"
	SecurityQueryCoverageGapped   SecurityQueryCoverage = "gapped"
	SecurityQueryCoverageUnknown  SecurityQueryCoverage = "unknown"
)

// SecurityEventQueryPage 在事件页上附加持久删除代次及首屏覆盖；续页 Coverage 为空。
type SecurityEventQueryPage struct {
	SecurityEventPage
	RetentionGeneration int64
	Coverage            SecurityQueryCoverage
}

// SecurityEventObjectViewKey 是已签名且验证绑定后的父事件序号和对象锚点。
type SecurityEventObjectViewKey struct {
	EventSequence       int64
	AfterOrdinal        int
	RetentionGeneration int64
}

// SecurityEventObjectQueryPage 附带签发对象续页所需的父事件与删除代次。
type SecurityEventObjectQueryPage struct {
	SecurityEventObjectPage
	EventSequence       int64
	RetentionGeneration int64
}

// 先判持久起止、检查点锚及已删事件时间，再按未知优先检查半开窗口重叠。
// 检查点总体状态会累积历史缺口，不能将它当作某个窗口的状态。
const securityQueryCoverageSQL = `WITH requested(from_ms,to_ms) AS (VALUES (?,?))
	SELECT f.generation, COALESCE(c.event_watermark,0), CASE
		WHEN c.singleton IS NULL OR c.coverage='pending' OR c.covered_from>from_ms OR c.covered_through<to_ms THEN 'unknown'
		WHEN c.event_watermark>0 AND NOT EXISTS (SELECT 1 FROM security_event WHERE sequence=c.event_watermark) THEN 'unknown'
		WHEN f.deleted_from<to_ms AND f.deleted_through>=from_ms THEN 'unknown'
		WHEN EXISTS (SELECT 1 FROM security_audit_gap INDEXED BY security_audit_gap_query_overlap
			WHERE coverage='unknown' AND window_end>from_ms AND window_start<to_ms) THEN 'unknown'
		WHEN EXISTS (SELECT 1 FROM security_audit_gap INDEXED BY security_audit_gap_query_overlap
			WHERE coverage='gapped' AND window_end>from_ms AND window_start<to_ms) THEN 'gapped'
		ELSE 'complete' END
	FROM security_event_read_fence AS f LEFT JOIN security_audit_checkpoint AS c ON c.singleton=1
	CROSS JOIN requested WHERE f.singleton=1`

const securityQueryGenerationSQL = `SELECT generation FROM security_event_read_fence WHERE singleton=1`

// ListView 首屏先从同一条语句读取覆盖和代次，再固定事件水位；续页只用调用方验证的
// 原始代次与页键。调用方负责固定首次覆盖、签名以及重启/恢复栅栏。
func (r *SecurityEventRepo) ListView(ctx context.Context, filter SecurityEventFilter, key *SecurityEventPageKey, expectedGeneration int64) (SecurityEventQueryPage, error) {
	if err := validateSecurityEventFilter(filter); err != nil {
		return SecurityEventQueryPage{}, err
	}
	if expectedGeneration < 0 || (key == nil && expectedGeneration != 0) || (key != nil &&
		(key.SnapshotWatermark < 1 || key.BeforeSequence < 1 || key.BeforeSequence > key.SnapshotWatermark)) {
		return SecurityEventQueryPage{}, ErrInvalidSecurityEventFilter
	}
	ctx, cancel := context.WithTimeout(ctx, securityEventReadTimeout)
	defer cancel()
	conn, release, err := r.openSecurityQueryReader(ctx)
	if err != nil {
		return SecurityEventQueryPage{}, err
	}
	defer release()

	var view SecurityEventQueryPage
	var coverageWatermark int64
	if key == nil {
		err := conn.QueryRowxContext(ctx, securityQueryCoverageSQL, filter.From.UnixMilli(), filter.To.UnixMilli()).
			Scan(&view.RetentionGeneration, &coverageWatermark, &view.Coverage)
		if err != nil {
			return SecurityEventQueryPage{}, ErrSecurityEventUnavailable
		}
	} else {
		view.RetentionGeneration = expectedGeneration
		if err := confirmSecurityQueryGeneration(ctx, conn, expectedGeneration, true); err != nil {
			return SecurityEventQueryPage{}, err
		}
	}
	view.SecurityEventPage, err = listSecurityEvents(ctx, conn, filter, key)
	if err != nil {
		return SecurityEventQueryPage{}, err
	}
	if key != nil {
		if err := confirmSecurityEventListAnchors(ctx, conn, key); err != nil {
			return SecurityEventQueryPage{}, err
		}
	} else if coverageWatermark > view.SnapshotWatermark {
		view.Coverage = SecurityQueryCoverageUnknown
	}
	if err := confirmSecurityQueryGeneration(ctx, conn, view.RetentionGeneration, key != nil); err != nil {
		return SecurityEventQueryPage{}, err
	}
	return view, nil
}

func confirmSecurityEventListAnchors(ctx context.Context, conn *sqlx.Conn, key *SecurityEventPageKey) error {
	var anchorsPresent bool
	if err := conn.GetContext(ctx, &anchorsPresent, `SELECT EXISTS (SELECT 1 FROM security_event WHERE sequence=?)
		AND EXISTS (SELECT 1 FROM security_event WHERE sequence=?)`, key.SnapshotWatermark, key.BeforeSequence); err != nil {
		return ErrSecurityEventUnavailable
	}
	if !anchorsPresent {
		return ErrSecurityEventSnapshotExpired
	}
	return nil
}

// ListObjectsView 固定父事件序号和对象锚点；事件与对象仅成包追加，任意删除使旧代次失效。
func (r *SecurityEventRepo) ListObjectsView(ctx context.Context, eventID string, limit int, key *SecurityEventObjectViewKey) (SecurityEventObjectQueryPage, error) {
	limit, ok := securityEventPageLimit(limit)
	if !ok || eventID == "" || !validSecurityEventRef(eventID) || (key != nil &&
		(key.EventSequence < 1 || key.AfterOrdinal < 1 || key.AfterOrdinal > securityEventMaxObjects || key.RetentionGeneration < 0)) {
		return SecurityEventObjectQueryPage{}, ErrInvalidSecurityEventFilter
	}
	ctx, cancel := context.WithTimeout(ctx, securityEventReadTimeout)
	defer cancel()
	conn, release, err := r.openSecurityQueryReader(ctx)
	if err != nil {
		return SecurityEventObjectQueryPage{}, err
	}
	defer release()

	var view SecurityEventObjectQueryPage
	var afterOrdinal int
	if key != nil {
		afterOrdinal = key.AfterOrdinal
	}
	var anchorPresent bool
	err = conn.QueryRowxContext(ctx, `SELECT f.generation, COALESCE(e.sequence,0),
		EXISTS (SELECT 1 FROM security_event_object WHERE event_sequence=e.sequence AND ordinal=?)
		FROM security_event_read_fence AS f LEFT JOIN security_event AS e ON e.event_id=? WHERE f.singleton=1`, afterOrdinal, eventID).
		Scan(&view.RetentionGeneration, &view.EventSequence, &anchorPresent)
	if err != nil {
		return SecurityEventObjectQueryPage{}, ErrSecurityEventUnavailable
	}
	if key != nil && (key.RetentionGeneration != view.RetentionGeneration || key.EventSequence != view.EventSequence || !anchorPresent) {
		return SecurityEventObjectQueryPage{}, ErrSecurityEventSnapshotExpired
	}
	if view.EventSequence == 0 {
		return SecurityEventObjectQueryPage{}, ErrSecurityEventNotFound
	}
	view.SecurityEventObjectPage, err = listSecurityEventObjects(ctx, conn, view.EventSequence, afterOrdinal, limit)
	if err != nil {
		return SecurityEventObjectQueryPage{}, err
	}
	if err := confirmSecurityQueryGeneration(ctx, conn, view.RetentionGeneration, key != nil); err != nil {
		return SecurityEventObjectQueryPage{}, err
	}
	return view, nil
}

func confirmSecurityQueryGeneration(ctx context.Context, conn *sqlx.Conn, expected int64, continued bool) error {
	var actual int64
	if err := conn.GetContext(ctx, &actual, securityQueryGenerationSQL); err != nil || ctx.Err() != nil {
		return ErrSecurityEventUnavailable
	}
	if actual != expected {
		if continued {
			return ErrSecurityEventSnapshotExpired
		}
		return ErrSecurityEventUnavailable
	}
	return nil
}

// 借用一条连接但不开事务，避免主库 BEGIN IMMEDIATE 等待写锁。忙则立即降级，
// 连接归还前恢复池的忙等待策略，且恢复与查询共用同一个截止时间。
func (r *SecurityEventRepo) openSecurityQueryReader(ctx context.Context) (*sqlx.Conn, func(), error) {
	conn, err := r.db.Connx(ctx)
	if err != nil {
		return nil, nil, ErrSecurityEventUnavailable
	}
	var original int
	if err := conn.GetContext(ctx, &original, `PRAGMA busy_timeout`); err != nil {
		_ = conn.Close()
		return nil, nil, ErrSecurityEventUnavailable
	}
	release := func() {
		if original != 0 {
			if _, err := conn.ExecContext(ctx, `PRAGMA busy_timeout=`+strconv.Itoa(original)); err != nil {
				_ = conn.Raw(func(any) error { return driver.ErrBadConn })
			}
		}
		_ = conn.Close()
	}
	if original != 0 {
		if _, err := conn.ExecContext(ctx, `PRAGMA busy_timeout=0`); err != nil {
			release()
			return nil, nil, ErrSecurityEventUnavailable
		}
	}
	return conn, release, nil
}
