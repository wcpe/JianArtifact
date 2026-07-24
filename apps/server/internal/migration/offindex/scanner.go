// Package offindex 离线目录（Nexus blob）前置索引：全量/更新/重建扫描。
package offindex

import (
	"bufio"
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/migration/discover"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// Scanner 管理离线索引扫描任务。
type Scanner struct {
	repo *repository.OfflineIndexRepo

	mu      sync.Mutex
	running map[string]context.CancelFunc // root -> cancel
}

// New 构造 Scanner。
func New(repo *repository.OfflineIndexRepo) *Scanner {
	return &Scanner{
		repo:    repo,
		running: make(map[string]context.CancelFunc),
	}
}

// ScanMode full=清空重建；update=合并扫描并删失效；rebuild=同 full。
type ScanMode string

const (
	ModeFull    ScanMode = "full"
	ModeUpdate  ScanMode = "update"
	ModeRebuild ScanMode = "rebuild"
)

// StartScan 异步启动扫描；mode 为 full|update|rebuild；同一 path 已在扫则返回错误。
func (s *Scanner) StartScan(root string, mode string) error {
	abs, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("路径无效: %w", err)
	}
	st, err := os.Stat(abs)
	if err != nil || !st.IsDir() {
		return fmt.Errorf("离线目录不存在或不是目录")
	}
	if !isBlobStore(abs) {
		return fmt.Errorf("路径不是 Nexus blob store（未发现 content/vol-*）")
	}
	m := ScanMode(strings.TrimSpace(mode))
	if m == "" || m == ModeRebuild {
		m = ModeFull
	}
	if m != ModeFull && m != ModeUpdate {
		return fmt.Errorf("mode 须为 full / update / rebuild")
	}
	return s.startScan(abs, m)
}

func (s *Scanner) startScan(abs string, mode ScanMode) error {

	s.mu.Lock()
	if _, ok := s.running[abs]; ok {
		s.mu.Unlock()
		return fmt.Errorf("该目录索引正在扫描中")
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.running[abs] = cancel
	s.mu.Unlock()

	meta := repository.OfflineDirIndex{
		RootPath:  abs,
		Status:    repository.OfflineIndexScanning,
		Mode:      string(mode),
		Message:   "扫描启动…",
		StartedAt: sql.NullString{String: time.Now().UTC().Format("2006-01-02 15:04:05"), Valid: true},
	}
	_ = s.repo.UpsertMeta(meta)

	go s.run(ctx, abs, mode)
	return nil
}

// Ensure StartScan 签名与 domain.OfflineIndexScanner 一致。
var _ interface {
	StartScan(root string, mode string) error
	Cancel(root string)
	Status(root string) (*repository.OfflineDirIndex, error)
} = (*Scanner)(nil)

// Cancel 取消扫描。
func (s *Scanner) Cancel(root string) {
	abs, _ := filepath.Abs(root)
	s.mu.Lock()
	cancel, ok := s.running[abs]
	s.mu.Unlock()
	if ok && cancel != nil {
		cancel()
	}
}

// Status 返回索引元数据；无记录时构造 idle。
func (s *Scanner) Status(root string) (*repository.OfflineDirIndex, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	row, err := s.repo.Get(abs)
	if err == nil {
		return row, nil
	}
	if err == repository.ErrNotFound {
		return &repository.OfflineDirIndex{
			RootPath: abs,
			Status:   repository.OfflineIndexIdle,
			Message:  "尚未建立索引",
		}, nil
	}
	return nil, err
}

func (s *Scanner) run(ctx context.Context, abs string, mode ScanMode) {
	defer func() {
		s.mu.Lock()
		delete(s.running, abs)
		s.mu.Unlock()
	}()

	contentRoot := contentRootOf(abs)
	clearFirst := mode == ModeFull || mode == ModeRebuild
	if clearFirst {
		if err := s.repo.ClearEntries(abs); err != nil {
			s.fail(abs, mode, "清空旧索引失败: "+err.Error())
			return
		}
	}

	var (
		scanned int64
		batch   []repository.OfflineDirIndexEntry
		keep    []string
		flushN  = 200
	)
	lastFlush := time.Time{}
	saveProgress := func(force bool, msg string) {
		now := time.Now()
		if !force && !lastFlush.IsZero() && now.Sub(lastFlush) < 400*time.Millisecond {
			return
		}
		lastFlush = now
		total, _ := s.repo.TotalEntries(abs)
		counts, _ := s.repo.CountByRepo(abs)
		_ = s.repo.UpsertMeta(repository.OfflineDirIndex{
			RootPath:     abs,
			Status:       repository.OfflineIndexScanning,
			Mode:         string(mode),
			TotalEntries: total,
			ScannedProps: scanned,
			RepoCount:    int64(len(counts)),
			Message:      msg,
			StartedAt:    sql.NullString{String: now.UTC().Format("2006-01-02 15:04:05"), Valid: true},
		})
	}

	flushBatch := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := s.repo.InsertEntries(batch); err != nil {
			return err
		}
		batch = batch[:0]
		return nil
	}

	// 流式：Walk 全部 .properties（全量索引不按仓过滤）
	err := filepath.WalkDir(contentRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".properties") {
			return nil
		}
		scanned++
		meta, err := readProps(path)
		if err != nil {
			return nil
		}
		if meta["deleted"] == "true" {
			return nil
		}
		repo := meta["@Bucket.repo-name"]
		blobName := meta["@BlobStore.blob-name"]
		if repo == "" || blobName == "" {
			return nil
		}
		bytesPath := strings.TrimSuffix(path, ".properties") + ".bytes"
		if _, err := os.Stat(bytesPath); err != nil {
			return nil
		}
		var mtime int64
		if fi, err := os.Stat(path); err == nil {
			mtime = fi.ModTime().Unix()
		}
		assetPath := strings.TrimPrefix(blobName, "/")
		batch = append(batch, repository.OfflineDirIndexEntry{
			RootPath:  abs,
			Repo:      repo,
			AssetPath: assetPath,
			BytesPath: bytesPath,
			PropPath:  path,
			PropMtime: mtime,
		})
		if mode == ModeUpdate {
			keep = append(keep, path)
		}
		if len(batch) >= flushN {
			if err := flushBatch(); err != nil {
				return err
			}
		}
		if scanned%50 == 0 {
			saveProgress(false, fmt.Sprintf("扫描中… 已处理 properties=%d", scanned))
		}
		return nil
	})

	if err != nil && err != context.Canceled {
		s.fail(abs, mode, "扫描失败: "+err.Error())
		return
	}
	if ctx.Err() != nil {
		s.fail(abs, mode, "用户取消扫描")
		return
	}
	if err := flushBatch(); err != nil {
		s.fail(abs, mode, "写入索引失败: "+err.Error())
		return
	}
	if mode == ModeUpdate {
		if _, err := s.repo.DeleteMissingProps(abs, keep); err != nil {
			log.Printf("离线索引清理失效条目：%v", err)
		}
	}

	total, _ := s.repo.TotalEntries(abs)
	counts, _ := s.repo.CountByRepo(abs)
	now := time.Now().UTC().Format("2006-01-02 15:04:05")
	_ = s.repo.UpsertMeta(repository.OfflineDirIndex{
		RootPath:     abs,
		Status:       repository.OfflineIndexReady,
		Mode:         string(mode),
		TotalEntries: total,
		ScannedProps: scanned,
		RepoCount:    int64(len(counts)),
		Message:      fmt.Sprintf("索引就绪：%d 资产 / %d 仓库", total, len(counts)),
		StartedAt:    sql.NullString{String: now, Valid: true},
		FinishedAt:   sql.NullString{String: now, Valid: true},
	})
	log.Printf("离线索引完成 path=%s entries=%d repos=%d", abs, total, len(counts))
}

func (s *Scanner) fail(abs string, mode ScanMode, msg string) {
	_ = s.repo.UpsertMeta(repository.OfflineDirIndex{
		RootPath:     abs,
		Status:       repository.OfflineIndexFailed,
		Mode:         string(mode),
		Message:      msg,
		ErrorMessage: sql.NullString{String: msg, Valid: true},
		FinishedAt:   sql.NullString{String: time.Now().UTC().Format("2006-01-02 15:04:05"), Valid: true},
	})
	log.Printf("离线索引失败 path=%s: %s", abs, msg)
}

func isBlobStore(root string) bool {
	content := filepath.Join(root, "content")
	if st, err := os.Stat(content); err == nil && st.IsDir() {
		entries, _ := os.ReadDir(content)
		for _, e := range entries {
			if e.IsDir() && strings.HasPrefix(e.Name(), "vol-") {
				return true
			}
		}
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), "vol-") {
			return true
		}
	}
	return false
}

func contentRootOf(root string) string {
	content := filepath.Join(root, "content")
	if st, err := os.Stat(content); err == nil && st.IsDir() {
		return content
	}
	return root
}

func readProps(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	buf := make([]byte, 0, 64*1024)
	sc.Buffer(buf, 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		i := strings.IndexByte(line, '=')
		if i <= 0 {
			continue
		}
		out[strings.TrimSpace(line[:i])] = strings.TrimSpace(line[i+1:])
	}
	return out, sc.Err()
}

// PlanFromIndex 用索引生成 discover Plan（ready 时）。
func PlanFromIndex(repo *repository.OfflineIndexRepo, root string, include []string) (discover.Plan, bool, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return discover.Plan{}, false, err
	}
	meta, err := repo.Get(abs)
	if err != nil {
		if err == repository.ErrNotFound {
			return discover.Plan{}, false, nil
		}
		return discover.Plan{}, false, err
	}
	if meta.Status != repository.OfflineIndexReady {
		return discover.Plan{}, false, nil
	}
	counts, err := repo.CountByRepo(abs)
	if err != nil {
		return discover.Plan{}, false, err
	}
	allow := map[string]bool{}
	for _, n := range include {
		if n != "" {
			allow[n] = true
		}
	}
	plan := discover.Plan{
		Repositories: []discover.PlanRepository{},
		Warnings:     []string{"使用离线目录索引（无需再扫 blob）"},
		Stats:        map[string]any{},
		Estimated:    false,
	}
	for name, n := range counts {
		if len(allow) > 0 && !allow[name] {
			continue
		}
		plan.Repositories = append(plan.Repositories, discover.PlanRepository{
			Name:            name,
			Format:          discover.FormatMaven,
			Type:            "hosted",
			EstimatedAssets: n,
		})
	}
	if len(plan.Repositories) == 0 && len(allow) > 0 {
		plan.Warnings = append(plan.Warnings, "索引中未匹配 includeRepositories")
	}
	// finalize stats
	var total int64
	for _, r := range plan.Repositories {
		total += r.EstimatedAssets
	}
	plan.Stats["repositoryCount"] = len(plan.Repositories)
	plan.Stats["estimatedAssets"] = total
	plan.Stats["indexRoot"] = abs
	return plan, true, nil
}
