package blobstore

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// 本文件承载 FR-41 存储治理清理作业所需的文件系统维护原语。
// 与 store.go 分开放置：这些方法只被周期清理调用，不参与读写主路径，
// 且必须逐个证明「不误删」——活动 blob、隔离区中在途的文件、正在写入的临时文件
// 一律不在可删范围内。
//
// 目录命名空间的职责边界：清理作业只碰 quarantine 与 tmp/oci-upload 两个命名空间；
// 活动目录（<root>/ab/cd/<hash>）只属于 blob-gc。

// RemoveEmptyQuarantineDirs 删除隔离区根下早于 cutoff 的空操作目录，返回删除数量。
//
// 安全条件（缺一不可，任一不满足即跳过）：
//   - 只处理 quarantine 根下的一级子目录，永不递归、永不触碰活动分片目录与 tmp；
//   - 目录必须为空：非空目录（在途操作尚未回收的隔离文件或回滚快照仍在）返回
//     ENOTEMPTY 并被跳过，因此绝不会连带删除任何 blob 字节；
//   - 目录修改时间必须早于 cutoff：Stage 先 MkdirAll 再 rename，刚创建、尚未写入
//     文件的目录 mtime 为「现在」，宽限期使其不可能被本轮删除。
//
// 删除失败（含权限、占用）只跳过，不返回错误：清理是尽力而为的后台动作，
// 失败会让目录留到下一轮，而不是让作业整体失败。
func (s *Store) RemoveEmptyQuarantineDirs(cutoff time.Time) (int, error) {
	root := filepath.Join(s.root, quarantineDirName)
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("读取 blob 隔离区目录：%w", err)
	}
	removed := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		info, statErr := entry.Info()
		if statErr != nil || !info.ModTime().Before(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(root, entry.Name())); err != nil {
			// 非空目录（在途或未回收的隔离内容）与占用/权限问题都走这里：跳过。
			continue
		}
		removed++
	}
	return removed, nil
}

// CleanupOCIUploadTempsBefore 删除 OCI 分块上传暂存目录中修改时间早于 cutoff 的残留文件。
// 返回已删除数量与因占用等原因跳过的数量。
//
// 安全条件：
//   - 只遍历 <root>/tmp/oci-upload 这一个专用目录（CreateOCIUploadTemp 的唯一落点）；
//   - 普通 blob 写入的 tmp/blob-*、备份恢复的 tmp/restore-*、隔离区与活动 blob 都不在其内，
//     因而不可能被误删；
//   - 按 mtime 判定：仍在写入的上传会话其 mtime 持续更新，不会命中阈值；
//   - 少数删不掉的残留（例如 Windows 上仍被进程占用的句柄）计入 skipped，留待下一轮重试，
//     不作为作业失败上报。
func (s *Store) CleanupOCIUploadTempsBefore(cutoff time.Time) (removed, skipped int, err error) {
	dir := filepath.Join(s.root, tmpDirName, ociUploadTmpDirName)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, fmt.Errorf("读取 OCI 上传暂存目录：%w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		info, statErr := entry.Info()
		if statErr != nil || !info.ModTime().Before(cutoff) {
			continue
		}
		if removeErr := os.Remove(filepath.Join(dir, entry.Name())); removeErr != nil {
			skipped++
			continue
		}
		removed++
	}
	return removed, skipped, nil
}
