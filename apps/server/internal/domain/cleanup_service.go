package domain

// 本文件实现定向清理：把"已确认可回收"的四类垃圾**移入回收站**，而不是物理删除。
//
// 三条必须守住的口径：
//
//  1. **移入而不是删除**：清理是批量动作，点错一次就不可逆是不可接受的。所有目标统一
//     rename 到 data/trash/<entryId>/，原路径记进 trash_entry；恢复时原样放回。
//     只有回收站的 purge 才真正释放磁盘。
//
//  2. **白名单按"具名根 + 相对路径"判定，不做字符串前缀比较**：后者会被
//     `/data/blobs-evil` 这类同前缀兄弟目录绕过，也会被 `../` 逃逸绕过。
//     因此实现是「先算出相对路径，再拒绝任何 `..` 段」，而不是 `strings.HasPrefix`。
//
//  3. **预览与执行必须共用同一套候选计算**：两处各写一遍，迟早漂移成"预览说 3 项、
//     实际删 5 项"。实现上 Preview 与 Execute 都调 collectCandidates，execute 另外用
//     客户端回传的 items 做子集校验（不在候选集里的路径导致整批拒绝，不做部分信任）。

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// 四类可回收垃圾。取值同时是 clean_task.kind 与 trash_entry.kind。
const (
	CleanupKindEmptyDirs   = "empty_dirs"
	CleanupKindStaleUpload = "stale_uploads"
	CleanupKindPreRestore  = "pre_restore"
	CleanupKindQuarantine  = "quarantine"
)

// TrashDirName 是回收站在数据目录下的位置。
const TrashDirName = "trash"

// preRestoreKeepFloor 是 pre-restore 目录的**保底保留数**：即使调用方传 keepLast=0，
// 也至少保留最新一个。回滚备份是"恢复失败时唯一的退路"，允许一键删光等于把安全网撤掉。
const preRestoreKeepFloor = 1

// CleanupCandidate 是一个待回收项。
type CleanupCandidate struct {
	// Path 是相对数据目录的路径（用 / 分隔），对外只暴露相对路径：
	// 绝对路径会泄漏部署目录结构，且让前端有能力构造任意路径。
	Path  string `json:"path"`
	IsDir bool   `json:"isDir"`
	Bytes int64  `json:"bytes"`
}

// CleanupPreview 是一次预览结果。
type CleanupPreview struct {
	Kind       string             `json:"kind"`
	Items      []CleanupCandidate `json:"items"`
	ItemCount  int                `json:"itemCount"`
	FreedBytes int64              `json:"freedBytes"`
	// Skipped 说明因安全原因被跳过的项（符号链接、无法读取等），让"预览比实际少"可解释。
	Skipped []string `json:"skipped,omitempty"`
}

// CleanupService 执行四类定向清理并把目标移入回收站。
type CleanupService struct {
	dataDir string
	tasks   *repository.CleanTaskRepo
	// backupUploadExpired 返回过期上传会话的目录名；nil 表示不处理该类。
	backupUploadExpired func() ([]string, error)
	// pendingRestore 判断某隔离区操作是否仍被待生效恢复引用；nil 表示一律视为未被引用。
	pendingRestore func(operationID string) bool
	// clock 便于测试固定时间。
	clock func() time.Time
	// trashCapacity 是回收站字节上限（0 = 不限制）；超限时拒绝新清理。
	trashCapacity int64
	// rename 是移入/搬回回收站时使用的重命名操作；默认 os.Rename。
	// 抽成字段是为了让「中途失败必须回滚」这条一致性要求可被测试精确验证——
	// 靠制造真实跨设备/权限失败来触发第二项失败既不可靠也难以跨平台。
	rename func(oldpath, newpath string) error
}

// SetTrashCapacity 设置回收站容量护栏（0 = 不限制）。
func (s *CleanupService) SetTrashCapacity(maxBytes int64) {
	s.trashCapacity = maxBytes
}

// SetRenameForTest 替换移入回收站时使用的重命名实现，仅供测试注入失败用。
//
// 为什么需要： 「中途失败必须回滚」是这条链路最重要的一致性要求，但要靠真实的
// 跨设备/权限失败来让**第二项**恰好失败，在 Linux 与 Windows 上行为不一致，
// 用普通文件占位又会让第一项就失败、回滚路径根本没被执行到。
// 生产代码不会调用本方法，rename 恒为 os.Rename。
func (s *CleanupService) SetRenameForTest(fn func(oldpath, newpath string) error) {
	if fn != nil {
		s.rename = fn
	}
}

// NewCleanupService 构造清理服务。
func NewCleanupService(dataDir string, tasks *repository.CleanTaskRepo) *CleanupService {
	return &CleanupService{dataDir: dataDir, tasks: tasks, clock: time.Now, rename: os.Rename}
}

// SetBackupUploadExpiredSource 注入过期上传会话来源（复用既有 24h TTL 口径，不另造常量）。
func (s *CleanupService) SetBackupUploadExpiredSource(fn func() ([]string, error)) {
	s.backupUploadExpired = fn
}

// SetPendingRestoreChecker 注入"隔离区操作是否被待生效恢复引用"的判定。
func (s *CleanupService) SetPendingRestoreChecker(fn func(operationID string) bool) {
	s.pendingRestore = fn
}

// Preview 计算候选集，**纯只读**。
func (s *CleanupService) Preview(kind string, keepLast int) (CleanupPreview, error) {
	items, skipped, err := s.collectCandidates(kind, keepLast)
	if err != nil {
		return CleanupPreview{}, err
	}
	preview := CleanupPreview{Kind: kind, Items: items, ItemCount: len(items), Skipped: skipped}
	for _, item := range items {
		preview.FreedBytes += item.Bytes
	}
	return preview, nil
}

// collectCandidates 是预览与执行共用的唯一候选计算入口。
//
// 共用而不是各写一遍：两处漂移会变成"预览说 3 项、实际删 5 项"，而这类批量操作
// 用户正是靠预览来确认的。
func (s *CleanupService) collectCandidates(kind string, keepLast int) ([]CleanupCandidate, []string, error) {
	switch kind {
	case CleanupKindEmptyDirs:
		return s.collectEmptyDirs()
	case CleanupKindStaleUpload:
		return s.collectStaleUploads()
	case CleanupKindPreRestore:
		return s.collectPreRestore(keepLast)
	case CleanupKindQuarantine:
		return s.collectQuarantine()
	default:
		return nil, nil, fmt.Errorf("%w：未知的清理类别 %q", ErrValidation, kind)
	}
}

// collectEmptyDirs 收集活动 blob 目录下**不含任何普通文件**的叶子目录。
//
// 跳过 blobs/tmp 与 blobs/quarantine：前者是上传中转、后者是隔离区（各自有清理通道），
// 把它们当"空目录"回收会在上传途中打断写入。
func (s *CleanupService) collectEmptyDirs() ([]CleanupCandidate, []string, error) {
	root, err := s.safeJoin("blobs")
	if err != nil {
		return nil, nil, err
	}
	var items []CleanupCandidate
	var skipped []string
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			skipped = append(skipped, s.relPath(path))
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		name := d.Name()
		if path != root && (name == "tmp" || name == "quarantine") {
			return filepath.SkipDir
		}
		entries, readErr := os.ReadDir(path)
		if readErr != nil {
			skipped = append(skipped, s.relPath(path))
			return nil
		}
		if len(entries) != 0 || path == root {
			return nil
		}
		// 只收叶子空目录：上层目录因仍有子项而自然不满足 len==0。
		items = append(items, CleanupCandidate{Path: s.relPath(path), IsDir: true})
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Path < items[j].Path })
	return items, skipped, nil
}

// collectStaleUploads 收集过期上传会话目录。
//
// 数据来源复用既有会话记录的 expires_at（uploadTTL=24h），**不新造 idle 常量**——
// 自造阈值会让"界面说中断了"与"服务端仍在接收"两套口径打架。
func (s *CleanupService) collectStaleUploads() ([]CleanupCandidate, []string, error) {
	if s.backupUploadExpired == nil {
		return nil, nil, nil
	}
	names, err := s.backupUploadExpired()
	if err != nil {
		return nil, nil, err
	}
	root, err := s.safeJoin("backup-uploads")
	if err != nil {
		return nil, nil, err
	}
	items := make([]CleanupCandidate, 0, len(names))
	var skipped []string
	for _, name := range names {
		// 只接受单段目录名：会话 ID 由服务端生成，不应含分隔符；含则说明数据被污染，跳过。
		if name == "" || strings.ContainsAny(name, `/\`) || name == "." || name == ".." {
			skipped = append(skipped, name)
			continue
		}
		full := filepath.Join(root, name)
		info, statErr := os.Lstat(full)
		if statErr != nil || !info.IsDir() {
			continue
		}
		bytes, _ := dirUsageBytes(full)
		items = append(items, CleanupCandidate{Path: s.relPath(full), IsDir: true, Bytes: bytes})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Path < items[j].Path })
	return items, skipped, nil
}

// collectPreRestore 收集可回收的 pre-restore 回滚备份。
//
// **永远保留最新一个**（preRestoreKeepFloor）：它们是恢复失败时唯一的退路。
// 排序按目录名（含时间戳，字典序即时间序），从旧到新回收。
func (s *CleanupService) collectPreRestore(keepLast int) ([]CleanupCandidate, []string, error) {
	entries, err := os.ReadDir(s.dataDir)
	if err != nil {
		return nil, nil, err
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() && strings.HasPrefix(entry.Name(), "pre-restore-") {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	if keepLast < preRestoreKeepFloor {
		keepLast = preRestoreKeepFloor
	}
	if len(names) <= keepLast {
		return nil, nil, nil
	}
	items := make([]CleanupCandidate, 0, len(names)-keepLast)
	for _, name := range names[:len(names)-keepLast] {
		full := filepath.Join(s.dataDir, name)
		bytes, _ := dirUsageBytes(full)
		items = append(items, CleanupCandidate{Path: name, IsDir: true, Bytes: bytes})
	}
	return items, nil, nil
}

// collectQuarantine 收集隔离区里**不被待生效恢复引用**的条目。
//
// 隔离区可能是失败回滚的中间态：删错会让数据无法恢复。因此宁可少收——被待生效恢复
// 引用的操作整目录跳过。
func (s *CleanupService) collectQuarantine() ([]CleanupCandidate, []string, error) {
	root, err := s.safeJoin(filepath.Join("blobs", "quarantine"))
	if err != nil {
		return nil, nil, err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	var items []CleanupCandidate
	var skipped []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		operationID := entry.Name()
		if s.pendingRestore != nil && s.pendingRestore(operationID) {
			skipped = append(skipped, s.relPath(filepath.Join(root, operationID)))
			continue
		}
		full := filepath.Join(root, operationID)
		bytes, _ := dirUsageBytes(full)
		items = append(items, CleanupCandidate{Path: s.relPath(full), IsDir: true, Bytes: bytes})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Path < items[j].Path })
	return items, skipped, nil
}

// safeJoin 把相对数据目录的路径解析为绝对路径，并拒绝任何逃逸。
//
// 为什么不用 strings.HasPrefix(joined, root)：`data/blobs-evil` 与 `data/blobs` 共享前缀，
// 会被误判为"在根内"。这里改用"先 Clean 再判断相对路径有没有 `..` 段"，同时拒绝绝对路径。
func (s *CleanupService) safeJoin(rel string) (string, error) {
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("%w：不接受绝对路径 %q", ErrValidation, rel)
	}
	cleaned := filepath.Clean(rel)
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w：路径逃逸 %q", ErrValidation, rel)
	}
	return filepath.Join(s.dataDir, cleaned), nil
}

// relPath 把绝对路径转成相对数据目录的 / 分隔路径（对外只暴露相对路径）。
func (s *CleanupService) relPath(abs string) string {
	rel, err := filepath.Rel(s.dataDir, abs)
	if err != nil {
		return filepath.ToSlash(abs)
	}
	return filepath.ToSlash(rel)
}

// ExecuteOptions 是一次清理执行的输入。
type ExecuteOptions struct {
	Kind string
	// Items 为空表示"整类全收"；非空表示只收这些（相对数据目录的路径）。
	Items    []string
	KeepLast int
}

// Execute 建立任务并把候选项移入回收站。
//
// 返回任务 ID 与最终状态。**同步执行**：四类候选的计算与 rename 都是毫秒级
// （最大的 pre-restore 目录也只含一个 db 文件），异步化只会让"点完看不到结果"更糟。
// 若将来出现大目录，再改为后台执行并复用同一任务记录（clean_task 已支持进度字段）。
func (s *CleanupService) Execute(opts ExecuteOptions) (*repository.CleanTask, error) {
	items, skipped, err := s.collectCandidates(opts.Kind, opts.KeepLast)
	if err != nil {
		return nil, err
	}
	selected := items
	if len(opts.Items) > 0 {
		selected, err = filterSelected(items, opts.Items)
		if err != nil {
			return nil, err
		}
	}
	if len(selected) == 0 {
		return nil, fmt.Errorf("%w：没有可回收的项", ErrValidation)
	}

	// 容量护栏：回收站超上限时拒绝新的清理。
	//
	// 刻意"拒绝"而不是"自动淘汰最旧条目"：静默删除会让用户以为东西还在回收站里，
	// 而实际早已被系统清掉——这比直接报错危险得多。
	if s.trashCapacity > 0 {
		stats, err := s.tasks.StagedTrashStats()
		if err != nil {
			return nil, err
		}
		if stats.Bytes >= s.trashCapacity {
			return nil, fmt.Errorf("%w：回收站已占用 %d 字节（上限 %d），请先彻底删除或清空回收站",
				ErrValidation, stats.Bytes, s.trashCapacity)
		}
	}

	task, err := s.tasks.Create(opts.Kind, len(selected))
	if err != nil {
		return nil, err
	}
	if err := s.tasks.MarkRunning(task.ID); err != nil {
		return nil, err
	}

	processed := 0
	moved := make([]repository.TrashEntry, 0, len(selected))
	for _, item := range selected {
		entry, moveErr := s.moveToTrash(task.ID, opts.Kind, item)
		if moveErr != nil {
			// 逐项落库而不是「全部移完再一次性写」：后者一旦第 N 项失败，
			// 前面已 rename 进 data/trash/<taskID>/ 的文件就没有任何 DB 记录，
			// 既列不出来、也无法恢复，而 trash-gc 只扫 staged 记录，永远清不到——
			// 它们会静默占盘且不可见。
			s.rollbackMoved(moved)
			_ = s.tasks.MarkFailed(task.ID, 0, moveErr.Error())
			return s.tasks.Get(task.ID)
		}
		// 传切片而非单个值：InsertTrashEntries 会把自增 id 回填进来，
		// 失败回滚要靠它把已落库的记录标成 restored。
		batch := []repository.TrashEntry{entry}
		if err := s.tasks.InsertTrashEntries(batch); err != nil {
			// 记录写不进去就把刚移动的那一项也搬回原位，避免「磁盘已改、DB 无记录」。
			s.rollbackMoved(append(moved, batch[0]))
			_ = s.tasks.MarkFailed(task.ID, 0, err.Error())
			return s.tasks.Get(task.ID)
		}
		moved = append(moved, batch[0])
		processed++
		_ = s.tasks.UpdateProgress(task.ID, processed)
	}
	// freed_bytes 记 0：移入回收站时磁盘尚未释放，真正腾空发生在 purge（见迁移注释）。
	if err := s.tasks.MarkSucceeded(task.ID, processed, 0); err != nil {
		return nil, err
	}
	_ = skipped
	return s.tasks.Get(task.ID)
}

// filterSelected 校验客户端回传的 items 是否都在候选集内。
//
// 不在候选集里 → **整批拒绝**，不做"删掉能删的那部分"：部分执行会让用户以为
// 预览清单被完整执行了，而实际漏掉的项没有任何提示。
func filterSelected(all []CleanupCandidate, wanted []string) ([]CleanupCandidate, error) {
	byPath := make(map[string]CleanupCandidate, len(all))
	for _, item := range all {
		byPath[item.Path] = item
	}
	selected := make([]CleanupCandidate, 0, len(wanted))
	seen := make(map[string]struct{}, len(wanted))
	for _, path := range wanted {
		item, ok := byPath[path]
		if !ok {
			return nil, fmt.Errorf("%w：%q 不在当前预览清单内（清单可能已变化，请重新预览）", ErrValidation, path)
		}
		if _, dup := seen[path]; dup {
			continue
		}
		seen[path] = struct{}{}
		selected = append(selected, item)
	}
	return selected, nil
}

// moveToTrash 把一项移入 data/trash/<taskId>/<序号>，并返回回收站条目记录。
//
// 用序号而不是原路径在回收站里重建目录树：两类目标的原始路径可能很深
// （backup-uploads/<session>/chunks/...），按原样重建会在回收站里制造同样的深路径，
// 而恢复只需要"原路径"这一个字段就能还原。
//
// undoMove 把单条已移入回收站的文件搬回原位，用于「移动后写记录失败」的自愈。
func (s *CleanupService) undoMove(entry repository.TrashEntry) error {
	if err := os.MkdirAll(filepath.Dir(entry.OriginalPath), 0o750); err != nil {
		return err
	}
	return s.rename(entry.TrashPath, entry.OriginalPath)
}

// rollbackMoved 把本次任务已成功移入回收站的条目全部搬回原位。
//
// 只在整批失败时调用：清理要么是「一批可恢复的移动」，要么什么都没发生，
// 不能留下「一部分移走了、任务记为失败、用户分不清哪些被动过」的中间态。
// 已落库的记录一并标为 restored，避免回收站里留下指向已搬回原位的死条目。
func (s *CleanupService) rollbackMoved(moved []repository.TrashEntry) {
	allBack := true
	for i := len(moved) - 1; i >= 0; i-- {
		if undoErr := s.undoMove(moved[i]); undoErr != nil {
			// 搬不回去就保留 staged 记录：宁可让用户在回收站里看到并能手动恢复，
			// 也不要留下一个不可见的孤儿目录。
			allBack = false
			continue
		}
		if moved[i].ID != 0 {
			_ = s.tasks.MarkTrashResolved(moved[i].ID, repository.TrashRestored)
		}
	}
	// 全部搬回后，本次任务建的空目录（trash/<taskID>）就没有存在意义了。
	// 留着会在回收站根下累积一串空目录，让「回收站占了多少」看着莫名其妙。
	if allBack && len(moved) > 0 {
		dir := filepath.Join(s.dataDir, TrashDirName, fmt.Sprintf("%d", moved[0].TaskID))
		_ = os.Remove(dir) // 只在空目录时成功；非空说明还有别的东西，保留原样
	}
}

// moveToTrash 把单个候选移入回收站（同盘 rename），并返回待落库的条目。
func (s *CleanupService) moveToTrash(taskID int64, kind string, item CleanupCandidate) (repository.TrashEntry, error) {
	src, err := s.safeJoin(filepath.FromSlash(item.Path))
	if err != nil {
		return repository.TrashEntry{}, err
	}
	// 符号链接一律跳过：跟随它可能把根外的东西移走（清理不该有这种能力）。
	info, err := os.Lstat(src)
	if err != nil {
		return repository.TrashEntry{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return repository.TrashEntry{}, fmt.Errorf("%w：%q 是符号链接，拒绝回收", ErrValidation, item.Path)
	}
	dstDir := filepath.Join(s.dataDir, TrashDirName, fmt.Sprintf("%d", taskID))
	if err := os.MkdirAll(dstDir, 0o750); err != nil {
		return repository.TrashEntry{}, err
	}
	dst := filepath.Join(dstDir, fmt.Sprintf("%d", time.Now().UnixNano()))
	if err := s.rename(src, dst); err != nil {
		// 跨文件系统时 rename 会失败（EXDEV）。这里**不退化到复制**：把几十 GB 目录复制一遍
		// 会让"清理"变成一次长时间高 IO 的操作，复制失败留下的半成品更难收拾；而回收站与
		// 目标本来就在同一数据目录下，正常部署不会跨盘。如实报错并给出处置建议即可。
		return repository.TrashEntry{}, fmt.Errorf(
			"%w：无法移入回收站（%v）。回收站与目标需在同一文件系统；若把 %s 单独挂载，请先合并或改用作业清理",
			ErrValidation, err, filepath.Join(s.dataDir, TrashDirName))
	}
	return repository.TrashEntry{
		TaskID:       taskID,
		Kind:         kind,
		OriginalPath: src,
		TrashPath:    dst,
		IsDir:        info.IsDir(),
		Bytes:        item.Bytes,
	}, nil
}

// dirUsageBytes 返回路径的字节合计（文件取自身大小，目录递归求和）。
func dirUsageBytes(path string) (int64, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return 0, err
	}
	if !info.IsDir() {
		return info.Size(), nil
	}
	var total int64
	walkErr := filepath.WalkDir(path, func(_ string, d os.DirEntry, err error) error {
		if err != nil || d == nil || d.IsDir() {
			return nil
		}
		if fi, statErr := d.Info(); statErr == nil {
			total += fi.Size()
		}
		return nil
	})
	return total, walkErr
}
