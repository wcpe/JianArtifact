package repository

// 本文件是清理任务队列（clean_task）与回收站条目（trash_entry）的持久层。
//
// 三条必须守住的口径：
//
//  1. **「启动收口」是正确性要求，不是收尾美化**：进程重启后，上一条 running 任务不会有人
//     再推进它。若不主动标成 interrupted，它会永远停在 running —— 于是①单并发守卫永远拒绝
//     新的清理请求（看起来像功能坏了），②历史里出现一条永不结束的任务。收口必须在启动早期
//     执行，且只影响 running（queued 同理：它们从未开始，应一并标为中断）。
//
//  2. **单并发以数据库为准，不只靠进程内锁**：进程内互斥只能防同一个进程内的并发；一旦
//     实例重启或未来多进程，锁就失效了。因此「是否存在 running」必须查库，并且「查 + 插入」
//     要在同一个写事务里完成，否则两个并发请求会同时看到「没有 running」。
//
//  3. **逐条状态迁移必须独立**：恢复/彻底删除是逐项动作，某一项失败不该连累其余。因此
//     trash_entry 的 status 用逐行 UPDATE 表达，不用整批 JSON 重写。

import (
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
)

// 清理任务状态。取值与迁移 0051 的注释一一对应。
const (
	CleanTaskQueued      = "queued"
	CleanTaskRunning     = "running"
	CleanTaskSucceeded   = "succeeded"
	CleanTaskFailed      = "failed"
	CleanTaskInterrupted = "interrupted"
)

// 回收站条目状态。
const (
	TrashStaged   = "staged"
	TrashRestored = "restored"
	TrashPurged   = "purged"
)

// ErrCleanTaskRunning 表示已有清理任务在执行（单并发守卫）。由领域层转成 409。
var ErrCleanTaskRunning = errCleanTaskRunning{}

type errCleanTaskRunning struct{}

func (errCleanTaskRunning) Error() string { return "已有清理任务正在执行" }

// CleanTask 是 clean_task 的读模型。
type CleanTask struct {
	ID             int64   `db:"id"`
	Kind           string  `db:"kind"`
	Status         string  `db:"status"`
	TotalItems     int     `db:"total_items"`
	ProcessedItems int     `db:"processed_items"`
	FreedBytes     int64   `db:"freed_bytes"`
	ErrorMessage   *string `db:"error_message"`
	CreatedAt      string  `db:"created_at"`
	StartedAt      *string `db:"started_at"`
	FinishedAt     *string `db:"finished_at"`
}

// CleanTaskRepo 读写 clean_task 与 trash_entry。
type CleanTaskRepo struct {
	db *persistence.DB
}

func NewCleanTaskRepo(db *persistence.DB) *CleanTaskRepo {
	return &CleanTaskRepo{db: db}
}

const cleanTaskColumns = `id, kind, status, total_items, processed_items, freed_bytes,
	error_message, created_at, started_at, finished_at`

// Create 在**同一写事务**里检查单并发并插入一条 queued 任务。
//
// 为什么查与插必须同事务：两次清理请求若并发到达，各自先 SELECT 都会看到「没有 running」，
// 于是都插入成功——单并发守卫形同虚设。SQLite 只有一个写者，把两步放进同一个写事务即可
// 串行化这个判断。
//
// 无 running/queued 时返回新任务；已有则返回 ErrCleanTaskRunning（调用方转 409），
// 不排队、不并发——排队会让用户以为"点了就会执行"，而实际排队位置不可见。
func (r *CleanTaskRepo) Create(kind string, totalItems int) (*CleanTask, error) {
	tx, err := r.db.Beginx()
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	var active int
	if err := tx.Get(&active,
		`SELECT COUNT(*) FROM clean_task WHERE status IN (?, ?)`, CleanTaskQueued, CleanTaskRunning); err != nil {
		return nil, err
	}
	if active > 0 {
		return nil, ErrCleanTaskRunning
	}
	res, err := tx.Exec(
		`INSERT INTO clean_task (kind, status, total_items) VALUES (?, ?, ?)`,
		kind, CleanTaskQueued, totalItems)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return r.Get(id)
}

// Get 按 id 读取任务；不存在返回 sql.ErrNoRows。
func (r *CleanTaskRepo) Get(id int64) (*CleanTask, error) {
	var task CleanTask
	if err := r.db.Get(&task, `SELECT `+cleanTaskColumns+` FROM clean_task WHERE id = ?`, id); err != nil {
		return nil, err
	}
	return &task, nil
}

// MarkRunning 把任务从 queued 置为 running 并记录开始时刻。
//
// 只对 queued 生效：若行数不为 1（已被别的执行者领走或状态被启动收口改成 interrupted），
// 返回 ErrCleanTaskRunning，避免同一条任务被执行两次。
func (r *CleanTaskRepo) MarkRunning(id int64) error {
	res, err := r.db.Exec(
		`UPDATE clean_task SET status = ?, started_at = datetime('now') WHERE id = ? AND status = ?`,
		CleanTaskRunning, id, CleanTaskQueued)
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return ErrCleanTaskRunning
	}
	return nil
}

// UpdateProgress 记录已处理项数（权限为只增，便于断点观察）。
func (r *CleanTaskRepo) UpdateProgress(id int64, processed int) error {
	_, err := r.db.Exec(
		`UPDATE clean_task SET processed_items = ? WHERE id = ? AND status = ?`,
		processed, id, CleanTaskRunning)
	return err
}

// MarkSucceeded 收尾成功。
//
// freedBytes 的语义见迁移注释：移入回收站时尚未真正释放磁盘，因此这里通常为 0，
// 真正腾出空间发生在回收站的 purge 上。
func (r *CleanTaskRepo) MarkSucceeded(id int64, processed int, freedBytes int64) error {
	_, err := r.db.Exec(
		`UPDATE clean_task SET status = ?, processed_items = ?, freed_bytes = ?, finished_at = datetime('now')
		 WHERE id = ?`,
		CleanTaskSucceeded, processed, freedBytes, id)
	return err
}

// MarkFailed 收尾失败，保留可读原因供历史追溯。
func (r *CleanTaskRepo) MarkFailed(id int64, processed int, reason string) error {
	_, err := r.db.Exec(
		`UPDATE clean_task SET status = ?, processed_items = ?, error_message = ?, finished_at = datetime('now')
		 WHERE id = ?`,
		CleanTaskFailed, processed, reason, id)
	return err
}

// MarkInterruptedOnStartup 把残留的 queued/running 标为 interrupted，返回受影响行数。
//
// 在启动早期调用（与既有 ReconcileStartup 同处）。queued 也要收口：它们从未开始执行，
// 留在库里同样会让单并发守卫永久拒绝新请求。
func (r *CleanTaskRepo) MarkInterruptedOnStartup() (int64, error) {
	res, err := r.db.Exec(
		`UPDATE clean_task SET status = ?, finished_at = datetime('now'),
		        error_message = COALESCE(error_message, '进程重启，清理任务中断')
		 WHERE status IN (?, ?)`,
		CleanTaskInterrupted, CleanTaskQueued, CleanTaskRunning)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// List 按创建时间倒序分页返回历史；limit<=0 时取默认 20，上限 100。
func (r *CleanTaskRepo) List(limit, offset int) ([]CleanTask, int, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	var total int
	if err := r.db.Get(&total, `SELECT COUNT(*) FROM clean_task`); err != nil {
		return nil, 0, err
	}
	var tasks []CleanTask
	if err := r.db.Select(&tasks,
		`SELECT `+cleanTaskColumns+` FROM clean_task ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?`,
		limit, offset); err != nil {
		return nil, 0, err
	}
	return tasks, total, nil
}

// TrashEntry 是 trash_entry 的读模型。
type TrashEntry struct {
	ID           int64   `db:"id"`
	TaskID       int64   `db:"task_id"`
	Kind         string  `db:"kind"`
	OriginalPath string  `db:"original_path"`
	TrashPath    string  `db:"trash_path"`
	IsDir        bool    `db:"is_dir"`
	Bytes        int64   `db:"bytes"`
	Status       string  `db:"status"`
	CreatedAt    string  `db:"created_at"`
	ResolvedAt   *string `db:"resolved_at"`
}

const trashEntryColumns = `id, task_id, kind, original_path, trash_path, is_dir, bytes, status,
	created_at, resolved_at`

// InsertTrashEntries 批量写入回收站条目（与「移入」动作同事务由调用方保证）。
func (r *CleanTaskRepo) InsertTrashEntries(entries []TrashEntry) error {
	if len(entries) == 0 {
		return nil
	}
	tx, err := r.db.Beginx()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, e := range entries {
		if _, err := tx.Exec(
			`INSERT INTO trash_entry (task_id, kind, original_path, trash_path, is_dir, bytes, status)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			e.TaskID, e.Kind, e.OriginalPath, e.TrashPath, e.IsDir, e.Bytes, TrashStaged); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ListTrash 分页列出回收站条目；status 为空时只看 staged。
func (r *CleanTaskRepo) ListTrash(status string, limit, offset int) ([]TrashEntry, int, error) {
	if status == "" {
		status = TrashStaged
	}
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	var total int
	if err := r.db.Get(&total, `SELECT COUNT(*) FROM trash_entry WHERE status = ?`, status); err != nil {
		return nil, 0, err
	}
	var entries []TrashEntry
	if err := r.db.Select(&entries,
		`SELECT `+trashEntryColumns+` FROM trash_entry WHERE status = ?
		 ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?`, status, limit, offset); err != nil {
		return nil, 0, err
	}
	return entries, total, nil
}

// GetTrash 按 id 读取条目；不存在返回 sql.ErrNoRows。
func (r *CleanTaskRepo) GetTrash(id int64) (*TrashEntry, error) {
	var entry TrashEntry
	if err := r.db.Get(&entry, `SELECT `+trashEntryColumns+` FROM trash_entry WHERE id = ?`, id); err != nil {
		return nil, err
	}
	return &entry, nil
}

// MarkTrashResolved 把条目从 staged 迁到 restored / purged（逐条、幂等）。
//
// 只允许 staged → 终态：已恢复的不该被再次删除，已彻底删除的不该再被恢复。
func (r *CleanTaskRepo) MarkTrashResolved(id int64, status string) error {
	if status != TrashRestored && status != TrashPurged {
		return errInvalidTrashStatus{}
	}
	_, err := r.db.Exec(
		`UPDATE trash_entry SET status = ?, resolved_at = datetime('now') WHERE id = ? AND status = ?`,
		status, id, TrashStaged)
	return err
}

type errInvalidTrashStatus struct{}

func (errInvalidTrashStatus) Error() string { return "非法的回收站条目状态" }

// TrashStats 是回收站当前占用（供容量护栏与界面展示）。
type TrashStats struct {
	// 显式 json tag：本类型同时用于 db 扫描与 HTTP 响应，缺 tag 时按 Go 字段名
	// 输出（Entries/Bytes），前端按小写读会拿到 undefined，界面显示成「项 / NAN KB」。
	Entries int   `db:"entries" json:"entries"`
	Bytes   int64 `db:"bytes" json:"bytes"`
}

// StagedTrashStats 统计 staged 条目的数量与字节合计。
func (r *CleanTaskRepo) StagedTrashStats() (TrashStats, error) {
	var stats TrashStats
	err := r.db.Get(&stats,
		`SELECT COUNT(*) AS entries, COALESCE(SUM(bytes), 0) AS bytes FROM trash_entry WHERE status = ?`,
		TrashStaged)
	return stats, err
}

// ExpiredStagedTrash 返回 created_at 早于 cutoff 的 staged 条目（保留期回收用）。
func (r *CleanTaskRepo) ExpiredStagedTrash(cutoff time.Time, limit int) ([]TrashEntry, error) {
	if limit <= 0 {
		limit = 200
	}
	var entries []TrashEntry
	if err := r.db.Select(&entries,
		`SELECT `+trashEntryColumns+` FROM trash_entry
		 WHERE status = ? AND created_at < ? ORDER BY created_at ASC LIMIT ?`,
		TrashStaged, cutoff.UTC().Format("2006-01-02 15:04:05"), limit); err != nil {
		return nil, err
	}
	return entries, nil
}

// 编译期断言：确保 sqlx 仍被使用（本文件通过 persistence.DB 间接依赖）。
var _ = sqlx.Tx{}
