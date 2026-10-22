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

// asset_path_fts 的后台异步回填、只读状态快照与全量重建。
//
// 本文件只做**编排**：批次读写与状态推进在 repository.SearchIndexStateRepo，索引行原语在
// repository/search_index_repo.go。之所以单独一个服务而不是塞进调度器，是因为回填的
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
	// Ready 表示索引可用于加速搜索：回填已完成（completed_at 非空）且**没有漏索引**。
	// 为 false 时上层应把搜索当作「尚未提速」看待。
	//
	// 只有 MissingRows > 0 才会把 Ready 打下来：孤儿索引行（OrphanRows）不伪造命中、也不让
	// 结果缺失，它由仓库级联删除天然产生，若也判成未就绪，任何删过仓库的实例都会永久
	// ready=false，真正的漏索引信号会被这份噪音埋掉。
	//
	// 注意 Ready 只影响**报告**：真正的查询路径判定在 repository.AssetRepo.SearchIndexReady
	// （只看 completed_at）。因此漂移被检出后，必须由观测点显式降级（见 checkDrift /
	// demoteOnMissing）才会让搜索落到降级路径；在降级之前索引仍被使用，只是可能漏行。
	// 这正是必须把漂移单独暴露出来的原因。
	Ready bool
	// Indexed 是已回填行数（按 last_id 水位回 asset 真源计数）。
	Indexed int64
	// Total 是回填开始时的 asset 行数快照，仅用于进度展示与一致性校验。
	Total int64
	// LastID 是已回填到的最大 asset.id（水位），0 表示尚未开始。
	LastID int64
	// UpdatedAt 是状态行最后一次变更时刻，供排查「回填是否还在推进」。
	UpdatedAt string
	// IndexRows 是索引里真实存在的文档数（读 FTS5 的 docsize 影子表，见
	// repository.SearchIndexStateRepo.MeasureDrift）。与 Indexed 的区别：后者是「水位以内的
	// 资产行数」，前者是「索引里到底有多少条」。
	IndexRows int64
	// AssetRows 是 asset 真源当前的行数。
	AssetRows int64
	// MissingRows 是**漏索引**的行数：asset 里有、索引里没有。危险方向，会把 Ready 打下来。
	MissingRows int64
	// OrphanRows 是**孤儿索引行**的行数：索引里有、asset 已不存在。只浪费空间，不影响 Ready。
	OrphanRows int64
}

// MissingIndexRows 报告是否存在**漏索引**（存在 asset 行但索引里没有它）。
//
// 只把这一侧计入 Ready：孤儿索引行（OrphanRows）不伪造命中、也不让结果缺失，代价只是索引
// 偏大——仓库级联删除天然会留下它，若也判成「未就绪」，任何一次删仓库都会把 ready 永久打成
// false，信号很快会被运维忽略。反过来，漏索引意味着有制品搜不到，必须让 ready 说出来。
func (s SearchIndexStatus) MissingIndexRows() bool { return s.MissingRows > 0 }

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
	// DemoteOnMissing 在检出漏索引时把完成标记降级为未完成，使查询退回全表检索。
	DemoteOnMissing() error
	// Reset 清空索引并重置水位（completed_at 归 NULL）。
	Reset() error
	// CountIndexed 数出水位以内的 asset 行数。
	CountIndexed(lastID int64) (int64, error)
	// MeasureDrift 取「索引行数 vs 资产行数」的对比快照。
	MeasureDrift() (repository.SearchIndexDrift, error)
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
//
// 除进度字段外，每次都现算一次「索引行数 vs 资产行数」的漂移：索引正确性原本完全依赖
// 「所有 asset 写入必经仓储层」这一约定，而 completed_at 一旦置位就永不再回填，此后任何
// 旁路写入都会留下永久静默的漂移——状态若只报进度，用户唯一能感知到的就是「某些制品搜不到」。
//
// **该端点不得被前端轮询。** 每次调用要在 asset 与 FTS 影子表上各做一次全表计数，成本与
// 表大小线性相关（百万级 asset 上是秒级开销），且刻意**没有**加缓存：漂移是「旁路写入后
// 多久能被看见」的观测信号，加缓存会让刚发生的漂移在窗口内报成正常。调用场景限定为运维
// 手动查询（以及测试里的收敛轮询），任何人要接自动轮询前先读这段。
//
// 口径补充：四个数字一致**不等于**索引内容正确。path 被就地 UPDATE 时索引文本不会重建，
// 行数却完全一致（盲区细节见 repository.SearchIndexStateRepo.MeasureDrift）。
func (s *SearchIndexService) Status() (SearchIndexStatus, error) {
	state, err := s.store.Load()
	if err != nil {
		return SearchIndexStatus{}, fmt.Errorf("读取搜索索引状态：%w", err)
	}
	indexed, err := s.store.CountIndexed(state.LastID)
	if err != nil {
		return SearchIndexStatus{}, fmt.Errorf("统计已回填行数：%w", err)
	}
	drift, err := s.store.MeasureDrift()
	if err != nil {
		return SearchIndexStatus{}, fmt.Errorf("统计索引漂移：%w", err)
	}
	status := SearchIndexStatus{
		Indexed:     indexed,
		Total:       state.TotalRows,
		LastID:      state.LastID,
		UpdatedAt:   state.UpdatedAt,
		IndexRows:   drift.IndexRows,
		AssetRows:   drift.AssetRows,
		MissingRows: drift.MissingRows,
		OrphanRows:  drift.OrphanRows,
	}
	status.Ready = state.Completed() && !status.MissingIndexRows()
	return status, nil
}

// checkDrift 取一次「索引行数 vs 资产行数」的比对快照（**只读**，不改任何状态）。
//
// stage 是调用点的中文前缀（启动自检 / 回填完成自检），让运维一眼看出这是哪一次比对。
//
// 之所以把判定单独拎出来：漂移判定现在参与「能不能标记完成」这一决定，判定、记日志、降级
// 必须是三个可以分别指认的动作。上一版只有一个名为 logDrift 的函数，实际内部会调
// DemoteOnMissing 改状态——这正是「先置完成、再自检」这个顺序错误长期没被认出来的原因。
//
// 比对失败时如实返回错误，由调用方决定完整性未知时怎么办（见 backfill 里两处调用点：
// 回填完成点选择**不置位**，启动自检选择保持既有标记）。
func (s *SearchIndexService) checkDrift(stage string) (repository.SearchIndexDrift, error) {
	drift, err := s.store.MeasureDrift()
	if err != nil {
		log.Printf("%s：无法比对索引行数与资产行数：%v", stage, err)
		return repository.SearchIndexDrift{}, err
	}
	return drift, nil
}

// logDrift 打一条「索引行数 vs 资产行数」的中文自检日志（**不含任何制品路径**：路径是用户数据，
// 不该出现在运行日志里）。纯日志，不参与任何状态变更。
//
// 两个方向分开说：漏索引会让制品搜不到（必须重建），孤儿行只是索引偏大（可择机重建）。
// 合并成一个「差额」会掩盖「漏 k 行 + 孤儿 k 行」这种自相抵消的形态。
func logDrift(stage string, drift repository.SearchIndexDrift) {
	if !drift.HasMissing() && drift.OrphanRows == 0 {
		log.Printf("%s：索引 %d 行与资产 %d 行一一对应，无漂移", stage, drift.IndexRows, drift.AssetRows)
		return
	}
	hint := "孤儿索引行只占用索引空间、不影响结果，可择机重建（POST /api/v1/search/index/rebuild）"
	if drift.HasMissing() {
		hint = "这些制品搜不到，需重建索引恢复（POST /api/v1/search/index/rebuild）"
	}
	log.Printf("%s：索引漂移——索引 %d 行、资产 %d 行、漏索引 %d 行、孤儿索引行 %d 行；%s",
		stage, drift.IndexRows, drift.AssetRows, drift.MissingRows, drift.OrphanRows, hint)
}

// demoteOnMissing 在检出漏索引时把完成标记降级为未完成，让查询退回全表检索（显式的状态变更）。
//
// 为什么必须联动：查询路径的就绪判定只看 completed_at（repository.AssetRepo.SearchIndexReady），
// 若不联动，端点会报 ready=false 而搜索仍在用索引、继续静默少返回行。清掉 completed_at 后
// 搜索立即退回 LIKE（慢但完整），重建完成时 MarkCompleted 自动恢复。
//
// 只降级、不重建：重建是管理员动作（可能要跑很久），自动触发会把一次偶发漂移放大成长时间的
// 慢搜索。
func (s *SearchIndexService) demoteOnMissing(stage string) {
	if err := s.store.DemoteOnMissing(); err != nil {
		log.Printf("%s：漏索引降级失败（搜索仍在用不完整索引）：%v", stage, err)
		return
	}
	log.Printf("%s：已把搜索降级为全表检索（结果完整、速度下降），重建索引即可恢复加速", stage)
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

// Running 报告是否已有回填或重建任务在执行（只读，不占用运行槽）。
//
// 存在的理由：Rebuild 是**异步受理**（返回 nil 只表示已入队），调用方若需要「任务真的结束」
// 而不是「数据看起来够了」，必须能观测运行槽。测试用它避免在后台协程仍在收尾时就关掉数据库。
func (s *SearchIndexService) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

// end 释放运行槽。
func (s *SearchIndexService) end() {
	s.mu.Lock()
	s.running = false
	s.mu.Unlock()
}

// backfill 是 §3.2 的算法本体：读水位 → 上界快照 → 分批推进 → 完成自检 → 置完成。
//
// 返回错误表示本次回填失败或**无法确认完整**（此时不置 completed_at；已推进的水位保留，
// 下次启动续跑）。ctx 取消不算失败：把已推进的水位留在库里，返回 nil。
func (s *SearchIndexService) backfill(ctx context.Context) error {
	started := time.Now()
	state, err := s.store.Load()
	if err != nil {
		return fmt.Errorf("搜索索引回填失败：读取回填状态出错：%w", err)
	}
	if state.Completed() {
		// 已完成：立刻退出，不做任何回填（这是每次进程启动的常态路径）。
		//
		// 但「不再回填」不等于「不检查」：completed_at 一旦置位，回填就永不再跑，此后任何绕过
		// 仓储层的写入都会留下永久静默的漂移。这条早退分支正是进程启动的必经之路，因此在
		// 这里做一次行数比对并记日志——它是「某些制品搜不到」唯一的早期信号。
		drift, driftErr := s.checkDrift("搜索索引启动自检")
		if driftErr != nil {
			// 比对失败时保持既有标记：completed_at 是上一次「校验通过」留下的结论，一次测量
			// 失败不该把用户直接打回全表检索。若漂移是真的，下一次测量成功时会降级并说明。
			return nil
		}
		logDrift("搜索索引启动自检", drift)
		if drift.HasMissing() {
			s.demoteOnMissing("搜索索引启动自检")
		}
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

	// 完成自检必须**先于** MarkCompleted，理由是一条完整的失败链：
	// completed_at 一旦置位，查询路径立刻改用这份索引返回结果（SearchIndexReady 只看它）；
	// 而漏掉的行走水位之前的 id，下次启动 FetchBatch(lastID=水位, upper) 返回空集，于是又
	// 走到这里并把 completed_at 重新置位——缺口从此没有任何自愈路径，用户看到「搜索少结果」，
	// 运维看到「一切正常」，且每次重启都会重演同一个窗口。
	drift, driftErr := s.checkDrift("搜索索引回填完成自检")
	if driftErr != nil {
		// 完整性未知时不置位：宁可下次启动重跑（搜索走 LIKE，正确但慢），也不要带着未知缺口
		// 标记完成。
		return fmt.Errorf("搜索索引回填已跑完全部批次，但无法比对索引与资产行数，因此未标记完成（搜索继续走全表检索，下次启动重跑）：%w", driftErr)
	}
	logDrift("搜索索引回填完成自检", drift)
	if drift.HasMissing() {
		// 漏索引的行 id ≤ 水位，续跑永远覆盖不到它们，只能靠全量重建修复，因此这里既不置位
		// 也不假装成功。显式降级一次：本路径里 completed_at 通常本就是 NULL（回填的前提），
		// 这里覆盖的是「回填期间被外部置位」的极端情形，让「检出漏索引 ⇒ 未完成」成为一条
		// 不依赖调用点的硬约束。
		s.demoteOnMissing("搜索索引回填完成自检")
		return fmt.Errorf("搜索索引回填未标记完成：仍检出 %d 行漏索引（水位 %d 已到上界，续跑无法覆盖它们），搜索继续走全表检索；需重建索引（POST /api/v1/search/index/rebuild），耗时 %s",
			drift.MissingRows, lastID, time.Since(started).Round(time.Millisecond))
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
