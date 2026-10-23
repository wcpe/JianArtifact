package domain

// 本文件实现回收站：让定向清理**可逆**。
//
// 三条必须守住的口径：
//
//  1. **恢复绝不覆盖**：目标位置已存在时跳过并如实报冲突。覆盖等于执行一次删除——
//     而用户点的是"恢复"，反向语义会造成最难排查的数据丢失（原文件没了、回收站里的
//     那份也变成了"已恢复"状态）。
//
//  2. **彻底删除只删回收站副本**：purge 只作用于 `${data}/trash/`，无论 original_path
//     当前是否存在都不碰它。把它做成"顺手清理一下原位置"会让一次误点同时毁掉两处。
//
//  3. **容量护栏宁拒绝不淘汰**：回收站超上限时拒绝新的清理，而**不是**自动淘汰最旧条目。
//     静默删除等于绕过用户的确认流程——用户以为东西还在回收站，其实早已被系统删掉。

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// TrashRestoreResult 是一次恢复的结果。
type TrashRestoreResult struct {
	Restored int `json:"restored"`
	// Conflicts 是"目标已存在，故跳过"的条目（原路径 + 原因）。
	// 单独列出而不是合并进失败计数：这不是错误，是**保护性跳过**。
	Conflicts []TrashConflict `json:"conflicts,omitempty"`
}

// TrashConflict 描述一次保护性跳过。
type TrashConflict struct {
	ID           int64  `json:"id"`
	OriginalPath string `json:"originalPath"`
	Reason       string `json:"reason"`
}

// TrashPurgeResult 是一次彻底删除的结果。
type TrashPurgeResult struct {
	Purged     int   `json:"purged"`
	FreedBytes int64 `json:"freedBytes"`
}

// TrashService 管理回收站条目：恢复、彻底删除、清空、保留期回收。
type TrashService struct {
	dataDir string
	repo    *repository.CleanTaskRepo
}

func NewTrashService(dataDir string, repo *repository.CleanTaskRepo) *TrashService {
	return &TrashService{dataDir: dataDir, repo: repo}
}

// List 分页列出回收站条目（默认只看 staged）。
func (s *TrashService) List(limit, offset int) ([]repository.TrashEntry, int, error) {
	return s.repo.ListTrash(repository.TrashStaged, limit, offset)
}

// Stats 返回当前回收站占用（供界面与容量护栏）。
func (s *TrashService) Stats() (repository.TrashStats, error) {
	return s.repo.StagedTrashStats()
}

// Restore 把指定条目移回原位置。
//
// 逐条处理、互不连累：一条冲突不影响其余条目恢复。
func (s *TrashService) Restore(ids []int64) (TrashRestoreResult, error) {
	var result TrashRestoreResult
	for _, id := range ids {
		entry, err := s.repo.GetTrash(id)
		if err != nil {
			return result, fmt.Errorf("读取回收站条目 %d：%w", id, err)
		}
		if entry.Status != repository.TrashStaged {
			// 已恢复/已彻底删除的条目再点恢复：跳过而不是报错（幂等）。
			result.Conflicts = append(result.Conflicts, TrashConflict{
				ID: entry.ID, OriginalPath: entry.OriginalPath, Reason: "条目已不在回收站（已恢复或已彻底删除）",
			})
			continue
		}
		// 目标已存在 → 跳过。**绝不覆盖**：覆盖就是一次删除，而用户点的是恢复。
		if _, statErr := os.Lstat(entry.OriginalPath); statErr == nil {
			result.Conflicts = append(result.Conflicts, TrashConflict{
				ID: entry.ID, OriginalPath: entry.OriginalPath, Reason: "目标位置已存在，未覆盖",
			})
			continue
		}
		if err := os.MkdirAll(filepath.Dir(entry.OriginalPath), 0o750); err != nil {
			return result, fmt.Errorf("恢复 %d 建父目录：%w", id, err)
		}
		if err := os.Rename(entry.TrashPath, entry.OriginalPath); err != nil {
			return result, fmt.Errorf("恢复 %d：%w", id, err)
		}
		if err := s.repo.MarkTrashResolved(id, repository.TrashRestored); err != nil {
			return result, err
		}
		result.Restored++
	}
	return result, nil
}

// Purge 彻底删除指定条目（不可逆），只作用于回收站副本。
func (s *TrashService) Purge(ids []int64) (TrashPurgeResult, error) {
	var result TrashPurgeResult
	for _, id := range ids {
		entry, err := s.repo.GetTrash(id)
		if err != nil {
			return result, fmt.Errorf("读取回收站条目 %d：%w", id, err)
		}
		if entry.Status != repository.TrashStaged {
			continue // 幂等：已处理的条目跳过
		}
		// 纵深防御：trash_path 由本服务写入，但仍校验它确实在回收站根内，
		// 避免数据被外部改动后 purge 把回收站之外的东西删掉。
		if !s.trashPathWithin(entry.TrashPath) {
			return result, fmt.Errorf("%w：条目 %d 的回收站路径不在回收站根内（%s）",
				ErrValidation, id, entry.TrashPath)
		}
		if err := os.RemoveAll(entry.TrashPath); err != nil {
			return result, fmt.Errorf("彻底删除 %d：%w", id, err)
		}
		// 顺手清掉可能为空的 task 目录（不影响正确性，避免回收站里堆积层级）。
		_ = os.Remove(filepath.Dir(entry.TrashPath))
		if err := s.repo.MarkTrashResolved(id, repository.TrashPurged); err != nil {
			return result, err
		}
		result.Purged++
		result.FreedBytes += entry.Bytes
	}
	return result, nil
}

// PurgeAll 清空回收站（不可逆）。
//
// 实现为"取出全部 staged 再逐条删"，而不是 `rm -rf trash`：后者会留下状态为 staged
// 但文件已不存在的记录，列表里就会出现一堆点不动的条目。
func (s *TrashService) PurgeAll() (TrashPurgeResult, error) {
	var result TrashPurgeResult
	for {
		entries, total, err := s.repo.ListTrash(repository.TrashStaged, 100, 0)
		if err != nil {
			return result, err
		}
		if total == 0 || len(entries) == 0 {
			return result, nil
		}
		ids := make([]int64, 0, len(entries))
		for _, entry := range entries {
			ids = append(ids, entry.ID)
		}
		batch, err := s.Purge(ids)
		if err != nil {
			return result, err
		}
		result.Purged += batch.Purged
		result.FreedBytes += batch.FreedBytes
	}
}

// ReconcileExpired 按保留期回收 staged 条目，供 trash-gc 作业调用。
//
// retention<=0 表示**不自动清理**（只保留手动彻底删除）：把 0 解释成"立刻全删"会让
// 一次配置笔误清空整个回收站，而"不清理"最坏只是多占磁盘。
func (s *TrashService) ReconcileExpired(retention time.Duration) (TrashPurgeResult, error) {
	if retention <= 0 {
		return TrashPurgeResult{}, nil
	}
	cutoff := time.Now().Add(-retention)
	var result TrashPurgeResult
	for {
		entries, err := s.repo.ExpiredStagedTrash(cutoff, 200)
		if err != nil {
			return result, err
		}
		if len(entries) == 0 {
			return result, nil
		}
		ids := make([]int64, 0, len(entries))
		for _, entry := range entries {
			ids = append(ids, entry.ID)
		}
		batch, err := s.Purge(ids)
		if err != nil {
			return result, err
		}
		if batch.Purged == 0 {
			// 全部条目都因状态已变被跳过：避免死循环。
			return result, nil
		}
		result.Purged += batch.Purged
		result.FreedBytes += batch.FreedBytes
	}
}

// EnsureCapacity 在清理前检查回收站容量（护栏）。
//
// 超限返回错误而不是自动淘汰：静默删除会让用户以为东西还在回收站。
func (s *TrashService) EnsureCapacity(maxBytes int64) error {
	if maxBytes <= 0 {
		return nil // 0 = 不限制
	}
	stats, err := s.Stats()
	if err != nil {
		return err
	}
	if stats.Bytes >= maxBytes {
		return fmt.Errorf("%w：回收站已占用 %d 字节（上限 %d），请先在「回收站」里彻底删除或清空后再清理",
			ErrValidation, stats.Bytes, maxBytes)
	}
	return nil
}

// trashPathWithin 判断某路径是否确实位于回收站根内（纵深防御）。
//
// 恢复/删除都以数据库里的 trash_path 为准，而那些值是本服务写入的；这里仍做一次
// 归属校验，避免数据被外部改动后 purge 删到回收站之外。
func (s *TrashService) trashPathWithin(trashPath string) bool {
	root := filepath.Join(s.dataDir, TrashDirName)
	cleaned := filepath.Clean(trashPath)
	return strings.HasPrefix(cleaned, root+string(filepath.Separator))
}
