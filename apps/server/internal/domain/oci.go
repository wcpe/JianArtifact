package domain

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

const maxOCIManifestBytes = 32 << 20

const defaultOCIStagedBlobTTL = time.Hour

var ociDigestPattern = regexp.MustCompile(`^sha256:([0-9a-f]{64})$`)

// OCIService 负责 OCI manifest、tag 与 blob 的领域规则。
// 上传会话的暂存由协议层管理，最终字节与引用统一落入 AssetService。
type OCIService struct {
	assets    *AssetService
	repos     *RepositoryService
	pending   map[string]*ociPendingBlob
	stagedTTL time.Duration
	mu        sync.Mutex
}

type ociPendingBlob struct {
	asset *repository.Asset
	timer *time.Timer
}

// NewOCIService 构造 OCI 领域服务。
func NewOCIService(assets *AssetService, repos *RepositoryService) *OCIService {
	return &OCIService{assets: assets, repos: repos, pending: make(map[string]*ociPendingBlob), stagedTTL: defaultOCIStagedBlobTTL}
}

// OCIPublishResult 是 manifest 发布摘要。
type OCIPublishResult struct {
	Digest string
	Size   int64
}

// PutBlob 校验 sha256 后写入 hosted OCI blob。digest 只接受 sha256。
func (s *OCIService) PutBlob(repoName, digest string, r io.Reader) (*repository.Asset, error) {
	return s.putBlob(repoName, digest, r, time.Time{})
}

// putBlob 是 PutBlob 的共享实现；sourceModified 非零时将暂存资产行标注源端时间，
// 该时间随 pending 引用在 manifest 批次发布时落入 asset 行（迁移保留源时间戳）。
func (s *OCIService) putBlob(repoName, digest string, r io.Reader, sourceModified time.Time) (*repository.Asset, error) {
	hexDigest, ok := validOCIDigest(digest)
	if !ok {
		return nil, ErrValidation
	}
	repo, err := s.repos.Get(repoName)
	if err != nil {
		return nil, err
	}
	if repo.Format != "docker" || repo.Type != "hosted" {
		return nil, ErrConflict
	}
	asset, err := s.assets.StageOCIBlob(r, "application/octet-stream")
	if err != nil {
		return nil, err
	}
	if asset.BlobHash != hexDigest {
		s.assets.removeUnreferencedBlob(asset.BlobHash)
		return nil, fmt.Errorf("%w: blob digest 不匹配", ErrValidation)
	}
	applySourceTimestamp(asset, sourceModified)
	s.storePending(repo.ID, hexDigest, asset)
	return asset, nil
}

// PutManifest 校验 manifest 正文、digest 与依赖 blob，并写入不可变 digest 及 tag 引用。
func (s *OCIService) PutManifest(repoName, image, reference string, body []byte, contentType string) (*OCIPublishResult, error) {
	return s.putManifest(repoName, image, reference, body, contentType, time.Time{})
}

// putManifest 是 PutManifest 的共享实现；sourceModified 非零时将 manifest 与
// tag 资产行 created_at/updated_at 固定为源端时间（迁移保留源时间戳）。
// pending blob 各自携带其 PutBlob 时的源端时间，不在此覆盖。
func (s *OCIService) putManifest(repoName, image, reference string, body []byte, contentType string, sourceModified time.Time) (*OCIPublishResult, error) {
	if !validOCIImage(image) || len(body) == 0 || len(body) > maxOCIManifestBytes {
		return nil, ErrValidation
	}
	repo, err := s.repos.Get(repoName)
	if err != nil {
		return nil, err
	}
	if repo.Format != "docker" || repo.Type != "hosted" {
		return nil, ErrConflict
	}
	digestBytes := sha256.Sum256(body)
	digest := "sha256:" + hex.EncodeToString(digestBytes[:])
	if strings.HasPrefix(reference, "sha256:") && reference != digest {
		return nil, ErrValidation
	}
	if !validOCIReference(reference) {
		return nil, ErrValidation
	}
	refs, err := manifestReferences(body)
	if err != nil {
		return nil, err
	}
	pending := make([]*repository.Asset, 0, len(refs)+2)
	restore := make([]func(), 0, len(refs))
	defer func() {
		for _, restorePending := range restore {
			restorePending()
		}
	}()
	seenRefs := make(map[string]struct{}, len(refs))
	for _, ref := range refs {
		hexRef, _ := validOCIDigest(ref)
		if _, seen := seenRefs[hexRef]; seen {
			continue
		}
		seenRefs[hexRef] = struct{}{}
		if asset, restorePending := s.takePending(repo.ID, hexRef); asset != nil {
			copy := *asset
			copy.Path = ociBlobPath(hexRef)
			pending = append(pending, &copy)
			restore = append(restore, restorePending)
			continue
		}
		if _, rc, err := s.assets.Get(repoName, ociBlobPath(hexRef)); err != nil {
			if _, manifestRC, manifestErr := s.assets.Get(repoName, ociManifestPath(image, ref)); manifestErr != nil {
				return nil, fmt.Errorf("%w: manifest 引用 blob 不存在", ErrNotFound)
			} else {
				_ = manifestRC.Close()
			}
		} else {
			_ = rc.Close()
		}
	}
	manifestPath := ociManifestPath(image, digest)
	if _, rc, err := s.assets.Get(repoName, manifestPath); err != nil {
		if !errors.Is(err, ErrNotFound) {
			return nil, err
		}
	} else {
		_ = rc.Close()
	}
	tagPath := ""
	if !strings.HasPrefix(reference, "sha256:") {
		tagPath = ociTagPath(image, reference)
		if cfg, cfgErr := repo.DecodeConfig(); cfgErr != nil {
			return nil, cfgErr
		} else if cfg.ImmutableRelease {
			if _, tagRC, tagErr := s.assets.Get(repoName, tagPath); tagErr == nil {
				_ = tagRC.Close()
				return nil, ErrConflict
			}
		}
	}
	manifest, err := s.assets.StageOCIBlob(bytes.NewReader(body), contentType)
	if err != nil {
		return nil, err
	}
	manifest.Path = manifestPath
	applySourceTimestamp(manifest, sourceModified)
	pending = append(pending, manifest)
	if tagPath != "" {
		tag, tagErr := s.assets.StageOCIBlob(strings.NewReader(digest), "text/plain")
		if tagErr != nil {
			s.assets.removeUnreferencedBlob(manifest.BlobHash)
			return nil, tagErr
		}
		tag.Path = tagPath
		// tag 文件内容是 manifest digest 的引用，随 manifest 同一源端时间。
		applySourceTimestamp(tag, sourceModified)
		pending = append(pending, tag)
	}
	if _, err := s.assets.PublishOCIAssets(repoName, pending); err != nil {
		s.assets.removeUnreferencedBlob(manifest.BlobHash)
		if tagPath != "" {
			s.assets.removeUnreferencedBlob(pending[len(pending)-1].BlobHash)
		}
		return nil, err
	}
	restore = nil
	return &OCIPublishResult{Digest: digest, Size: int64(len(body))}, nil
}

// CreateUploadTemp 创建当前实例 OCI 分块上传会话的专用临时文件。
func (s *OCIService) CreateUploadTemp() (*os.File, error) {
	return s.assets.blobs.CreateOCIUploadTemp()
}

// CleanupUploadTemps 在启动阶段清理前一个进程遗留的 OCI 分块上传会话文件。
func (s *OCIService) CleanupUploadTemps() error {
	return s.assets.blobs.CleanupOCIUploadTemps()
}

func (s *OCIService) storePending(repositoryID int64, hexDigest string, asset *repository.Asset) {
	key := ociPendingKey(repositoryID, hexDigest)
	entry := &ociPendingBlob{asset: asset}
	entry.timer = time.AfterFunc(s.stagedTTL, func() { s.expirePending(key, entry) })
	s.mu.Lock()
	previous := s.pending[key]
	s.pending[key] = entry
	s.mu.Unlock()
	if previous != nil {
		previous.timer.Stop()
	}
}

func (s *OCIService) takePending(repositoryID int64, hexDigest string) (*repository.Asset, func()) {
	key := ociPendingKey(repositoryID, hexDigest)
	s.mu.Lock()
	entry := s.pending[key]
	if entry != nil {
		delete(s.pending, key)
		entry.timer.Stop()
	}
	s.mu.Unlock()
	if entry == nil {
		return nil, func() {}
	}
	copy := *entry.asset
	return &copy, func() { s.storePending(repositoryID, hexDigest, entry.asset) }
}

func (s *OCIService) expirePending(key string, entry *ociPendingBlob) {
	s.mu.Lock()
	if s.pending[key] != entry {
		s.mu.Unlock()
		return
	}
	delete(s.pending, key)
	s.mu.Unlock()
	s.assets.removeUnreferencedBlob(entry.asset.BlobHash)
}

func ociPendingKey(repositoryID int64, hexDigest string) string {
	return fmt.Sprintf("%d:%s", repositoryID, hexDigest)
}

// ResolveManifest 按 hosted/proxy/group 语义返回 manifest 或 tag 对应正文。
func (s *OCIService) ResolveManifest(ctx context.Context, repoName, image, reference string) (*repository.Asset, io.ReadCloser, error) {
	if !validOCIImage(image) || !validOCIReference(reference) {
		return nil, nil, ErrValidation
	}
	repo, err := s.repos.Get(repoName)
	if err != nil {
		return nil, nil, err
	}
	switch repo.Type {
	case "group":
		cfg, cfgErr := repo.DecodeConfig()
		if cfgErr != nil {
			return nil, nil, cfgErr
		}
		for _, member := range cfg.Members {
			asset, rc, memberErr := s.ResolveManifest(ctx, member, image, reference)
			if memberErr == nil {
				return asset, rc, nil
			}
		}
		return nil, nil, ErrNotFound
	case "proxy":
		expected := ""
		if strings.HasPrefix(reference, "sha256:") {
			expected = reference
		}
		return s.assets.ResolveOCIProxy(ctx, repoName, "v2/"+image+"/manifests/"+reference, ociPullScope(image), expected)
	default:
		if strings.HasPrefix(reference, "sha256:") {
			return s.assets.Resolve(ctx, repoName, ociManifestPath(image, reference))
		}
		tagAsset, tagReader, tagErr := s.assets.Resolve(ctx, repoName, ociTagPath(image, reference))
		if tagErr != nil {
			return nil, nil, tagErr
		}
		defer func() { _ = tagReader.Close() }()
		data, readErr := io.ReadAll(tagReader)
		if readErr != nil {
			return nil, nil, readErr
		}
		digest := strings.TrimSpace(string(data))
		if !validOCIReference(digest) {
			return nil, nil, ErrNotFound
		}
		_ = tagAsset
		return s.assets.Resolve(ctx, repoName, ociManifestPath(image, digest))
	}
}

// ResolveBlob 按仓库类型读取 blob。proxy/group 请求使用标准 v2 路径回源。
func (s *OCIService) ResolveBlob(ctx context.Context, repoName, image, digest string) (*repository.Asset, io.ReadCloser, error) {
	hexDigest, ok := validOCIDigest(digest)
	if !ok {
		return nil, nil, ErrValidation
	}
	repo, err := s.repos.Get(repoName)
	if err != nil {
		return nil, nil, err
	}
	if repo.Type == "group" {
		cfg, cfgErr := repo.DecodeConfig()
		if cfgErr != nil {
			return nil, nil, cfgErr
		}
		for _, member := range cfg.Members {
			asset, rc, memberErr := s.ResolveBlob(ctx, member, image, digest)
			if memberErr == nil {
				return asset, rc, nil
			}
		}
		return nil, nil, ErrNotFound
	}
	if repo.Type == "proxy" {
		return s.assets.ResolveOCIProxy(ctx, repoName, "v2/"+image+"/blobs/"+digest, ociPullScope(image), digest)
	}
	return s.assets.Resolve(ctx, repoName, ociBlobPath(hexDigest))
}

// Tags 返回当前仓库的 tag 列表，group 按成员顺序去重排序。
func (s *OCIService) Tags(ctx context.Context, repoName, image string) ([]string, error) {
	if !validOCIImage(image) {
		return nil, ErrValidation
	}
	repo, err := s.repos.Get(repoName)
	if err != nil {
		return nil, err
	}
	if repo.Type == "proxy" {
		asset, rc, resolveErr := s.assets.ResolveOCIProxy(ctx, repoName, "v2/"+image+"/tags/list", ociPullScope(image), "")
		if resolveErr != nil {
			return nil, resolveErr
		}
		defer func() { _ = rc.Close() }()
		var doc struct {
			Tags []string `json:"tags"`
		}
		if err := json.NewDecoder(rc).Decode(&doc); err != nil {
			return nil, err
		}
		_ = asset
		sort.Strings(doc.Tags)
		return doc.Tags, nil
	}
	if repo.Type == "group" {
		cfg, cfgErr := repo.DecodeConfig()
		if cfgErr != nil {
			return nil, cfgErr
		}
		set := make(map[string]struct{})
		for _, member := range cfg.Members {
			tags, memberErr := s.Tags(ctx, member, image)
			if memberErr != nil {
				continue
			}
			for _, tag := range tags {
				set[tag] = struct{}{}
			}
		}
		return sortedStrings(set), nil
	}
	prefix := "oci/tags/" + image + "/"
	paths, err := s.assets.ListPathsByPrefix(repoName, prefix)
	if err != nil {
		return nil, err
	}
	result := make([]string, 0, len(paths))
	for _, item := range paths {
		result = append(result, strings.TrimPrefix(item, prefix))
	}
	sort.Strings(result)
	return result, nil
}

func manifestReferences(body []byte) ([]string, error) {
	var doc struct {
		Config struct {
			Digest string `json:"digest"`
		} `json:"config"`
		Layers []struct {
			Digest string `json:"digest"`
		} `json:"layers"`
		Manifests []struct {
			Digest string `json:"digest"`
		} `json:"manifests"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, ErrValidation
	}
	refs := make([]string, 0, 1+len(doc.Layers)+len(doc.Manifests))
	if doc.Config.Digest != "" {
		refs = append(refs, doc.Config.Digest)
	}
	for _, layer := range doc.Layers {
		refs = append(refs, layer.Digest)
	}
	for _, child := range doc.Manifests {
		refs = append(refs, child.Digest)
	}
	for _, ref := range refs {
		if _, ok := validOCIDigest(ref); !ok {
			return nil, ErrValidation
		}
	}
	return refs, nil
}

func validOCIDigest(value string) (string, bool) {
	match := ociDigestPattern.FindStringSubmatch(value)
	if len(match) != 2 {
		return "", false
	}
	return match[1], true
}

func validOCIImage(value string) bool {
	return value != "" && !strings.HasPrefix(value, "/") && !strings.HasSuffix(value, "/") && !strings.Contains(value, "..") && path.Clean(value) == value
}

func validOCIReference(value string) bool {
	if strings.HasPrefix(value, "sha256:") {
		_, ok := validOCIDigest(value)
		return ok
	}
	return value != "" && !strings.ContainsAny(value, "/\\") && value != "." && value != ".."
}

func ociPullScope(image string) string {
	return "repository:" + image + ":pull"
}

func ociBlobPath(hexDigest string) string {
	return "oci/blobs/sha256/" + hexDigest
}

func ociManifestPath(image, digest string) string {
	return path.Join("oci/manifests", image, strings.TrimPrefix(digest, "sha256:"))
}

func ociTagPath(image, tag string) string {
	return path.Join("oci/tags", image, tag)
}

func sortedStrings(set map[string]struct{}) []string {
	result := make([]string, 0, len(set))
	for item := range set {
		result = append(result, item)
	}
	sort.Strings(result)
	return result
}
