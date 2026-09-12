package domain

import (
	"bytes"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"hash"
	"io"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

const maxAssetOperationItems = 500

// AssetOperationAction 是统一制品操作的动作。
type AssetOperationAction string

const (
	AssetOperationDelete AssetOperationAction = "delete"
	AssetOperationMove   AssetOperationAction = "move"
	AssetOperationRename AssetOperationAction = "rename"
)

// AssetOperationTargetType 是管理端传入的格式感知目标类型。
type AssetOperationTargetType string

const (
	AssetTargetRawPath       AssetOperationTargetType = "raw_path"
	AssetTargetMavenVersion  AssetOperationTargetType = "maven_version"
	AssetTargetMavenArtifact AssetOperationTargetType = "maven_artifact"
	AssetTargetNpmPackage    AssetOperationTargetType = "npm_package"
	AssetTargetNpmVersion    AssetOperationTargetType = "npm_version"
	AssetTargetAssetPath     AssetOperationTargetType = "asset_path"
)

// AssetOperationTarget 是一个文件、目录或格式逻辑根。
type AssetOperationTarget struct {
	Type AssetOperationTargetType `json:"type"`
	Path string                   `json:"path"`
}

// AssetOperation 是统一原子资产 API 的领域请求。DestinationPath 是移动目录，
// NewPath 是单项重命名后的完整路径。
type AssetOperation struct {
	Action          AssetOperationAction
	Targets         []AssetOperationTarget
	DestinationPath string
	NewPath         string
	Audit           AssetOperationAudit
}

// AssetOperationAudit 持有源端身份快照与同事务审计回调，不包含密码或令牌。
type AssetOperationAudit struct {
	Actor  repository.OperationActor
	Commit func(operationID string, items []repository.AssetMutationItem) repository.MutationCompletionHook
}

// AssetOperationResult 返回操作标识以及实际影响的资产路径。
type AssetOperationResult struct {
	OperationID string
	Affected    int
	Paths       []string
}

var (
	// ErrLogicalDeleteRequired 表示请求必须使用格式感知的逻辑目标。
	ErrLogicalDeleteRequired = errors.New("需要格式感知的逻辑删除目标")
	// ErrOperationLimit 表示递归展开后的实际资产超过单批上限。
	ErrOperationLimit = errors.New("制品操作超过 500 条上限")
	// ErrOperationUnsupported 表示该格式不支持动作。
	ErrOperationUnsupported = errors.New("制品操作不支持")
	// ErrClusterPeerUpgradeRequired 表示活动复制对端未全部支持 v2 原子操作。
	ErrClusterPeerUpgradeRequired = fmt.Errorf("%w: 活动集群对端不支持 v2 原子操作", ErrConflict)
)

// ApplyOperation 在 hosted 仓库中规划并原子执行删除、Raw 移动或单项重命名。
func (s *AssetService) ApplyOperation(repoName string, op AssetOperation) (*AssetOperationResult, error) {
	id := newOperationID()
	fail := func(err error) (*AssetOperationResult, error) {
		return &AssetOperationResult{OperationID: id}, err
	}
	if err := s.requireBusinessWrite(); err != nil {
		return fail(err)
	}
	repo, err := s.repos.GetByName(repoName)
	if err != nil {
		return fail(mapNotFound(err))
	}
	if repo.Type != "hosted" {
		return fail(ErrConflict)
	}
	if s.mutator == nil {
		return fail(fmt.Errorf("%w: 资产事务引擎未就绪", ErrConflict))
	}
	if len(op.Targets) == 0 {
		return fail(fmt.Errorf("%w: targets 不能为空", ErrValidation))
	}
	if len(op.Targets) > maxAssetOperationItems {
		return fail(ErrOperationLimit)
	}
	nodeID := s.NodeID()

	var items []repository.AssetMutationItem
	var created []string
	var errPlan error
	switch op.Action {
	case AssetOperationDelete:
		items, created, errPlan = s.planDelete(repo, op.Targets)
	case AssetOperationMove, AssetOperationRename:
		items, errPlan = s.planRawMove(repo, op)
	default:
		errPlan = fmt.Errorf("%w: 未知动作 %q", ErrValidation, op.Action)
	}
	if errPlan != nil {
		s.cleanupPlannedBlobs(created)
		return fail(errPlan)
	}
	if len(items) == 0 || len(items) > maxAssetOperationItems {
		s.cleanupPlannedBlobs(created)
		return fail(ErrOperationLimit)
	}

	// 复制退役：批量资产操作恒走原子 operation 信封路径。
	envelope, envelopeErr := assetOperationEnvelope(repoName, id, nodeID, op.Audit.Actor, items)
	if envelopeErr != nil {
		s.releaseItemBlobs(items)
		s.cleanupPlannedBlobs(created)
		return fail(envelopeErr)
	}
	err = s.mutator.ApplyWithOutboxAndHook(items, envelope, operationAuditHook(op.Audit, id, items))
	if err != nil {
		s.releaseItemBlobs(items)
		s.cleanupPlannedBlobs(created)
		return &AssetOperationResult{OperationID: id}, err
	}
	s.releaseItemBlobs(items)
	for _, item := range items {
		if item.Before != nil {
			s.negCache.remove(repo.ID, item.Before.Path)
		}
		if item.After != nil {
			s.negCache.remove(repo.ID, item.After.Path)
		}
	}
	paths := make([]string, 0, len(items))
	for _, item := range items {
		paths = append(paths, item.Path)
	}
	sort.Strings(paths)
	return &AssetOperationResult{OperationID: id, Affected: len(items), Paths: paths}, nil
}

func (s *AssetService) cleanupPlannedBlobs(hashes []string) {
	for _, hash := range hashes {
		s.releaseStagedBlob(hash)
		_ = s.mutator.RemoveIfUnreferenced(hash)
	}
}

func (s *AssetService) releaseItemBlobs(items []repository.AssetMutationItem) {
	for _, item := range items {
		if item.After != nil {
			s.releaseStagedBlob(item.After.BlobHash)
		}
	}
}

func operationAuditHook(audit AssetOperationAudit, operationID string, items []repository.AssetMutationItem) repository.MutationCompletionHook {
	if audit.Commit == nil {
		return nil
	}
	return audit.Commit(operationID, items)
}

func assetOperationEnvelope(repoName, operationID, sourceNode string, actor repository.OperationActor, items []repository.AssetMutationItem) (repository.OperationEnvelope, error) {
	version := repository.ReplicationVersion{NodeID: sourceNode, TS: time.Now().UTC().Format(time.RFC3339Nano)}
	operationItems := make([]repository.OperationItem, 0, len(items)*2)
	for _, item := range items {
		if item.Before != nil && (item.After == nil || item.Before.Path != item.After.Path) {
			operationItems = append(operationItems, repository.OperationItem{
				Type: EntityAsset, Key: AssetKey(repoName, item.Before.Path), Op: OpDelete,
				DependsOn: []string{RepoKey(repoName)}, Version: version,
			})
		}
		if item.After == nil {
			continue
		}
		data, err := json.Marshal(AssetChangeData{
			Path: item.After.Path, BlobHash: item.After.BlobHash, Size: item.After.Size,
			ContentType: item.After.ContentType, Sha1: item.After.Sha1, Md5: item.After.Md5,
			CreatedAt: item.After.CreatedAt, UpdatedAt: item.After.UpdatedAt,
		})
		if err != nil {
			return repository.OperationEnvelope{}, err
		}
		operationItems = append(operationItems, repository.OperationItem{
			Type: EntityAsset, Key: AssetKey(repoName, item.After.Path), Op: OpPut, Data: string(data),
			BlobHashes: []string{item.After.BlobHash}, DependsOn: []string{RepoKey(repoName)}, Version: version,
		})
	}
	sort.Slice(operationItems, func(i, j int) bool { return operationItems[i].Key < operationItems[j].Key })
	manifest, err := repository.OperationManifestSHA256(operationItems)
	if err != nil {
		return repository.OperationEnvelope{}, err
	}
	return repository.OperationEnvelope{
		Kind: "operation", OperationID: operationID, SourceNode: sourceNode, Version: version,
		ItemCount: len(operationItems), ManifestSHA256: manifest, Actor: actor, Items: operationItems,
	}, nil
}

func (s *AssetService) planRawMove(repo *repository.Repository, op AssetOperation) ([]repository.AssetMutationItem, error) {
	if repo.Format != "raw" {
		return nil, fmt.Errorf("%w: Maven/npm 不支持移动或重命名", ErrOperationUnsupported)
	}
	if op.Action == AssetOperationRename && len(op.Targets) != 1 {
		return nil, fmt.Errorf("%w: 重命名只允许单项", ErrValidation)
	}
	if op.Action == AssetOperationMove && strings.TrimSpace(op.DestinationPath) == "" {
		return nil, fmt.Errorf("%w: destinationPath 不能为空", ErrValidation)
	}
	if op.Action == AssetOperationRename && strings.TrimSpace(op.NewPath) == "" {
		return nil, fmt.Errorf("%w: newPath 不能为空", ErrValidation)
	}
	destinationPath := ""
	if op.Action == AssetOperationMove {
		var err error
		destinationPath, err = normalizeOperationPath(op.DestinationPath)
		if err != nil {
			return nil, err
		}
	}
	type movePair struct {
		asset   repository.Asset
		newPath string
	}
	pairs := make([]movePair, 0)
	for _, target := range op.Targets {
		if target.Type != AssetTargetRawPath {
			return nil, fmt.Errorf("%w: Raw 操作目标必须为 raw_path", ErrValidation)
		}
		root, err := normalizeOperationPath(target.Path)
		if err != nil {
			return nil, err
		}
		if op.Action == AssetOperationMove && (destinationPath == root || strings.HasPrefix(destinationPath, root+"/")) {
			return nil, fmt.Errorf("%w: 目标目录不能位于源目录子树", ErrConflict)
		}
		assets, err := s.expandPath(repo.ID, target.Path)
		if err != nil {
			return nil, err
		}
		for _, asset := range assets {
			newPath := op.NewPath
			if op.Action == AssetOperationMove {
				relative := path.Base(asset.Path)
				if asset.Path != root {
					relative = strings.TrimPrefix(asset.Path, root+"/")
				}
				newPath = destinationPath + "/" + relative
			}
			newPath, err = normalizeOperationPath(newPath)
			if err != nil {
				return nil, err
			}
			pairs = append(pairs, movePair{asset: asset, newPath: newPath})
		}
	}
	if len(pairs) > maxAssetOperationItems {
		return nil, ErrOperationLimit
	}
	if op.Action == AssetOperationRename && len(pairs) != 1 {
		return nil, fmt.Errorf("%w: 重命名目标必须是单个文件", ErrValidation)
	}
	items := make([]repository.AssetMutationItem, 0, len(pairs))
	seen := make(map[string]struct{}, len(pairs))
	seenSources := make(map[string]struct{}, len(pairs))
	for _, pair := range pairs {
		asset, newPath := pair.asset, pair.newPath
		sourceKey := fmt.Sprintf("%d/%s", asset.RepositoryID, asset.Path)
		if _, ok := seenSources[sourceKey]; ok {
			return nil, fmt.Errorf("%w: 重叠移动目标", ErrConflict)
		}
		seenSources[sourceKey] = struct{}{}
		if _, ok := seen[newPath]; ok || newPath == asset.Path {
			return nil, fmt.Errorf("%w: 目标路径冲突", ErrConflict)
		}
		seen[newPath] = struct{}{}
		if _, err := s.assets.GetByPath(repo.ID, newPath); err == nil {
			return nil, fmt.Errorf("%w: 目标路径已存在 %s", ErrConflict, newPath)
		} else if !errors.Is(err, repository.ErrNotFound) {
			return nil, err
		}
		after := asset
		after.Path = newPath
		items = append(items, repository.AssetMutationItem{RepositoryID: repo.ID, Path: asset.Path, Before: &asset, After: &after})
	}
	return items, nil
}

func (s *AssetService) planDelete(repo *repository.Repository, targets []AssetOperationTarget) ([]repository.AssetMutationItem, []string, error) {
	var items []repository.AssetMutationItem
	var created []string
	for _, target := range targets {
		var selected []repository.Asset
		var extra []repository.AssetMutationItem
		var err error
		switch repo.Format {
		case "raw":
			if target.Type != AssetTargetRawPath {
				return nil, nil, fmt.Errorf("%w: Raw 删除目标必须为 raw_path", ErrValidation)
			}
			selected, err = s.expandPath(repo.ID, target.Path)
		case "maven":
			if target.Type != AssetTargetMavenVersion && target.Type != AssetTargetMavenArtifact {
				return nil, nil, ErrLogicalDeleteRequired
			}
			selected, err = s.expandMavenTarget(repo.ID, target)
			if err == nil && target.Type == AssetTargetMavenVersion {
				extra, created, err = s.planMavenMetadata(repo.ID, target.Path, created)
			}
		case "npm":
			if target.Type != AssetTargetNpmPackage && target.Type != AssetTargetNpmVersion && target.Type != AssetTargetAssetPath {
				return nil, nil, ErrLogicalDeleteRequired
			}
			logical := target
			if target.Type == AssetTargetAssetPath {
				logical, err = s.resolveNpmAssetPath(repo.ID, target.Path)
			}
			if err == nil {
				selected, extra, created, err = s.planNpmTarget(repo.ID, logical, created)
			}
		default:
			return nil, nil, fmt.Errorf("%w: 格式 %s 暂不支持格式感知删除", ErrOperationUnsupported, repo.Format)
		}
		if err != nil {
			return nil, nil, err
		}
		items = appendUniqueMutationItems(items, deleteMutationItems(selected)...)
		items = appendUniqueMutationItems(items, extra...)
		if len(items) > maxAssetOperationItems {
			return nil, nil, ErrOperationLimit
		}
	}
	return items, created, nil
}

func deleteMutationItems(assets []repository.Asset) []repository.AssetMutationItem {
	items := make([]repository.AssetMutationItem, 0, len(assets))
	for i := range assets {
		asset := assets[i]
		items = append(items, repository.AssetMutationItem{RepositoryID: asset.RepositoryID, Path: asset.Path, Before: &asset})
	}
	return items
}

func appendUniqueMutationItems(dst []repository.AssetMutationItem, src ...repository.AssetMutationItem) []repository.AssetMutationItem {
	seen := make(map[string]int, len(dst))
	for i, item := range dst {
		seen[fmt.Sprintf("%d/%s", item.RepositoryID, item.Path)] = i
	}
	for _, item := range src {
		key := fmt.Sprintf("%d/%s", item.RepositoryID, item.Path)
		if i, ok := seen[key]; ok {
			if dst[i].After == nil && item.After != nil {
				dst[i] = item
			}
			continue
		}
		seen[key] = len(dst)
		dst = append(dst, item)
	}
	return dst
}

func (s *AssetService) expandPath(repoID int64, raw string) ([]repository.Asset, error) {
	p, err := normalizeOperationPath(raw)
	if err != nil {
		return nil, err
	}
	if asset, err := s.assets.GetByPath(repoID, p); err == nil {
		return []repository.Asset{*asset}, nil
	}
	assets, err := s.assets.ListByRepo(repoID, p+"/", maxAssetOperationItems+1, 0)
	if err != nil {
		return nil, err
	}
	if len(assets) == 0 {
		return nil, ErrNotFound
	}
	if len(assets) > maxAssetOperationItems {
		return nil, ErrOperationLimit
	}
	return assets, nil
}

func (s *AssetService) expandMavenTarget(repoID int64, target AssetOperationTarget) ([]repository.Asset, error) {
	p, err := normalizeOperationPath(target.Path)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(p, "/")
	switch target.Type {
	case AssetTargetMavenVersion:
		if len(parts) < 3 {
			return nil, fmt.Errorf("%w: Maven version 目标必须为 group/artifact/version", ErrLogicalDeleteRequired)
		}
	case AssetTargetMavenArtifact:
		if len(parts) < 2 {
			return nil, fmt.Errorf("%w: Maven artifact 目标必须为 group/artifact", ErrLogicalDeleteRequired)
		}
	default:
		return nil, ErrLogicalDeleteRequired
	}
	if isMavenFileTarget(parts[len(parts)-1]) {
		return nil, fmt.Errorf("%w: Maven 文件必须由逻辑制品删除", ErrLogicalDeleteRequired)
	}
	assets, err := s.assets.ListByRepo(repoID, p+"/", maxAssetOperationItems+1, 0)
	if err != nil {
		return nil, err
	}
	if len(assets) == 0 {
		parent := strings.Join(parts[:len(parts)-1], "/")
		parentAssets, parentErr := s.assets.ListByRepo(repoID, parent+"/", maxAssetOperationItems+1, 0)
		if parentErr != nil {
			return nil, parentErr
		}
		if len(parentAssets) > 0 && isMavenLogicalRoot(parent, target.Type, parentAssets) {
			return nil, fmt.Errorf("%w: Maven 删除目标必须是完整 artifact 或 version 根", ErrLogicalDeleteRequired)
		}
		return nil, ErrNotFound
	}
	if !isMavenLogicalRoot(p, target.Type, assets) {
		return nil, fmt.Errorf("%w: Maven 删除目标必须是完整 artifact 或 version 根", ErrLogicalDeleteRequired)
	}
	return assets, nil
}

func isMavenFileTarget(name string) bool {
	for _, suffix := range []string{".pom", ".xml", ".jar", ".war", ".ear", ".aar", ".zip", ".sha1", ".sha256", ".md5", ".asc", ".module"} {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}

func isMavenLogicalRoot(root string, targetType AssetOperationTargetType, assets []repository.Asset) bool {
	parts := strings.Split(root, "/")
	artifact := parts[len(parts)-1]
	version := ""
	if targetType == AssetTargetMavenVersion {
		artifact = parts[len(parts)-2]
		version = parts[len(parts)-1]
	}
	for _, asset := range assets {
		relative := strings.TrimPrefix(asset.Path, root+"/")
		segments := strings.Split(relative, "/")
		if targetType == AssetTargetMavenVersion {
			if len(segments) == 1 && strings.HasPrefix(segments[0], artifact+"-"+version+".") {
				return true
			}
			continue
		}
		if len(segments) == 2 && strings.HasPrefix(segments[1], artifact+"-"+segments[0]+".") {
			return true
		}
	}
	return false
}

func normalizeOperationPath(raw string) (string, error) {
	p := strings.Trim(strings.TrimSpace(raw), "/")
	if p == "" || path.IsAbs(p) {
		return "", fmt.Errorf("%w: 路径不能为空或为绝对路径", ErrValidation)
	}
	for _, part := range strings.Split(p, "/") {
		if part == "" || part == "." || part == ".." {
			return "", fmt.Errorf("%w: 路径非法", ErrValidation)
		}
	}
	return p, nil
}

type mavenOperationMetadata struct {
	XMLName    xml.Name `xml:"metadata"`
	GroupID    string   `xml:"groupId,omitempty"`
	ArtifactID string   `xml:"artifactId,omitempty"`
	Versioning struct {
		Latest   string `xml:"latest,omitempty"`
		Release  string `xml:"release,omitempty"`
		Versions struct {
			Version []string `xml:"version"`
		} `xml:"versions"`
		LastUpdated string `xml:"lastUpdated,omitempty"`
	} `xml:"versioning"`
}

func (s *AssetService) planMavenMetadata(repoID int64, version string, created []string) ([]repository.AssetMutationItem, []string, error) {
	parts := strings.Split(strings.Trim(version, "/"), "/")
	artifactPrefix := strings.Join(parts[:len(parts)-1], "/")
	metadataPath := artifactPrefix + "/maven-metadata.xml"
	metadata, err := s.assets.GetByPath(repoID, metadataPath)
	if errors.Is(err, repository.ErrNotFound) {
		return nil, created, nil
	}
	if err != nil {
		return nil, created, err
	}
	data, err := s.readAsset(metadata)
	if err != nil {
		return nil, created, err
	}
	var doc mavenOperationMetadata
	if err := xml.Unmarshal(data, &doc); err != nil {
		return nil, created, fmt.Errorf("%w: Maven metadata 非法", ErrConflict)
	}
	removedVersion := parts[len(parts)-1]
	remaining := doc.Versioning.Versions.Version[:0]
	for _, v := range doc.Versioning.Versions.Version {
		if v != removedVersion {
			remaining = append(remaining, v)
		}
	}
	doc.Versioning.Versions.Version = remaining
	if len(remaining) == 0 {
		return mavenMetadataDeletes(s, repoID, metadata), created, nil
	}
	doc.Versioning.Latest = remaining[len(remaining)-1]
	doc.Versioning.Release = ""
	for i := len(remaining) - 1; i >= 0; i-- {
		if !strings.HasSuffix(remaining[i], "-SNAPSHOT") {
			doc.Versioning.Release = remaining[i]
			break
		}
	}
	doc.Versioning.LastUpdated = time.Now().UTC().Format("20060102150405")
	updated, err := xml.Marshal(&doc)
	if err != nil {
		return nil, created, err
	}
	updated = append([]byte(xml.Header), updated...)
	return s.replaceMetadata(repoID, metadata, updated, created)
}

func mavenMetadataDeletes(s *AssetService, repoID int64, metadata *repository.Asset) []repository.AssetMutationItem {
	result := deleteMutationItems([]repository.Asset{*metadata})
	for _, ext := range []string{".md5", ".sha1", ".sha256"} {
		if asset, err := s.assets.GetByPath(repoID, metadata.Path+ext); err == nil {
			result = append(result, deleteMutationItems([]repository.Asset{*asset})...)
		}
	}
	return appendUniqueMutationItems(nil, result...)
}

func (s *AssetService) replaceMetadata(repoID int64, old *repository.Asset, data []byte, created []string) ([]repository.AssetMutationItem, []string, error) {
	hash, sha1sum, md5sum, size, created, err := s.putPlannedBlob(data, created)
	if err != nil {
		return nil, created, err
	}
	updated := *old
	updated.BlobHash, updated.Sha1, updated.Md5, updated.Size = hash, sha1sum, md5sum, size
	items := []repository.AssetMutationItem{{RepositoryID: repoID, Path: old.Path, Before: old, After: &updated}}
	for _, ext := range []string{".md5", ".sha1", ".sha256"} {
		checksumAsset, err := s.assets.GetByPath(repoID, old.Path+ext)
		if errors.Is(err, repository.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, created, err
		}
		value, err := metadataChecksum(ext, data)
		if err != nil {
			return nil, created, err
		}
		checksumHash, checksumSHA1, checksumMD5, checksumSize, nextCreated, err := s.putPlannedBlob([]byte(value), created)
		if err != nil {
			return nil, created, err
		}
		created = nextCreated
		updatedChecksum := *checksumAsset
		updatedChecksum.BlobHash, updatedChecksum.Sha1, updatedChecksum.Md5, updatedChecksum.Size = checksumHash, checksumSHA1, checksumMD5, checksumSize
		items = append(items, repository.AssetMutationItem{RepositoryID: repoID, Path: checksumAsset.Path, Before: checksumAsset, After: &updatedChecksum})
	}
	return items, created, nil
}

// putPlannedBlob 为规划阶段的 blob 保留暂存保护，直至操作提交或失败回收。
func (s *AssetService) putPlannedBlob(data []byte, created []string) (string, string, string, int64, []string, error) {
	sum := sha256.Sum256(data)
	existed := s.blobs.Exists(hex.EncodeToString(sum[:]))
	hash, sha1sum, md5sum, size, err := s.putStagedBlob(bytes.NewReader(data))
	if err != nil {
		return "", "", "", 0, created, err
	}
	if !existed {
		created = append(created, hash)
	}
	return hash, sha1sum, md5sum, size, created, nil
}

func metadataChecksum(ext string, data []byte) (string, error) {
	var sum hash.Hash
	switch ext {
	case ".md5":
		sum = md5.New()
	case ".sha1":
		sum = sha1.New()
	case ".sha256":
		sum = sha256.New()
	default:
		return "", fmt.Errorf("%w: 不支持的 Maven 校验和扩展名", ErrValidation)
	}
	_, _ = sum.Write(data)
	return hex.EncodeToString(sum.Sum(nil)), nil
}

func (s *AssetService) readAsset(asset *repository.Asset) ([]byte, error) {
	rc, _, err := s.blobs.Open(asset.BlobHash)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	return io.ReadAll(rc)
}

func (s *AssetService) resolveNpmAssetPath(repoID int64, raw string) (AssetOperationTarget, error) {
	p, err := normalizeOperationPath(raw)
	if err != nil {
		return AssetOperationTarget{}, err
	}
	if strings.Contains(p, "/-/") {
		pkg, file, _ := strings.Cut(p, "/-/")
		metadata, err := s.assets.GetByPath(repoID, pkg)
		if err != nil {
			return AssetOperationTarget{}, ErrNotFound
		}
		doc, err := s.readJSONAsset(metadata)
		if err != nil {
			return AssetOperationTarget{}, fmt.Errorf("%w: npm packument 非法", ErrConflict)
		}
		versions, _ := doc["versions"].(map[string]any)
		matches := make([]string, 0, 1)
		for version, rawVersion := range versions {
			vm, _ := rawVersion.(map[string]any)
			dist, _ := vm["dist"].(map[string]any)
			tarball, _ := dist["tarball"].(string)
			if path.Base(tarball) == file {
				matches = append(matches, version)
			}
		}
		if len(matches) != 1 {
			return AssetOperationTarget{}, fmt.Errorf("%w: tarball 未能唯一映射 npm 版本", ErrLogicalDeleteRequired)
		}
		return AssetOperationTarget{Type: AssetTargetNpmVersion, Path: pkg + "@" + matches[0]}, nil
	}
	if _, err := s.assets.GetByPath(repoID, p); err != nil {
		return AssetOperationTarget{}, mapNotFound(err)
	}
	return AssetOperationTarget{Type: AssetTargetNpmPackage, Path: p}, nil
}

func (s *AssetService) planNpmTarget(repoID int64, target AssetOperationTarget, created []string) ([]repository.Asset, []repository.AssetMutationItem, []string, error) {
	pkg := target.Path
	version := ""
	if target.Type == AssetTargetNpmVersion {
		at := strings.LastIndex(pkg, "@")
		if at <= 0 || at == len(pkg)-1 {
			return nil, nil, created, fmt.Errorf("%w: npm_version 必须为 package@version", ErrValidation)
		}
		version, pkg = pkg[at+1:], pkg[:at]
	}
	pkg, err := normalizeOperationPath(pkg)
	if err != nil {
		return nil, nil, created, err
	}
	packument, err := s.assets.GetByPath(repoID, pkg)
	if err != nil {
		return nil, nil, created, mapNotFound(err)
	}
	if target.Type == AssetTargetNpmPackage {
		assets, err := s.expandNpmPackage(repoID, pkg)
		return assets, nil, created, err
	}
	doc, err := s.readJSONAsset(packument)
	if err != nil {
		return nil, nil, created, fmt.Errorf("%w: npm packument 非法", ErrConflict)
	}
	versions, _ := doc["versions"].(map[string]any)
	rawVersion, ok := versions[version]
	if !ok {
		return nil, nil, created, ErrNotFound
	}
	paths := []repository.Asset{*packument}
	vm, _ := rawVersion.(map[string]any)
	dist, _ := vm["dist"].(map[string]any)
	if tarball, _ := dist["tarball"].(string); tarball != "" {
		if asset, err := s.assets.GetByPath(repoID, pkg+"/-/"+path.Base(tarball)); err == nil {
			paths = append(paths, *asset)
		}
	}
	delete(versions, version)
	if len(versions) == 0 {
		assets, err := s.expandNpmPackage(repoID, pkg)
		return assets, nil, created, err
	}
	if tags, ok := doc["dist-tags"].(map[string]any); ok {
		for tag, value := range tags {
			if value == version {
				delete(tags, tag)
			}
		}
	}
	if times, ok := doc["time"].(map[string]any); ok {
		delete(times, version)
	}
	updated, err := json.Marshal(doc)
	if err != nil {
		return nil, nil, created, err
	}
	newHash, sha1sum, md5sum, size, created, err := s.putPlannedBlob(updated, created)
	if err != nil {
		return nil, nil, created, err
	}
	updatedAsset := *packument
	updatedAsset.BlobHash, updatedAsset.Sha1, updatedAsset.Md5, updatedAsset.Size = newHash, sha1sum, md5sum, size
	paths[0] = *packument
	paths = append(paths, updatedAsset)
	return nil, npmVersionMutationItems(paths, updatedAsset), created, nil
}

func npmVersionMutationItems(paths []repository.Asset, updated repository.Asset) []repository.AssetMutationItem {
	items := deleteMutationItems(paths[1:])
	old := paths[0]
	items = append(items, repository.AssetMutationItem{RepositoryID: old.RepositoryID, Path: old.Path, Before: &old, After: &updated})
	return items
}

func (s *AssetService) expandNpmPackage(repoID int64, pkg string) ([]repository.Asset, error) {
	packument, err := s.assets.GetByPath(repoID, pkg)
	if err != nil {
		return nil, mapNotFound(err)
	}
	assets, err := s.assets.ListByRepo(repoID, pkg+"/-/", maxAssetOperationItems+1, 0)
	if err != nil {
		return nil, err
	}
	assets = append([]repository.Asset{*packument}, assets...)
	if len(assets) > maxAssetOperationItems {
		return nil, ErrOperationLimit
	}
	return assets, nil
}

func (s *AssetService) readJSONAsset(asset *repository.Asset) (map[string]any, error) {
	data, err := s.readAsset(asset)
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	return doc, nil
}
