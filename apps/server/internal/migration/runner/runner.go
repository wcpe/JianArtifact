// Package runner 异步执行迁移任务：按 plan 流式写入 hosted + blob，支持冲突策略与协作取消。
package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/migration/discover"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// AssetWriter 写入目标 hosted 仓库（由 domain.AssetService 满足）。
type AssetWriter interface {
	Put(repoName, path string, r io.Reader, contentType string) (*repository.Asset, error)
	// Exists 返回路径是否已存在。
	Exists(repoName, path string) (bool, error)
	// LoadPathSet 一次加载仓库全部路径集合（skip 策略批量预检，可返回 nil 表示不支持）。
	LoadPathSet(repoName string) (map[string]bool, error)
}

// RepoAdmin 确保目标 hosted 仓库存在。
type RepoAdmin interface {
	EnsureHosted(name, format string) error
}

// TaskStore 任务读写。
type TaskStore interface {
	GetByID(id int64) (*repository.MigrationTask, error)
	UpdateStatus(id int64, status string, errMsg *string, setStarted, setFinished bool) error
	SaveCheckpoint(id int64, checkpointJSON string) error
	SaveReportMeta(id int64, reportJSON string) error
}

// Runner 后台执行器。
type Runner struct {
	tasks  TaskStore
	assets AssetWriter
	repos  RepoAdmin
	// 离线目录持久化索引（可选）：就绪时枚举走索引
	offlineIndex *repository.OfflineIndexRepo

	mu      sync.Mutex
	running map[int64]context.CancelFunc
	// 任务级互斥：同一 task 不并发跑两次
	locks sync.Map // int64 -> *sync.Mutex
}

// New 构造 Runner。
func New(tasks TaskStore, assets AssetWriter, repos RepoAdmin) *Runner {
	return &Runner{
		tasks:   tasks,
		assets:  assets,
		repos:   repos,
		running: make(map[int64]context.CancelFunc),
	}
}

// SetOfflineIndex 注入离线索引（可空）。
func (r *Runner) SetOfflineIndex(idx *repository.OfflineIndexRepo) {
	r.offlineIndex = idx
}

// StartAsync 实现 domain.MigrationRunner：后台 goroutine 执行。
func (r *Runner) StartAsync(taskID int64) {
	go r.run(taskID)
}

// Finalize 对 completed 任务同步做增量：再枚举源，仅复制目标不存在的路径，写入 report.delta。
func (r *Runner) Finalize(ctx context.Context, taskID int64) error {
	lk := r.taskLock(taskID)
	if !lk.TryLock() {
		return fmt.Errorf("任务 %d 正在执行", taskID)
	}
	defer lk.Unlock()

	task, err := r.tasks.GetByID(taskID)
	if err != nil {
		return err
	}
	if task.Status != repository.MigrationStatusCompleted {
		return fmt.Errorf("仅 completed 可 finalize，当前 %s", task.Status)
	}

	items, err := r.enumerate(ctx, task, nil)
	if err != nil {
		return err
	}
	var copied, skipped int64
	for _, item := range items {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if err := r.repos.EnsureHosted(item.Repo, item.Format); err != nil {
			return err
		}
		exists, err := r.assets.Exists(item.Repo, item.Path)
		if err != nil {
			return err
		}
		if exists {
			skipped++
			continue
		}
		rc, err := item.Open()
		if err != nil {
			return err
		}
		_, putErr := r.assets.Put(item.Repo, item.Path, rc, "application/octet-stream")
		_ = rc.Close()
		if putErr != nil {
			return putErr
		}
		copied++
	}
	report := loadReport(task.ReportJSON)
	// 合并 delta
	raw := map[string]interface{}{
		"copied":   report.Copied,
		"skipped":  report.Skipped,
		"failed":   report.Failed,
		"failures": report.Failures,
		"delta": map[string]interface{}{
			"copied":  copied,
			"skipped": skipped,
		},
	}
	return r.tasks.SaveReportMeta(taskID, mustJSON(raw))
}

// Cancel 协作取消：取消 context；若未在跑则无操作。
func (r *Runner) Cancel(taskID int64) {
	r.mu.Lock()
	cancel, ok := r.running[taskID]
	r.mu.Unlock()
	if ok && cancel != nil {
		cancel()
	}
}

func (r *Runner) taskLock(id int64) *sync.Mutex {
	v, _ := r.locks.LoadOrStore(id, &sync.Mutex{})
	return v.(*sync.Mutex)
}

func (r *Runner) run(taskID int64) {
	lk := r.taskLock(taskID)
	if !lk.TryLock() {
		log.Printf("迁移任务 %d 已在执行，跳过", taskID)
		return
	}
	defer lk.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	r.mu.Lock()
	r.running[taskID] = cancel
	r.mu.Unlock()
	defer func() {
		cancel()
		r.mu.Lock()
		delete(r.running, taskID)
		r.mu.Unlock()
	}()

	task, err := r.tasks.GetByID(taskID)
	if err != nil {
		log.Printf("迁移任务 %d 加载失败：%v", taskID, err)
		return
	}
	if task.Status != repository.MigrationStatusRunning {
		log.Printf("迁移任务 %d 状态为 %s，跳过执行", taskID, task.Status)
		return
	}

	report := loadReport(task.ReportJSON)
	cp := loadCheckpoint(task.CheckpointJSON)

	// plan 估算作为枚举阶段分母（发现时已统计过）
	var planEst int64
	if task.PlanJSON != "" && task.PlanJSON != "{}" {
		var plan discover.Plan
		if json.Unmarshal([]byte(task.PlanJSON), &plan) == nil {
			for _, pr := range plan.Repositories {
				planEst += pr.EstimatedAssets
			}
		}
	}

	// 时间节流写 report：约 300ms，前端 1s 轮询几乎每帧有变化
	lastFlush := time.Time{}
	flushReport := func(force bool) {
		now := time.Now()
		if !force && !lastFlush.IsZero() && now.Sub(lastFlush) < 300*time.Millisecond {
			return
		}
		lastFlush = now
		_ = r.tasks.SaveReportMeta(taskID, mustJSON(report))
	}

	// 枚举：写 phase + 中间 found（字段始终写入 JSON，便于前端）
	report.Phase = "enumerating"
	report.Found = 0
	report.Processed = 0
	report.Total = planEst
	report.Message = "正在枚举源…"
	report.Percent = 0
	flushReport(true)
	log.Printf("迁移任务 %d：正在枚举源…", taskID)

	items, err := r.enumerate(ctx, task, func(found int64, repo string) {
		report.Found = found
		if planEst > 0 {
			report.Total = planEst
			// 枚举阶段占总进度 0–50%
			report.Percent = int(min64(50, found*50/planEst))
		} else if found > 0 {
			report.Total = found
			report.Percent = int(min64(45, 5+found/10))
		}
		if repo != "" {
			report.Message = "枚举中：" + repo + "（已发现 " + itoa(found) + "）"
			report.CurrentRepo = repo
		} else {
			report.Message = "枚举中…（已发现 " + itoa(found) + "）"
		}
		flushReport(false)
	})
	if err != nil {
		r.fail(taskID, fmt.Sprintf("枚举源失败：%v", err), report)
		return
	}
	nItems := int64(len(items))
	report.Phase = "copying"
	report.Found = nItems
	report.Total = nItems
	report.Processed = 0
	report.Percent = 50
	report.Message = "枚举完成，开始处理 " + itoa(nItems) + " 项"
	report.CurrentRepo = ""
	flushReport(true)
	log.Printf("迁移任务 %d：枚举完成 items=%d，开始搬运", taskID, len(items))

	// 从 checkpoint 跳过已完成项
	done := map[string]bool{}
	for _, k := range cp.Done {
		done[k] = true
	}

	// skip/fail：按仓预加载路径集合，避免 N 次 Exists 查询
	pathCache := map[string]map[string]bool{}
	pathExists := func(repo, path string) (bool, error) {
		if set, ok := pathCache[repo]; ok {
			return set[path], nil
		}
		set, err := r.assets.LoadPathSet(repo)
		if err != nil {
			return false, err
		}
		if set == nil {
			// 不支持批量：回退单查
			return r.assets.Exists(repo, path)
		}
		pathCache[repo] = set
		return set[path], nil
	}

	const flushEvery = 20
	dirty := 0
	flushCP := func(force bool) {
		if !force && dirty < flushEvery {
			flushReport(false)
			return
		}
		_ = r.tasks.SaveCheckpoint(taskID, mustJSON(cp))
		flushReport(true)
		dirty = 0
	}

	processed := int64(0)
	for _, item := range items {
		select {
		case <-ctx.Done():
			msg := "用户取消"
			report.Message = msg
			flushCP(true)
			_ = r.tasks.UpdateStatus(taskID, repository.MigrationStatusCancelled, &msg, false, true)
			return
		default:
		}
		key := item.Repo + "\x00" + item.Path
		if done[key] {
			processed++
			report.Processed = processed
			if nItems > 0 {
				report.Percent = 50 + int(processed*50/nItems)
			}
			continue
		}

		report.CurrentRepo = item.Repo
		if err := r.repos.EnsureHosted(item.Repo, item.Format); err != nil {
			report.Failed++
			report.Failures = appendLimited(report.Failures, failEntry(item, err.Error()))
			flushCP(true)
			r.fail(taskID, fmt.Sprintf("创建仓库 %s 失败：%v", item.Repo, err), report)
			return
		}

		exists, err := pathExists(item.Repo, item.Path)
		if err != nil {
			report.Failed++
			flushCP(true)
			r.fail(taskID, err.Error(), report)
			return
		}
		switch task.ConflictPolicy {
		case repository.MigrationConflictSkip:
			if exists {
				report.Skipped++
				done[key] = true
				cp.Done = append(cp.Done, key)
				processed++
				report.Processed = processed
				if nItems > 0 {
					report.Percent = 50 + int(processed*50/nItems)
				}
				report.Message = "跳过已存在 " + itoa(report.Skipped) + " / 处理 " + itoa(processed) + "/" + itoa(nItems)
				dirty++
				flushCP(false)
				continue
			}
		case repository.MigrationConflictFail:
			if exists {
				report.Failed++
				report.Failures = appendLimited(report.Failures, failEntry(item, "目标路径已存在"))
				flushCP(true)
				r.fail(taskID, fmt.Sprintf("冲突：%s/%s", item.Repo, item.Path), report)
				return
			}
		case repository.MigrationConflictOverwrite:
			// 直接 Put
		}

		report.Message = "写入 " + item.Repo + "（已复制 " + itoa(report.Copied) + "）"
		rc, err := item.Open()
		if err != nil {
			report.Failed++
			report.Failures = appendLimited(report.Failures, failEntry(item, err.Error()))
			flushCP(true)
			r.fail(taskID, fmt.Sprintf("打开源 %s/%s：%v", item.Repo, item.Path, err), report)
			return
		}
		_, putErr := r.assets.Put(item.Repo, item.Path, rc, "application/octet-stream")
		_ = rc.Close()
		if putErr != nil {
			report.Failed++
			report.Failures = appendLimited(report.Failures, failEntry(item, putErr.Error()))
			flushCP(true)
			r.fail(taskID, fmt.Sprintf("写入 %s/%s：%v", item.Repo, item.Path, putErr), report)
			return
		}
		// overwrite 时同步缓存
		if set, ok := pathCache[item.Repo]; ok {
			set[item.Path] = true
		}
		report.Copied++
		done[key] = true
		cp.Done = append(cp.Done, key)
		processed++
		report.Processed = processed
		if nItems > 0 {
			report.Percent = 50 + int(processed*50/nItems)
		}
		dirty++
		flushCP(false)
	}

	// 完成
	report.Phase = "completed"
	report.Message = "完成：复制 " + itoa(report.Copied) + "，跳过 " + itoa(report.Skipped) + "，失败 " + itoa(report.Failed)
	report.CurrentRepo = ""
	report.Percent = 100
	report.Processed = nItems
	_ = r.tasks.SaveReportMeta(taskID, mustJSON(report))
	_ = r.tasks.SaveCheckpoint(taskID, mustJSON(checkpoint{Done: cp.Done, Complete: true}))
	_ = r.tasks.UpdateStatus(taskID, repository.MigrationStatusCompleted, nil, false, true)
	log.Printf("迁移任务 %d 完成：copied=%d skipped=%d failed=%d", taskID, report.Copied, report.Skipped, report.Failed)
}

func (r *Runner) fail(taskID int64, msg string, report *execReport) {
	_ = r.tasks.SaveReportMeta(taskID, mustJSON(report))
	_ = r.tasks.UpdateStatus(taskID, repository.MigrationStatusFailed, &msg, false, true)
	log.Printf("迁移任务 %d 失败：%s", taskID, msg)
}

// sourceItem 待复制的一项。
type sourceItem struct {
	Repo   string
	Path   string
	Format string
	Open   func() (io.ReadCloser, error)
}

func (r *Runner) enumerate(ctx context.Context, task *repository.MigrationTask, onProg discover.EnumProgress) ([]sourceItem, error) {
	var plan discover.Plan
	if task.PlanJSON != "" && task.PlanJSON != "{}" {
		if err := json.Unmarshal([]byte(task.PlanJSON), &plan); err != nil {
			return nil, fmt.Errorf("解析 plan：%w", err)
		}
	}
	var cfg map[string]any
	_ = json.Unmarshal([]byte(task.SourceConfig), &cfg)
	path, _ := cfg["path"].(string)
	urlStr, _ := cfg["url"].(string)

	// 仅迁移白名单仓库，避免全量占满磁盘（真机验收常用）
	plan.Repositories = filterPlanRepos(plan.Repositories, includeReposFromConfig(cfg))

	switch task.SourceType {
	case repository.MigrationSourceOfflineBundle:
		return enumerateOfflineBundle(path, plan)
	case repository.MigrationSourceOfflineDir:
		return r.enumerateOfflineDir(ctx, path, plan, onProg)
	case repository.MigrationSourceOnlineREST:
		cred := ""
		if task.CredentialRef.Valid && task.CredentialRef.String != "" {
			var err error
			cred, err = resolveCred(task.CredentialRef.String)
			if err != nil {
				return nil, err
			}
		}
		return enumerateOnlineREST(urlStr, cred, plan)
	default:
		return nil, fmt.Errorf("不支持的 sourceType %s", task.SourceType)
	}
}

// includeReposFromConfig 读取 sourceConfig.includeRepositories（字符串数组）。
func includeReposFromConfig(cfg map[string]any) []string {
	if cfg == nil {
		return nil
	}
	raw, ok := cfg["includeRepositories"]
	if !ok || raw == nil {
		return nil
	}
	switch v := raw.(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, x := range v {
			if s, ok := x.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

// filterPlanRepos 若 include 非空，仅保留名单内仓库。
func filterPlanRepos(repos []discover.PlanRepository, include []string) []discover.PlanRepository {
	if len(include) == 0 {
		return repos
	}
	allow := make(map[string]bool, len(include))
	for _, n := range include {
		allow[n] = true
	}
	out := make([]discover.PlanRepository, 0, len(include))
	for _, r := range repos {
		if allow[r.Name] {
			out = append(out, r)
		}
	}
	return out
}

func enumerateOfflineBundle(root string, plan discover.Plan) ([]sourceItem, error) {
	if root == "" {
		return nil, errors.New("sourceConfig.path 为空")
	}
	contentRoot := filepath.Join(root, "content")
	// 仅处理 plan 中的仓库（支持 includeRepositories 多选后收窄）
	formatByRepo := map[string]string{}
	for _, r := range plan.Repositories {
		formatByRepo[r.Name] = r.Format
		if formatByRepo[r.Name] == "" {
			formatByRepo[r.Name] = "raw"
		}
	}
	if len(formatByRepo) == 0 {
		return nil, nil
	}
	var items []sourceItem
	err := filepath.WalkDir(contentRoot, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(contentRoot, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		parts := strings.SplitN(rel, "/", 2)
		if len(parts) < 2 {
			return nil
		}
		repo, assetPath := parts[0], parts[1]
		format, ok := formatByRepo[repo]
		if !ok {
			// 不在 plan 内的仓库整仓跳过
			return nil
		}
		full := p
		items = append(items, sourceItem{
			Repo:   repo,
			Path:   assetPath,
			Format: format,
			Open: func() (io.ReadCloser, error) {
				return os.Open(full)
			},
		})
		return nil
	})
	return items, err
}

func (r *Runner) enumerateOfflineDir(ctx context.Context, root string, plan discover.Plan, onProg discover.EnumProgress) ([]sourceItem, error) {
	if root == "" {
		return nil, errors.New("sourceConfig.path 为空")
	}
	formatByRepo := map[string]string{}
	for _, pr := range plan.Repositories {
		formatByRepo[pr.Name] = pr.Format
		if formatByRepo[pr.Name] == "" {
			formatByRepo[pr.Name] = "maven"
		}
	}
	if len(formatByRepo) == 0 {
		return nil, nil
	}

	// 优先读持久化索引
	if r.offlineIndex != nil {
		if items, ok, err := r.enumerateFromOfflineIndex(ctx, root, formatByRepo, onProg); err != nil {
			return nil, err
		} else if ok {
			return items, nil
		}
	}

	// Nexus blob store：.../blobs/default 或 content 下 vol-*
	if contentRoot, ok := nexusBlobContentRoot(root); ok {
		return enumerateNexusBlobStore(ctx, contentRoot, formatByRepo, onProg)
	}

	reposRoot := filepath.Join(root, "repositories")
	if st, err := os.Stat(reposRoot); err != nil || !st.IsDir() {
		reposRoot = root
	}
	var items []sourceItem
	var found int64
	for name, format := range formatByRepo {
		if err := ctx.Err(); err != nil {
			return items, err
		}
		content := filepath.Join(reposRoot, name, "content")
		if _, err := os.Stat(content); err != nil {
			content = filepath.Join(reposRoot, name)
		}
		_ = filepath.WalkDir(content, func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			if d.Name() == ".format" {
				return nil
			}
			rel, err := filepath.Rel(content, p)
			if err != nil {
				return nil
			}
			assetPath := filepath.ToSlash(rel)
			full := p
			fmtName := format
			repoName := name
			items = append(items, sourceItem{
				Repo:   repoName,
				Path:   assetPath,
				Format: fmtName,
				Open: func() (io.ReadCloser, error) {
					return os.Open(full)
				},
			})
			found++
			if onProg != nil {
				onProg(found, name)
			}
			return nil
		})
	}
	return items, nil
}

func nexusBlobContentRoot(root string) (string, bool) {
	content := filepath.Join(root, "content")
	if st, err := os.Stat(content); err == nil && st.IsDir() {
		entries, _ := os.ReadDir(content)
		for _, e := range entries {
			if e.IsDir() && strings.HasPrefix(e.Name(), "vol-") {
				return content, true
			}
		}
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", false
	}
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), "vol-") {
			return root, true
		}
	}
	return "", false
}

// enumerateFromOfflineIndex 索引就绪时从 DB 枚举；返回 ok=false 表示无可用索引。
func (r *Runner) enumerateFromOfflineIndex(ctx context.Context, root string, formatByRepo map[string]string, onProg discover.EnumProgress) ([]sourceItem, bool, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, false, nil
	}
	meta, err := r.offlineIndex.Get(abs)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, false, nil
		}
		return nil, false, err
	}
	if meta.Status != repository.OfflineIndexReady {
		return nil, false, nil
	}
	repos := make([]string, 0, len(formatByRepo))
	for name := range formatByRepo {
		repos = append(repos, name)
	}
	entries, err := r.offlineIndex.ListEntries(abs, repos)
	if err != nil {
		return nil, false, err
	}
	items := make([]sourceItem, 0, len(entries))
	var found int64
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return items, true, err
		}
		format, ok := formatByRepo[e.Repo]
		if !ok {
			continue
		}
		full := e.BytesPath
		items = append(items, sourceItem{
			Repo:   e.Repo,
			Path:   e.AssetPath,
			Format: format,
			Open: func() (io.ReadCloser, error) {
				return os.Open(full)
			},
		})
		found++
		if onProg != nil {
			onProg(found, e.Repo)
		}
	}
	return items, true, nil
}

func enumerateNexusBlobStore(ctx context.Context, contentRoot string, formatByRepo map[string]string, onProg discover.EnumProgress) ([]sourceItem, error) {
	if len(formatByRepo) == 0 {
		return nil, nil
	}
	repos := make([]string, 0, len(formatByRepo))
	for name := range formatByRepo {
		repos = append(repos, name)
	}
	blobs, err := discover.EnumerateNexusBlobAssetsWithProgress(ctx, contentRoot, repos, onProg)
	if err != nil {
		return nil, err
	}
	items := make([]sourceItem, 0, len(blobs))
	for _, b := range blobs {
		format, ok := formatByRepo[b.Repo]
		if !ok {
			continue
		}
		full := b.BytesPath
		items = append(items, sourceItem{
			Repo:   b.Repo,
			Path:   b.Path,
			Format: format,
			Open: func() (io.ReadCloser, error) {
				return os.Open(full)
			},
		})
	}
	return items, nil
}

type checkpoint struct {
	Done     []string `json:"done"`
	Complete bool     `json:"complete,omitempty"`
}

type execReport struct {
	Copied  int64 `json:"copied"`
	Skipped int64 `json:"skipped"`
	Failed  int64 `json:"failed"`
	// Phase：enumerating | copying | completed（不 omitempty，前端始终可读）
	Phase string `json:"phase"`
	// Found：枚举阶段已发现条数
	Found int64 `json:"found"`
	// Processed：搬运阶段已处理条数
	Processed int64 `json:"processed"`
	// Total：估算或总数（进度分母）
	Total int64 `json:"total"`
	// Percent：0–100 综合进度
	Percent int `json:"percent"`
	// Message：人类可读阶段说明
	Message string `json:"message"`
	// CurrentRepo：当前处理仓库
	CurrentRepo string                   `json:"currentRepo"`
	Failures    []map[string]interface{} `json:"failures,omitempty"`
}

func itoa(n int64) string {
	return fmt.Sprintf("%d", n)
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func loadCheckpoint(s string) checkpoint {
	var c checkpoint
	if s == "" || s == "{}" {
		return c
	}
	_ = json.Unmarshal([]byte(s), &c)
	return c
}

func loadReport(s string) *execReport {
	r := &execReport{}
	if s == "" || s == "{}" {
		return r
	}
	_ = json.Unmarshal([]byte(s), r)
	return r
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

func failEntry(item sourceItem, msg string) map[string]interface{} {
	return map[string]interface{}{"repo": item.Repo, "path": item.Path, "error": msg}
}

func appendLimited(list []map[string]interface{}, item map[string]interface{}) []map[string]interface{} {
	const max = 50
	if len(list) >= max {
		return list
	}
	return append(list, item)
}

// AssetServiceAdapter 适配 domain.AssetService。
type AssetServiceAdapter struct {
	Assets *domain.AssetService
	Repos  *repository.RepoRepo
	AssetR *repository.AssetRepo
}

func (a AssetServiceAdapter) Put(repoName, path string, r io.Reader, contentType string) (*repository.Asset, error) {
	return a.Assets.Put(repoName, path, r, contentType)
}

func (a AssetServiceAdapter) Exists(repoName, path string) (bool, error) {
	repo, err := a.Repos.GetByName(repoName)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	_, err = a.AssetR.GetByPath(repo.ID, path)
	if errors.Is(err, repository.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (a AssetServiceAdapter) LoadPathSet(repoName string) (map[string]bool, error) {
	repo, err := a.Repos.GetByName(repoName)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return map[string]bool{}, nil
		}
		return nil, err
	}
	paths, err := a.AssetR.ListAllPaths(repo.ID)
	if err != nil {
		return nil, err
	}
	set := make(map[string]bool, len(paths))
	for _, p := range paths {
		set[p] = true
	}
	return set, nil
}

// RepoAdminAdapter 确保 hosted 仓库。
type RepoAdminAdapter struct {
	Repos *domain.RepositoryService
}

func (a RepoAdminAdapter) EnsureHosted(name, format string) error {
	_, err := a.Repos.Get(name)
	if err == nil {
		return nil
	}
	if !errors.Is(err, domain.ErrNotFound) {
		return err
	}
	if format == "" {
		format = "raw"
	}
	_, err = a.Repos.Create(name, format, "hosted", "private", "", repository.RepositoryConfig{})
	return err
}

// TaskStoreAdapter 适配 MigrationTaskRepo。
type TaskStoreAdapter struct {
	Repo *repository.MigrationTaskRepo
}

func (a TaskStoreAdapter) GetByID(id int64) (*repository.MigrationTask, error) {
	return a.Repo.GetByID(id)
}

func (a TaskStoreAdapter) UpdateStatus(id int64, status string, errMsg *string, setStarted, setFinished bool) error {
	return a.Repo.UpdateStatus(id, status, errMsg, setStarted, setFinished)
}

func (a TaskStoreAdapter) SaveCheckpoint(id int64, checkpointJSON string) error {
	return a.Repo.SaveCheckpoint(id, checkpointJSON)
}

func (a TaskStoreAdapter) SaveReportMeta(id int64, reportJSON string) error {
	return a.Repo.SaveReportMeta(id, reportJSON)
}
