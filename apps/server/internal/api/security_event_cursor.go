package api

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"math"
	"strings"
	"time"
)

var (
	errInvalidSecurityEventCursor = errors.New("无效的安全事件游标")
	errExpiredSecurityEventCursor = errors.New("安全事件游标已过期或快照不可继续")
	errSecurityEventCursorConfig  = errors.New("安全事件游标配置无效")
)

const (
	securityEventCursorVersion  = 1
	securityEventCursorTTLMS    = 15 * 60 * 1000
	securityEventCursorMaxMS    = 253402300799999 // 公元 9999 年末，以毫秒计。
	securityEventCursorMaxRef   = 128
	securityEventCursorMaxOrder = 256 // 对齐仓储的对象数量和锚点上限。
	securityEventCursorDomain   = "jianartifact/security-event-cursor/v1/"
)

// 两种用途均为固定布局的大端二进制；独立签名域使相同布局不可互换。
const (
	cursorNodeAt    = 1
	cursorBootAt    = cursorNodeAt + sha256.Size
	cursorSessionAt = cursorBootAt + 16
	cursorIssuedAt  = cursorSessionAt + sha256.Size
	cursorGenAt     = cursorIssuedAt + 8
	cursorSeqAt     = cursorGenAt + 8
	cursorAnchorAt  = cursorSeqAt + 8
	cursorQueryAt   = cursorAnchorAt + 8
	cursorCoverAt   = cursorQueryAt + sha256.Size
	cursorBodySize  = cursorCoverAt + 1
	cursorRawSize   = cursorBodySize + sha256.Size
	cursorTokenSize = (cursorRawSize*8 + 5) / 6
)

var securityEventCursorEncoding = base64.RawURLEncoding.Strict()

type securityEventCursorCoverage byte

const (
	securityEventCursorCoverageUnknown securityEventCursorCoverage = iota + 1
	securityEventCursorCoverageComplete
	securityEventCursorCoverageGap
)

type securityEventCursorPurpose byte

const (
	securityEventCursorListPurpose securityEventCursorPurpose = iota
	securityEventCursorObjectPurpose
)

// 仅从已通过授权的交互会话提取；不能由请求参数或管理令牌填充。
type securityEventCursorSession struct {
	userID int64
	jti    string
}

type securityEventCursorListState struct {
	watermark      int64
	beforeSequence int64
	fingerprint    [sha256.Size]byte
	coverage       securityEventCursorCoverage
}

type securityEventCursorObjectState struct {
	eventSequence int64
	afterOrdinal  int
}

type securityEventCursorCodec struct {
	signKeys [2][sha256.Size]byte
	bindKey  [sha256.Size]byte
	node     [sha256.Size]byte
	boot     [16]byte
	now      func() time.Time
}

// 稳定密钥要求至少 32 字节且由服务端随机生成；启动栅栏须由装配方每次启动独立随机生成。
// 密钥和节点身份跨重启保持不变，才能将同节点旧签名识别为过期。时钟由服务端提供。
func newSecurityEventCursorCodec(key []byte, nodeID string, boot [16]byte, now func() time.Time) (*securityEventCursorCodec, error) {
	if len(key) < sha256.Size || !securityEventCursorRef(nodeID, true) || boot == [16]byte{} {
		return nil, errSecurityEventCursorConfig
	}
	if now == nil {
		now = time.Now
	}
	stable := bytes.Clone(key)
	defer clear(stable)
	c := &securityEventCursorCodec{boot: boot, now: now}
	c.signKeys[securityEventCursorListPurpose] = securityEventCursorMAC(stable, securityEventCursorDomain+"list-sign")
	c.signKeys[securityEventCursorObjectPurpose] = securityEventCursorMAC(stable, securityEventCursorDomain+"objects-sign")
	c.bindKey = securityEventCursorMAC(stable, securityEventCursorDomain+"binding")
	c.node = securityEventCursorMAC(c.bindKey[:], "node\x00", []byte(nodeID))
	return c, nil
}

func securityEventCursorMAC(key []byte, domain string, parts ...[]byte) (tag [sha256.Size]byte) {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(domain))
	for _, part := range parts {
		_, _ = mac.Write(part)
	}
	mac.Sum(tag[:0])
	return tag
}

func securityEventCursorRef(ref string, node bool) bool {
	if len(ref) == 0 || len(ref) > securityEventCursorMaxRef || !node && strings.HasPrefix(strings.ToLower(ref[:min(len(ref), 4)]), "jat_") {
		return false
	}
	for i := range len(ref) {
		ch := ref[i]
		if !('a' <= ch && ch <= 'z' || 'A' <= ch && ch <= 'Z' || '0' <= ch && ch <= '9' || ch == '-' || ch == '_' || node && ch == '.') {
			return false
		}
	}
	return !node || strings.Trim(ref, "-_.") != ""
}

func (c *securityEventCursorCodec) sessionTag(session securityEventCursorSession) ([sha256.Size]byte, error) {
	// JWTManager 签发的 JTI 为 16 字节随机值的小写十六进制表示。
	if session.userID <= 0 || len(session.jti) != 32 {
		return [sha256.Size]byte{}, errInvalidSecurityEventCursor
	}
	for i := range len(session.jti) {
		ch := session.jti[i]
		if !('0' <= ch && ch <= '9' || 'a' <= ch && ch <= 'f') {
			return [sha256.Size]byte{}, errInvalidSecurityEventCursor
		}
	}
	var user [8]byte
	binary.BigEndian.PutUint64(user[:], uint64(session.userID))
	return securityEventCursorMAC(c.bindKey[:], "session\x00", user[:], []byte(session.jti)), nil
}

func (c *securityEventCursorCodec) clockMS() (uint64, error) {
	now := c.now()
	if now.Before(time.UnixMilli(1)) || !now.Before(time.UnixMilli(securityEventCursorMaxMS+1)) {
		return 0, errInvalidSecurityEventCursor
	}
	return uint64(now.UnixMilli()), nil
}

func (c *securityEventCursorCodec) issueList(session securityEventCursorSession, gen uint64, state securityEventCursorListState) (string, error) {
	tag := securityEventCursorMAC(c.bindKey[:], "filter\x00", state.fingerprint[:])
	return c.issue(securityEventCursorListPurpose, session, gen, state.watermark, uint64(state.beforeSequence), tag, state.coverage)
}

func (c *securityEventCursorCodec) issueObjects(session securityEventCursorSession, gen uint64, eventID string, state securityEventCursorObjectState) (string, error) {
	if !securityEventCursorRef(eventID, false) {
		return "", errInvalidSecurityEventCursor
	}
	tag := securityEventCursorMAC(c.bindKey[:], "event\x00", []byte(eventID))
	return c.issue(securityEventCursorObjectPurpose, session, gen, state.eventSequence, uint64(state.afterOrdinal), tag, 0)
}

func (c *securityEventCursorCodec) issue(purpose securityEventCursorPurpose, session securityEventCursorSession, gen uint64, seq int64, anchor uint64, query [sha256.Size]byte, cover securityEventCursorCoverage) (string, error) {
	binding, err := c.sessionTag(session)
	if err != nil {
		return "", err
	}
	issued, err := c.clockMS()
	if err != nil {
		return "", err
	}
	p := securityEventCursorChain{codec: c, purpose: purpose}
	p.body[0] = securityEventCursorVersion
	copy(p.body[cursorNodeAt:cursorBootAt], c.node[:])
	copy(p.body[cursorBootAt:cursorSessionAt], c.boot[:])
	copy(p.body[cursorSessionAt:cursorIssuedAt], binding[:])
	binary.BigEndian.PutUint64(p.body[cursorIssuedAt:], issued)
	binary.BigEndian.PutUint64(p.body[cursorGenAt:], gen)
	binary.BigEndian.PutUint64(p.body[cursorSeqAt:], uint64(seq))
	binary.BigEndian.PutUint64(p.body[cursorAnchorAt:], anchor)
	copy(p.body[cursorQueryAt:cursorCoverAt], query[:])
	p.body[cursorCoverAt] = byte(cover)
	if !p.valid() {
		return "", errInvalidSecurityEventCursor
	}
	return p.seal(), nil
}

type securityEventCursorChain struct {
	codec   *securityEventCursorCodec
	purpose securityEventCursorPurpose
	body    [cursorBodySize]byte
}

func (p securityEventCursorChain) number(at int) uint64 {
	return binary.BigEndian.Uint64(p.body[at:])
}

func (p securityEventCursorChain) valid() bool {
	issued, seq, anchor := p.number(cursorIssuedAt), p.number(cursorSeqAt), p.number(cursorAnchorAt)
	var zeroBoot [16]byte
	if p.body[0] != securityEventCursorVersion || bytes.Equal(p.body[cursorBootAt:cursorSessionAt], zeroBoot[:]) ||
		issued == 0 || issued > securityEventCursorMaxMS-securityEventCursorTTLMS || p.number(cursorGenAt) > math.MaxInt64 ||
		seq == 0 || seq > math.MaxInt64 || anchor == 0 {
		return false
	}
	cover := securityEventCursorCoverage(p.body[cursorCoverAt])
	if p.purpose == securityEventCursorListPurpose {
		return anchor <= seq && cover >= securityEventCursorCoverageUnknown && cover <= securityEventCursorCoverageGap
	}
	return anchor <= securityEventCursorMaxOrder && cover == 0
}

func (p securityEventCursorChain) seal() string {
	var raw [cursorRawSize]byte
	copy(raw[:], p.body[:])
	tag := securityEventCursorMAC(p.codec.signKeys[p.purpose][:], "", p.body[:])
	copy(raw[cursorBodySize:], tag[:])
	return securityEventCursorEncoding.EncodeToString(raw[:])
}

func (c *securityEventCursorCodec) open(purpose securityEventCursorPurpose, values []string, session securityEventCursorSession, query [sha256.Size]byte) (securityEventCursorChain, error) {
	// 调用方需传入未合并的参数值：无游标由调用方进入首屏，单个空值或重复值均无效。
	if len(values) != 1 || len(values[0]) != cursorTokenSize || strings.ContainsAny(values[0], "\r\n") {
		return securityEventCursorChain{}, errInvalidSecurityEventCursor
	}
	var raw [cursorRawSize]byte
	if n, err := securityEventCursorEncoding.Decode(raw[:], []byte(values[0])); err != nil || n != cursorRawSize {
		return securityEventCursorChain{}, errInvalidSecurityEventCursor
	}
	tag := securityEventCursorMAC(c.signKeys[purpose][:], "", raw[:cursorBodySize])
	binding, err := c.sessionTag(session)
	if err != nil || !hmac.Equal(raw[cursorBodySize:], tag[:]) || !hmac.Equal(raw[cursorNodeAt:cursorBootAt], c.node[:]) ||
		!hmac.Equal(raw[cursorSessionAt:cursorIssuedAt], binding[:]) || !hmac.Equal(raw[cursorQueryAt:cursorCoverAt], query[:]) {
		return securityEventCursorChain{}, errInvalidSecurityEventCursor
	}
	p := securityEventCursorChain{codec: c, purpose: purpose}
	copy(p.body[:], raw[:cursorBodySize])
	if !p.valid() {
		return securityEventCursorChain{}, errInvalidSecurityEventCursor
	}
	if err := p.live(); err != nil {
		return securityEventCursorChain{}, err
	}
	return p, nil
}

func (p securityEventCursorChain) live() error {
	if p.codec == nil {
		return errInvalidSecurityEventCursor
	}
	if !hmac.Equal(p.body[cursorBootAt:cursorSessionAt], p.codec.boot[:]) {
		return errExpiredSecurityEventCursor
	}
	now, err := p.codec.clockMS()
	issued := p.number(cursorIssuedAt)
	if err != nil || now < issued {
		return errInvalidSecurityEventCursor
	}
	if now-issued >= securityEventCursorTTLMS {
		return errExpiredSecurityEventCursor
	}
	return nil
}

// 代次为 0 至 SQLite 有符号整数上界；代次 0 是合法初始状态，不表示读取失败。
// 当前代次必须来自和续页读取一致的持久视图；仓储错误不能用 0 代替。
func (p securityEventCursorChain) checkRetention(current uint64) error {
	if current > math.MaxInt64 {
		return errInvalidSecurityEventCursor
	}
	if err := p.live(); err != nil {
		return err
	}
	if p.number(cursorGenAt) != current {
		return errExpiredSecurityEventCursor
	}
	return nil
}

type securityEventListCursor struct {
	chain       securityEventCursorChain
	fingerprint [sha256.Size]byte
}

func (c *securityEventCursorCodec) decodeList(values []string, session securityEventCursorSession, fingerprint [sha256.Size]byte) (securityEventListCursor, error) {
	tag := securityEventCursorMAC(c.bindKey[:], "filter\x00", fingerprint[:])
	p, err := c.open(securityEventCursorListPurpose, values, session, tag)
	return securityEventListCursor{chain: p, fingerprint: fingerprint}, err
}

// snapshot 返回给仓储作为同一读视图的校验入参；此处尚未验证当前持久代次。
func (p securityEventListCursor) snapshot() (securityEventCursorListState, uint64, error) {
	gen := p.chain.number(cursorGenAt)
	state, err := p.page(gen)
	return state, gen, err
}

func (p securityEventListCursor) page(currentGen uint64) (securityEventCursorListState, error) {
	if err := p.chain.checkRetention(currentGen); err != nil {
		return securityEventCursorListState{}, err
	}
	return securityEventCursorListState{
		watermark: int64(p.chain.number(cursorSeqAt)), beforeSequence: int64(p.chain.number(cursorAnchorAt)),
		fingerprint: p.fingerprint, coverage: securityEventCursorCoverage(p.chain.body[cursorCoverAt]),
	}, nil
}

func (p securityEventListCursor) next(currentGen uint64, beforeSequence int64) (string, error) {
	state, err := p.page(currentGen)
	if err != nil {
		return "", err
	}
	if beforeSequence <= 0 || beforeSequence >= state.beforeSequence {
		return "", errInvalidSecurityEventCursor
	}
	binary.BigEndian.PutUint64(p.chain.body[cursorAnchorAt:], uint64(beforeSequence))
	return p.chain.seal(), nil
}

type securityEventObjectCursor struct{ chain securityEventCursorChain }

func (c *securityEventCursorCodec) decodeObjects(values []string, session securityEventCursorSession, eventID string) (securityEventObjectCursor, error) {
	if !securityEventCursorRef(eventID, false) {
		return securityEventObjectCursor{}, errInvalidSecurityEventCursor
	}
	tag := securityEventCursorMAC(c.bindKey[:], "event\x00", []byte(eventID))
	p, err := c.open(securityEventCursorObjectPurpose, values, session, tag)
	return securityEventObjectCursor{chain: p}, err
}

// snapshot 返回签名中的事件序号、对象锚点和代次，供仓储在同一读视图校验。
func (p securityEventObjectCursor) snapshot() (securityEventCursorObjectState, uint64, error) {
	gen := p.chain.number(cursorGenAt)
	state, err := p.page(gen, int64(p.chain.number(cursorSeqAt)))
	return state, gen, err
}

// 当前事件序号须来自同一个持久视图；已确认不存在时用 0，不用 0 表示读取错误。
func (p securityEventObjectCursor) page(currentGen uint64, currentEventSequence int64) (securityEventCursorObjectState, error) {
	if err := p.chain.checkRetention(currentGen); err != nil {
		return securityEventCursorObjectState{}, err
	}
	seq := int64(p.chain.number(cursorSeqAt))
	if currentEventSequence != seq {
		return securityEventCursorObjectState{}, errExpiredSecurityEventCursor
	}
	return securityEventCursorObjectState{eventSequence: seq, afterOrdinal: int(p.chain.number(cursorAnchorAt))}, nil
}

func (p securityEventObjectCursor) next(currentGen uint64, currentEventSequence int64, afterOrdinal int) (string, error) {
	state, err := p.page(currentGen, currentEventSequence)
	if err != nil {
		return "", err
	}
	if afterOrdinal <= state.afterOrdinal || afterOrdinal > securityEventCursorMaxOrder {
		return "", errInvalidSecurityEventCursor
	}
	binary.BigEndian.PutUint64(p.chain.body[cursorAnchorAt:], uint64(afterOrdinal))
	return p.chain.seal(), nil
}
