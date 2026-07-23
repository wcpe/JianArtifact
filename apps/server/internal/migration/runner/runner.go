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

	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/migration/discover"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// AssetWriter 写入目标 hosted 仓库（由 domain.AssetService 满足）。
type AssetWriter interface {
	Put(repoName, path string, r io.Reader, contentType string) (*repository.Asset, error)
	// Exists 返回路径是否已存在。
	Exists(repoName, path string) (bool, error)
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

	items, err := r.enumerate(task)
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

	items, err := r.enumerate(task)
	if err != nil {
		r.fail(taskID, fmt.Sprintf("枚举源失败：%v", err), report)
		return
	}

	// 从 checkpoint 跳过已完成项
	done := map[string]bool{}
	for _, k := range cp.Done {
		done[k] = true
	}

	for _, item := range items {
		select {
		case <-ctx.Done():
			msg := "用户取消"
			_ = r.tasks.UpdateStatus(taskID, repository.MigrationStatusCancelled, &msg, false, true)
			_ = r.tasks.SaveReportMeta(taskID, mustJSON(report))
			_ = r.tasks.SaveCheckpoint(taskID, mustJSON(cp))
			return
		default:
		}
		key := item.Repo + "\x00" + item.Path
		if done[key] {
			continue
		}

		// 确保仓库
		if err := r.repos.EnsureHosted(item.Repo, item.Format); err != nil {
			report.Failed++
			report.Failures = appendLimited(report.Failures, failEntry(item, err.Error()))
			r.fail(taskID, fmt.Sprintf("创建仓库 %s 失败：%v", item.Repo, err), report)
			_ = r.tasks.SaveCheckpoint(taskID, mustJSON(cp))
			return
		}

		exists, err := r.assets.Exists(item.Repo, item.Path)
		if err != nil {
			report.Failed++
			r.fail(taskID, err.Error(), report)
			return
		}
		switch task.ConflictPolicy {
		case repository.MigrationConflictSkip:
			if exists {
				report.Skipped++
				done[key] = true
				cp.Done = append(cp.Done, key)
				_ = r.tasks.SaveCheckpoint(taskID, mustJSON(cp))
				_ = r.tasks.SaveReportMeta(taskID, mustJSON(report))
				continue
			}
		case repository.MigrationConflictFail:
			if exists {
				report.Failed++
				report.Failures = appendLimited(report.Failures, failEntry(item, "目标路径已存在"))
				r.fail(taskID, fmt.Sprintf("冲突：%s/%s", item.Repo, item.Path), report)
				_ = r.tasks.SaveCheckpoint(taskID, mustJSON(cp))
				return
			}
		case repository.MigrationConflictOverwrite:
			// 直接 Put
		}

		rc, err := item.Open()
		if err != nil {
			report.Failed++
			report.Failures = appendLimited(report.Failures, failEntry(item, err.Error()))
			r.fail(taskID, fmt.Sprintf("打开源 %s/%s：%v", item.Repo, item.Path, err), report)
			_ = r.tasks.SaveCheckpoint(taskID, mustJSON(cp))
			return
		}
		_, putErr := r.assets.Put(item.Repo, item.Path, rc, "application/octet-stream")
		_ = rc.Close()
		if putErr != nil {
			report.Failed++
			report.Failures = appendLimited(report.Failures, failEntry(item, putErr.Error()))
			r.fail(taskID, fmt.Sprintf("写入 %s/%s：%v", item.Repo, item.Path, putErr), report)
			_ = r.tasks.SaveCheckpoint(taskID, mustJSON(cp))
			return
		}
		report.Copied++
		done[key] = true
		cp.Done = append(cp.Done, key)
		_ = r.tasks.SaveCheckpoint(taskID, mustJSON(cp))
		_ = r.tasks.SaveReportMeta(taskID, mustJSON(report))
	}

	// 完成
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

func (r *Runner) enumerate(task *repository.MigrationTask) ([]sourceItem, error) {
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

	switch task.SourceType {
	case repository.MigrationSourceOfflineBundle:
		return enumerateOfflineBundle(path, plan)
	case repository.MigrationSourceOfflineDir:
		return enumerateOfflineDir(path, plan)
	case repository.MigrationSourceOnlineREST:
		return enumerateOnlineREST(urlStr, task.CredentialRef.String, plan)
	default:
		return nil, fmt.Errorf("不支持的 sourceType %s", task.SourceType)
	}
}

func enumerateOfflineBundle(root string, plan discover.Plan) ([]sourceItem, error) {
	if root == "" {
		return nil, errors.New("sourceConfig.path 为空")
	}
	contentRoot := filepath.Join(root, "content")
	formatByRepo := map[string]string{}
	for _, r := range plan.Repositories {
		formatByRepo[r.Name] = r.Format
		if formatByRepo[r.Name] == "" {
			formatByRepo[r.Name] = "raw"
		}
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
		format := formatByRepo[repo]
		if format == "" {
			format = "raw"
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

func enumerateOfflineDir(root string, plan discover.Plan) ([]sourceItem, error) {
	if root == "" {
		return nil, errors.New("sourceConfig.path 为空")
	}
	reposRoot := filepath.Join(root, "repositories")
	if st, err := os.Stat(reposRoot); err != nil || !st.IsDir() {
		reposRoot = root
	}
	formatByRepo := map[string]string{}
	for _, r := range plan.Repositories {
		formatByRepo[r.Name] = r.Format
	}
	var items []sourceItem
	for name, format := range formatByRepo {
		if format == "" {
			format = "raw"
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
			return nil
		})
	}
	return items, nil
}

func enumerateOnlineREST(baseURL, credRef string, plan discover.Plan) ([]sourceItem, error) {
	// MVP：在线执行依赖 downloadUrl 列表；若无详细资产清单，返回明确错误引导先用离线或后续增强。
	// 为可测性：若 plan 为空仍报错。
	if baseURL == "" {
		return nil, errors.New("sourceConfig.url 为空")
	}
	_ = credRef
	// 在线完整枚举+下载在 execute 增强；当前若无本地夹具则返回错误
	return nil, errors.New("online_rest 执行请使用后续完整下载器；当前 MVP 请用 offline_bundle/offline_dir 验收")
}

type checkpoint struct {
	Done     []string `json:"done"`
	Complete bool     `json:"complete,omitempty"`
}

type execReport struct {
	Copied   int64                    `json:"copied"`
	Skipped  int64                    `json:"skipped"`
	Failed   int64                    `json:"failed"`
	Failures []map[string]interface{} `json:"failures,omitempty"`
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
	_, err = a.Repos.Create(name, format, "hosted", "private", repository.RepositoryConfig{})
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
