package repository

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"time"
)

var (
	// ErrSecurityCoverageBusy 表示低优先级采集已让路，原批次可重试。
	ErrSecurityCoverageBusy = errors.New("安全采集暂让出写入")
	// ErrSecurityCoverageLease 表示当前写者已被替换，不得继续推进检查点。
	ErrSecurityCoverageLease = errors.New("安全采集写者已被替换")
	// ErrSecurityCoverageOrder 表示批次顺序或幂等回执不匹配。
	ErrSecurityCoverageOrder = errors.New("安全采集批次不匹配")
)

const (
	// SecurityAuditBatchLimit 是单个工作事务中的最大观察数。
	SecurityAuditBatchLimit = 50
	// SecurityAuditGapWindowLimit 是内存中完整保留的 UTC 分钟数。
	SecurityAuditGapWindowLimit = 64
)

// SecurityAuditObservation 是低优先级的非关键事实，至多附一个已知的拒绝对象。
type SecurityAuditObservation struct {
	Event  SecurityEvent
	Object *SecurityEventObject
}

func (o SecurityAuditObservation) objects() []SecurityEventObject {
	if o.Object == nil {
		return nil
	}
	return []SecurityEventObject{*o.Object}
}

// ValidateSecurityAuditObservation 使用既有事件词表，且拒绝把关键事件放入异步批次。
func ValidateSecurityAuditObservation(o SecurityAuditObservation) error {
	e := o.Event
	if e.EventID == "" || !securityEventOneOf(e.EventType, "auth.attempt", "protocol.write_denied", "asset.response_observed") ||
		(o.Object != nil && (e.EventType != "protocol.write_denied" || o.Object.ObjectKind != "asset")) {
		return ErrInvalidSecurityEvent
	}
	if e.EventType == "auth.attempt" {
		switch e.Outcome {
		case "succeeded":
			if e.ActorKind != "user" {
				return ErrInvalidSecurityEvent
			}
		case "denied", "failed", "unknown":
			if e.ActorKind != "unknown" || e.SessionID != "" || e.CredentialID != "" || !securityEventOneOf(e.AuthStrength, "", "none") {
				return ErrInvalidSecurityEvent
			}
			if e.Outcome == "unknown" && (e.ErrorCode != "" || e.AuthStrength != "") {
				return ErrInvalidSecurityEvent
			}
		default:
			return ErrInvalidSecurityEvent
		}
	}
	return validateSecurityEvent(e, o.objects())
}

// SecurityAuditGap 是有证抑制窗口或无法恢复计数的未知区间；记录不含任何动态来源标签。
// 有证计数仅针对该原因组被观察并抑制的事实，不是该分钟所有请求的总数。
type SecurityAuditGap struct {
	Start, End                     time.Time
	Reason, Coverage               string
	ObservedCount, SuppressedCount *int64
}

// SecurityAuditBatch 绑定一个写者的有序短事务和原批重试身份。
type SecurityAuditBatch struct {
	Number                      int64
	ID                          string
	HeartbeatAt, CoveredThrough time.Time
	CleanStop                   bool
	Observations                []SecurityAuditObservation
	Gaps                        []SecurityAuditGap
}

// SecurityAuditCheckpoint 区分数据库心跳、覆盖边界和持久事件序号。
// Coverage 仅适用于 [CoveredFrom, CoveredThrough)，区间之外或为空均未知。
// 区间内仍须查看缺口；已有事件、水位或心跳不是完整覆盖的证明。
type SecurityAuditCheckpoint struct {
	HeartbeatAt, CoveredFrom, CoveredThrough time.Time
	Watermark                                int64
	Coverage                                 string
	LastGap                                  SecurityAuditGap
}

func securityAuditBatchDigest(b SecurityAuditBatch) ([]byte, error) {
	if b.Number <= 0 || b.ID == "" || !validSecurityEventRef(b.ID) || !validSecurityEventTime(b.HeartbeatAt) ||
		!validSecurityEventTime(b.CoveredThrough) || b.CoveredThrough.After(b.HeartbeatAt) ||
		len(b.Observations)+len(b.Gaps) > SecurityAuditBatchLimit ||
		(b.CleanStop && (len(b.Observations) > 0 || len(b.Gaps) > 0)) {
		return nil, ErrSecurityCoverageOrder
	}
	h := sha256.New()
	// 编码固定结构只用于校验回执的批次内容，不把序列化数据当事件保存。
	enc := json.NewEncoder(h)
	observations, gaps := b.Observations, b.Gaps
	b.Observations, b.Gaps = nil, nil
	if err := enc.Encode(b); err != nil {
		return nil, ErrSecurityCoverageOrder
	}
	for _, observation := range observations {
		if err := ValidateSecurityAuditObservation(observation); err != nil {
			return nil, err
		}
		if err := enc.Encode(observation); err != nil {
			return nil, ErrSecurityCoverageOrder
		}
	}
	for _, gap := range gaps {
		if !validSecurityAuditGap(gap) {
			return nil, ErrSecurityCoverageOrder
		}
		if err := enc.Encode(gap); err != nil {
			return nil, ErrSecurityCoverageOrder
		}
	}
	return h.Sum(nil), nil
}

func validSecurityAuditGap(g SecurityAuditGap) bool {
	if !validSecurityEventTime(g.Start) || !validSecurityEventTime(g.End) || g.End.UnixMilli() <= g.Start.UnixMilli() {
		return false
	}
	if g.Reason == "counter_overflow" {
		return securityAuditGapCoverage(g) == "unknown" && g.ObservedCount == nil && g.SuppressedCount == nil
	}
	return securityEventOneOf(g.Reason, "global_rate", "source_rate", "source_table_full", "queue_full", "database_unavailable", "invalid_fact") &&
		securityAuditGapCoverage(g) == "gapped" && g.Start.Equal(g.Start.UTC().Truncate(time.Minute)) && g.End.Equal(g.Start.Add(time.Minute)) &&
		g.ObservedCount != nil && g.SuppressedCount != nil && *g.SuppressedCount > 0 && *g.ObservedCount >= *g.SuppressedCount
}

func securityAuditGapCoverage(g SecurityAuditGap) string {
	if g.Coverage != "" {
		return g.Coverage
	}
	if g.ObservedCount == nil || g.SuppressedCount == nil {
		return "unknown"
	}
	return "gapped"
}
