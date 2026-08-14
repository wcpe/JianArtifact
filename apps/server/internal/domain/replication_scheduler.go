package domain

import (
	"context"
	"log"
	"strconv"
	"sync"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// ReplicationScheduler 调度节点间同步（FR-85）。
//
// 拉取模型：本节点定期（默认 5s）从对端 Sync 一次（复用 ReplicationClient.Sync，
// FR-84），从持久化水位续拉；首次配置对端（本地无水位）时自动全量初始化（since=0）。
// 单一轮询既是"近实时同步"（对端写入 ≤ 轮询间隔内拉到）也是"定期对账兜底"
// （断网恢复 / 漏拉在下一周期自动补齐），无需额外事件总线。
type ReplicationScheduler struct {
	client       *ReplicationClient
	settings     *repository.SettingRepo
	peerURL      string
	interval     time.Duration
	watermarkKey string

	syncMu sync.Mutex // 同一时刻只跑一次 syncOnce
}

// NewReplicationScheduler 构造 ReplicationScheduler。
// peerURL 为对端基址（即 ReplicationClient 的 peerURL）；interval 为轮询间隔。
func NewReplicationScheduler(client *ReplicationClient, settings *repository.SettingRepo, peerURL string, interval time.Duration) *ReplicationScheduler {
	return &ReplicationScheduler{
		client:       client,
		settings:     settings,
		peerURL:      peerURL,
		interval:     interval,
		watermarkKey: "repl:watermark:" + peerURL,
	}
}

// Start 启动后台同步循环，直到 ctx 取消。
func (s *ReplicationScheduler) Start(ctx context.Context) {
	go s.run(ctx)
}

func (s *ReplicationScheduler) run(ctx context.Context) {
	// 首启：本地无对端水位 → 全量初始化。
	watermark, had := s.readWatermark()
	if !had {
		log.Printf("复制调度：首次配置对端 %s，执行全量初始化", s.peerURL)
		if seq, err := s.syncOnce(0); err == nil {
			watermark = seq
		} else {
			log.Printf("复制调度：首启全量失败（对端 %s）：%v", s.peerURL, err)
		}
	}

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Printf("复制调度：停止（对端 %s）", s.peerURL)
			return
		case <-ticker.C:
			if seq, err := s.syncOnce(watermark); err == nil {
				watermark = seq
			}
		}
	}
}

// syncOnce 从水位同步一次并持久化新水位；失败保留旧水位（下轮 / 对账兜底）。
func (s *ReplicationScheduler) syncOnce(watermark int64) (int64, error) {
	s.syncMu.Lock()
	defer s.syncMu.Unlock()

	newSeq, err := s.client.Sync(watermark)
	if err != nil {
		log.Printf("复制调度：同步失败（对端 %s，水位 %d）：%v", s.peerURL, watermark, err)
		return watermark, err
	}
	if err := s.settings.Set(s.watermarkKey, strconv.FormatInt(newSeq, 10)); err != nil {
		log.Printf("复制调度：水位持久化失败（对端 %s）：%v", s.peerURL, err)
	}
	if newSeq != watermark {
		log.Printf("复制调度：同步完成（对端 %s，水位 %d → %d）", s.peerURL, watermark, newSeq)
	}
	return newSeq, nil
}

// readWatermark 读取持久化对端水位；返回 (seq, 是否有记录)。
func (s *ReplicationScheduler) readWatermark() (int64, bool) {
	v, err := s.settings.Get(s.watermarkKey)
	if err != nil {
		return 0, false
	}
	seq, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, false
	}
	return seq, true
}
