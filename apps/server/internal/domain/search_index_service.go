package domain

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// FR-149 §3.2：asset_path_fts 的后台异步回填、只读状态快照与全量重建。
//
// 本文件只做**编排**：批次读写与状态推进在 repository.SearchIndexStateRepo，索引行原语在
// repository/search_index_repo.go。之所以单独一个服务而不是塞进 FR-44 调度器，是因为回填的
// 语义与周期作业相反——它要尽快开始、只跑一次、跑完即退出；塞进去还会污染
// jianartifact_scheduler_job_* 的运行中/失败计数语义。
//
// 三条硬约束（都来自单写者与在线可用）：
//
//  1. **不阻塞启动**：Start 只是挂一个 goroutine，调用方（main）立刻继续监听。
//  2. **批次必须短、批间必须让出**：SQLite 的写者只有一个（WAL 的「多读 + 单写」），
//     回填事务一旦长，在线写入就会在忙等待里排队。每批一个短事务，提交后让出 1ms。
//     实测一批 2000 行的 FTS 写事务约 20ms，远小于 busy_timeout(5000)（本机）。
//  3. **回填期间的搜索走降级路径**：completed_at 为 NULL 时搜索整体走既有 LIKE 路径
//     （正确但尚未提速），因此「重建时清空索引」不会让用户看到漏结果。这条取舍的另一半是
//     本服务必须让 ready 状态可被观察到——见 SearchIndexStatus。
const (
	// searchIndexBackfillBatchSize 是单批行数（规格 §3.2）。取 2000 的理由：一批的
	// FTS 写入在毫秒级到几十毫秒级，既不至于让单事务过长，也不会让批数多到让「让出」变成本体开销。
	searchIndexBackfillBatchSize = 2000
	// searchIndexBackfillPause 是批间让出时长。1ms 足以让等待连接的在线请求插进来；
	// 更长的 sleep 只会拖长一次性回填的总时长。
	searchIndexBackfillPause = time.Millisecond
	// searchIndexBackfillRetries 是单批的最大尝试次数（含首次）。失败即指数退避重试，
	// 仍失败则记错误日志并退出 goroutine——不阻塞进程，下次启动从水位续跑。
	searchIndexBackfillRetries = 3
	// searchIndexBackfillRetryBase 是首次重试的等待时长，之后每次翻倍。
	searchIndexBackfillRetryBase = 200 * time.Millisecond
	// searchIndexProgressStep 是进度日志的节流粒度：较上次记录跨过 ≥10% 才记一行，
	// 避免每批一行刷屏（百万级、每批 2000 会有 500 行日志）。
	searchIndexProgressStep = int64(10)
)

// ErrSearchIndexRunning 表示已有一个回填或重建任务在跑。
// 对齐既有运维接口（维护作业手动触发）的口径：HTTP 侧映射 409 + job_running。
var ErrSearchIndexRunning = errors.New("搜索索引回填正在运行")

// SearchIndexStatus 是状态端点（GET /api/v1/search/index）的领域快照。
type SearchIndexStatus struct {
	// Ready 表示回填是否已完成（completed_at 非空）。为 false 时搜索走既有 LIKE 路径。
	Ready bool
	// Indexed 是已回填行数（按 last_id 水位回 asset 真源计数）。
	Indexed int64
	// Total 是回填开始时的 asset 行数快照，仅用于进度展示与一致性校验。
	Total int64
	// LastID 是已回填到的最大 asset.id（水位），0 表示尚未开始。
	LastID int64
	// UpdatedAt 是状态行最后一次变更时刻，供排查「回填是否还在推进」。
	UpdatedAt string
}

// SearchIndexStore 是回填所需的最小存储能力，由 repository.SearchIndexStateRepo 实现。
//
// 定义在消费方（domain）而不是实现方，是为了让「中途中断后从水位续跑」这类用例可以精确
// 控制批次边界——测试用一层包装即可在指定批次后注入取消，不必依赖 sleep 抢时序，也不必在
// 生产代码里留测试钩子。
type SearchIndexStore interface {
	// Load 读回填状态。
	Load() (repository.SearchIndexState, error)
	// SnapshotBounds 取本次回填的上界与总行数快照。
	SnapshotBounds() (upper int64, total int64, err error)
	// SaveBounds 记录总行数快照。
	SaveBounds(total int64) error
	// FetchBatch 取 (after, upper] 区间内按 id 升序的一批待回填行。
	FetchBatch(after, upper int64, limit int) ([]repository.SearchIndexRow, error)
	// WriteBatch 同事务写入本批索引并推进水位。
	WriteBatch(rows []repository.SearchIndexRow, lastID int64) error
	// MarkCompleted 把回填标记为完成。
	MarkCompleted() error
	// Reset 清空索引并重置水位（completed_at 归 NULL）。
	Reset() error
	// CountIndexed 数出水位以内的 asset 行数。
	CountIndexed(lastID int64) (int64, error)
}

// SearchIndexService 编排索引回填与重建。零值不可用，用 NewSearchIndexService 构造。
type SearchIndexService struct {
	store SearchIndexStore

	// mu 保护 running 与 baseCtx。回填与重建**不可并跑**：两者都在写同一张索引与同一行水位，
	// 并发会让水位互相覆盖（表现为索引里缺一段或重复回填），因此用一把互斥 + 运行中标志
	// 保证同一时刻只有一个任务；409 也由此判定。
	mu      sync.Mutex
	running bool
	// baseCtx 是进程生命周期上下文（Start 注入），重建在后台使用它。
	baseCtx context.Context
}

// NewSearchIndexService 构造 SearchIndexService。
func NewSearchIndexService(store SearchIndexStore) *SearchIndexService {
	return &SearchIndexService{store: store}
}

// Start 启动一次性的后台回填（应在进程启动、迁移成功后调用一次）。
//
// 它立即返回，不阻塞启动；ctx 取消即优雅停机——已推进的水位在每批提交时落库，因此中断后
// 下次启动会从该水位继续。回填已完成时本方法只做一次状态读取便退出（零开销）。
func (s *SearchIndexService) Start(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Lock()
	s.baseCtx = ctx
	s.mu.Unlock()
	go func() {
		if err := s.Run(ctx); err != nil && !errors.Is(err, ErrSearchIndexRunning) {
			// 失败不阻塞进程：ready 保持 false，搜索继续走降级路径（可用但慢），
			// 下次启动自动从 last_id 续跑。错误行里带上进度与耗时，便于运维判断要不要重建。
			log.Printf("%v", err)
		}
	}()
}

// Run 同步执行一次回填，成功返回 nil。
//
// 生产入口是 Start（后台）与 Rebuild（清空后重跑）；Run 也直接对测试开放，让「跑完再断言」
// 成为确定性用例。已有任务在跑时返回 ErrSearchIndexRunning。
func (s *SearchIndexService) Run(ctx context.Context) error {
	if !s.begin() {
		return ErrSearchIndexRunning
	}
	defer s.end()
	return s.backfill(ctx)
}

// Status 返回状态快照（只读）。
func (s *SearchIndexService) Status() (SearchIndexStatus, error) {
	state, err := s.store.Load()
	if err != nil {
		return SearchIndexStatus{}, fmt.Errorf("读取搜索索引状态：%w", err)
	}
	indexed, err := s.store.CountIndexed(state.LastID)
	if err != nil {
		return SearchIndexStatus{}, fmt.Errorf("统计已回填行数：%w", err)
	}
	return SearchIndexStatus{
		Ready:     state.Completed(),
		Indexed:   indexed,
		Total:     state.TotalRows,
		LastID:    state.LastID,
		UpdatedAt: state.UpdatedAt,
	}, nil
}

// Rebuild 受理一次全量重建：清空索引 → 水位归零 → 后台按回填算法重跑。
//
// 返回 nil 只表示**已受理**（HTTP 202）——真正的清空与重灌在后台进行。这样设计的原因：
// 百万级索引的清空不是瞬时操作，而重建全程在线（清空后 completed_at 为 NULL，搜索自动走
// 降级路径，正确性不受影响），没有任何理由让运维请求等它。
// 已有任务在跑时返回 ErrSearchIndexRunning（HTTP 409）。
func (s *SearchIndexService) Rebuild() error {
	if !s.begin() {
		return ErrSearchIndexRunning
	}
	s.mu.Lock()
	ctx := s.baseCtx
	s.mu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	go func() {
		defer s.end()
		if err := s.store.Reset(); err != nil {
			log.Printf("搜索索引重建失败：清空索引与重置水位出错：%v", err)
			return
		}
		log.Printf("搜索索引重建：索引已清空、水位已归零，开始重新回填")
		if err := s.backfill(ctx); err != nil {
			log.Printf("%v", err)
		}
	}()
	return nil
}

// begin 尝试占用运行槽；已被占用返回 false。
func (s *SearchIndexService) begin() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		return false
	}
	s.running = true
	return true
}

// end 释放运行槽。
func (s *SearchIndexService) end() {
	s.mu.Lock()
	s.running = false
	s.mu.Unlock()
}

// backfill 是 §3.2 的算法本体：读水位 → 上界快照 → 分批推进 → 置完成。
//
// 返回错误表示本次回填失败（此时不置 completed_at；已推进的水位保留，下次启动续跑）。
// ctx 取消不算失败：把已推进的水位留在库里，返回 nil。
func (s *SearchIndexService) backfill(ctx context.Context) error {
	started := time.Now()
	state, err := s.store.Load()
	if err != nil {
		return fmt.Errorf("搜索索引回填失败：读取回填状态出错：%w", err)
	}
	if state.Completed() {
		// 已完成：立刻退出，零开销（这是每次进程启动的常态路径）。
		return nil
	}
	upper, total, err := s.store.SnapshotBounds()
	if err != nil {
		return fmt.Errorf("搜索索引回填失败：读取资产上界出错：%w", err)
	}
	if err := s.store.SaveBounds(total); err != nil {
		return fmt.Errorf("搜索索引回填失败：记录回填总量出错：%w", err)
	}

	// 续跑起点：上一次已推进到的水位。中断后重启会走这里，因此日志要带「本次回填」增量。
	lastID := state.LastID
	filled := int64(0)
	indexed, err := s.store.CountIndexed(lastID)
	if err != nil {
		return fmt.Errorf("搜索索引回填失败：统计已回填行数出错：%w", err)
	}

	// 进度日志节流：跨过 ≥10% 才记一行。初值取 -step，让「续跑起点」也记一行；
	// 全新回填（起点为 0）不单独记 0%，避免小库上出现「0% → 100%」两条无信息量的日志。
	lastLoggedPct := -searchIndexProgressStep
	logProgress := func() {
		if total <= 0 {
			return
		}
		pct := indexed * 100 / total
		if pct > 100 {
			pct = 100
		}
		if pct-lastLoggedPct < searchIndexProgressStep {
			return
		}
		lastLoggedPct = pct
		// 日志不含任何路径内容：路径是用户数据，不该出现在运行日志里。
		log.Printf("搜索索引回填进度：%d%%（已回填 %d/%d 行）", pct, indexed, total)
	}
	if indexed > 0 {
		logProgress()
	}

	for {
		// 停机检查放在每批开头：水位已在上一批提交时落库，这里直接退出即可续跑。
		if err := ctx.Err(); err != nil {
			log.Printf("搜索索引回填已中断：已回填 %d/%d 行（水位 %d），本次补入 %d 行，耗时 %s；下次启动将从该水位续跑",
				indexed, total, lastID, filled, time.Since(started).Round(time.Millisecond))
			return nil
		}
		nextID, n, err := s.runBatchWithRetry(ctx, lastID, upper)
		if err != nil {
			// 被停机打断不算失败：与批间取消同一口径（水位已落库，下次启动续跑）。
			if ctx.Err() != nil {
				log.Printf("搜索索引回填已中断：已回填 %d/%d 行（水位 %d），本次补入 %d 行，耗时 %s；下次启动将从该水位续跑",
					indexed, total, lastID, filled, time.Since(started).Round(time.Millisecond))
				return nil
			}
			return fmt.Errorf("搜索索引回填失败：已回填 %d/%d 行（水位 %d），本次补入 %d 行，耗时 %s：%w",
				indexed, total, lastID, filled, time.Since(started).Round(time.Millisecond), err)
		}
		if n == 0 {
			break // 区间内已无待回填行
		}
		lastID = nextID
		filled += int64(n)
		indexed += int64(n)
		logProgress()
		// 批间让出写锁：提交已结束，这里必须给在线写入插入的机会（单写者硬约束）。
		select {
		case <-ctx.Done():
		case <-time.After(searchIndexBackfillPause):
		}
	}

	if err := s.store.MarkCompleted(); err != nil {
		return fmt.Errorf("搜索索引回填失败：标记完成出错：%w", err)
	}
	log.Printf("搜索索引回填完成：共 %d 行，本次补入 %d 行，水位 %d，耗时 %s",
		total, filled, lastID, time.Since(started).Round(time.Millisecond))
	return nil
}

// runBatchWithRetry 取一批并写入，失败按指数退避重试。
//
// 单批失败**不推进水位**（WriteBatch 的索引写入与水位推进同事务，失败即整体回滚），
// 因此重试是安全的、续跑是精确的。ctx 取消时立刻返回，不继续退避等待。
func (s *SearchIndexService) runBatchWithRetry(ctx context.Context, after, upper int64) (int64, int, error) {
	delay := searchIndexBackfillRetryBase
	var lastErr error
	for attempt := 1; attempt <= searchIndexBackfillRetries; attempt++ {
		rows, err := s.store.FetchBatch(after, upper, searchIndexBackfillBatchSize)
		if err == nil {
			if len(rows) == 0 {
				return after, 0, nil
			}
			batchLast := rows[len(rows)-1].ID
			if err = s.store.WriteBatch(rows, batchLast); err == nil {
				return batchLast, len(rows), nil
			}
		}
		lastErr = err
		if attempt == searchIndexBackfillRetries {
			break
		}
		// 被取消时不再退避：本次回填的语义是「优雅停机」，而不是「必须成功」。
		if ctx.Err() != nil {
			return after, 0, ctx.Err()
		}
		log.Printf("搜索索引回填：水位 %d 起的一批写入失败（第 %d/%d 次），%s 后重试：%v",
			after, attempt, searchIndexBackfillRetries, delay, err)
		select {
		case <-ctx.Done():
			return after, 0, ctx.Err()
		case <-time.After(delay):
		}
		delay *= 2
	}
	return after, 0, lastErr
}
