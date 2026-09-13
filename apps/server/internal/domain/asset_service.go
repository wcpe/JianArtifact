package domain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jmoiron/sqlx"
	"golang.org/x/sync/singleflight"

	"github.com/wcpe/jianartifact/apps/server/internal/blobstore"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
	"github.com/wcpe/jianartifact/apps/server/internal/upstream"
)

// maxResolveDepth 限制 group 成员递归解析深度，防止成员相互引用形成环导致的无限递归。
const maxResolveDepth = 16

// AssetService 编排制品的发布与拉取：元数据落 asset 表，内容落内容寻址 blob。
//
// 分层（见 internal/doc.go）：domain -> repository, blobstore, upstream。协议层
// （internal/protocol）经此服务读写制品，不直接触碰 SQLite、文件系统或上游 HTTP。
//
// 读路径按仓库 Type 分派（见 Resolve）：hosted 读本地缓存；proxy 命中即返回，
// 未命中经 upstream 回源并缓存（single-flight 收敛并发回源）；group 先本地快查、
// 未命中则并行回源（见 groupGet），并对不可达上游做 auto-block 短窗阻止（见 proxyHealth）。
type AssetService struct {
	repos     *repository.RepoRepo
	assets    *repository.AssetRepo
	blobs     *blobstore.Store
	upstream  *upstream.Client
	mutator   *AssetMutationCoordinator
	writeGate BusinessWriteGate
	sf        singleflight.Group
	recorder  ChangeRecorder
	nodeID    func() string
	health    *proxyHealth
	negCache  *negativeCache // FR-111：404 负缓存（proxy/group 读路径短窗缓存明确 404）
}

// NewAssetService 构造 AssetService。upstream 供 proxy 回源使用（hosted-only 部署可传 nil）。
func NewAssetService(repos *repository.RepoRepo, assets *repository.AssetRepo, blobs *blobstore.Store, up *upstream.Client) *AssetService {
	mutator := NewAssetMutationCoordinatorDeferred(assets.DB(), blobs)
	svc := &AssetService{repos: repos, assets: assets, blobs: blobs, upstream: up, mutator: mutator, negCache: newNegativeCache()}
	svc.health = newProxyHealth(svc.probeUpstream)
	return svc
}

// SetMutationCoordinator 注入与其它写路径共享的节点级协调器。
func (s *AssetService) SetMutationCoordinator(c *AssetMutationCoordinator) { s.mutator = c }

// MutationCoordinator 返回当前节点共享的资产生命周期协调器，供同一装配图中的写服务复用。
func (s *AssetService) MutationCoordinator() *AssetMutationCoordinator { return s.mutator }

// RecoverMutationIntents 显式处理启动时遗留的全部资产 intent，仅供可写节点。
func (s *AssetService) RecoverMutationIntents() error {
	if s.mutator == nil {
		return nil
	}
	return s.mutator.Recover()
}

// RecoverReceivedMutationIntents 仅恢复复制接收产生的 intent，供 standby 启动时
// 解除崩溃前 prepared/staged/committing 批次；旧角色本地 intent 保持不动。
func (s *AssetService) RecoverReceivedMutationIntents() error {
	if s.mutator == nil {
		return nil
	}
	return s.mutator.RecoverReceived()
}

// CleanupUnreferencedBlobs 清理由失败写入遗留且没有任何活动资产引用的 blob。
// 该方法仅由 primary 的定时任务调用，服务启动阶段不得调用。
func (s *AssetService) CleanupUnreferencedBlobs() (int, error) {
	if s.mutator == nil {
		return 0, nil
	}
	removed := 0
	err := s.blobs.WalkActive(func(hash string) error {
		if err := s.mutator.RemoveIfUnreferenced(hash); err != nil {
			return err
		}
		if !s.blobs.Exists(hash) {
			removed++
		}
		return nil
	})
	return removed, err
}

// SetBusinessWriteGate 注入本地业务写栅栏。复制接收直接由 ReplicationService 应用，
// 不经 AssetService，因此不会被备用节点只读策略拦截。
func (s *AssetService) SetBusinessWriteGate(gate BusinessWriteGate) { s.writeGate = gate }

func (s *AssetService) requireBusinessWrite() error {
	return requireBusinessWrite(s.writeGate)
}

// SetAutoBlockBase 调整 auto-block 退避起始时长（默认 40s；测试与调优用，<=0 忽略）。
func (s *AssetService) SetAutoBlockBase(d time.Duration) { s.health.setBase(d) }

// SetNegativeCacheTTL 调整 404 负缓存 TTL（默认 60s；测试与调优用，<=0 忽略）。
func (s *AssetService) SetNegativeCacheTTL(d time.Duration) { s.negCache.setTTL(d) }

// Status 返回 proxy 仓库上游连接状态（供 FR-114 状态展示）。
func (s *AssetService) Status(repoID int64) RemoteHealth { return s.health.status(repoID) }

// RecheckConnection 手动重测 proxy 仓库上游连接（FR-114）：立即 HEAD 探测并更新
// auto-block 状态，返回最新状态视图。仅 online 仓库可调用（调用方负责校验）。
func (s *AssetService) RecheckConnection(repoID int64, remoteURL string) RemoteHealth {
	credentialRef := ""
	if repo, err := s.repos.GetByID(repoID); err == nil {
		if cfg, err := repo.DecodeConfig(); err == nil && cfg.RemoteURL != "" {
			remoteURL = cfg.RemoteURL
			credentialRef = cfg.CredentialRef
		}
	}
	return s.health.recheck(repoID, remoteURL, credentialRef)
}

// SetChangeRecorder 注入复制变更日志记录器（FR-83）；nil 表示不记录。
// 记录器为 ReplicationService 时，顺带把本服务的 404 负缓存注入对端共享，
// SetChangeRecorder 注入变更记录器（复制退役后为 no-op 实现，保留注入点）。
// 另需 SetNodeIdentity 提供节点稳定标识：原子 operation 信封的 version 要用它。
func (s *AssetService) SetChangeRecorder(r ChangeRecorder) { s.recorder = r }

// SetNodeIdentity 注入节点稳定标识（信封 version.NodeID 必须跨重启稳定）。
func (s *AssetService) SetNodeIdentity(n *NodeIdentity) { s.nodeID = n.NodeID }

// NodeID 返回节点稳定标识（供格式元数据等信封构造方使用）。
// 未接线时回退 "local"：单元测试直接构造 AssetService 的场景兜底；
// 生产装配必须经 SetNodeIdentity 注入真实标识（wiring_publish_test 守护）。
func (s *AssetService) NodeID() string {
	if s.nodeID != nil {
		return s.nodeID()
	}
	return "local"
}

// Put 向 hosted 仓库发布一件制品：流式写入 blob，再覆盖写 asset 元数据。
// 仓库不存在返回 ErrNotFound；非 hosted 仓库（proxy/group）返回 ErrConflict。
// 格式（raw/maven/npm）语义由协议层按仓库 format 分派后自行处理，此处仅按内容寻址存字节。
func (s *AssetService) Put(repoName, path string, r io.Reader, contentType string) (*repository.Asset, error) {
	if err := s.requireBusinessWrite(); err != nil {
		return nil, err
	}
	repo, err := s.repos.GetByName(repoName)
	if err != nil {
		return nil, mapNotFound(err)
	}
	if repo.Type != "hosted" {
		return nil, ErrConflict
	}
	// 复制退役后所有节点都是普通节点：发布一律走原子 operation 信封路径。
	return s.putWithPrimaryOperation(repoName, repo, path, r, contentType)
}

// putWithPrimaryOperation 将主节点的单制品发布写入不可拆分的 v2 operation，
// 避免 standby 仅拉取 v2 时遗漏仍停留在旧变更流中的协议发布。
func (s *AssetService) putWithPrimaryOperation(repoName string, repo *repository.Repository, path string, r io.Reader, contentType string) (*repository.Asset, error) {
	asset, err := s.StageBlob(r, contentType)
	if err != nil {
		return nil, err
	}
	asset.RepositoryID = repo.ID
	asset.Path = path
	if _, err := s.PublishAssets(repoName, []*repository.Asset{asset}); err != nil {
		return nil, s.cleanupFailedWrite(asset.BlobHash, err)
	}
	stored, err := s.assets.GetByPath(repo.ID, path)
	if err != nil {
		return nil, err
	}
	s.negCache.remove(repo.ID, path)
	return stored, nil
}

// PutWithTimestamps 与 Put 语义相同，但将资产 created_at/updated_at 固定为源端时间，
// 用于在线迁移保留源 Nexus 资产的时间戳。sourceModified 为零值时回退到 Put（使用本地当前时间），
// 离线路径或缺失时间戳时语义等同 Put。时间按 UTC "YYYY-MM-DD HH:MM:SS" 写入，
// 与 asset 表 datetime('now') 默认值格式一致。
func (s *AssetService) PutWithTimestamps(repoName, path string, r io.Reader, contentType string, sourceModified time.Time) (*repository.Asset, error) {
	if sourceModified.IsZero() {
		return s.Put(repoName, path, r, contentType)
	}
	return s.PutWithCommitHook(repoName, path, r, contentType, func(asset *repository.Asset) repository.MutationCompletionHook {
		// 源端时间写入资产行：created_at 与 updated_at 均与源一致（用户要求迁移后两时间都对齐源）。
		applySourceTimestamp(asset, sourceModified)
		return func(tx *sqlx.Tx) error { return nil }
	})
}

// applySourceTimestamp 将资产行 created_at/updated_at 固定为源端时间的 UTC
// "YYYY-MM-DD HH:MM:SS"（与 asset 表 datetime('now') 默认值格式一致）；
// 零值不修改，保持 datetime('now') 本地时间语义。供各格式迁移导入器在
// PublishAssets 批次发布前标注资产行时间。
func applySourceTimestamp(asset *repository.Asset, sourceModified time.Time) {
	if asset == nil || sourceModified.IsZero() {
		return
	}
	ts := sourceModified.UTC().Format("2006-01-02 15:04:05")
	asset.CreatedAt = ts
	asset.UpdatedAt = ts
}

// PutWithCommitHook 将内容写入 blob 后，与调用方的同库元数据一起完成资产事务。
// 附加事务失败时，资产视图与新写入的无引用 blob 都不会对外保留。
func (s *AssetService) PutWithCommitHook(repoName, path string, r io.Reader, contentType string, hook func(*repository.Asset) repository.MutationCompletionHook) (*repository.Asset, error) {
	if err := s.requireBusinessWrite(); err != nil {
		return nil, err
	}
	repo, err := s.repos.GetByName(repoName)
	if err != nil {
		return nil, mapNotFound(err)
	}
	if repo.Type != "hosted" || hook == nil || s.mutator == nil {
		return nil, ErrConflict
	}
	hash, sha1sum, md5sum, size, err := s.putStagedBlob(r)
	if err != nil {
		return nil, err
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	asset := &repository.Asset{RepositoryID: repo.ID, Path: path, BlobHash: hash, Size: size, ContentType: contentType, Sha1: sha1sum, Md5: md5sum}
	if err := s.mutator.PutWithCommitHook(asset, hook(asset)); err != nil {
		return nil, s.cleanupFailedWrite(hash, err)
	}
	s.releaseStagedBlob(hash)
	stored, err := s.assets.GetByPath(repo.ID, path)
	if err != nil {
		return nil, err
	}
	s.negCache.remove(repo.ID, path)
	s.recordAssetPut(repo.Name, stored)
	return stored, nil
}

// StageBlob 将内容写入内容寻址存储，但不建立可见资产引用。
// 仅调用方的原子资产提交成功后，返回的资产才能对外可见。
func (s *AssetService) StageBlob(r io.Reader, contentType string) (*repository.Asset, error) {
	if err := s.requireBusinessWrite(); err != nil {
		return nil, err
	}
	hash, sha1sum, md5sum, size, err := s.putStagedBlob(r)
	if err != nil {
		return nil, err
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	return &repository.Asset{BlobHash: hash, Size: size, ContentType: contentType, Sha1: sha1sum, Md5: md5sum}, nil
}

// StageOCIBlob 保留 OCI 上传调用的兼容入口。
func (s *AssetService) StageOCIBlob(r io.Reader, contentType string) (*repository.Asset, error) {
	return s.StageBlob(r, contentType)
}

func (s *AssetService) putStagedBlob(r io.Reader) (string, string, string, int64, error) {
	if s.mutator == nil {
		return s.blobs.Put(r)
	}
	return s.blobs.PutStaged(r)
}

func (s *AssetService) releaseStagedBlob(hash string) {
	if s.mutator != nil {
		s.blobs.ReleaseStaged(hash)
	}
}

// PublishAssets 将一组已落盘 blob 的资产引用一次性公开。
// 当复制服务可用时，同一 SQLite 事务会同步写入一个 v2 operation outbox。
func (s *AssetService) PublishAssets(repoName string, after []*repository.Asset) (string, error) {
	return s.publishAssets(repoName, after, nil)
}

// PublishAssetsWithCommitHook 将一组资产和附加元数据作为同一事务公开。
func (s *AssetService) PublishAssetsWithCommitHook(repoName string, after []*repository.Asset, hook repository.MutationCompletionHook) (string, error) {
	if hook == nil {
		return "", ErrValidation
	}
	return s.publishAssets(repoName, after, hook)
}

func (s *AssetService) publishAssets(repoName string, after []*repository.Asset, hook repository.MutationCompletionHook) (string, error) {
	if err := s.requireBusinessWrite(); err != nil {
		return "", err
	}
	repo, err := s.repos.GetByName(repoName)
	if err != nil {
		return "", mapNotFound(err)
	}
	proxyMetadataBatch := (repo.Format == "pypi" || repo.Format == "nuget") && repo.Type == "proxy" && hook != nil
	if (repo.Type != "hosted" && !proxyMetadataBatch) || len(after) == 0 {
		return "", ErrConflict
	}
	items, err := s.publishMutationItems(repo, after)
	if err != nil {
		return "", err
	}
	operationID := newOperationID()
	if repo.Type == "hosted" {
		nid := s.NodeID()
		envelope, envelopeErr := ociOperationEnvelope(nid, repoName, operationID, items)
		if envelopeErr != nil {
			return "", envelopeErr
		}
		if err := s.mutator.ApplyWithOutboxAndHook(items, envelope, hook); err != nil {
			return "", err
		}
	} else if _, err := s.mutator.ApplyWithOperationIDAndHook(operationID, items, hook); err != nil {
		return "", err
	}
	s.releasePublishedBlobs(after)
	return operationID, nil
}

// PublishAssetsWithEnvelope 将资产、格式元数据等同批逻辑实体和源端审计绑定到一个 v2 operation。
// 调用方必须提供包含每个资产项的完整 operation 清单；不额外记录可拆分的 v1 asset change。
func (s *AssetService) PublishAssetsWithEnvelope(repoName string, after []*repository.Asset, envelope repository.OperationEnvelope, hook repository.MutationCompletionHook) (string, error) {
	if err := s.requireBusinessWrite(); err != nil {
		return envelope.OperationID, err
	}
	repo, err := s.repos.GetByName(repoName)
	if err != nil {
		return envelope.OperationID, mapNotFound(err)
	}
	if repo.Type != "hosted" && !((repo.Format == "pypi" || repo.Format == "nuget") && repo.Type == "proxy") {
		return envelope.OperationID, ErrConflict
	}
	if hook == nil || envelope.OperationID == "" {
		return envelope.OperationID, ErrValidation
	}
	items, err := s.publishMutationItems(repo, after)
	if err != nil {
		return envelope.OperationID, err
	}
	if err := s.mutator.ApplyWithOutboxAndHook(items, envelope, hook); err != nil {
		return envelope.OperationID, err
	}
	s.releasePublishedBlobs(after)
	for _, item := range items {
		s.negCache.remove(repo.ID, item.Path)
	}
	return envelope.OperationID, nil
}

func (s *AssetService) releasePublishedBlobs(assets []*repository.Asset) {
	for _, asset := range assets {
		if asset != nil {
			s.releaseStagedBlob(asset.BlobHash)
		}
	}
}

// DiscardStagedAssets 释放未提交批次的暂存内容，并仅在不存在活动引用时删除 blob。
// 调用方在任一预检、暂存或提交失败时调用，避免失败批次留下孤立内容。
func (s *AssetService) DiscardStagedAssets(assets []*repository.Asset) error {
	seen := make(map[string]struct{}, len(assets))
	for _, asset := range assets {
		if asset == nil || asset.BlobHash == "" {
			continue
		}
		if _, exists := seen[asset.BlobHash]; exists {
			continue
		}
		seen[asset.BlobHash] = struct{}{}
		s.releaseStagedBlob(asset.BlobHash)
		if s.mutator == nil {
			if err := s.blobs.Remove(asset.BlobHash); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			continue
		}
		if err := s.mutator.RemoveIfUnreferenced(asset.BlobHash); err != nil {
			return err
		}
	}
	return nil
}

func (s *AssetService) publishMutationItems(repo *repository.Repository, after []*repository.Asset) ([]repository.AssetMutationItem, error) {
	if len(after) == 0 {
		return nil, ErrConflict
	}
	items := make([]repository.AssetMutationItem, 0, len(after))
	for _, asset := range after {
		if asset == nil || asset.Path == "" || asset.BlobHash == "" {
			return nil, ErrValidation
		}
		copy := *asset
		copy.RepositoryID = repo.ID
		before, getErr := s.assets.GetByPath(repo.ID, copy.Path)
		if getErr != nil && !errors.Is(getErr, repository.ErrNotFound) {
			return nil, getErr
		}
		if errors.Is(getErr, repository.ErrNotFound) {
			before = nil
		}
		items = append(items, repository.AssetMutationItem{RepositoryID: repo.ID, Path: copy.Path, Before: before, After: &copy})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Path < items[j].Path })
	if err := validateUniqueAssetPaths(items); err != nil {
		return nil, err
	}
	return items, nil
}

// PublishOCIAssets 保留 OCI 发布调用的兼容入口。
func (s *AssetService) PublishOCIAssets(repoName string, after []*repository.Asset) (string, error) {
	return s.PublishAssets(repoName, after)
}

func validateUniqueAssetPaths(items []repository.AssetMutationItem) error {
	for i := 1; i < len(items); i++ {
		if items[i-1].Path == items[i].Path {
			return ErrValidation
		}
	}
	return nil
}

func ociOperationEnvelope(nodeID, repoName, operationID string, items []repository.AssetMutationItem) (repository.OperationEnvelope, error) {
	version := repository.ReplicationVersion{NodeID: nodeID, TS: time.Now().UTC().Format(time.RFC3339Nano)}
	operationItems := make([]repository.OperationItem, 0, len(items))
	for _, item := range items {
		data, err := json.Marshal(AssetChangeData{Path: item.After.Path, BlobHash: item.After.BlobHash, Size: item.After.Size,
			ContentType: item.After.ContentType, Sha1: item.After.Sha1, Md5: item.After.Md5})
		if err != nil {
			return repository.OperationEnvelope{}, err
		}
		operationItems = append(operationItems, repository.OperationItem{Type: EntityAsset, Key: AssetKey(repoName, item.After.Path),
			Op: OpPut, Data: string(data), BlobHashes: []string{item.After.BlobHash}, DependsOn: []string{RepoKey(repoName)}, Version: version})
	}
	manifest, err := repository.OperationManifestSHA256(operationItems)
	if err != nil {
		return repository.OperationEnvelope{}, err
	}
	return repository.OperationEnvelope{Kind: "operation", OperationID: operationID, SourceNode: version.NodeID,
		Version: version, ItemCount: len(operationItems), ManifestSHA256: manifest, Items: operationItems}, nil
}

// CacheVerifiedPyPIProxy 将已由 PyPI 专用上游链路读取的文件落入 proxy 本地投影。
// 调用方必须在传入前完成 URL 边界校验；该方法仅接受声明哈希完全匹配的内容。
func (s *AssetService) CacheVerifiedPyPIProxy(repoName, path, expectedSHA256 string, r io.Reader, contentType string) (*repository.Asset, error) {
	return s.cacheVerifiedPyPIProxy(repoName, path, expectedSHA256, r, contentType, nil)
}

// CacheVerifiedPyPIProxyWithCommitHook 将 proxy 包与其格式元数据作为同一资产事务提交。
// 元数据写入失败时，包资产不会对外可见。
func (s *AssetService) CacheVerifiedPyPIProxyWithCommitHook(repoName, path, expectedSHA256 string, r io.Reader, contentType string, hook func(*repository.Asset) repository.MutationCompletionHook) (*repository.Asset, error) {
	if hook == nil {
		return nil, ErrValidation
	}
	return s.cacheVerifiedPyPIProxy(repoName, path, expectedSHA256, r, contentType, hook)
}

func (s *AssetService) cacheVerifiedPyPIProxy(repoName, path, expectedSHA256 string, r io.Reader, contentType string, hook func(*repository.Asset) repository.MutationCompletionHook) (*repository.Asset, error) {
	if err := s.requireBusinessWrite(); err != nil {
		return nil, err
	}
	repo, err := s.repos.GetByName(repoName)
	if err != nil {
		return nil, mapNotFound(err)
	}
	if repo.Format != "pypi" || repo.Type != "proxy" {
		return nil, ErrConflict
	}
	if hook != nil && s.mutator == nil {
		return nil, ErrConflict
	}
	hash, sha1sum, md5sum, size, err := s.putStagedBlob(r)
	if err != nil {
		return nil, err
	}
	if hash != expectedSHA256 {
		return nil, s.cleanupFailedWrite(hash, ErrValidation)
	}
	asset, err := s.persistAssetWithHook(repo, path, hash, sha1sum, md5sum, size, contentType, hook)
	if err != nil {
		return nil, s.cleanupFailedWrite(hash, err)
	}
	return asset, nil
}

func (s *AssetService) persistAssetWithHook(repo *repository.Repository, path, hash, sha1sum, md5sum string, size int64, contentType string, hook func(*repository.Asset) repository.MutationCompletionHook) (*repository.Asset, error) {
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	after := &repository.Asset{RepositoryID: repo.ID, Path: path, BlobHash: hash, Size: size, ContentType: contentType, Sha1: sha1sum, Md5: md5sum}
	if s.mutator != nil {
		var err error
		if hook != nil {
			err = s.mutator.PutWithCommitHook(after, hook(after))
		} else {
			err = s.mutator.Put(after)
		}
		if err != nil {
			return nil, err
		}
	} else if hook != nil {
		return nil, ErrConflict
	} else if err := s.assets.Upsert(repo.ID, path, hash, size, contentType, sha1sum, md5sum); err != nil {
		return nil, err
	}
	s.releaseStagedBlob(hash)
	asset, err := s.assets.GetByPath(repo.ID, path)
	if err != nil {
		return nil, err
	}
	// 写路径成功：同路径此前「确认不存在」的负缓存失效，新制品立即可见。
	s.negCache.remove(repo.ID, path)
	s.recordAssetPut(repo.Name, asset)
	return asset, nil
}

func (s *AssetService) recordAssetPut(repoName string, asset *repository.Asset) {
	if asset == nil {
		return
	}
	s.recordChange(EntityAsset, AssetKey(repoName, asset.Path), OpPut, AssetChangeData{
		Path: asset.Path, BlobHash: asset.BlobHash, Size: asset.Size, ContentType: asset.ContentType, Sha1: asset.Sha1, Md5: asset.Md5,
		CreatedAt: asset.CreatedAt, UpdatedAt: asset.UpdatedAt,
	})
}

// recordChange 记录复制变更日志；记录失败不阻断业务写（复制尽力最终一致，对账兜底，见 ADR-0013）。
func (s *AssetService) recordChange(entityType, entityKey, op string, data any) {
	if s.recorder == nil {
		return
	}
	if err := s.recorder.Record(entityType, entityKey, op, data); err != nil {
		log.Printf("复制变更日志记录失败 entity=%s key=%s op=%s：%v", entityType, entityKey, op, err)
	}
}

// BackfillChecksumsResult 是历史资产 sha1/md5 回填的统计。
type BackfillChecksumsResult struct {
	Scanned   int // 本批扫描条数
	Updated   int // 成功写回条数
	Skipped   int // blob 缺失或校验失败跳过
	Remaining int // 库内仍缺校验和的条数
}

// BackfillChecksums 对 sha1/md5 为空的历史资产从 blob 流式补算并写回（不现算读路径）。
// batch 为单批上限（≤0 默认 500）。可重复调用直至 Remaining=0。
func (s *AssetService) BackfillChecksums(batch int) (*BackfillChecksumsResult, error) {
	if err := s.requireBusinessWrite(); err != nil {
		return nil, err
	}
	if batch <= 0 {
		batch = 500
	}
	list, err := s.assets.ListMissingChecksums(batch)
	if err != nil {
		return nil, err
	}
	res := &BackfillChecksumsResult{Scanned: len(list)}
	for i := range list {
		a := &list[i]
		sha1sum, md5sum, err := s.blobs.Checksums(a.BlobHash)
		if err != nil {
			res.Skipped++
			continue
		}
		if err := s.assets.UpdateChecksums(a.ID, sha1sum, md5sum); err != nil {
			return res, err
		}
		res.Updated++
	}
	remain, err := s.assets.CountMissingChecksums()
	if err != nil {
		return res, err
	}
	res.Remaining = remain
	return res, nil
}

// AssetTimeEntry 是待回填时间的资产条目（时间已格式化为 UTC "YYYY-MM-DD HH:MM:SS"）。
type AssetTimeEntry struct {
	RepoName  string
	Path      string
	CreatedAt string
	UpdatedAt string
}

// BackfillTimesResult 是时间回填的统计。
type BackfillTimesResult struct {
	Scanned int // 传入源条目数
	Updated int // 成功更新时间条数
	Skipped int // 本地无此资产 / 仓库不存在 / 更新失败跳过
}

// BackfillTimes 按 仓库名+路径 回填资产 created_at/updated_at，与源 Nexus 时间对齐。
// batch 为单批上限（≤0 默认 1000）；幂等可重复执行。
func (s *AssetService) BackfillTimes(entries []AssetTimeEntry, batch int) (*BackfillTimesResult, error) {
	if err := s.requireBusinessWrite(); err != nil {
		return nil, err
	}
	if batch <= 0 {
		batch = 1000
	}
	res := &BackfillTimesResult{Scanned: len(entries)}
	repoIDs := make(map[string]int64, 8)
	lookup := func(name string) (int64, bool) {
		if id, ok := repoIDs[name]; ok {
			return id, true
		}
		repo, err := s.repos.GetByName(name)
		if err != nil {
			return 0, false
		}
		repoIDs[name] = repo.ID
		return repo.ID, true
	}
	for i := 0; i < len(entries); i += batch {
		end := i + batch
		if end > len(entries) {
			end = len(entries)
		}
		for _, e := range entries[i:end] {
			repoID, ok := lookup(e.RepoName)
			if !ok {
				res.Skipped++
				continue
			}
			n, err := s.assets.UpdateTimes(repoID, e.Path, e.CreatedAt, e.UpdatedAt)
			if err != nil || n == 0 {
				res.Skipped++
				continue
			}
			res.Updated++
		}
	}
	return res, nil
}

// EmitTimeChanges 为全部 hosted 仓库资产重新记录带创建/更新时间的 put 变更，
// 供对端复制应用后同步时间（幂等：对端已有则更新时间为源值）。返回统计。
func (s *AssetService) EmitTimeChanges() (*BackfillTimesResult, error) {
	if err := s.requireBusinessWrite(); err != nil {
		return nil, err
	}
	res := &BackfillTimesResult{}
	offset := 0
	const pageSize = 100
	for {
		repos, err := s.repos.List(pageSize, offset)
		if err != nil {
			return nil, err
		}
		for i := range repos {
			r := &repos[i]
			if r.Type != "hosted" {
				continue
			}
			for ao := 0; ; ao += 1000 {
				assets, err := s.assets.ListByRepo(r.ID, "", 1000, ao)
				if err != nil {
					return nil, err
				}
				for j := range assets {
					a := &assets[j]
					s.recordChange(EntityAsset, AssetKey(r.Name, a.Path), OpPut, AssetChangeData{
						Path: a.Path, BlobHash: a.BlobHash, Size: a.Size,
						ContentType: a.ContentType, Sha1: a.Sha1, Md5: a.Md5,
						CreatedAt: a.CreatedAt, UpdatedAt: a.UpdatedAt,
					})
					res.Updated++
				}
				if len(assets) < 1000 {
					break
				}
			}
		}
		if len(repos) < pageSize {
			break
		}
		offset += pageSize
	}
	res.Scanned = res.Updated
	return res, nil
}

// Get 从 hosted 仓库拉取一件制品（仅本地缓存读），返回元数据与内容可读流（调用方负责关闭）。
// 仓库或路径不存在均返回 ErrNotFound。proxy/group 的读路径请用 Resolve。
func (s *AssetService) Get(repoName, path string) (*repository.Asset, io.ReadCloser, error) {
	repo, err := s.repos.GetByName(repoName)
	if err != nil {
		return nil, nil, mapNotFound(err)
	}
	return s.localGet(repo, path)
}

// Resolve 按仓库 Type 解析读请求：hosted 读本地；proxy 命中即返回、未命中回源缓存后返回；
// group 按成员有序解析、首个命中即返回。返回元数据与内容可读流（调用方负责关闭）。
// 全未命中返回 ErrNotFound；回源失败返回 ErrUpstream / ErrUpstreamTimeout。
// 目录形路径（空串或以 / 结尾）不是制品，直接 ErrNotFound：否则 proxy 会把上游
// 返回的 HTML 目录索引页当成制品缓存（文件树出现空白名的 text/html 假文件）。
func (s *AssetService) Resolve(ctx context.Context, repoName, path string) (*repository.Asset, io.ReadCloser, error) {
	if path == "" || strings.HasSuffix(path, "/") {
		return nil, nil, ErrNotFound
	}
	return s.resolvePath(ctx, repoName, path)
}

// ResolveIndex 解析并缓存 proxy 的目录索引。
// 普通制品路径仍由 Resolve 拒绝尾斜杠，避免把 Raw 目录页误当制品；
// PyPI Simple 等格式索引明确需要该语义，因此单独开放入口。
func (s *AssetService) ResolveIndex(ctx context.Context, repoName, path string) (*repository.Asset, io.ReadCloser, error) {
	if path == "" || !strings.HasSuffix(path, "/") {
		return nil, nil, ErrNotFound
	}
	return s.resolvePath(ctx, repoName, path)
}

func (s *AssetService) resolvePath(ctx context.Context, repoName, path string) (*repository.Asset, io.ReadCloser, error) {
	repo, err := s.repos.GetByName(repoName)
	if err != nil {
		return nil, nil, mapNotFound(err)
	}
	return s.resolve(ctx, repo, path, 0)
}

// resolve 是 Resolve 的内部递归实现，depth 用于遏制 group 成员环引用。
// 入口先查 404 负缓存：此前「确认不存在」且 TTL 未过期的路径直接 404，
// 不再回源/探测（覆盖 Resolve/groupGet/proxyGet 各层入口，见 FR-111）。
func (s *AssetService) resolve(ctx context.Context, repo *repository.Repository, path string, depth int) (*repository.Asset, io.ReadCloser, error) {
	if depth > maxResolveDepth {
		return nil, nil, ErrNotFound
	}
	if s.negCache.hit(repo.ID, path) {
		return nil, nil, ErrNotFound
	}
	switch repo.Type {
	case "proxy":
		return s.proxyGet(ctx, repo, path)
	case "group":
		return s.groupGet(ctx, repo, path, depth)
	default: // hosted 及未知类型均按本地读处理
		return s.localGet(repo, path)
	}
}

// localGet 读本地缓存：查 asset 元数据并打开对应 blob。路径不存在返回 ErrNotFound。
func (s *AssetService) localGet(repo *repository.Repository, path string) (*repository.Asset, io.ReadCloser, error) {
	var asset *repository.Asset
	var rc io.ReadCloser
	read := func() error {
		var err error
		asset, err = s.assets.GetByPath(repo.ID, path)
		if err != nil {
			return mapNotFound(err)
		}
		rc, _, err = s.blobs.Open(asset.BlobHash)
		return err
	}
	var err error
	if s.mutator != nil {
		err = s.mutator.Read(read)
	} else {
		err = read()
	}
	if err != nil {
		return nil, nil, err
	}
	return asset, rc, nil
}

// proxyGet 处理 proxy 仓库读：本地缓存命中即返回；未命中经 single-flight 收敛回源，
// 流式落 blob 并 upsert asset（缓存）后再从本地读返回。
// 上游处于 auto-block 阻止窗口时零连接快速失败（不发起上游连接，见 proxyHealth）。
func (s *AssetService) proxyGet(ctx context.Context, repo *repository.Repository, path string) (*repository.Asset, io.ReadCloser, error) {
	if asset, rc, err := s.localGet(repo, path); err == nil {
		return asset, rc, nil
	} else if !errors.Is(err, ErrNotFound) {
		return nil, nil, err
	}
	// 备用节点只服务已同步到本地的缓存；未命中不得回源并写入本地。
	if s.requireBusinessWrite() != nil {
		return nil, nil, ErrNotFound
	}
	if s.upstream == nil {
		return nil, nil, ErrNotFound
	}
	cfg, err := repo.DecodeConfig()
	if err != nil {
		return nil, nil, err
	}
	if cfg.RemoteURL == "" {
		return nil, nil, ErrNotFound
	}
	// 仓库手动 offline（FR-113）：不回源，快速 404（本地已确认未命中）。
	if !repo.Online {
		return nil, nil, ErrNotFound
	}
	// 阻止窗口内直接快速失败，不发起上游连接（group 读不拖慢的关键）。
	if s.health.shouldBlock(repo.ID) {
		return nil, nil, ErrUpstream
	}

	key := fmt.Sprintf("%d\x00%s", repo.ID, path)
	_, err, _ = s.sf.Do(key, func() (any, error) {
		// 并发回源收敛：进入临界区先复查缓存，避免重复下载。
		if _, e := s.assets.GetByPath(repo.ID, path); e == nil {
			return nil, nil
		}
		// 角色切换与并发请求交错时再次校验，避免已排队的回源落入备用节点。
		if s.requireBusinessWrite() != nil {
			return nil, ErrNotFound
		}
		after, ferr := s.fetchProxyAsset(ctx, repo, path)
		if ferr != nil {
			return nil, ferr
		}
		if s.mutator != nil {
			if err := s.mutator.Put(after); err != nil {
				return nil, s.cleanupFailedWrite(after.BlobHash, err)
			}
		} else if err := s.assets.Upsert(repo.ID, after.Path, after.BlobHash, after.Size, after.ContentType, after.Sha1, after.Md5); err != nil {
			return nil, s.cleanupFailedWrite(after.BlobHash, err)
		}
		s.releaseStagedBlob(after.BlobHash)
		cached, err := s.assets.GetByPath(repo.ID, path)
		if err != nil {
			return nil, err
		}
		s.negCache.remove(repo.ID, path)
		s.recordAssetPut(repo.Name, cached)
		return nil, nil
	})
	if err != nil {
		return nil, nil, err
	}
	return s.localGet(repo, path)
}

// fetchProxyAsset 从上游流式读取一项代理缓存，但不建立可见资产引用。
// 调用方必须把返回资产与所属格式元数据作为同一事务发布，失败时负责清理 blob。
func (s *AssetService) fetchProxyAsset(ctx context.Context, repo *repository.Repository, path string) (*repository.Asset, error) {
	if err := s.requireBusinessWrite(); err != nil {
		return nil, err
	}
	if repo == nil || repo.Type != "proxy" || s.upstream == nil || !repo.Online {
		return nil, ErrNotFound
	}
	cfg, err := repo.DecodeConfig()
	if err != nil {
		return nil, err
	}
	if cfg.RemoteURL == "" {
		return nil, ErrNotFound
	}
	if s.health.shouldBlock(repo.ID) {
		return nil, ErrUpstream
	}
	body, header, err := s.upstream.FetchWithCredential(ctx, cfg.RemoteURL, path, cfg.CredentialRef)
	if err != nil {
		switch {
		case errors.Is(err, upstream.ErrNotFound):
			s.negCache.add(repo.ID, path)
		case errors.Is(err, upstream.ErrGone):
			// 410 表示当前资源永久下架，不代表上游仓库不可达；
			// 不能因此触发仓库级 auto-block，否则 Go proxy 等客户端
			// 的后续请求会被错误改写成 502，无法按标准链路回退。
		default:
			s.health.recordFailure(repo.ID, cfg.RemoteURL, cfg.CredentialRef)
		}
		return nil, mapUpstreamErr(err)
	}
	defer func() { _ = body.Close() }()
	s.health.recordSuccess(repo.ID)
	hash, sha1sum, md5sum, size, err := s.putStagedBlob(body)
	if err != nil {
		return nil, err
	}
	contentType := header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	return &repository.Asset{RepositoryID: repo.ID, Path: path, BlobHash: hash, Size: size, ContentType: contentType, Sha1: sha1sum, Md5: md5sum}, nil
}

func (s *AssetService) cleanupFailedWrite(hash string, cause error) error {
	s.releaseStagedBlob(hash)
	if s.mutator == nil {
		if err := s.blobs.Remove(hash); err != nil {
			return errors.Join(cause, fmt.Errorf("清理未引用 blob 失败：%w", err))
		}
		return cause
	}
	if err := s.mutator.RemoveIfUnreferenced(hash); err != nil {
		return errors.Join(cause, fmt.Errorf("清理未引用 blob 失败：%w", err))
	}
	return cause
}

// ResolveOCIProxy 按 OCI Bearer 质询读取 proxy 资源。只有通过期望 sha256 摘要
// 校验的上游正文才会写入本地缓存，防止错误 manifest/blob 污染后续拉取。
func (s *AssetService) ResolveOCIProxy(ctx context.Context, repoName, path, pullScope, expectedDigest string) (*repository.Asset, io.ReadCloser, error) {
	repo, err := s.repos.GetByName(repoName)
	if err != nil {
		return nil, nil, mapNotFound(err)
	}
	if repo.Type != "proxy" {
		return s.Resolve(ctx, repoName, path)
	}
	if asset, rc, err := s.ociLocalGet(repo, path, expectedDigest); err == nil {
		return asset, rc, nil
	} else if !errors.Is(err, ErrNotFound) {
		return nil, nil, err
	}
	if s.requireBusinessWrite() != nil || s.upstream == nil || !repo.Online || s.health.shouldBlock(repo.ID) {
		return nil, nil, ErrNotFound
	}
	cfg, err := repo.DecodeConfig()
	if err != nil || cfg.RemoteURL == "" {
		return nil, nil, ErrNotFound
	}
	key := fmt.Sprintf("oci:%d\x00%s", repo.ID, path)
	_, err, _ = s.sf.Do(key, func() (any, error) {
		if cached, cacheErr := s.assets.GetByPath(repo.ID, path); cacheErr == nil {
			if expectedDigest == "" || cached.BlobHash == strings.TrimPrefix(expectedDigest, "sha256:") {
				return nil, nil
			}
		}
		if s.requireBusinessWrite() != nil {
			return nil, ErrNotFound
		}
		session, sessionErr := s.upstream.NewSession(cfg.RemoteURL, cfg.CredentialRef)
		if sessionErr != nil {
			s.health.recordFailure(repo.ID, cfg.RemoteURL, cfg.CredentialRef)
			return nil, mapUpstreamErr(sessionErr)
		}
		body, header, fetchErr := session.ReadOCIPath(ctx, path, pullScope)
		if fetchErr != nil {
			if !errors.Is(fetchErr, upstream.ErrNotFound) {
				s.health.recordFailure(repo.ID, cfg.RemoteURL, cfg.CredentialRef)
			}
			return nil, mapUpstreamErr(fetchErr)
		}
		defer func() { _ = body.Close() }()
		s.health.recordSuccess(repo.ID)
		hash, sha1sum, md5sum, size, putErr := s.putStagedBlob(body)
		if putErr != nil {
			return nil, putErr
		}
		if !ociDigestMatches(hash, expectedDigest, header.Get("Docker-Content-Digest")) {
			s.removeUnreferencedBlob(hash)
			return nil, ErrUpstream
		}
		contentType := header.Get("Content-Type")
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		after := &repository.Asset{RepositoryID: repo.ID, Path: path, BlobHash: hash, Size: size, ContentType: contentType, Sha1: sha1sum, Md5: md5sum}
		if s.mutator != nil {
			if putErr := s.mutator.Put(after); putErr != nil {
				s.removeUnreferencedBlob(hash)
				return nil, putErr
			}
		} else if putErr := s.assets.Upsert(repo.ID, path, hash, size, contentType, sha1sum, md5sum); putErr != nil {
			s.removeUnreferencedBlob(hash)
			return nil, putErr
		}
		s.releaseStagedBlob(hash)
		cached, getErr := s.assets.GetByPath(repo.ID, path)
		if getErr != nil {
			return nil, getErr
		}
		s.negCache.remove(repo.ID, path)
		s.recordAssetPut(repo.Name, cached)
		return nil, nil
	})
	if err != nil {
		return nil, nil, err
	}
	return s.ociLocalGet(repo, path, expectedDigest)
}

func (s *AssetService) ociLocalGet(repo *repository.Repository, path, expectedDigest string) (*repository.Asset, io.ReadCloser, error) {
	asset, rc, err := s.localGet(repo, path)
	if err != nil || expectedDigest == "" {
		return asset, rc, err
	}
	if strings.TrimPrefix(expectedDigest, "sha256:") == asset.BlobHash {
		return asset, rc, nil
	}
	_ = rc.Close()
	return nil, nil, ErrNotFound
}

func (s *AssetService) removeUnreferencedBlob(hash string) {
	s.releaseStagedBlob(hash)
	if s.mutator != nil {
		_ = s.mutator.RemoveIfUnreferenced(hash)
		return
	}
	_ = s.blobs.Remove(hash)
}

func ociDigestMatches(hash, expectedDigest, contentDigest string) bool {
	expected := strings.TrimPrefix(expectedDigest, "sha256:")
	if expected != "" && hash != expected {
		return false
	}
	if contentDigest == "" {
		return true
	}
	if !strings.HasPrefix(contentDigest, "sha256:") || len(contentDigest) != len("sha256:")+64 {
		return false
	}
	return hash == strings.TrimPrefix(contentDigest, "sha256:")
}

// groupMemberTimeout 是 group 仓库逐成员解析时每个成员的最大等待时间。
// 防止单个不可达 proxy 成员阻塞后续成员的探测；配合并行探测与 auto-block 阻止，
// 值取小（3s）让不可达上游快速失败、整体响应控制在秒级。
const groupMemberTimeout = 3 * time.Second

// groupGet 处理 group 仓库读，目标是"本地命中优先、缺失快速 404"：
//  1. 先串行快查全部成员的本地缓存（hosted / 已缓存 proxy 命中即返回，不触发网络）——
//     保持成员顺序语义（先成员命中优先）。
//  2. 本地全未命中 → 对未被 auto-block 阻止的 proxy 成员并行回源（取最快成功；首个成功后取消其余），
//     全部失败或超时快速返回 ErrNotFound。
//
// 每个成员限时 groupMemberTimeout；不可达成员经 proxyHealth 阻止窗口跳过（零连接）。
//
// 全未命中的 404 是否写负缓存（FR-111 M-1）：
// 只有「确认不存在」才写——即本次探测中没有任何成员被跳过（offline/auto-block 阻止）
// 或回源失败（上游 5xx/超时）。任一成员不可判定（可能只是暂时不可用），则不写负缓存，
// 避免把上游故障误缓存成永久不存在（上游恢复后同路径立即重试成功）。
func (s *AssetService) groupGet(ctx context.Context, repo *repository.Repository, path string, depth int) (*repository.Asset, io.ReadCloser, error) {
	cfg, err := repo.DecodeConfig()
	if err != nil {
		return nil, nil, err
	}
	if depth > maxResolveDepth {
		return nil, nil, ErrNotFound
	}

	// uncertain 标记本次 404 是否「不可判定」：任何成员被跳过或回源失败即置位。
	var uncertain atomic.Bool

	// 阶段一：串行快查本地缓存（不触发网络），命中即返回，保持成员顺序语义。
	// 跳过 offline 成员（FR-113：手动置离线的仓库不参与聚合读）；离线成员未参与判定，
	// 其 404 结果不可信，置不可判定（FR-111 M-1）。
	for _, name := range cfg.Members {
		member, err := s.repos.GetByName(name)
		if err != nil {
			// 成员解析失败（悬空引用）：存在无法判定的成员，整体 404 不可信，置不可判定。
			uncertain.Store(true)
			continue
		}
		if !member.Online {
			uncertain.Store(true)
			continue
		}
		if asset, rc, err := s.localGet(member, path); err == nil {
			return asset, rc, nil
		}
	}
	// 备用节点组仓库只可读成员的本地缓存，禁止第二阶段并行回源与负缓存写入。
	if s.requireBusinessWrite() != nil {
		return nil, nil, ErrNotFound
	}

	// 阶段二：本地全未命中 → 并行回源未被阻止的 proxy 成员。
	// 收集需要回源的成员（proxy 且未处于阻止窗口；hosted 已在阶段一确认未命中）。
	type job struct {
		idx    int
		repoID int64
		member *repository.Repository
	}
	jobs := make([]job, 0, len(cfg.Members))
	for _, name := range cfg.Members {
		member, err := s.repos.GetByName(name)
		if err != nil {
			// 成员解析失败（悬空引用）：存在无法判定的成员，置不可判定（FR-111 M-1）。
			uncertain.Store(true)
			continue
		}
		if !member.Online {
			// 离线成员跳过：未参与本次判定，404 不可信，置不可判定（FR-111 M-1）。
			uncertain.Store(true)
			continue
		}
		if member.Type != "proxy" {
			continue // hosted 等已在阶段一确认未命中，不算跳过
		}
		if s.health.shouldBlock(member.ID) {
			// auto-block 阻止窗口内跳过：未参与判定，置不可判定（FR-111 M-1）。
			uncertain.Store(true)
			continue
		}
		jobs = append(jobs, job{idx: len(jobs), repoID: member.ID, member: member})
	}
	if len(jobs) == 0 {
		// 无可回源成员：全部 hosted 成员已在阶段一确认未命中（确认不存在，可写负缓存）；
		// 但若有成员被 offline/阻止跳过（uncertain），404 不可信，不写（FR-111 M-1）。
		if !uncertain.Load() {
			s.negCache.add(repo.ID, path)
		}
		return nil, nil, ErrNotFound
	}

	// 并行回源：首个成功即返回并取消其余；全部失败/超时快速 404。
	results := make([]bool, len(jobs))
	groupCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	for _, j := range jobs {
		wg.Add(1)
		go func(j job) {
			defer wg.Done()
			memberCtx, mCancel := context.WithTimeout(groupCtx, groupMemberTimeout)
			defer mCancel()
			asset, rc, err := s.resolve(memberCtx, j.member, path, depth+1)
			if err != nil {
				// 明确 404（含成员自身负缓存命中）是「确认不存在」；其余错误（回源失败/
				// 超时）说明该成员上游暂时不可用，本次 404 不可判定（FR-111 M-1）。
				if !errors.Is(err, ErrNotFound) {
					uncertain.Store(true)
				}
				return
			}
			// 成功：proxyGet 已把制品落本地缓存，此处关闭读流即可；
			// 记录成功成员供后续按 members 顺序 localGet 返回。
			_ = rc.Close()
			_ = asset
			results[j.idx] = true
			cancel() // 首个成功：取消其它等待者
		}(j)
	}
	wg.Wait()

	// 有成员成功：按 members 顺序返回第一个成功成员的本地缓存制品。
	for i, ok := range results {
		if ok {
			asset, rc, err := s.localGet(jobs[i].member, path)
			if err == nil {
				return asset, rc, nil
			}
		}
	}
	// 全未命中：确认不存在（无成员被跳过/失败）才写 group 层负缓存（FR-111 M-1）。
	if !uncertain.Load() {
		s.negCache.add(repo.ID, path)
	}
	return nil, nil, ErrNotFound
}

// ListAssetsByPrefix 返回 hosted 仓库内指定前缀的资产元数据，供协议构造完整原子快照。
func (s *AssetService) ListAssetsByPrefix(repoName, prefix string, limit int) ([]repository.Asset, error) {
	repo, err := s.repos.GetByName(repoName)
	if err != nil {
		return nil, mapNotFound(err)
	}
	if repo.Type != "hosted" {
		return nil, ErrConflict
	}
	return s.assets.ListByRepo(repo.ID, prefix, limit, 0)
}

// ListPathsByPrefix 列出仓库内以 prefix 开头的全部资产路径（npm unpublish 整包删除用）。
func (s *AssetService) ListPathsByPrefix(repoName, prefix string) ([]string, error) {
	repo, err := s.repos.GetByName(repoName)
	if err != nil {
		return nil, mapNotFound(err)
	}
	assets, err := s.assets.ListByRepo(repo.ID, prefix, 10000, 0)
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(assets))
	for _, a := range assets {
		paths = append(paths, a.Path)
	}
	return paths, nil
}

// Delete 删除制品元数据（blob 内容不即时清理）。
// 仓库或路径不存在均返回 ErrNotFound。
func (s *AssetService) Delete(repoName, path string) error {
	if err := s.requireBusinessWrite(); err != nil {
		return err
	}
	repo, err := s.repos.GetByName(repoName)
	if err != nil {
		return mapNotFound(err)
	}
	if s.mutator != nil {
		if err := s.mutator.Delete(repo.ID, path); err != nil {
			return mapNotFound(err)
		}
	} else if err := s.assets.DeleteByPath(repo.ID, path); err != nil {
		return mapNotFound(err)
	}
	// 写路径成功：失效同路径负缓存（FR-111），避免删除后重传的制品被缓存 404 挡住。
	s.negCache.remove(repo.ID, path)
	s.recordChange(EntityAsset, AssetKey(repoName, path), OpDelete, TombstoneData{Deleted: true})
	return nil
}

// mapUpstreamErr 把 upstream 层错误映射为领域错误：404 视为未命中（ErrNotFound）、
// 410 同时保留通用回源失败和明确下架语义、超时映射 ErrUpstreamTimeout，其余映射 ErrUpstream。
func mapUpstreamErr(err error) error {
	switch {
	case errors.Is(err, upstream.ErrNotFound):
		return ErrNotFound
	case errors.Is(err, upstream.ErrGone):
		return fmt.Errorf("%w: %w", ErrUpstream, ErrUpstreamGone)
	case upstream.IsTimeout(err):
		return fmt.Errorf("%w: %v", ErrUpstreamTimeout, err)
	default:
		return fmt.Errorf("%w: %v", ErrUpstream, err)
	}
}

// probeUpstream 后台探测上游可达性：与回源使用同一个安全客户端和凭据解析路径。
func (s *AssetService) probeUpstream(ctx context.Context, remoteURL, credentialRef string) error {
	if s.upstream == nil {
		return ErrUpstream
	}
	status, err := s.upstream.ProbeWithCredential(ctx, remoteURL, credentialRef)
	if err != nil {
		return err
	}
	if shouldBlockStatus(status) {
		return fmt.Errorf("上游返回状态码 %d", status)
	}
	return nil
}

// shouldBlockStatus 判断上游返回状态码是否应视为失败（5xx / 401 / 403）。
// 404 等其余 4xx 视为上游可达（仅资源缺失），不触发阻止。
func shouldBlockStatus(code int) bool {
	return code >= 500 || code == 401 || code == 403
}
