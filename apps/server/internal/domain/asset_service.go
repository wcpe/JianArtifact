package domain

import (
	"context"
	"errors"
	"fmt"
	"io"

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
// 未命中经 upstream 回源并缓存（single-flight 收敛并发回源）；group 按成员有序聚合。
type AssetService struct {
	repos    *repository.RepoRepo
	assets   *repository.AssetRepo
	blobs    *blobstore.Store
	upstream *upstream.Client
	sf       singleflight.Group
}

// NewAssetService 构造 AssetService。upstream 供 proxy 回源使用（hosted-only 部署可传 nil）。
func NewAssetService(repos *repository.RepoRepo, assets *repository.AssetRepo, blobs *blobstore.Store, up *upstream.Client) *AssetService {
	return &AssetService{repos: repos, assets: assets, blobs: blobs, upstream: up}
}

// Put 向 hosted 仓库发布一件制品：流式写入 blob，再覆盖写 asset 元数据。
// 仓库不存在返回 ErrNotFound；非 hosted 仓库（proxy/group）返回 ErrConflict。
// 格式（raw/maven/npm）语义由协议层按仓库 format 分派后自行处理，此处仅按内容寻址存字节。
func (s *AssetService) Put(repoName, path string, r io.Reader, contentType string) (*repository.Asset, error) {
	repo, err := s.repos.GetByName(repoName)
	if err != nil {
		return nil, mapNotFound(err)
	}
	if repo.Type != "hosted" {
		return nil, ErrConflict
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	hash, sha1sum, md5sum, size, err := s.blobs.Put(r)
	if err != nil {
		return nil, err
	}
	if err := s.assets.Upsert(repo.ID, path, hash, size, contentType, sha1sum, md5sum); err != nil {
		return nil, err
	}
	return s.assets.GetByPath(repo.ID, path)
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
func (s *AssetService) Resolve(ctx context.Context, repoName, path string) (*repository.Asset, io.ReadCloser, error) {
	repo, err := s.repos.GetByName(repoName)
	if err != nil {
		return nil, nil, mapNotFound(err)
	}
	return s.resolve(ctx, repo, path, 0)
}

// resolve 是 Resolve 的内部递归实现，depth 用于遏制 group 成员环引用。
func (s *AssetService) resolve(ctx context.Context, repo *repository.Repository, path string, depth int) (*repository.Asset, io.ReadCloser, error) {
	if depth > maxResolveDepth {
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
	asset, err := s.assets.GetByPath(repo.ID, path)
	if err != nil {
		return nil, nil, mapNotFound(err)
	}
	rc, _, err := s.blobs.Open(asset.BlobHash)
	if err != nil {
		return nil, nil, err
	}
	return asset, rc, nil
}

// proxyGet 处理 proxy 仓库读：本地缓存命中即返回；未命中经 single-flight 收敛回源，
// 流式落 blob 并 upsert asset（缓存）后再从本地读返回。
func (s *AssetService) proxyGet(ctx context.Context, repo *repository.Repository, path string) (*repository.Asset, io.ReadCloser, error) {
	if asset, rc, err := s.localGet(repo, path); err == nil {
		return asset, rc, nil
	} else if !errors.Is(err, ErrNotFound) {
		return nil, nil, err
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

	key := fmt.Sprintf("%d\x00%s", repo.ID, path)
	_, err, _ = s.sf.Do(key, func() (any, error) {
		// 并发回源收敛：进入临界区先复查缓存，避免重复下载。
		if _, e := s.assets.GetByPath(repo.ID, path); e == nil {
			return nil, nil
		}
		body, header, ferr := s.upstream.Fetch(ctx, cfg.RemoteURL, path)
		if ferr != nil {
			return nil, mapUpstreamErr(ferr)
		}
		defer func() { _ = body.Close() }()
		hash, sha1sum, md5sum, size, ferr := s.blobs.Put(body)
		if ferr != nil {
			return nil, ferr
		}
		ct := header.Get("Content-Type")
		if ct == "" {
			ct = "application/octet-stream"
		}
		return nil, s.assets.Upsert(repo.ID, path, hash, size, ct, sha1sum, md5sum)
	})
	if err != nil {
		return nil, nil, err
	}
	return s.localGet(repo, path)
}

// groupGet 处理 group 仓库读：按 Members 顺序逐一解析，首个命中即返回；全未命中返回 ErrNotFound。
// 成员报 ErrNotFound 则继续下一成员；成员回源等其它错误亦跳过（不因单个成员故障阻断聚合读）。
func (s *AssetService) groupGet(ctx context.Context, repo *repository.Repository, path string, depth int) (*repository.Asset, io.ReadCloser, error) {
	cfg, err := repo.DecodeConfig()
	if err != nil {
		return nil, nil, err
	}
	for _, name := range cfg.Members {
		member, err := s.repos.GetByName(name)
		if err != nil {
			continue
		}
		asset, rc, err := s.resolve(ctx, member, path, depth+1)
		if err == nil {
			return asset, rc, nil
		}
	}
	return nil, nil, ErrNotFound
}

// Delete 删除制品元数据（blob 内容不即时清理）。
// 仓库或路径不存在均返回 ErrNotFound。
func (s *AssetService) Delete(repoName, path string) error {
	repo, err := s.repos.GetByName(repoName)
	if err != nil {
		return mapNotFound(err)
	}
	return mapNotFound(s.assets.DeleteByPath(repo.ID, path))
}

// mapUpstreamErr 把 upstream 层错误映射为领域错误：404 视为未命中（ErrNotFound）、
// 超时映射 ErrUpstreamTimeout、其余映射 ErrUpstream（保留底层错误便于排错）。
func mapUpstreamErr(err error) error {
	switch {
	case errors.Is(err, upstream.ErrNotFound):
		return ErrNotFound
	case upstream.IsTimeout(err):
		return fmt.Errorf("%w: %v", ErrUpstreamTimeout, err)
	default:
		return fmt.Errorf("%w: %v", ErrUpstream, err)
	}
}
