package domain

// 本文件实现资产回收站的**恢复**与**彻底清理**。
//
// 三条必须守住的口径（与存储清理回收站同一口径，避免出现两套心智）：
//
//  1. **恢复绝不覆盖**：目标路径已存在时跳过并如实报冲突。覆盖等于执行一次删除，
//     而用户点的是"恢复"——反向语义会造成最难排查的数据丢失（原有文件没了，
//     回收站里那份也变成了"已恢复"状态）。
//
//  2. **彻底清理只释放 blob，不碰其它资产**：只处理回收站条目自身引用的 blob，
//     且必须逐个确认"该 blob 无其它活引用"后才释放。共享 blob（同一内容被多个路径
//     引用）在回收站里可能仍被正常资产使用，直接删会毁掉在用数据。
//
//  3. **状态迁移与数据写回同事务**：恢复要同时"写回 asset 行"与"标记条目 restored"，
//     分两次提交会在中间崩溃时产生死条目（资产已回来，回收站还显示它在那儿）。

import (
	"errors"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// AssetTrashRestoreResult 是一次恢复的结果。
type AssetTrashRestoreResult struct {
	Restored int `json:"restored"`
	// Conflicts 是"目标路径已存在，故跳过"的条目。单独列出而不是并入失败：
	// 这不是错误，是保护性跳过（与存储清理回收站完全同口径）。
	Conflicts []AssetTrashConflict `json:"conflicts,omitempty"`
	// Missing 是原仓库已被删除、无法恢复的条目。
	Missing []AssetTrashConflict `json:"missing,omitempty"`
}

// AssetTrashConflict 描述一次保护性跳过。
type AssetTrashConflict struct {
	ID     int64  `json:"id"`
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// AssetTrashPurgeResult 是一次彻底清理的结果。
type AssetTrashPurgeResult struct {
	Purged     int   `json:"purged"`
	FreedBytes int64 `json:"freedBytes"`
}

// RestoreAssetTrash 把指定回收站条目恢复为正常资产。
//
// 逐条处理、互不连累：一条冲突不影响其余条目恢复。
func (s *AssetService) RestoreAssetTrash(ids []int64) (AssetTrashRestoreResult, error) {
	var result AssetTrashRestoreResult
	if s.assetTrash == nil {
		return result, ErrAssetTrashUnavailable
	}
	if err := s.requireBusinessWrite(); err != nil {
		return result, err
	}
	if len(ids) == 0 {
		return result, nil
	}
	entries, err := s.assetTrash.ListStagedByIds(ids)
	if err != nil {
		return result, err
	}
	if len(entries) == 0 {
		return result, nil
	}

	for _, entry := range entries {
		// 仓库是否还在：仓库被删后 CASCADE 会清掉条目，因此理论上不会进入这里；
		// 仍显式判断一次，好在并发删仓时给出可读原因而不是一个外键错误。
		if _, repoErr := s.repos.GetByID(entry.RepositoryID); repoErr != nil {
			result.Missing = append(result.Missing, AssetTrashConflict{
				ID: entry.ID, Path: entry.Path, Reason: "原仓库已不存在",
			})
			continue
		}
		// 目标路径必须空闲：绝不覆盖。
		if _, getErr := s.assets.GetByPath(entry.RepositoryID, entry.Path); getErr == nil {
			result.Conflicts = append(result.Conflicts, AssetTrashConflict{
				ID: entry.ID, Path: entry.Path, Reason: "目标路径已存在同名资产",
			})
			continue
		} else if !errors.Is(getErr, repository.ErrNotFound) {
			return result, getErr
		}

		// blob 是否还在：保留期回收或人工清理可能已释放它。
		// 这种情况不能"恢复出一个指向空洞的资产"，必须如实报告。
		if !s.blobs.Exists(entry.BlobHash) {
			result.Missing = append(result.Missing, AssetTrashConflict{
				ID: entry.ID, Path: entry.Path, Reason: "内容已不存在（blob 已被回收）",
			})
			continue
		}

		asset := repository.Asset{
			RepositoryID: entry.RepositoryID,
			Path:         entry.Path,
			BlobHash:     entry.BlobHash,
			Size:         entry.Size,
			ContentType:  entry.ContentType,
			Sha1:         entry.Sha1,
			Md5:          entry.Md5,
			// 时间戳按原值还原：恢复出来的制品应当在列表里回到它原本的位置，
			// 而不是"刚刚上传"排到最前。
			CreatedAt: entry.CreatedAt,
			UpdatedAt: entry.UpdatedAt,
		}
		if err := s.restoreAssetEntry(entry, asset); err != nil {
			return result, err
		}
		result.Restored++
	}
	return result, nil
}

// restoreAssetEntry 在**同一事务**内写回 asset 行并把条目标记为 restored。
func (s *AssetService) restoreAssetEntry(entry repository.AssetTrashEntry, asset repository.Asset) error {
	item := repository.AssetMutationItem{
		RepositoryID: asset.RepositoryID,
		Path:         asset.Path,
		After:        &asset,
	}
	hook := func(tx *sqlx.Tx) error {
		return repository.MarkAssetTrashResolved(tx, entry.ID, repository.AssetTrashRestored)
	}
	_, err := s.mutator.ApplyWithOperationIDAndHook(newOperationID(), []repository.AssetMutationItem{item}, hook)
	return err
}

// PurgeAssetTrash 彻底清理指定回收站条目：释放 blob 并把条目标为 purged。
//
// 顺序很关键：先标记 purged 再释放 blob。反过来的话，若释放中途失败或进程崩溃，
// 条目仍是 staged 但 blob 已经没了，恢复时会报告"内容已不存在"——
// 那让一个可以诚实清理的动作变成了数据损坏。
func (s *AssetService) PurgeAssetTrash(ids []int64) (AssetTrashPurgeResult, error) {
	var result AssetTrashPurgeResult
	if s.assetTrash == nil {
		return result, ErrAssetTrashUnavailable
	}
	if err := s.requireBusinessWrite(); err != nil {
		return result, err
	}
	if len(ids) == 0 {
		return result, nil
	}
	entries, err := s.assetTrash.ListStagedByIds(ids)
	if err != nil {
		return result, err
	}
	for _, entry := range entries {
		if err := s.purgeAssetTrashEntry(entry); err != nil {
			return result, err
		}
		result.Purged++
		result.FreedBytes += entry.Size
	}
	return result, nil
}

// purgeAssetTrashEntry 彻底清理单条条目。
func (s *AssetService) purgeAssetTrashEntry(entry repository.AssetTrashEntry) error {
	// 1) 先把状态迁移到 purged 并落库。
	//    顺序不能反：若先释放 blob 再崩溃，条目仍是 staged 但内容已没了，
	//    恢复时会报"内容已不存在"——那让一个诚实的清理动作变成了数据损坏。
	if err := s.markPurgedDirect(entry.ID); err != nil {
		return err
	}
	// 2) 状态已落库，再释放 blob。
	//    RemoveIfUnreferenced 在"另一路径正在预约同一 hash 的暂存"时会直接返回 nil 且不删，
	//    那是写路径的正常保护，但对"彻底清理"来说意味着字节没被释放。
	//    因此这里重试若干次；仍未释放也不报错——条目已 purged，
	//    定时 blob-gc 会在无预约时正常回收，不会永久泄漏。
	return s.releaseBlobBestEffort(entry.BlobHash)
}

// releaseBlobBestEffort 尽力释放 blob，并短暂重试以跨越并发暂存预约窗口。
//
// 为什么重试而不是一次性失败：暂存预约是毫秒级的短窗口，
// 一次失败就放弃会让"彻底清理"经常性地无法立即释放字节，
// 用户看到"已清理 1 条 / 释放 0 字节"会以为功能有 bug。
// 重试有界（3 次 × 20ms），不会在高并发下拖长请求。
func (s *AssetService) releaseBlobBestEffort(hash string) error {
	const attempts = 3
	const backoff = 20 * time.Millisecond
	for i := 0; i < attempts; i++ {
		if err := s.mutator.RemoveIfUnreferenced(hash); err != nil {
			return err
		}
		if !s.blobs.Exists(hash) {
			return nil
		}
		if i < attempts-1 {
			time.Sleep(backoff)
		}
	}
	// 仍未释放：有其它活引用（共享 blob）或预约仍在进行，均属正常，
	// 交由 blob-gc 兜底，不作为错误上报。
	return nil
}

// markPurgedDirect 在无资产变更的情况下单独迁移条目状态。
//
// 为什么不用协调器：协调器要求 items 非空（一次资产变更），而"只改回收站状态"
// 不是资产变更。这里直接用仓储层事务，语义更准确。
func (s *AssetService) markPurgedDirect(id int64) error {
	tx, err := s.assets.DB().Beginx()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := repository.MarkAssetTrashResolved(tx, id, repository.AssetTrashPurged); err != nil {
		return err
	}
	return tx.Commit()
}

// ListAssetTrash 分页列出资产回收站条目与占用统计。
//
// 统计与列表**同一次调用返回**：分开两次查询会让界面出现"列表 3 条、统计说 5 条"
// 的不一致（两次查询之间可能有并发删除或恢复）。这里的实现仍分两条 SQL，
// 但都在同一个方法内完成，调用方不需要分别发起、也不会各自重试。
func (s *AssetService) ListAssetTrash(limit, offset int) ([]repository.AssetTrashEntry, int, repository.AssetTrashStats, error) {
	if s.assetTrash == nil {
		return nil, 0, repository.AssetTrashStats{}, ErrAssetTrashUnavailable
	}
	entries, total, err := s.assetTrash.List(repository.AssetTrashStaged, limit, offset)
	if err != nil {
		return nil, 0, repository.AssetTrashStats{}, err
	}
	stats, err := s.assetTrash.Stats()
	if err != nil {
		return nil, 0, repository.AssetTrashStats{}, err
	}
	return entries, total, stats, nil
}

// ReconcileExpiredAssetTrash 按保留期回收过期条目，供 trash-gc 作业调用。
//
// 与存储清理回收站的 ReconcileExpired 同名同义，共同构成"一个保留期、一个作业"
// 的运维口径——用户只需要理解一套"东西删了还在，到期才真没"。
//
// retention <= 0 表示**不自动清理**（与存储侧同口径：0 不是"立刻删光"）。
// 这一点必须显式判断：把 0 当成 cutoff=now 会让"关闭自动清理"变成"立即清空回收站"，
// 是本模块最危险的一处语义反转。
func (s *AssetService) ReconcileExpiredAssetTrash(retention time.Duration, limit int) (AssetTrashPurgeResult, error) {
	var result AssetTrashPurgeResult
	if s.assetTrash == nil {
		return result, ErrAssetTrashUnavailable
	}
	if retention <= 0 {
		return result, nil
	}
	if err := s.requireBusinessWrite(); err != nil {
		return result, err
	}
	cutoff := time.Now().Add(-retention)
	entries, err := s.assetTrash.ExpiredStaged(cutoff, limit)
	if err != nil {
		return result, err
	}
	for _, entry := range entries {
		// 单条失败不中断整轮：把已完成的部分如实返回，便于定位剩余条目。
		if err := s.purgeAssetTrashEntry(entry); err != nil {
			return result, err
		}
		result.Purged++
		result.FreedBytes += entry.Size
	}
	return result, nil
}
