package domain

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

const (
	cargoMaxMetadataBytes = 16 << 20
	cargoMaxCrateBytes    = 2 << 30
)

var cargoNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

// CargoService 负责 Cargo sparse 索引、crate 发布与下载的领域规则。
// 索引和字节均复用通用 asset/blob 存储，避免为单一协议引入第二套内容真源。
type CargoService struct {
	assets *AssetService
	repos  *RepositoryService
}

// NewCargoService 构造 Cargo sparse 领域服务。
func NewCargoService(assets *AssetService, repos *RepositoryService) *CargoService {
	return &CargoService{assets: assets, repos: repos}
}

// CargoPublishResult 是一次 Cargo publish 的结果摘要。
type CargoPublishResult struct {
	Name     string
	Version  string
	Checksum string
	Path     string
	Size     int64
}

// CargoPublishGuard 在 crate 字节落库前校验发布策略；返回的结算函数必须只在发布成功后调用。
type CargoPublishGuard func(path string, size int64) (func(bool), error)

// CargoConfig 是返回给 Cargo 客户端的 sparse 根配置。
type CargoConfig struct {
	Dl           string `json:"dl"`
	Api          string `json:"api,omitempty"`
	AuthRequired bool   `json:"auth-required"`
}

// Config 生成仓库专属 config.json。baseURL 必须是节点对外根地址。
func (s *CargoService) Config(repoName, baseURL string, anonymousRead bool) (*CargoConfig, error) {
	repo, err := s.repos.Get(repoName)
	if err != nil {
		return nil, err
	}
	if repo.Format != "cargo" {
		return nil, ErrConflict
	}
	baseURL = strings.TrimRight(baseURL, "/")
	result := &CargoConfig{
		Dl:           baseURL + "/cargo/" + repoName + "/api/v1/crates/{crate}/{version}/download",
		AuthRequired: !anonymousRead,
	}
	if repo.Type == "hosted" {
		result.Api = baseURL + "/cargo/" + repoName
	}
	return result, nil
}

// Publish 解析 Cargo publish 二进制帧，并按“crate 字节先落盘、索引最后可见”顺序发布。
func (s *CargoService) Publish(ctx context.Context, repoName string, r io.Reader) (*CargoPublishResult, error) {
	return s.PublishWithGuard(ctx, repoName, r, nil)
}

// PublishWithGuard 在解析出规范 crate 路径及实际字节数后、写入任何资产前执行发布策略。
func (s *CargoService) PublishWithGuard(ctx context.Context, repoName string, r io.Reader, guard CargoPublishGuard) (*CargoPublishResult, error) {
	return s.publishCargo(ctx, repoName, r, guard, time.Time{})
}

// publishCargo 是 Publish 的共享实现；sourceModified 非零时将 crate 资产行
// created_at/updated_at 固定为源端时间（在线迁移保留源时间戳）。
// 索引资产是既有索引与新版本行合并后的衍生内容，保持本地时间语义。
func (s *CargoService) publishCargo(ctx context.Context, repoName string, r io.Reader, guard CargoPublishGuard, sourceModified time.Time) (*CargoPublishResult, error) {
	repo, err := s.repos.Get(repoName)
	if err != nil {
		return nil, err
	}
	if repo.Format != "cargo" || repo.Type != "hosted" {
		return nil, ErrConflict
	}
	metadata, crateLen, err := parseCargoPublishHeader(r)
	if err != nil {
		return nil, err
	}
	name, version, err := cargoNameVersion(metadata)
	if err != nil {
		return nil, err
	}
	cratePath := cargoAssetPath(name, version)
	indexPath := cargoHostedIndexAssetPath(name)
	settle := func(bool) {}
	if guard != nil {
		settle, err = guard(cratePath, int64(crateLen))
		if err != nil {
			return nil, err
		}
	}
	defer func() { settle(false) }()
	crateFile, err := spoolCargoCrate(r, crateLen)
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.Remove(crateFile.Name()); _ = crateFile.Close() }()
	name, version, indexLine, checksum, err := cargoMetadata(metadata, crateFile)
	if err != nil {
		return nil, err
	}
	if _, rc, err := s.assets.Get(repoName, cratePath); err == nil {
		_ = rc.Close()
		return nil, ErrConflict
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	if _, rc, err := s.assets.Get(repoName, indexPath); err == nil {
		_ = rc.Close()
		lines, readErr := s.readAsset(repoName, indexPath)
		if readErr != nil {
			return nil, readErr
		}
		for _, line := range lines {
			var item struct {
				Vers string `json:"vers"`
			}
			if json.Unmarshal(line, &item) == nil && item.Vers == version {
				return nil, ErrConflict
			}
		}
	}
	if _, err := crateFile.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	crateAsset, err := s.assets.StageBlob(crateFile, "application/x-tar")
	if err != nil {
		return nil, err
	}
	// crate 字节与源端资产一一对应，标注源端时间；索引为合并衍生行，保持本地时间。
	applySourceTimestamp(crateAsset, sourceModified)
	old, err := s.readAsset(repoName, indexPath)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, s.assets.cleanupFailedWrite(crateAsset.BlobHash, err)
	}
	old = append(old, indexLine)
	indexAsset, err := s.assets.StageBlob(bytes.NewReader(append(bytes.Join(old, []byte("\n")), '\n')), "application/json")
	if err != nil {
		return nil, s.assets.cleanupFailedWrite(crateAsset.BlobHash, err)
	}
	crateAsset.Path = cratePath
	indexAsset.Path = indexPath
	if _, err := s.assets.PublishAssets(repoName, []*repository.Asset{crateAsset, indexAsset}); err != nil {
		return nil, s.assets.cleanupFailedWrite(indexAsset.BlobHash, s.assets.cleanupFailedWrite(crateAsset.BlobHash, err))
	}
	settle(true)
	return &CargoPublishResult{Name: name, Version: version, Checksum: checksum, Path: cratePath, Size: int64(crateLen)}, nil
}

// Index 返回一个仓库的 sparse JSON Lines。group 依成员顺序合并同版本记录。
func (s *CargoService) Index(ctx context.Context, repoName, crateName string) ([]byte, error) {
	crateName, err := normalizeCargoName(crateName)
	if err != nil {
		return nil, err
	}
	repo, err := s.repos.Get(repoName)
	if err != nil {
		return nil, err
	}
	if repo.Format != "cargo" {
		return nil, ErrNotFound
	}
	switch repo.Type {
	case "group":
		cfg, cfgErr := repo.DecodeConfig()
		if cfgErr != nil {
			return nil, cfgErr
		}
		var merged []byte
		for _, member := range cfg.Members {
			data, memberErr := s.Index(ctx, member, crateName)
			if memberErr != nil {
				continue
			}
			merged = append(merged, data...)
		}
		lines, mergeErr := mergeCargoLines(nil, merged)
		if mergeErr != nil || len(lines) == 0 {
			return nil, ErrNotFound
		}
		return []byte(strings.Join(lines, "\n") + "\n"), nil
	case "proxy":
		return s.proxyIndex(ctx, repo, crateName)
	default:
		data, readErr := s.readAsset(repoName, cargoHostedIndexAssetPath(crateName))
		if readErr != nil {
			return nil, readErr
		}
		encoded := make([]string, 0, len(data))
		for _, line := range data {
			encoded = append(encoded, string(line))
		}
		return []byte(strings.Join(encoded, "\n") + "\n"), nil
	}
}

// Download 返回 crate 字节流。group 按成员配置顺序解析。
func (s *CargoService) Download(ctx context.Context, repoName, crateName, version string) (*repository.Asset, io.ReadCloser, error) {
	crateName, err := normalizeCargoName(crateName)
	if err != nil || version == "" || strings.ContainsAny(version, "/\\") {
		return nil, nil, ErrValidation
	}
	repo, err := s.repos.Get(repoName)
	if err != nil {
		return nil, nil, err
	}
	if repo.Format != "cargo" {
		return nil, nil, ErrNotFound
	}
	switch repo.Type {
	case "group":
		cfg, cfgErr := repo.DecodeConfig()
		if cfgErr != nil {
			return nil, nil, cfgErr
		}
		for _, member := range cfg.Members {
			asset, rc, memberErr := s.Download(ctx, member, crateName, version)
			if memberErr == nil {
				return asset, rc, nil
			}
		}
		return nil, nil, ErrNotFound
	case "proxy":
		return s.proxyDownload(ctx, repo, crateName, version)
	default:
		return s.assets.Resolve(ctx, repoName, cargoAssetPath(crateName, version))
	}
}

// SetYanked 更新 hosted sparse 索引中的 yanked 字段，不触碰 crate 字节。
func (s *CargoService) SetYanked(repoName, crateName, version string, yanked bool) error {
	repo, err := s.repos.Get(repoName)
	if err != nil {
		return err
	}
	if repo.Format != "cargo" || repo.Type != "hosted" {
		return ErrConflict
	}
	cfg, err := repo.DecodeConfig()
	if err != nil {
		return err
	}
	if cfg.ImmutableRelease {
		return ErrImmutableRelease
	}
	name, err := normalizeCargoName(crateName)
	if err != nil || version == "" {
		return ErrValidation
	}
	indexPath := cargoHostedIndexAssetPath(name)
	lines, err := s.readAsset(repoName, indexPath)
	if err != nil {
		return err
	}
	changed := false
	for i, line := range lines {
		var item map[string]any
		if json.Unmarshal(line, &item) != nil || item["vers"] != version {
			continue
		}
		item["yanked"] = yanked
		updated, marshalErr := json.Marshal(item)
		if marshalErr != nil {
			return marshalErr
		}
		lines[i] = updated
		changed = true
	}
	if !changed {
		return ErrNotFound
	}
	asset, err := s.assets.StageBlob(bytes.NewReader(append(bytes.Join(lines, []byte("\n")), '\n')), "application/json")
	if err != nil {
		return err
	}
	asset.Path = indexPath
	if _, err := s.assets.PublishAssets(repoName, []*repository.Asset{asset}); err != nil {
		return s.assets.cleanupFailedWrite(asset.BlobHash, err)
	}
	return nil
}

func (s *CargoService) proxyIndex(ctx context.Context, repo *repository.Repository, crateName string) ([]byte, error) {
	indexPath := cargoIndexPath(crateName)
	if _, rc, err := s.assets.localGet(repo, indexPath); err == nil {
		defer func() { _ = rc.Close() }()
		data, readErr := io.ReadAll(rc)
		if readErr != nil {
			return nil, readErr
		}
		if _, validationErr := cargoIndexChecksum(data, crateName, ""); validationErr != nil {
			return nil, ErrUpstream
		}
		return data, nil
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	if err := s.assets.requireBusinessWrite(); err != nil || s.assets.upstream == nil || !repo.Online {
		return nil, ErrNotFound
	}
	cfg, err := repo.DecodeConfig()
	if err != nil || cfg.RemoteURL == "" {
		return nil, ErrNotFound
	}
	key := fmt.Sprintf("cargo:index:%d\x00%s", repo.ID, indexPath)
	_, err, _ = s.assets.sf.Do(key, func() (any, error) {
		if _, _, cachedErr := s.assets.localGet(repo, indexPath); cachedErr == nil {
			return nil, nil
		}
		body, _, fetchErr := s.assets.upstream.FetchWithCredential(ctx, cfg.RemoteURL, indexPath, cfg.CredentialRef)
		if fetchErr != nil {
			return nil, mapUpstreamErr(fetchErr)
		}
		defer func() { _ = body.Close() }()
		data, readErr := io.ReadAll(io.LimitReader(body, cargoMaxMetadataBytes+1))
		if readErr != nil || len(data) > cargoMaxMetadataBytes {
			return nil, ErrUpstream
		}
		if _, validationErr := cargoIndexChecksum(data, crateName, ""); validationErr != nil {
			return nil, ErrUpstream
		}
		asset, stageErr := s.assets.StageBlob(bytes.NewReader(data), "application/json")
		if stageErr != nil {
			return nil, stageErr
		}
		if _, putErr := s.assets.persistAssetWithHook(repo, indexPath, asset.BlobHash, asset.Sha1, asset.Md5, asset.Size, asset.ContentType, nil); putErr != nil {
			return nil, s.assets.cleanupFailedWrite(asset.BlobHash, putErr)
		}
		return nil, nil
	})
	if err != nil {
		return nil, err
	}
	_, rc, err := s.assets.localGet(repo, indexPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	return io.ReadAll(rc)
}

func (s *CargoService) proxyDownload(ctx context.Context, repo *repository.Repository, crateName, version string) (*repository.Asset, io.ReadCloser, error) {
	assetPath := cargoAssetPath(crateName, version)
	if asset, rc, err := s.assets.localGet(repo, assetPath); err == nil {
		return asset, rc, nil
	} else if !errors.Is(err, ErrNotFound) {
		return nil, nil, err
	}
	if err := s.assets.requireBusinessWrite(); err != nil || s.assets.upstream == nil || !repo.Online {
		return nil, nil, ErrNotFound
	}
	cfg, err := repo.DecodeConfig()
	if err != nil || cfg.RemoteURL == "" {
		return nil, nil, ErrNotFound
	}
	key := fmt.Sprintf("cargo:download:%d\x00%s\x00%s", repo.ID, crateName, version)
	_, err, _ = s.assets.sf.Do(key, func() (any, error) {
		if _, _, cachedErr := s.assets.localGet(repo, assetPath); cachedErr == nil {
			return nil, nil
		}
		index, indexErr := s.proxyIndex(ctx, repo, crateName)
		if indexErr != nil {
			return nil, indexErr
		}
		checksum, checksumErr := cargoIndexChecksum(index, crateName, version)
		if checksumErr != nil {
			return nil, ErrNotFound
		}
		template, templateErr := s.proxyDownloadTemplate(ctx, cfg)
		if templateErr != nil {
			return nil, templateErr
		}
		downloadURL, urlErr := cargoDownloadURL(template, crateName, version, checksum)
		if urlErr != nil {
			return nil, ErrUpstream
		}
		body, _, fetchErr := s.assets.upstream.FetchWithCredential(ctx, downloadURL, "", cfg.CredentialRef)
		if fetchErr != nil {
			return nil, mapUpstreamErr(fetchErr)
		}
		defer func() { _ = body.Close() }()
		asset, stageErr := s.assets.StageBlob(body, "application/x-tar")
		if stageErr != nil {
			return nil, stageErr
		}
		if asset.BlobHash != checksum {
			return nil, s.assets.cleanupFailedWrite(asset.BlobHash, ErrUpstream)
		}
		if _, putErr := s.assets.persistAssetWithHook(repo, assetPath, asset.BlobHash, asset.Sha1, asset.Md5, asset.Size, asset.ContentType, nil); putErr != nil {
			return nil, s.assets.cleanupFailedWrite(asset.BlobHash, putErr)
		}
		return nil, nil
	})
	if err != nil {
		return nil, nil, err
	}
	return s.assets.localGet(repo, assetPath)
}

func (s *CargoService) proxyDownloadTemplate(ctx context.Context, cfg repository.RepositoryConfig) (string, error) {
	body, _, err := s.assets.upstream.FetchWithCredential(ctx, cfg.RemoteURL, "config.json", cfg.CredentialRef)
	if err != nil {
		return "", mapUpstreamErr(err)
	}
	defer func() { _ = body.Close() }()
	var config struct {
		Dl string `json:"dl"`
	}
	if err := json.NewDecoder(io.LimitReader(body, cargoMaxMetadataBytes)).Decode(&config); err != nil || config.Dl == "" {
		return "", ErrUpstream
	}
	return config.Dl, nil
}

func cargoIndexChecksum(data []byte, crateName, version string) (string, error) {
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var item struct {
			Name  string `json:"name"`
			Vers  string `json:"vers"`
			Cksum string `json:"cksum"`
		}
		if err := json.Unmarshal(line, &item); err != nil || item.Name != crateName || item.Vers == "" || !validCargoChecksum(item.Cksum) {
			return "", ErrValidation
		}
		if version == "" {
			continue
		}
		if item.Vers == version {
			return item.Cksum, nil
		}
	}
	if version == "" {
		return "", nil
	}
	return "", ErrNotFound
}

func validCargoChecksum(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

func cargoDownloadURL(template, crateName, version, checksum string) (string, error) {
	prefix := crateName
	if len(prefix) > 2 {
		prefix = prefix[:2]
	}
	replacer := strings.NewReplacer(
		"{crate}", url.PathEscape(crateName),
		"{version}", url.PathEscape(version),
		"{prefix}", url.PathEscape(prefix),
		"{lowerprefix}", url.PathEscape(strings.ToLower(prefix)),
		"{sha256-checksum}", checksum,
	)
	resolved := replacer.Replace(template)
	if strings.ContainsAny(resolved, "{}") {
		return "", ErrValidation
	}
	parsed, err := url.Parse(resolved)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", ErrValidation
	}
	return resolved, nil
}

func (s *CargoService) readAsset(repoName, assetPath string) ([][]byte, error) {
	_, rc, err := s.assets.Get(repoName, assetPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	var lines [][]byte
	scanner := bufio.NewScanner(rc)
	scanner.Buffer(make([]byte, 64*1024), cargoMaxMetadataBytes)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) > 0 {
			lines = append(lines, append([]byte(nil), line...))
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return lines, nil
}

func parseCargoPublishHeader(r io.Reader) (json.RawMessage, uint32, error) {
	readLen := func() (uint32, error) {
		var buf [4]byte
		if _, err := io.ReadFull(r, buf[:]); err != nil {
			return 0, fmt.Errorf("cargo 发布帧长度非法：%w", err)
		}
		return binary.LittleEndian.Uint32(buf[:]), nil
	}
	metadataLen, err := readLen()
	if err != nil || metadataLen == 0 || metadataLen > cargoMaxMetadataBytes {
		return nil, 0, ErrValidation
	}
	metadata := make([]byte, metadataLen)
	if _, err := io.ReadFull(r, metadata); err != nil {
		return nil, 0, ErrValidation
	}
	if !json.Valid(metadata) {
		return nil, 0, ErrValidation
	}
	crateLen, err := readLen()
	if err != nil || crateLen == 0 || int64(crateLen) > cargoMaxCrateBytes {
		return nil, 0, ErrValidation
	}
	return metadata, crateLen, nil
}

func spoolCargoCrate(r io.Reader, crateLen uint32) (*os.File, error) {
	f, err := os.CreateTemp("", "jianartifact-cargo-")
	if err != nil {
		return nil, err
	}
	if _, err := io.CopyN(f, r, int64(crateLen)); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return nil, ErrValidation
	}
	return f, nil
}

func cargoNameVersion(metadata json.RawMessage) (string, string, error) {
	var item struct {
		Name    string `json:"name"`
		Version string `json:"vers"`
	}
	if err := json.Unmarshal(metadata, &item); err != nil {
		return "", "", ErrValidation
	}
	name, err := normalizeCargoName(item.Name)
	if err != nil || item.Version == "" || strings.ContainsAny(item.Version, "/\\") {
		return "", "", ErrValidation
	}
	return name, item.Version, nil
}

func cargoMetadata(metadata json.RawMessage, crateFile *os.File) (string, string, []byte, string, error) {
	var item struct {
		Name     string          `json:"name"`
		Version  string          `json:"vers"`
		Deps     json.RawMessage `json:"deps"`
		Features json.RawMessage `json:"features"`
	}
	if err := json.Unmarshal(metadata, &item); err != nil {
		return "", "", nil, "", ErrValidation
	}
	name, version, err := cargoNameVersion(metadata)
	if err != nil {
		return "", "", nil, "", err
	}
	if _, err := crateFile.Seek(0, io.SeekStart); err != nil {
		return "", "", nil, "", err
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, crateFile); err != nil {
		return "", "", nil, "", err
	}
	checksum := hex.EncodeToString(hash.Sum(nil))
	line := map[string]any{"name": name, "vers": version, "deps": json.RawMessage(defaultJSON(item.Deps, []byte("[]"))), "cksum": checksum, "features": json.RawMessage(defaultJSON(item.Features, []byte("{}"))), "yanked": false}
	encoded, err := json.Marshal(line)
	if err != nil {
		return "", "", nil, "", err
	}
	return name, version, encoded, checksum, nil
}

func defaultJSON(raw, fallback []byte) []byte {
	if len(bytes.TrimSpace(raw)) == 0 || !json.Valid(raw) {
		return fallback
	}
	return raw
}

func normalizeCargoName(name string) (string, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if !cargoNamePattern.MatchString(name) {
		return "", ErrValidation
	}
	return name, nil
}

func cargoIndexPath(name string) string {
	name = strings.ToLower(name)
	switch len(name) {
	case 1:
		return "1/" + name
	case 2:
		return "2/" + name
	case 3:
		return "3/" + name[:1] + "/" + name
	default:
		return name[:2] + "/" + name[2:4] + "/" + name
	}
}

func cargoHostedIndexAssetPath(name string) string {
	return path.Join("cargo/index", cargoIndexPath(name))
}

func cargoAssetPath(name, version string) string {
	return path.Join("cargo/crates", name, version, name+"-"+version+".crate")
}

func mergeCargoLines(_ []string, data []byte) ([]string, error) {
	seen := make(map[string]struct{})
	lines := make([]string, 0)
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var item struct {
			Vers string `json:"vers"`
		}
		if err := json.Unmarshal(line, &item); err != nil || item.Vers == "" {
			return nil, ErrValidation
		}
		if _, ok := seen[item.Vers]; ok {
			continue
		}
		seen[item.Vers] = struct{}{}
		lines = append(lines, string(line))
	}
	sort.Strings(lines)
	return lines, nil
}
