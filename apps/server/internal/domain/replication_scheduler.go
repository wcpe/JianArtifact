package domain

import (
	"context"
	"encoding/json"
	"log"
	"strconv"
	"sync"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// 复制状态持久化键（FR-86 / FR-88）。对端水位键由 ReplicationWatermarkKey 派生。
const (
	// SettingKeyClusterPrefix 集群配置键前缀。所有 repl:* 键都是**节点本地**配置，
	// 绝不参与复制同步（写路径不记录变更、应用路径拒绝应用），避免对端配置互相覆盖造成混乱。
	SettingKeyClusterPrefix = "repl:"

	SettingKeyReplEnabled      = "repl:enabled"       // 自动同步开关（true/false；缺省视为 true）
	SettingKeyReplLastSync     = "repl:last_sync_at"  // 最近成功同步时间（RFC3339）
	SettingKeyReplLastError    = "repl:last_error"    // 最近同步失败摘要（成功同步后清空）
	SettingKeyReplPeerURL      = "repl:peer_url"      // 对端基址（FR-88，web 可配置；单值兼容）
	SettingKeyReplPeerToken    = "repl:peer_token"    // 对端同步令牌（FR-88，web 可配置；单值兼容）
	SettingKeyReplPeers        = "repl:peers"         // 多对端列表（FR-D，JSON 数组 [{url,token}]；优先于单值）
	SettingKeyReplBackfill     = "repl:backfill_done" // 历史数据回填完成标记（"true" 表示已完成）
	SettingKeyReplSyncInterval = "repl:sync_interval" // 同步轮询间隔（秒，FR-89，web 可配置；缺省回退 env/默认 5s）
)

// ReplicationWatermarkKey 返回对端水位在 setting 表中的键。
func ReplicationWatermarkKey(peerURL string) string { return "repl:watermark:" + peerURL }

// syncRequest 是手动立即同步的请求（带完成信号）。
type syncRequest struct {
	done chan struct{}
}

// ReplicationScheduler 调度节点间同步（FR-85，FR-88 改造）。
//
// FR-85：拉取模型——本节点定期（默认 5s）从对端 Sync 一次，从持久化水位续拉；
// 单一轮询既是近实时同步也是对账兜底。
//
// FR-88：对端配置（URL/令牌）从 setting 表读取（web 可配置），调度器**常驻**，
// 每轮检查对端配置与自动同步开关：
//   - 无对端 → 跳过；
//   - 有对端 + 自动开关开 → 轮询同步；
//   - 收到"立即同步"信号（SyncNow）→ 立即同步一次（无论开关状态）。
//
// 配置对端本身不开始同步（需显式开自动开关或点立即同步），满足"连接 ≠ 同步"。
type ReplicationScheduler struct {
	client    *ReplicationClient
	settings  *repository.SettingRepo
	logs      *repository.SyncLogRepo // 同步历史日志（FR-88 可视化）
	interval  time.Duration
	syncNowCh chan syncRequest

	syncMu sync.Mutex // 同一时刻只跑一次 doSync
	errMu  sync.Mutex

	// 连续失败收缩状态按对端隔离，避免一个对端的成功重置另一个对端的失败计数。
	errStates map[string]syncErrorState
}

type syncErrorState struct {
	key   string
	start time.Time
	count int
}

// errProgressEvery 连续失败每多少次打一条进度，避免完全静默太久（默认 20）。
const errProgressEvery = 20

// NewReplicationScheduler 构造 ReplicationScheduler。interval 为轮询间隔。
// 对端配置从 setting 读取（FR-88），无需在构造时指定。
func NewReplicationScheduler(client *ReplicationClient, settings *repository.SettingRepo, logs *repository.SyncLogRepo, interval time.Duration) *ReplicationScheduler {
	return &ReplicationScheduler{
		client:    client,
		settings:  settings,
		logs:      logs,
		interval:  interval,
		syncNowCh: make(chan syncRequest, 1),
		errStates: make(map[string]syncErrorState),
	}
}

// Start 启动后台同步循环，直到 ctx 取消。
func (s *ReplicationScheduler) Start(ctx context.Context) {
	go s.run(ctx)
}

// SyncNow 请求一次立即同步（无论自动开关状态），等待完成（带超时）。供 web 手动同步按钮调用。
func (s *ReplicationScheduler) SyncNow(timeout time.Duration) {
	done := make(chan struct{})
	select {
	case s.syncNowCh <- syncRequest{done: done}:
	default:
		return // 队列满：已有同步请求在排队，直接返回
	}
	select {
	case <-done:
	case <-time.After(timeout):
	}
}

func (s *ReplicationScheduler) run(ctx context.Context) {
	interval := s.interval
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Println("复制调度：停止")
			return
		case req := <-s.syncNowCh:
			s.syncOnceManual() // 手动：无论开关
			if req.done != nil {
				close(req.done)
			}
		case <-ticker.C:
			// FR-89：同步间隔可 web 配置、运行时生效——每轮先读 setting，
			// 变化则重置 ticker 并按新间隔等下一轮（本轮跳过，避免旧节奏下多同步一次）。
			if next := s.ReadSyncInterval(); next > 0 && next != interval {
				interval = next
				ticker.Reset(interval)
				log.Printf("复制调度：同步间隔调整为 %s", interval)
				continue
			}
			s.syncOnceAuto() // 自动：受开关控制
		}
	}
}

// ReadSyncInterval 读取 setting 中的同步间隔（FR-89，秒）；未配置 / 非法返回 0（保持当前间隔）。
func (s *ReplicationScheduler) ReadSyncInterval() time.Duration {
	v, err := s.settings.Get(SettingKeyReplSyncInterval)
	if err != nil {
		return 0
	}
	secs, err := strconv.Atoi(v)
	if err != nil || secs <= 0 {
		return 0
	}
	return time.Duration(secs) * time.Second
}

// syncOnceAuto 自动轮询：需已配置对端且自动开关开启；对每个对端同步一轮。
func (s *ReplicationScheduler) syncOnceAuto() {
	if !s.syncEnabled() {
		return
	}
	for _, p := range s.readPeers() {
		if p.URL == "" {
			continue
		}
		s.doSync(p.URL, p.Token)
	}
}

// syncOnceManual 手动立即同步：对每个对端同步一轮（无论开关状态）。
func (s *ReplicationScheduler) syncOnceManual() {
	for _, p := range s.readPeers() {
		if p.URL == "" {
			continue
		}
		s.doSync(p.URL, p.Token)
	}
}

// logSyncErr 记录一次同步失败，并对连续相同错误做收缩：
//   - 首次失败：打印完整日志并记录错误状态；
//   - 相同错误持续：递增计数，每 errProgressEvery 次打一条进度；
//   - 错误变化：先汇总上一次连续失败（起始→终止 + 次数），再打印新错误。
func (s *ReplicationScheduler) logSyncErr(peerURL string, watermark int64, err error) {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	key := err.Error()
	now := time.Now()
	state := s.errStates[peerURL]
	if key == state.key && !state.start.IsZero() {
		state.count++
		s.errStates[peerURL] = state
		if state.count%errProgressEvery == 0 {
			log.Printf("复制调度：同步持续失败（对端 %s）：%s（已连续失败 %d 次，自 %s）", peerURL, key, state.count, state.start.Format(time.RFC3339))
		}
		return
	}
	if state.key != "" && state.count > 0 {
		log.Printf("复制调度：同步失败告一段落（对端 %s）：%s（自 %s 至 %s，共 %d 次）", peerURL, state.key, state.start.Format(time.RFC3339), now.Format(time.RFC3339), state.count)
	}
	s.errStates[peerURL] = syncErrorState{key: key, start: now, count: 1}
	log.Printf("复制调度：同步失败（对端 %s，水位 %d）：%v", peerURL, watermark, err)
}

// logSyncOK 记录指定对端同步成功；若此前有连续失败，打印恢复汇总并清空该对端状态。
func (s *ReplicationScheduler) logSyncOK(peerURL string) {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	state := s.errStates[peerURL]
	if state.key == "" || state.count == 0 {
		return
	}
	log.Printf("复制调度：同步恢复（对端 %s）：此前连续失败 %s（自 %s 至 %s，共 %d 次）", peerURL, state.key, state.start.Format(time.RFC3339), time.Now().Format(time.RFC3339), state.count)
	delete(s.errStates, peerURL)
}

// doSync 执行一轮同步：设置对端 → 从水位拉取变更 → 持久化水位与状态。
// 同步历史仅记录"有变更或失败"的事件：开始写进行中记录，空同步（无变更且成功）删除不留痕，
// 有变更回写成功统计，失败回写错误摘要。
func (s *ReplicationScheduler) doSync(peerURL, token string) {
	s.syncMu.Lock()
	defer s.syncMu.Unlock()

	s.client.SetPeer(peerURL, token)
	watermark, _ := s.readWatermark(peerURL)
	logID, _ := s.logs.Start(peerURL, watermark)
	stats, err := s.client.Sync(watermark)
	if err != nil {
		_ = s.logs.Finish(logID, false, stats.ToSeq, stats.Changes, stats.Applied, stats.Failed, stats.Blobs, stats.ByEntity, err.Error())
		_ = s.settings.Set(SettingKeyReplLastError, err.Error())
		s.logSyncErr(peerURL, watermark, err)
		return
	}
	if stats.Changes == 0 {
		// 空同步：无变更，删除进行中记录，不留痕（水位/状态仍照常更新）。
		_ = s.logs.Delete(logID)
		if err := s.settings.Set(ReplicationWatermarkKey(peerURL), strconv.FormatInt(stats.ToSeq, 10)); err != nil {
			log.Printf("复制调度：水位持久化失败（对端 %s）：%v", peerURL, err)
		}
		s.logSyncOK(peerURL)
		_ = s.settings.Set(SettingKeyReplLastSync, time.Now().UTC().Format(time.RFC3339Nano))
		_ = s.settings.Set(SettingKeyReplLastError, "")
		return
	}
	_ = s.logs.Finish(logID, true, stats.ToSeq, stats.Changes, stats.Applied, stats.Failed, stats.Blobs, stats.ByEntity, "")
	if err := s.settings.Set(ReplicationWatermarkKey(peerURL), strconv.FormatInt(stats.ToSeq, 10)); err != nil {
		log.Printf("复制调度：水位持久化失败（对端 %s）：%v", peerURL, err)
	}
	s.logSyncOK(peerURL)
	_ = s.settings.Set(SettingKeyReplLastSync, time.Now().UTC().Format(time.RFC3339Nano))
	_ = s.settings.Set(SettingKeyReplLastError, "")
	if stats.ToSeq != watermark {
		log.Printf("复制调度：同步完成（对端 %s，水位 %d → %d）", peerURL, watermark, stats.ToSeq)
	}
}

// readPeers 返回全部对端（URL + 令牌明文，供同步使用），
// 优先 repl:peers 多对端 JSON，为空回退旧单值 repl:peer_url / repl:peer_token（FR-D）。
func (s *ReplicationScheduler) readPeers() []Peer {
	if v, err := s.settings.Get(SettingKeyReplPeers); err == nil && v != "" {
		var peers []Peer
		if json.Unmarshal([]byte(v), &peers) == nil && len(peers) > 0 {
			return peers
		}
	}
	peerURL, _ := s.settings.Get(SettingKeyReplPeerURL)
	token, _ := s.settings.Get(SettingKeyReplPeerToken)
	if peerURL == "" {
		return nil
	}
	return []Peer{{URL: peerURL, Token: token}}
}

// syncEnabled 读取自动同步开关（FR-86）；缺省 / 读取失败视为启用。
func (s *ReplicationScheduler) syncEnabled() bool {
	v, err := s.settings.Get(SettingKeyReplEnabled)
	if err != nil {
		return true
	}
	return v != "false"
}

// readWatermark 读取指定对端的水位；返回 (seq, 是否有记录)。
func (s *ReplicationScheduler) readWatermark(peerURL string) (int64, bool) {
	v, err := s.settings.Get(ReplicationWatermarkKey(peerURL))
	if err != nil {
		return 0, false
	}
	seq, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, false
	}
	return seq, true
}
