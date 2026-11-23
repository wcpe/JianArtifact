package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"math"
	"strings"
	"testing"
	"time"
)

const (
	cursorTestJTI   = "0123456789abcdef0123456789abcdef"
	cursorTestNode  = "node-test_2"
	cursorTestEvent = "event-test_3"
)

func cursorTestFixture(t *testing.T, clock func() time.Time) (*securityEventCursorCodec, securityEventCursorSession, securityEventCursorListState) {
	t.Helper()
	c, err := newSecurityEventCursorCodec(bytes.Repeat([]byte{7}, 32), cursorTestNode, [16]byte{1}, clock)
	if err != nil {
		t.Fatal(err)
	}
	return c, securityEventCursorSession{userID: 7, jti: cursorTestJTI}, securityEventCursorListState{
		watermark: 15, beforeSequence: 12, fingerprint: sha256.Sum256([]byte("test-filter")), coverage: securityEventCursorCoverageGap,
	}
}

func TestSecurityEventCursorListChain(t *testing.T) {
	start := time.Unix(1_700_000_000, 123_000_000)
	now := start
	c, session, first := cursorTestFixture(t, func() time.Time { return now })
	token, err := c.issueList(session, 4, first)
	if err != nil {
		t.Fatal(err)
	}
	now = start.Add(14 * time.Minute)
	p, err := c.decodeList([]string{token}, session, first.fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if got, gen, err := p.snapshot(); err != nil || got != first || gen != 4 {
		t.Fatalf("仓储读取入参未保留首签状态: %+v, 代次 %d, %v", got, gen, err)
	}
	if got, err := p.page(4); err != nil || got != first {
		t.Fatalf("首游标状态未保留: %+v, %v", got, err)
	}
	for _, anchor := range []int64{0, 12, 13} {
		if _, err := p.next(4, anchor); !errors.Is(err, errInvalidSecurityEventCursor) {
			t.Fatalf("非前进的列表锚点 %d: %v", anchor, err)
		}
	}
	token, err = p.next(4, 9)
	if err != nil {
		t.Fatal(err)
	}
	now = start.Add(15*time.Minute - time.Millisecond)
	p, err = c.decodeList([]string{token}, session, first.fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	first.beforeSequence = 9
	if got, err := p.page(4); err != nil || got != first {
		t.Fatalf("续页只应改变锚点: %+v, %v", got, err)
	}
	now = start.Add(15 * time.Minute)
	if _, err := c.decodeList([]string{token}, session, first.fingerprint); !errors.Is(err, errExpiredSecurityEventCursor) {
		t.Fatalf("首签满 15 分钟: %v", err)
	}
	if _, err := p.next(4, 8); !errors.Is(err, errExpiredSecurityEventCursor) {
		t.Fatalf("已解码链也不得续命: %v", err)
	}
}

func TestSecurityEventCursorBindings(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	clock := func() time.Time { return now }
	c, session, state := cursorTestFixture(t, clock)
	token, err := c.issueList(session, 0, state)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(session.jti)) || bytes.Contains(raw, []byte(cursorTestNode)) || bytes.Contains(raw, state.fingerprint[:]) {
		t.Fatal("游标公开了原始绑定值")
	}
	damaged := bytes.Clone(raw)
	damaged[len(damaged)-1] ^= 1
	otherFP := sha256.Sum256([]byte("other-test-filter"))
	restart, err := newSecurityEventCursorCodec(bytes.Repeat([]byte{7}, 32), cursorTestNode, [16]byte{2}, clock)
	if err != nil {
		t.Fatal(err)
	}
	otherNode, err := newSecurityEventCursorCodec(bytes.Repeat([]byte{7}, 32), "other-node", [16]byte{2}, clock)
	if err != nil {
		t.Fatal(err)
	}
	otherKey, err := newSecurityEventCursorCodec(bytes.Repeat([]byte{8}, 32), cursorTestNode, [16]byte{2}, clock)
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name        string
		codec       *securityEventCursorCodec
		values      []string
		session     securityEventCursorSession
		fingerprint [32]byte
		want        error
	}{
		{"same-node restart", restart, []string{token}, session, state.fingerprint, errExpiredSecurityEventCursor},
		{"other node", otherNode, []string{token}, session, state.fingerprint, errInvalidSecurityEventCursor},
		{"other key", otherKey, []string{token}, session, state.fingerprint, errInvalidSecurityEventCursor},
		{"damaged signature across restart", restart, []string{base64.RawURLEncoding.EncodeToString(damaged)}, session, state.fingerprint, errInvalidSecurityEventCursor},
		{"wrong user across restart", restart, []string{token}, securityEventCursorSession{8, session.jti}, state.fingerprint, errInvalidSecurityEventCursor},
		{"wrong session", c, []string{token}, securityEventCursorSession{7, strings.Repeat("a", 32)}, state.fingerprint, errInvalidSecurityEventCursor},
		{"wrong filter across restart", restart, []string{token}, session, otherFP, errInvalidSecurityEventCursor},
		{"missing", c, nil, session, state.fingerprint, errInvalidSecurityEventCursor},
		{"blank", c, []string{" "}, session, state.fingerprint, errInvalidSecurityEventCursor},
		{"empty", c, []string{""}, session, state.fingerprint, errInvalidSecurityEventCursor},
		{"duplicate", c, []string{token, token}, session, state.fingerprint, errInvalidSecurityEventCursor},
		{"newline", c, []string{token[:3] + "\n" + token[4:]}, session, state.fingerprint, errInvalidSecurityEventCursor},
		{"padding", c, []string{token + "="}, session, state.fingerprint, errInvalidSecurityEventCursor},
		{"oversized", c, []string{strings.Repeat("a", 4096)}, session, state.fingerprint, errInvalidSecurityEventCursor},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := tt.codec.decodeList(tt.values, tt.session, tt.fingerprint); !errors.Is(err, tt.want) {
				t.Fatalf("解码错误 = %v, want %v", err, tt.want)
			}
		})
	}
	p, err := c.decodeList([]string{token}, session, state.fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.page(1); !errors.Is(err, errExpiredSecurityEventCursor) {
		t.Fatalf("留存代次变化应失效: %v", err)
	}
	if _, err := p.page(math.MaxUint64); !errors.Is(err, errInvalidSecurityEventCursor) {
		t.Fatalf("越界留存代次: %v", err)
	}
	if _, err := p.next(1, 11); !errors.Is(err, errExpiredSecurityEventCursor) {
		t.Fatalf("留存代次变化后不可签发: %v", err)
	}
	if _, err := c.decodeObjects([]string{token}, session, cursorTestEvent); !errors.Is(err, errInvalidSecurityEventCursor) {
		t.Fatalf("列表游标不可用于对象: %v", err)
	}
}

func TestSecurityEventCursorObjects(t *testing.T) {
	c, session, list := cursorTestFixture(t, func() time.Time { return time.Unix(1_700_000_000, 0) })
	state := securityEventCursorObjectState{eventSequence: 12, afterOrdinal: 3}
	token, err := c.issueObjects(session, 4, cursorTestEvent, state)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || bytes.Contains(raw, []byte(cursorTestEvent)) {
		t.Fatal("对象游标公开了原始事件标识，或编码失败")
	}
	if _, err := c.decodeList([]string{token}, session, list.fingerprint); !errors.Is(err, errInvalidSecurityEventCursor) {
		t.Fatalf("对象游标不可用于列表: %v", err)
	}
	if _, err := c.decodeObjects([]string{token}, session, "other-event"); !errors.Is(err, errInvalidSecurityEventCursor) {
		t.Fatalf("不接受别的事件: %v", err)
	}
	p, err := c.decodeObjects([]string{token}, session, cursorTestEvent)
	if err != nil {
		t.Fatal(err)
	}
	if got, gen, err := p.snapshot(); err != nil || got != state || gen != 4 {
		t.Fatalf("仓储明细读取入参未保留首签状态: %+v, 代次 %d, %v", got, gen, err)
	}
	if got, err := p.page(4, 12); err != nil || got != state {
		t.Fatalf("明细锚点状态: %+v, %v", got, err)
	}
	for _, seq := range []int64{0, 13} {
		if _, err := p.page(4, seq); !errors.Is(err, errExpiredSecurityEventCursor) {
			t.Fatalf("事件已丢失或更换顺序 %d: %v", seq, err)
		}
	}
	for _, ordinal := range []int{2, 3, 257} {
		if _, err := p.next(4, 12, ordinal); !errors.Is(err, errInvalidSecurityEventCursor) {
			t.Fatalf("无效明细锚点 %d: %v", ordinal, err)
		}
	}
	next, err := p.next(4, 12, 4)
	if err != nil || next == token {
		t.Fatalf("同一事件顺序但不同锚点应产生不同游标: %v", err)
	}
	p, err = c.decodeObjects([]string{next}, session, cursorTestEvent)
	if err != nil {
		t.Fatal(err)
	}
	state.afterOrdinal = 4
	if got, err := p.page(4, 12); err != nil || got != state {
		t.Fatalf("续页对象锚点状态: %+v, %v", got, err)
	}
}

func TestSecurityEventCursorIssueBounds(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	clock := func() time.Time { return now }
	c, session, state := cursorTestFixture(t, clock)
	for _, s := range []securityEventCursorSession{{0, cursorTestJTI}, {-1, cursorTestJTI}, {7, " "}, {7, "api_credential"}} {
		if _, err := c.issueList(s, 0, state); !errors.Is(err, errInvalidSecurityEventCursor) {
			t.Fatalf("会话必须来自正确的交互作用域: %v", err)
		}
	}
	for _, page := range []securityEventCursorListState{
		{0, 1, state.fingerprint, state.coverage}, {12, 0, state.fingerprint, state.coverage},
		{12, 13, state.fingerprint, state.coverage}, {12, math.MinInt64, state.fingerprint, state.coverage},
		{12, 11, state.fingerprint, 0},
	} {
		if _, err := c.issueList(session, 0, page); !errors.Is(err, errInvalidSecurityEventCursor) {
			t.Fatalf("列表越界签发: %v", err)
		}
	}
	for _, page := range []securityEventCursorObjectState{{0, 1}, {12, 0}, {12, math.MaxInt32}} {
		if _, err := c.issueObjects(session, 0, cursorTestEvent, page); !errors.Is(err, errInvalidSecurityEventCursor) {
			t.Fatalf("明细越界签发: %v", err)
		}
	}
	if _, err := c.issueObjects(session, 0, strings.Repeat("a", 129), securityEventCursorObjectState{12, 1}); !errors.Is(err, errInvalidSecurityEventCursor) {
		t.Fatalf("事件标识越界签发: %v", err)
	}
	if _, err := c.issueList(session, math.MaxInt64+1, state); !errors.Is(err, errInvalidSecurityEventCursor) {
		t.Fatalf("留存代次超出 SQLite 域: %v", err)
	}
	token, err := c.issueList(session, math.MaxInt64, securityEventCursorListState{math.MaxInt64, math.MaxInt64, state.fingerprint, securityEventCursorCoverageComplete})
	if err != nil {
		t.Fatal(err)
	}
	p, err := c.decodeList([]string{token}, session, state.fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := p.page(math.MaxInt64); err != nil || got.watermark != math.MaxInt64 || got.coverage != securityEventCursorCoverageComplete {
		t.Fatalf("有效上界解码: %+v, %v", got, err)
	}
	now = now.Add(-time.Millisecond)
	if _, err := c.decodeList([]string{token}, session, state.fingerprint); !errors.Is(err, errInvalidSecurityEventCursor) {
		t.Fatalf("拒绝同启动期未来签发时间: %v", err)
	}
	for _, invalidTime := range []time.Time{time.Time{}, time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC), time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)} {
		now = invalidTime
		if _, err := c.issueList(session, 0, state); !errors.Is(err, errInvalidSecurityEventCursor) {
			t.Fatalf("超出有效签发时间范围: %v", err)
		}
	}
}

func TestSecurityEventCursorConfigAndParallel(t *testing.T) {
	clock := func() time.Time { return time.Unix(1_700_000_000, 0) }
	c, session, state := cursorTestFixture(t, clock)
	key := bytes.Repeat([]byte{7}, 32)
	for _, tt := range []struct {
		key  []byte
		node string
		boot [16]byte
	}{
		{key[:31], cursorTestNode, [16]byte{1}}, {key, " node", [16]byte{1}}, {key, "", [16]byte{1}}, {key, cursorTestNode, [16]byte{}},
	} {
		if _, err := newSecurityEventCursorCodec(tt.key, tt.node, tt.boot, clock); err == nil {
			t.Fatal("接受无效的服务端配置")
		}
	}
	signer, err := newSecurityEventCursorCodec(key, cursorTestNode, [16]byte{1}, clock)
	if err != nil {
		t.Fatal(err)
	}
	clear(key)
	state.coverage = securityEventCursorCoverageUnknown
	for i := int64(1); i <= 8; i++ {
		t.Run("parallel", func(t *testing.T) {
			t.Parallel()
			page := state
			page.beforeSequence = i
			token, err := signer.issueList(session, 0, page)
			if err != nil {
				t.Fatal(err)
			}
			p, err := c.decodeList([]string{token}, session, page.fingerprint)
			if err != nil {
				t.Fatal(err)
			}
			if got, err := p.page(0); err != nil || got != page {
				t.Fatalf("密钥副本或并发状态: %+v, %v", got, err)
			}
		})
	}
}
