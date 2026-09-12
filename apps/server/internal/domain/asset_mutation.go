package domain

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/blobstore"
	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// AssetMutationCoordinator 串行化本节点资产引用变更，并协调 SQLite intent 与 blob 隔离移动。
// 文件系统操作始终在 SQLite 写事务之外执行；读门保证读请求不会看到半完成视图。
type AssetMutationCoordinator struct {
	mutations          *repository.AssetMutationRepo
	blobs              *blobstore.Store
	finalize           func(blobstore.QuarantineEntry) error
	finalizeSnapshot   func(blobstore.RollbackSnapshot) error
	removeUnreferenced func(string) error
	retainBlob         func(string) bool
	gate               sync.RWMutex
}

var operationIDFallback atomic.Uint64

// NewAssetMutationCoordinator 构造节点级资产变更协调器并恢复未完成 intent。
func NewAssetMutationCoordinator(db *persistence.DB, blobs *blobstore.Store) (*AssetMutationCoordinator, error) {
	c := NewAssetMutationCoordinatorDeferred(db, blobs)
	if err := c.Recover(); err != nil {
		return nil, err
	}
	return c, nil
}

// NewAssetMutationCoordinatorDeferred 构造节点级资产变更协调器，但不处理既有
// intent。调用方必须在确认当前节点允许恢复本地业务历史后显式调用 Recover。
func NewAssetMutationCoordinatorDeferred(db *persistence.DB, blobs *blobstore.Store) *AssetMutationCoordinator {
	return &AssetMutationCoordinator{mutations: repository.NewAssetMutationRepo(db), blobs: blobs, finalize: blobs.Finalize, finalizeSnapshot: blobs.FinalizeRollbackSnapshot, removeUnreferenced: blobs.Remove}
}

// SetBlobRetentionChecker 注入 relay 日志保留检查。
// relay 节点必须保留已转发 record 引用的 blob，防止慢子节点在上游先删除元数据后无法回补内容。
func (c *AssetMutationCoordinator) SetBlobRetentionChecker(check func(string) bool) {
	c.retainBlob = check
}

// Read 在读门内执行元数据与 blob 打开，确保不会观察 staged/committing 中间状态。
func (c *AssetMutationCoordinator) Read(fn func() error) error {
	c.gate.RLock()
	defer c.gate.RUnlock()
	return fn()
}

// Apply 原子应用一组资产变更。items 必须按确定顺序传入；空批次拒绝。
func (c *AssetMutationCoordinator) Apply(items []repository.AssetMutationItem) error {
	_, err := c.ApplyWithID(items)
	return err
}

// ApplyWithID 原子应用一组资产变更并返回可关联审计与复制的操作标识。
func (c *AssetMutationCoordinator) ApplyWithID(items []repository.AssetMutationItem) (string, error) {
	if len(items) == 0 {
		return "", fmt.Errorf("%w: 资产操作不能为空", ErrValidation)
	}
	c.gate.Lock()
	defer c.gate.Unlock()
	return c.applyLockedWithID(items)
}

// ApplyWithOperationID 使用调用方预先分配的操作标识原子应用资产变更。
// 统一资产操作需在规划失败时也返回同一标识，成功时则将其写入持久化操作记录。
func (c *AssetMutationCoordinator) ApplyWithOperationID(operationID string, items []repository.AssetMutationItem) (string, error) {
	return c.ApplyWithOperationIDAndHook(operationID, items, nil)
}

// ApplyWithOperationIDAndHook 在本地资产操作与附加审计同一事务内完成。
func (c *AssetMutationCoordinator) ApplyWithOperationIDAndHook(operationID string, items []repository.AssetMutationItem, hook repository.MutationCompletionHook) (string, error) {
	if len(items) == 0 {
		return operationID, fmt.Errorf("%w: 资产操作不能为空", ErrValidation)
	}
	c.gate.Lock()
	defer c.gate.Unlock()
	return c.applyLockedWithEnvelopeAndHook(items, nil, operationID, hook)
}

// ApplyWithOutbox 在资产完成事务中同时写入不可拆分的复制 operation outbox。
// 调用方应提供完整、已排序的 operation items；空清单不能建立复制记录。
func (c *AssetMutationCoordinator) ApplyWithOutbox(items []repository.AssetMutationItem, envelope repository.OperationEnvelope) error {
	return c.ApplyWithOutboxAndHook(items, envelope, nil)
}

// ApplyWithOutboxAndHook 在资产、operation outbox 与源端审计同一事务内完成。
func (c *AssetMutationCoordinator) ApplyWithOutboxAndHook(items []repository.AssetMutationItem, envelope repository.OperationEnvelope, hook repository.MutationCompletionHook) error {
	if len(items) == 0 || len(envelope.Items) == 0 {
		return fmt.Errorf("%w: 资产操作和 operation 清单不能为空", ErrValidation)
	}
	if envelope.OperationID == "" {
		envelope.OperationID = newOperationID()
	}
	if envelope.ItemCount == 0 {
		envelope.ItemCount = len(envelope.Items)
	}
	c.gate.Lock()
	defer c.gate.Unlock()
	_, err := c.applyLockedWithEnvelopeAndHook(items, &envelope, "", hook)
	return err
}

func (c *AssetMutationCoordinator) applyLocked(items []repository.AssetMutationItem) error {
	_, err := c.applyLockedWithID(items)
	return err
}

func (c *AssetMutationCoordinator) applyLockedWithID(items []repository.AssetMutationItem) (string, error) {
	return c.applyLockedWithEnvelope(items, nil)
}

func (c *AssetMutationCoordinator) applyLockedWithHook(items []repository.AssetMutationItem, hook repository.MutationCompletionHook) (string, error) {
	return c.applyLockedWithEnvelopeAndHook(items, nil, "", hook)
}

func (c *AssetMutationCoordinator) applyLockedWithEnvelope(items []repository.AssetMutationItem, envelope *repository.OperationEnvelope) (string, error) {
	return c.applyLockedWithEnvelopeAndID(items, envelope, "")
}

func (c *AssetMutationCoordinator) applyLockedWithEnvelopeAndID(items []repository.AssetMutationItem, envelope *repository.OperationEnvelope, operationID string) (string, error) {
	return c.applyLockedWithEnvelopeAndHook(items, envelope, operationID, nil)
}

func (c *AssetMutationCoordinator) applyLockedWithEnvelopeAndHook(items []repository.AssetMutationItem, envelope *repository.OperationEnvelope, operationID string, hook repository.MutationCompletionHook) (string, error) {
	id := operationID
	if id == "" {
		id = newOperationID()
	}
	if envelope != nil {
		id = envelope.OperationID
	}
	for i := range items {
		items[i].OperationID = id
		items[i].Ordinal = i
	}
	beginErr := c.mutations.Begin(id, items)
	if beginErr != nil {
		return id, beginErr
	}
	entries, err := c.stageUnreferenced(id, items)
	if err != nil {
		_ = c.mutations.MarkRollingBack(id, err.Error())
		c.restore(id, entries)
		_ = c.mutations.MarkRolledBack(id, err.Error())
		return id, err
	}
	if err := c.mutations.MarkStaged(id, quarantineRecords(entries)); err != nil {
		c.restore(id, entries)
		_ = c.mutations.MarkRolledBack(id, err.Error())
		return id, err
	}
	snapshot, err := c.blobs.CreateRollbackSnapshot(entries)
	if err != nil {
		return id, c.rollbackBeforeCompletion(id, entries, blobstore.RollbackSnapshot{}, err)
	}
	for _, entry := range entries {
		if err := c.mutations.MarkQuarantine(id, entry.Hash, "pending_gc", ""); err != nil {
			return id, c.rollbackBeforeCompletion(id, entries, snapshot, err)
		}
		if err := c.finalize(entry); err != nil {
			return id, c.rollbackBeforeCompletion(id, entries, snapshot, err)
		}
	}
	var completeErr error
	if envelope == nil && hook != nil {
		completeErr = c.mutations.CompleteWithHook(id, items, hook)
	} else if envelope == nil {
		completeErr = c.mutations.Complete(id, items)
	} else if hook != nil {
		outbox := repository.NewReplicationOperationRepo(c.mutations.DB())
		completeErr = c.mutations.CompleteWithOutboxAndHook(id, items, outbox, *envelope, hook)
	} else {
		outbox := repository.NewReplicationOperationRepo(c.mutations.DB())
		completeErr = c.mutations.CompleteWithOutbox(id, items, outbox, *envelope)
	}
	if completeErr != nil {
		return id, c.rollbackBeforeCompletion(id, entries, snapshot, completeErr)
	}
	if snapshot.OperationID != "" {
		if err := c.finalizeSnapshot(snapshot); err != nil {
			// 资产视图、outbox/receipt、实体版本和审计已经在同一事务提交。
			// 此后快照仅是冗余回滚副本，删除失败只能留给启动清理重试，
			// 不能再补偿回滚已发布的业务状态。
			for _, entry := range entries {
				_ = c.mutations.MarkQuarantine(id, entry.Hash, "pending_gc", err.Error())
			}
			return id, nil
		}
	}
	for _, entry := range entries {
		_ = c.mutations.MarkQuarantine(id, entry.Hash, "deleted", "")
	}
	return id, nil
}

func (c *AssetMutationCoordinator) rollbackBeforeCompletion(id string, entries []blobstore.QuarantineEntry, snapshot blobstore.RollbackSnapshot, cause error) error {
	_ = c.mutations.MarkRollingBack(id, cause.Error())
	if err := c.restoreEntries(entries, snapshot); err != nil {
		return fmt.Errorf("物理回收失败且无法恢复 blob：%w", err)
	}
	for _, entry := range entries {
		if err := c.mutations.MarkQuarantine(id, entry.Hash, "restored", cause.Error()); err != nil {
			return err
		}
	}
	if err := c.mutations.MarkRolledBack(id, cause.Error()); err != nil {
		return err
	}
	return cause
}

func (c *AssetMutationCoordinator) restoreEntries(entries []blobstore.QuarantineEntry, snapshot blobstore.RollbackSnapshot) error {
	if snapshot.OperationID != "" {
		return c.blobs.RestoreRollbackSnapshot(snapshot)
	}
	for _, entry := range entries {
		if err := c.blobs.Restore(entry); err != nil {
			return err
		}
	}
	return nil
}

func quarantineRecords(entries []blobstore.QuarantineEntry) []repository.BlobQuarantine {
	result := make([]repository.BlobQuarantine, 0, len(entries))
	for _, entry := range entries {
		result = append(result, repository.BlobQuarantine{OperationID: entry.OperationID, BlobHash: entry.Hash, QuarantinePath: entry.Path})
	}
	return result
}

// Put 按当前读门内的前镜像覆盖写入单个资产，避免并发覆盖使用陈旧引用计数。
func (c *AssetMutationCoordinator) Put(asset *repository.Asset) error {
	if asset == nil {
		return fmt.Errorf("%w: 资产不能为空", ErrValidation)
	}
	c.gate.Lock()
	defer c.gate.Unlock()
	before, err := c.current(asset.RepositoryID, asset.Path)
	if err != nil {
		return err
	}
	return c.applyLocked([]repository.AssetMutationItem{{RepositoryID: asset.RepositoryID, Path: asset.Path, Before: before, After: asset}})
}

// PutWithCommitHook 在资产视图和调用方附加元数据同一事务完成后才对外可见。
func (c *AssetMutationCoordinator) PutWithCommitHook(asset *repository.Asset, hook repository.MutationCompletionHook) error {
	if asset == nil || hook == nil {
		return fmt.Errorf("%w: 资产或提交回调不能为空", ErrValidation)
	}
	c.gate.Lock()
	defer c.gate.Unlock()
	before, err := c.current(asset.RepositoryID, asset.Path)
	if err != nil {
		return err
	}
	_, err = c.applyLockedWithHook([]repository.AssetMutationItem{{RepositoryID: asset.RepositoryID, Path: asset.Path, Before: before, After: asset}}, hook)
	return err
}

// Delete 按当前读门内的前镜像删除单个资产。
func (c *AssetMutationCoordinator) Delete(repositoryID int64, path string) error {
	c.gate.Lock()
	defer c.gate.Unlock()
	before, err := c.current(repositoryID, path)
	if err != nil {
		return err
	}
	if before == nil {
		return repository.ErrNotFound
	}
	return c.applyLocked([]repository.AssetMutationItem{{RepositoryID: repositoryID, Path: path, Before: before}})
}

// RemoveIfUnreferenced 清理失败发布留下的孤立活动 blob；有任一活动引用时保持不动。
func (c *AssetMutationCoordinator) RemoveIfUnreferenced(hash string) error {
	finish, ok := c.blobs.BeginUnreferencedRemoval(hash)
	if !ok {
		return nil
	}
	defer finish()
	c.gate.Lock()
	count, err := c.mutations.CountBlobReferences(hash)
	c.gate.Unlock()
	if err != nil || count != 0 {
		return err
	}
	// 孤立删除预约已阻止同 hash 暂存写入；磁盘 IO 必须在元数据门外执行。
	return c.removeUnreferenced(hash)
}

func (c *AssetMutationCoordinator) current(repositoryID int64, path string) (*repository.Asset, error) {
	// AssetMutationRepo 只负责操作状态；读取当前资产经其底层数据库执行，避免另建连接。
	return c.mutations.GetAsset(repositoryID, path)
}

func (c *AssetMutationCoordinator) stageUnreferenced(id string, items []repository.AssetMutationItem) ([]blobstore.QuarantineEntry, error) {
	removed := make(map[string]int)
	added := make(map[string]int)
	for _, item := range items {
		beforeBlobHash := afterHash(item.Before)
		afterBlobHash := afterHash(item.After)
		if beforeBlobHash == afterBlobHash {
			continue
		}
		if beforeBlobHash != "" {
			removed[beforeBlobHash]++
		}
		if afterBlobHash != "" {
			added[afterBlobHash]++
		}
	}
	var entries []blobstore.QuarantineEntry
	for hash, removeCount := range removed {
		if c.retainBlob != nil && c.retainBlob(hash) {
			continue
		}
		count, err := c.mutations.CountBlobReferences(hash)
		if err != nil {
			return entries, err
		}
		if count-removeCount+added[hash] != 0 {
			continue
		}
		entry, err := c.blobs.Stage(hash, id)
		if err != nil {
			return entries, err
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

func afterHash(asset *repository.Asset) string {
	if asset == nil {
		return ""
	}
	return asset.BlobHash
}

func (c *AssetMutationCoordinator) restore(id string, staged []blobstore.QuarantineEntry) {
	for _, entry := range staged {
		if err := c.blobs.Restore(entry); err != nil {
			_ = c.mutations.MarkQuarantine(id, entry.Hash, "staged", err.Error())
			continue
		}
		_ = c.mutations.MarkQuarantine(id, entry.Hash, "restored", "")
	}
}

// Recover 恢复进程崩溃时遗留的全部 intent，并重试已完成操作的快照清理。
func (c *AssetMutationCoordinator) Recover() error { return c.recover(false) }

// RecoverReceived 只恢复 standby 当前父流接收产生的 intent，不触碰旧角色留下的本地 intent。
func (c *AssetMutationCoordinator) RecoverReceived() error { return c.recover(true) }

func (c *AssetMutationCoordinator) recover(receivedOnly bool) error {
	c.gate.Lock()
	defer c.gate.Unlock()
	var incomplete []repository.AssetMutation
	var err error
	if receivedOnly {
		incomplete, err = c.mutations.GetIncompleteReceived()
	} else {
		incomplete, err = c.mutations.GetIncomplete()
	}
	if err != nil {
		return err
	}
	for _, op := range incomplete {
		entries, err := c.mutations.ListQuarantine(op.ID)
		if err != nil {
			return err
		}
		staged := make([]blobstore.QuarantineEntry, 0, len(entries))
		for _, entry := range entries {
			if entry.Status == "staged" || entry.Status == "pending_gc" {
				staged = append(staged, blobstore.QuarantineEntry{OperationID: entry.OperationID, Hash: entry.BlobHash, Path: entry.QuarantinePath})
			}
		}
		if len(staged) > 0 {
			snapshot := c.blobs.RollbackSnapshot(op.ID)
			if _, err := os.Stat(snapshot.Path); err == nil {
				if err := c.restoreEntries(staged, snapshot); err != nil {
					return err
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			} else if err := c.restoreEntries(staged, blobstore.RollbackSnapshot{}); err != nil {
				return err
			}
			for _, entry := range staged {
				if err := c.mutations.MarkQuarantine(op.ID, entry.Hash, "restored", ""); err != nil {
					return err
				}
			}
		}
		if err := c.mutations.MarkRolledBack(op.ID, "进程恢复回滚未完成资产操作"); err != nil {
			return err
		}
	}
	pending, err := c.mutations.ListPendingQuarantine()
	if err != nil {
		return err
	}
	for _, entry := range pending {
		op, err := c.mutations.Get(entry.OperationID)
		if err != nil {
			return err
		}
		if op.Status != "completed" {
			continue
		}
		entries, err := c.mutations.ListQuarantine(entry.OperationID)
		if err != nil {
			return err
		}
		staged := make([]blobstore.QuarantineEntry, 0, len(entries))
		for _, item := range entries {
			staged = append(staged, blobstore.QuarantineEntry{OperationID: item.OperationID, Hash: item.BlobHash, Path: item.QuarantinePath})
		}
		snapshot := c.blobs.RollbackSnapshot(entry.OperationID)
		if _, err := os.Stat(snapshot.Path); err == nil {
			if err := c.finalizeSnapshot(snapshot); err != nil {
				return err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		} else if c.quarantineEntriesGone(staged) {
			for _, item := range staged {
				if err := c.mutations.MarkQuarantine(item.OperationID, item.Hash, "deleted", ""); err != nil {
					return err
				}
			}
			continue
		} else {
			return errors.New("已完成资产操作的回滚快照缺失但隔离文件仍存在")
		}
		for _, item := range staged {
			if err := c.mutations.MarkQuarantine(item.OperationID, item.Hash, "deleted", ""); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *AssetMutationCoordinator) quarantineEntriesGone(entries []blobstore.QuarantineEntry) bool {
	for _, entry := range entries {
		if _, err := os.Stat(entry.Path); err == nil || !errors.Is(err, os.ErrNotExist) {
			return false
		}
	}
	return true
}

func newOperationID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		sequence := operationIDFallback.Add(1)
		for i := len(b) - 1; i >= 8; i-- {
			b[i] = byte(sequence)
			sequence >>= 8
		}
	}
	millis := uint64(time.Now().UnixMilli())
	for i := 5; i >= 0; i-- {
		b[i] = byte(millis)
		millis >>= 8
	}
	b[6] = b[6]&0x0f | 0x70
	b[8] = b[8]&0x3f | 0x80
	encoded := hex.EncodeToString(b[:])
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:]
}

// NewOperationID 为尚未进入领域执行阶段的统一制品操作生成追踪标识。
func NewOperationID() string {
	return newOperationID()
}
