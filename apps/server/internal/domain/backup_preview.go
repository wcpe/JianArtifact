package domain

import (
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/wcpe/jianartifact/apps/server/internal/archive"
)

// 备份包内容预览。
//
// # 为什么需要
//
// 之前只能看到"这个包多大、有多少制品、什么时候生成的"——全都来自 manifest 的
// 抽象计数。用户在决定**要不要用这个包覆盖本机数据**时，真正想知道的是里面到底有
// 什么：哪些仓库、哪些用户、哪些制品路径。只能盲选的话，"恢复"就成了一次赌博，
// 而这恰恰是不可撤销的操作。
//
// # 只读、可丢弃
//
// 预览要把包里的 db 快照解出来才能看明细。为此**把 db 条目单独解到临时目录**，
// 以 mode=ro 打开查询，查完立刻删掉；不碰 backups/、不碰 blobstore、不写任何标记。
// 因此预览是纯读路径，失败也只是"看不到内容"，不会影响包本身。
//
// 只在需要明细时才解包（Manifest 部分零成本），避免每次列表刷新都做一次 IO。
type BackupPreview struct {
	// Manifest 是包的元数据（与列表一致，便于对照）。
	Manifest archive.Manifest
	// Repositories 是包内仓库概要（按名字排序）。
	Repositories []PreviewRepository
	// Users 是包内账号概要（按用户名排序，不含任何口令哈希）。
	Users []PreviewUser
	// SampleAssets 是若干条制品路径样例（供快速判断"是不是我要的那个包"）。
	SampleAssets []string
	// TotalAssets 是制品总数（SampleAssets 有上限，总数以本字段为准）。
	TotalAssets int
	// BlobBytes 是包内 blob 的总字节数（来自 manifest）。
	BlobBytes int64
}

// PreviewRepository 是一个仓库的概要。
type PreviewRepository struct {
	Name   string `json:"name"`
	Format string `json:"format"`
	Type   string `json:"type"`
}

// PreviewUser 是一个账号的概要。
//
// 刻意**不含** password_hash 与任何凭据字段：预览的用途是"看清包里有什么"，
// 没有理由把口令哈希带到界面上。
type PreviewUser struct {
	Username string `json:"username"`
	Role     string `json:"role"`
	Status   string `json:"status"`
}

// maxPreviewAssets 限制样例制品条数。
//
// 为什么不返回全部：一个真实实例可能有几万个制品，全量序列化既慢又没人看。
// 用户要的是"大致内容"，具体某条制品在恢复后可以再查。
const maxPreviewAssets = 50

// Preview 读取指定备份包的内容概要。
//
// 返回的错误都是"看不了"这一类（包不存在 / 归档损坏 / 快照读不出），
// 调用方据此提示，不应影响包本身的可用性。
func (s *BackupService) Preview(packageID string) (BackupPreview, error) {
	// 先确认已登记：未登记的包不走这里（那是"未管理包"的职责）。
	rec, err := s.Get(packageID)
	if err != nil {
		return BackupPreview{}, err
	}
	path := s.PackagePath(packageID)
	if _, statErr := os.Stat(path); statErr != nil {
		return BackupPreview{}, ErrBackupFileMissing
	}
	reader, m, err := archive.Open(path)
	if err != nil {
		return BackupPreview{}, fmt.Errorf("打开备份包：%w", err)
	}
	out := BackupPreview{
		Manifest:  m,
		BlobBytes: m.Blobs.TotalBytes,
	}
	_ = rec // 登记仅用于"确实已登记"这一判断，其余明细以包内快照为准。

	// 解包只取 db：blob 与索引对"看清内容"没有帮助，跳过以省 IO。
	tmpDir, err := os.MkdirTemp("", "jianartifact-preview-")
	if err != nil {
		return out, fmt.Errorf("创建预览临时目录：%w", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	dbPath := filepath.Join(tmpDir, archive.DBName)
	if err := extractDBOnly(reader, dbPath); err != nil {
		return out, fmt.Errorf("解出数据库快照：%w", err)
	}

	// 以只读方式打开：预览绝不能写入被检验的快照（连 -wal 都不该产生）。
	conn, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return out, fmt.Errorf("打开快照：%w", err)
	}
	defer func() { _ = conn.Close() }()
	conn.SetMaxOpenConns(1)

	out.Repositories = previewRepositories(conn)
	out.Users = previewUsers(conn)
	out.SampleAssets, out.TotalAssets = previewAssets(conn)
	return out, nil
}

// extractDBOnly 把归档里的 db 条目解到 dst，其余条目跳过。
func extractDBOnly(reader *archive.Reader, dst string) error {
	// Extract 会把 manifest/db/blobs.index 写到 dstDir；blob 回调为空即跳过内容。
	dir := filepath.Dir(dst)
	if err := reader.Extract(dir, func(ref archive.BlobRef, size int64, rc io.Reader) error {
		// 显式丢弃 blob：预览不需要它们，但必须读掉以推进 tar 流。
		_, _ = io.Copy(io.Discard, rc)
		return nil
	}); err != nil {
		return err
	}
	if _, err := os.Stat(dst); err != nil {
		return fmt.Errorf("归档内缺少 %s", archive.DBName)
	}
	return nil
}

// previewRepositories 读仓库概要；表不存在或查询失败时返回空（预览是尽力而为）。
func previewRepositories(conn *sql.DB) []PreviewRepository {
	rows, err := conn.Query(`SELECT name, format, type FROM repository ORDER BY name`)
	if err != nil {
		return nil
	}
	defer func() { _ = rows.Close() }()
	var out []PreviewRepository
	for rows.Next() {
		var r PreviewRepository
		if scanErr := rows.Scan(&r.Name, &r.Format, &r.Type); scanErr == nil {
			out = append(out, r)
		}
	}
	return out
}

// previewUsers 读账号概要。**只取非敏感列**：不读 password_hash。
func previewUsers(conn *sql.DB) []PreviewUser {
	rows, err := conn.Query(`SELECT username, role, status FROM user ORDER BY username`)
	if err != nil {
		return nil
	}
	defer func() { _ = rows.Close() }()
	var out []PreviewUser
	for rows.Next() {
		var u PreviewUser
		if scanErr := rows.Scan(&u.Username, &u.Role, &u.Status); scanErr == nil {
			out = append(out, u)
		}
	}
	return out
}

// previewAssets 返回若干条制品路径样例与总数。
func previewAssets(conn *sql.DB) ([]string, int) {
	var total int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM asset`).Scan(&total); err != nil {
		return nil, 0
	}
	rows, err := conn.Query(`SELECT path FROM asset ORDER BY path LIMIT ?`, maxPreviewAssets)
	if err != nil {
		return nil, total
	}
	defer func() { _ = rows.Close() }()
	var samples []string
	for rows.Next() {
		var p string
		if scanErr := rows.Scan(&p); scanErr == nil {
			samples = append(samples, p)
		}
	}
	sort.Strings(samples)
	return samples, total
}
