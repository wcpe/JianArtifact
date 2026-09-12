package domain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/wcpe/jianartifact/apps/server/internal/nugetpackage"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
	"github.com/wcpe/jianartifact/apps/server/internal/upstream"
	"golang.org/x/net/html"
	"golang.org/x/sync/singleflight"
)

// PypiFile 是 PyPI Simple 输出所需的一条候选文件记录。
type PypiFile struct {
	Project        string
	ProjectDisplay string
	Version        string
	Filename       string
	AssetPath      string
	Sha256         string
	Size           int64
	RequiresPython string
	Yanked         string
}

// FormatMetadataService 编排 PyPI/NuGet 的格式元数据与通用资产服务。
// 格式协议只通过该服务访问索引元数据，不直接访问 SQLite/blob 文件。
type FormatMetadataService struct {
	assets    *AssetService
	repos     *repository.RepoRepo
	metadata  *repository.FormatMetadataRepo
	upstream  *upstream.Client
	recorder  ChangeRecorder
	writeGate BusinessWriteGate
	pypiSF    singleflight.Group
	nugetSF   singleflight.Group
}

func NewFormatMetadataService(assets *AssetService, repos *repository.RepoRepo, metadata *repository.FormatMetadataRepo, upstreamClients ...*upstream.Client) *FormatMetadataService {
	var client *upstream.Client
	if len(upstreamClients) > 0 {
		client = upstreamClients[0]
	}
	return &FormatMetadataService{assets: assets, repos: repos, metadata: metadata, upstream: client}
}

// SetBusinessWriteGate 注入备用节点本地业务写栅栏；nil 保持单节点兼容行为。
func (s *FormatMetadataService) SetBusinessWriteGate(gate BusinessWriteGate) { s.writeGate = gate }

func (s *FormatMetadataService) requireBusinessWrite() error {
	return requireBusinessWrite(s.writeGate)
}

// SetChangeRecorder 注入格式元数据的复制变更记录器；nil 表示不记录。
func (s *FormatMetadataService) SetChangeRecorder(recorder ChangeRecorder) { s.recorder = recorder }

func (s *FormatMetadataService) repo(name, format string, hostedOnly bool) (*repository.Repository, error) {
	r, err := s.repos.GetByName(name)
	if err != nil {
		return nil, mapNotFound(err)
	}
	if r.Format != format {
		return nil, ErrConflict
	}
	if hostedOnly && r.Type != "hosted" {
		return nil, ErrConflict
	}
	return r, nil
}

// NormalizePyPIProject 按 PEP 503 规范化项目名。
func NormalizePyPIProject(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	return regexp.MustCompile(`[-_.]+`).ReplaceAllString(name, "-")
}

func validPackageFilename(name string) bool {
	return name != "" && !strings.ContainsAny(name, `/\\`) && name != "." && name != ".."
}

// PublishPyPI 写入一个 PyPI 分发文件并登记 Simple 元数据。
func (s *FormatMetadataService) PublishPyPI(repoName, project, version, filename, requiresPython, yanked string, body io.Reader) (*PypiFile, error) {
	return s.PublishPyPIWithAudit(repoName, project, version, filename, requiresPython, yanked, body, AssetOperationAudit{})
}

// PublishPyPIWithAudit 将 PyPI 文件、Simple 元数据、源端审计和 v2 operation 置于同一提交边界。
func (s *FormatMetadataService) PublishPyPIWithAudit(repoName, project, version, filename, requiresPython, yanked string, body io.Reader, audit AssetOperationAudit) (*PypiFile, error) {
	if err := s.requireBusinessWrite(); err != nil {
		return nil, err
	}
	r, err := s.repo(repoName, "pypi", true)
	if err != nil {
		return nil, err
	}
	project = NormalizePyPIProject(project)
	if project == "" || version == "" || !validPackageFilename(filename) {
		return nil, ErrValidation
	}
	if _, err := s.metadata.Get(r.ID, "pypi", project, filename); err == nil {
		return nil, ErrConflict
	} else if !errors.Is(err, repository.ErrNotFound) {
		return nil, err
	}
	asset, err := s.assets.StageBlob(body, "application/octet-stream")
	if err != nil {
		return nil, err
	}
	asset.Path = "pypi/packages/" + project + "/" + filename
	meta := repository.FormatMetadata{
		RepositoryID: r.ID, Format: "pypi", NameNormalized: project, NameDisplay: project,
		Version: version, VersionNormalized: version, Filename: filename, AssetPath: asset.Path,
		Sha256: asset.BlobHash, Size: asset.Size, RequiresPython: requiresPython, Yanked: yanked,
		MetadataJSON: "{}", SourceKind: "hosted",
	}
	if err := s.publishHostedFormatAsset(repoName, r, asset, meta, audit); err != nil {
		return nil, s.assets.cleanupFailedWrite(asset.BlobHash, err)
	}
	return &PypiFile{Project: project, ProjectDisplay: project, Version: version, Filename: filename, AssetPath: asset.Path, Sha256: asset.BlobHash, Size: asset.Size, RequiresPython: requiresPython, Yanked: yanked}, nil
}

func (s *FormatMetadataService) publishHostedFormatAsset(repoName string, repo *repository.Repository, asset *repository.Asset, meta repository.FormatMetadata, audit AssetOperationAudit) error {
	if meta.CreatedAt == "" {
		meta.CreatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	if meta.UpdatedAt == "" {
		meta.UpdatedAt = meta.CreatedAt
	}
	operationID := newOperationID()
	envelope, err := s.formatMetadataPutOperationEnvelope(repoName, operationID, audit.Actor, asset, meta)
	if err != nil {
		return err
	}
	auditHook := operationAuditHook(audit, operationID, []repository.AssetMutationItem{{RepositoryID: repo.ID, Path: asset.Path, After: asset}})
	_, err = s.assets.PublishAssetsWithEnvelope(repoName, []*repository.Asset{asset}, envelope, func(tx *sqlx.Tx) error {
		if err := s.metadata.PutWithTimeTx(tx, meta); err != nil {
			return err
		}
		if auditHook != nil {
			return auditHook(tx)
		}
		return nil
	})
	return err
}

func (s *FormatMetadataService) formatMetadataPutOperationEnvelope(repoName, operationID string, actor repository.OperationActor, asset *repository.Asset, meta repository.FormatMetadata) (repository.OperationEnvelope, error) {
	nodeID := s.assets.NodeID()
	version := repository.ReplicationVersion{NodeID: nodeID, TS: time.Now().UTC().Format(time.RFC3339Nano)}
	assetData, err := json.Marshal(AssetChangeData{Path: asset.Path, BlobHash: asset.BlobHash, Size: asset.Size, ContentType: asset.ContentType, Sha1: asset.Sha1, Md5: asset.Md5, CreatedAt: asset.CreatedAt, UpdatedAt: asset.UpdatedAt})
	if err != nil {
		return repository.OperationEnvelope{}, err
	}
	metadataData, err := json.Marshal(formatMetadataChangeData(repoName, &meta))
	if err != nil {
		return repository.OperationEnvelope{}, err
	}
	items := []repository.OperationItem{
		{Type: EntityAsset, Key: AssetKey(repoName, asset.Path), Op: OpPut, Data: string(assetData), BlobHashes: []string{asset.BlobHash}, DependsOn: []string{RepoKey(repoName)}, Version: version},
		{Type: EntityFormatMetadata, Key: FormatMetadataKey(repoName, meta.Format, meta.NameNormalized, meta.Filename), Op: OpPut, Data: string(metadataData), DependsOn: []string{RepoKey(repoName)}, Version: version},
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Type+"\x00"+items[i].Key < items[j].Type+"\x00"+items[j].Key })
	manifest, err := repository.OperationManifestSHA256(items)
	if err != nil {
		return repository.OperationEnvelope{}, err
	}
	return repository.OperationEnvelope{Kind: "operation", OperationID: operationID, SourceNode: nodeID, Version: version, ItemCount: len(items), ManifestSHA256: manifest, Actor: actor, Items: items}, nil
}

func (s *FormatMetadataService) pypiFiles(repoName, project string) ([]repository.FormatMetadata, error) {
	r, err := s.repo(repoName, "pypi", false)
	if err != nil {
		return nil, err
	}
	project = NormalizePyPIProject(project)
	if r.Type != "group" {
		items, listErr := s.metadata.List(r.ID, "pypi", project)
		if listErr != nil {
			return nil, listErr
		}
		if r.Type == "proxy" && len(items) == 0 {
			loaded, loadErr, _ := s.pypiSF.Do("index\x00"+r.Name+"\x00"+project, func() (any, error) {
				cached, cachedErr := s.metadata.List(r.ID, "pypi", project)
				if cachedErr != nil || len(cached) > 0 {
					return cached, cachedErr
				}
				return s.loadPyPIProxy(r, project)
			})
			if loadErr != nil {
				return nil, loadErr
			}
			return loaded.([]repository.FormatMetadata), nil
		}
		return items, nil
	}
	cfg, err := r.DecodeConfig()
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool)
	var out []repository.FormatMetadata
	for _, member := range cfg.Members {
		_, e := s.repo(member, "pypi", false)
		if e != nil {
			continue
		}
		items, e := s.pypiFiles(member, project)
		if e != nil {
			return nil, e
		}
		for _, item := range items {
			if !seen[item.Filename] {
				seen[item.Filename] = true
				item.SourceMember = member
				out = append(out, item)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Filename < out[j].Filename })
	return out, nil
}

func (s *FormatMetadataService) PyPIProjects(repoName string) ([]string, error) {
	r, err := s.repo(repoName, "pypi", false)
	if err != nil {
		return nil, err
	}
	if r.Type != "group" {
		projects, listErr := s.metadata.ListProjects(r.ID, "pypi")
		if listErr != nil {
			return nil, listErr
		}
		if r.Type == "proxy" && len(projects) == 0 {
			return s.loadPyPIProxyProjects(r)
		}
		return projects, nil
	}
	cfg, err := r.DecodeConfig()
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, member := range cfg.Members {
		items, e := s.PyPIProjects(member)
		if e != nil {
			continue
		}
		for _, item := range items {
			seen[item] = true
		}
	}
	out := make([]string, 0, len(seen))
	for project := range seen {
		out = append(out, project)
	}
	sort.Strings(out)
	return out, nil
}

func (s *FormatMetadataService) loadPyPIProxy(repo *repository.Repository, project string) ([]repository.FormatMetadata, error) {
	if err := s.requireBusinessWrite(); err != nil {
		return nil, err
	}
	session, indexURL, err := s.pyPIUpstreamSession(repo, project)
	if err != nil {
		return nil, mapUpstreamErr(err)
	}
	rc, _, err := session.ReadPathWithAccept(context.Background(), project+"/", "application/vnd.pypi.simple.v1+json, text/html;q=0.9")
	if err != nil {
		return nil, mapUpstreamErr(err)
	}
	body, readErr := io.ReadAll(io.LimitReader(rc, 4<<20))
	_ = rc.Close()
	if readErr != nil {
		return nil, fmt.Errorf("%w: 读取 PyPI 上游索引失败", ErrUpstream)
	}
	entries := parsePyPIIndex(body, indexURL, project)
	result := make([]repository.FormatMetadata, 0, len(entries))
	for _, entry := range entries {
		sourceJSON, marshalErr := json.Marshal(pypiProxySource{URL: entry.sourceURL})
		if marshalErr != nil {
			return nil, marshalErr
		}
		item := repository.FormatMetadata{
			RepositoryID: repo.ID, Format: "pypi", NameNormalized: project, NameDisplay: project,
			Version: entry.version, VersionNormalized: entry.version, Filename: entry.filename,
			AssetPath: entry.assetPath, Sha256: entry.sha256, RequiresPython: entry.requiresPython,
			Yanked: entry.yanked, MetadataJSON: string(sourceJSON), SourceKind: "proxy",
		}
		result = append(result, item)
	}
	if err := s.metadata.PutAll(result); err != nil {
		return nil, err
	}
	return result, nil
}

func (s *FormatMetadataService) loadPyPIProxyProjects(repo *repository.Repository) ([]string, error) {
	session, _, err := s.pyPIUpstreamSession(repo, "")
	if err != nil {
		return nil, mapUpstreamErr(err)
	}
	rc, _, err := session.ReadPath(context.Background(), "")
	if err != nil {
		return nil, mapUpstreamErr(err)
	}
	body, readErr := io.ReadAll(io.LimitReader(rc, 4<<20))
	_ = rc.Close()
	if readErr != nil {
		return nil, fmt.Errorf("%w: 读取 PyPI 上游项目索引失败", ErrUpstream)
	}
	seen := map[string]bool{}
	for _, href := range parseSimpleHrefs(body) {
		parsed, parseErr := url.Parse(href)
		if parseErr != nil {
			continue
		}
		name := NormalizePyPIProject(strings.Trim(path.Base(strings.TrimSuffix(parsed.Path, "/")), "/"))
		if name != "" {
			seen[name] = true
		}
	}
	projects := make([]string, 0, len(seen))
	for name := range seen {
		projects = append(projects, name)
	}
	sort.Strings(projects)
	return projects, nil
}

type pypiProxyEntry struct {
	filename, assetPath, sourceURL, version, sha256, requiresPython, yanked string
}

type pypiProxySource struct {
	URL string `json:"url"`
}

const maxPyPIProxyPackageBytes int64 = 128 << 20

var pypiSHA256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func (s *FormatMetadataService) pyPIUpstreamSession(repo *repository.Repository, project string) (*upstream.Session, *url.URL, error) {
	if s.upstream == nil {
		return nil, nil, ErrUpstream
	}
	cfg, err := repo.DecodeConfig()
	if err != nil || cfg.RemoteURL == "" {
		return nil, nil, ErrUpstream
	}
	base, err := url.Parse(cfg.RemoteURL)
	if err != nil {
		return nil, nil, ErrUpstream
	}
	base.Path = strings.TrimRight(base.Path, "/") + "/"
	if project != "" {
		base.Path += url.PathEscape(project) + "/"
	}
	session, err := s.upstream.NewSession(cfg.RemoteURL, cfg.CredentialRef)
	if err != nil {
		return nil, nil, err
	}
	return session, base, nil
}

func parsePyPIIndex(body []byte, indexURL *url.URL, project string) []pypiProxyEntry {
	text := strings.TrimSpace(string(body))
	if strings.HasPrefix(text, "{") {
		var doc struct {
			Files []struct {
				Filename string            `json:"filename"`
				URL      string            `json:"url"`
				Hashes   map[string]string `json:"hashes"`
				Requires string            `json:"requires-python"`
				Yanked   any               `json:"yanked"`
			} `json:"files"`
		}
		if json.Unmarshal(body, &doc) == nil {
			out := make([]pypiProxyEntry, 0, len(doc.Files))
			for _, file := range doc.Files {
				if entry, ok := newPyPIProxyEntry(indexURL, project, file.Filename, file.URL, file.Hashes["sha256"], file.Requires, fmt.Sprint(file.Yanked)); ok {
					out = append(out, entry)
				}
			}
			return out
		}
	}
	var out []pypiProxyEntry
	for _, anchor := range parsePyPIAnchors(body) {
		parsed, err := url.Parse(anchor.href)
		if err != nil {
			continue
		}
		filename := path.Base(parsed.Path)
		if entry, ok := newPyPIProxyEntry(indexURL, project, filename, anchor.href, fragmentHash(parsed.Fragment), anchor.requiresPython, anchor.yanked); ok {
			out = append(out, entry)
		}
	}
	return out
}

func newPyPIProxyEntry(indexURL *url.URL, project, filename, rawURL, declaredSHA256, requiresPython, yanked string) (pypiProxyEntry, bool) {
	if indexURL == nil || !validPackageFilename(filename) || !pypiSHA256Pattern.MatchString(declaredSHA256) {
		return pypiProxyEntry{}, false
	}
	reference, err := url.Parse(rawURL)
	if err != nil || reference.User != nil {
		return pypiProxyEntry{}, false
	}
	resolved := indexURL.ResolveReference(reference)
	if resolved.Scheme != "http" && resolved.Scheme != "https" || resolved.Host == "" || resolved.User != nil || resolved.RawQuery != "" || path.Base(resolved.Path) != filename {
		return pypiProxyEntry{}, false
	}
	resolved.Fragment = ""
	return pypiProxyEntry{filename: filename, assetPath: "pypi/packages/" + project + "/" + filename, sourceURL: resolved.String(), version: versionFromFilename(filename), sha256: declaredSHA256, requiresPython: requiresPython, yanked: yanked}, true
}

func parseSimpleHrefs(body []byte) []string {
	anchors := parsePyPIAnchors(body)
	out := make([]string, 0, len(anchors))
	for _, anchor := range anchors {
		out = append(out, anchor.href)
	}
	return out
}

type pypiAnchor struct {
	href, requiresPython, yanked string
}

func parsePyPIAnchors(body []byte) []pypiAnchor {
	tokenizer := html.NewTokenizer(strings.NewReader(string(body)))
	var anchors []pypiAnchor
	for {
		tokenType := tokenizer.Next()
		if tokenType == html.ErrorToken {
			return anchors
		}
		if tokenType != html.StartTagToken && tokenType != html.SelfClosingTagToken {
			continue
		}
		token := tokenizer.Token()
		if !strings.EqualFold(token.Data, "a") {
			continue
		}
		anchor := pypiAnchor{}
		for _, attribute := range token.Attr {
			switch strings.ToLower(attribute.Key) {
			case "href":
				anchor.href = attribute.Val
			case "data-requires-python":
				anchor.requiresPython = attribute.Val
			case "data-yanked":
				anchor.yanked = attribute.Val
			}
		}
		if anchor.href != "" {
			anchors = append(anchors, anchor)
		}
	}
}

func fragmentHash(fragment string) string {
	if key, value, ok := strings.Cut(fragment, "="); ok && strings.EqualFold(key, "sha256") {
		return value
	}
	return fragment
}

func versionFromFilename(filename string) string {
	name := filename
	for _, suffix := range []string{".tar.gz", ".tar.bz2", ".tar.xz", ".whl", ".zip", ".egg", ".tgz"} {
		if strings.HasSuffix(strings.ToLower(name), suffix) {
			name = name[:len(name)-len(suffix)]
			break
		}
	}
	for _, part := range strings.Split(name, "-")[1:] {
		if part != "" && part[0] >= '0' && part[0] <= '9' {
			return part
		}
	}
	return "0"
}

func (s *FormatMetadataService) PyPIFiles(repoName, project string) ([]PypiFile, error) {
	items, err := s.pypiFiles(repoName, project)
	if err != nil {
		return nil, err
	}
	out := make([]PypiFile, 0, len(items))
	for _, item := range items {
		out = append(out, PypiFile{Project: item.NameNormalized, ProjectDisplay: item.NameDisplay, Version: item.Version, Filename: item.Filename, AssetPath: item.AssetPath, Sha256: item.Sha256, Size: item.Size, RequiresPython: item.RequiresPython, Yanked: item.Yanked})
	}
	return out, nil
}

func (s *FormatMetadataService) ResolvePyPI(ctx context.Context, repoName, project, filename string) (*repository.Asset, io.ReadCloser, error) {
	return s.ResolvePyPIWithAudit(ctx, repoName, project, filename, AssetOperationAudit{})
}

// ResolvePyPIWithAudit 解析 PyPI 包；代理缓存落盘时一并写入来源审计。
func (s *FormatMetadataService) ResolvePyPIWithAudit(ctx context.Context, repoName, project, filename string, audit AssetOperationAudit) (*repository.Asset, io.ReadCloser, error) {
	items, err := s.pypiFiles(repoName, project)
	if err != nil {
		return nil, nil, err
	}
	for _, item := range items {
		if item.Filename == filename {
			sourceRepo := repoName
			if item.SourceMember != "" {
				sourceRepo = item.SourceMember
			}
			if item.SourceKind == "proxy" {
				if err := s.cachePyPIProxyFile(ctx, sourceRepo, item, audit); err != nil {
					return nil, nil, err
				}
			}
			return s.assets.Get(sourceRepo, item.AssetPath)
		}
	}
	return nil, nil, fmt.Errorf("%w: PyPI 文件不存在", ErrNotFound)
}

func (s *FormatMetadataService) cachePyPIProxyFile(ctx context.Context, repoName string, item repository.FormatMetadata, audit AssetOperationAudit) error {
	repo, err := s.repo(repoName, "pypi", false)
	if err != nil {
		return err
	}
	if repo.Type != "proxy" {
		return ErrConflict
	}
	if _, rc, err := s.assets.Get(repoName, item.AssetPath); err == nil {
		_ = rc.Close()
		return nil
	} else if !errors.Is(err, ErrNotFound) {
		return err
	}
	_, cacheErr, _ := s.pypiSF.Do("package\x00"+repoName+"\x00"+item.NameNormalized+"\x00"+item.Filename, func() (any, error) {
		if _, rc, err := s.assets.Get(repoName, item.AssetPath); err == nil {
			_ = rc.Close()
			return nil, nil
		} else if !errors.Is(err, ErrNotFound) {
			return nil, err
		}
		stored, err := s.metadata.Get(repo.ID, "pypi", item.NameNormalized, item.Filename)
		if err != nil {
			return nil, err
		}
		sourceURL, err := pypiProxySourceURL(stored.MetadataJSON)
		if err != nil {
			return nil, err
		}
		session, _, err := s.pyPIUpstreamSession(repo, "")
		if err != nil {
			return nil, mapUpstreamErr(err)
		}
		body, header, err := session.ReadAbsolute(ctx, sourceURL)
		if err != nil {
			return nil, mapUpstreamErr(err)
		}
		defer func() { _ = body.Close() }()
		if contentLength, parseErr := strconv.ParseInt(header.Get("Content-Length"), 10, 64); parseErr == nil && contentLength > maxPyPIProxyPackageBytes {
			return nil, fmt.Errorf("%w: PyPI 上游文件超过大小限制", ErrUpstream)
		}
		reader := &pypiProxySizeReader{reader: body, remaining: maxPyPIProxyPackageBytes}
		asset, err := s.assets.StageBlob(reader, header.Get("Content-Type"))
		if err != nil {
			if errors.Is(err, errPyPIProxyPackageTooLarge) {
				return nil, fmt.Errorf("%w: PyPI 上游文件超过大小限制", ErrUpstream)
			}
			return nil, err
		}
		if asset.BlobHash != item.Sha256 {
			return nil, s.assets.cleanupFailedWrite(asset.BlobHash, fmt.Errorf("%w: PyPI 上游文件哈希不匹配", ErrUpstream))
		}
		asset.Path = item.AssetPath
		cached := *stored
		cached.Sha256, cached.Size, cached.AssetPath, cached.MetadataJSON = asset.BlobHash, asset.Size, asset.Path, "{}"
		if err := s.publishHostedFormatAsset(repoName, repo, asset, cached, audit); err != nil {
			return nil, s.assets.cleanupFailedWrite(asset.BlobHash, err)
		}
		return nil, nil
	})
	return cacheErr
}

func pypiProxySourceURL(raw string) (string, error) {
	var source pypiProxySource
	if err := json.Unmarshal([]byte(raw), &source); err != nil || source.URL == "" {
		return "", ErrUpstream
	}
	parsed, err := url.Parse(source.URL)
	if err != nil || parsed.User != nil || parsed.RawQuery != "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return "", ErrUpstream
	}
	return parsed.String(), nil
}

var errPyPIProxyPackageTooLarge = errors.New("PyPI 上游文件超过大小限制")

type pypiProxySizeReader struct {
	reader    io.Reader
	remaining int64
}

func (r *pypiProxySizeReader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		var probe [1]byte
		n, err := r.reader.Read(probe[:])
		if n > 0 {
			return 0, errPyPIProxyPackageTooLarge
		}
		return 0, err
	}
	if int64(len(p)) > r.remaining {
		p = p[:r.remaining]
	}
	n, err := r.reader.Read(p)
	r.remaining -= int64(n)
	return n, err
}

// NuGetPackage 描述一个可恢复的 NuGet 包版本。
type NuGetPackage struct {
	ID                string
	IDNormalized      string
	Version           string
	VersionNormalized string
	Filename          string
	AssetPath         string
	Sha256            string
	Size              int64
	MetadataJSON      string
	SourceKind        string
	SourceMember      string
}

func NormalizeNuGetID(id string) string           { return strings.ToLower(strings.TrimSpace(id)) }
func NormalizeNuGetVersion(version string) string { return strings.ToLower(strings.TrimSpace(version)) }

// PublishNuGet 写入一个已验证的 NuGet 元数据。包内容校验由协议层完成，服务负责资产与索引登记。
func (s *FormatMetadataService) PublishNuGet(repoName, id, version, filename, metadataJSON string, body io.Reader) (*NuGetPackage, error) {
	if err := s.requireBusinessWrite(); err != nil {
		return nil, err
	}
	r, err := s.repo(repoName, "nuget", true)
	if err != nil {
		return nil, err
	}
	idNorm, versionNorm := NormalizeNuGetID(id), NormalizeNuGetVersion(version)
	if idNorm == "" || versionNorm == "" || !validPackageFilename(filename) {
		return nil, ErrValidation
	}
	items, err := s.metadata.List(r.ID, "nuget", idNorm)
	if err != nil {
		return nil, err
	}
	for _, item := range items {
		if item.VersionNormalized == versionNorm {
			return nil, ErrConflict
		}
	}
	asset, err := s.assets.StageBlob(body, "application/octet-stream")
	if err != nil {
		return nil, err
	}
	asset.Path = "nuget/" + idNorm + "/" + versionNorm + "/" + filename
	meta := repository.FormatMetadata{RepositoryID: r.ID, Format: "nuget", NameNormalized: idNorm, NameDisplay: id, Version: version, VersionNormalized: versionNorm, Filename: filename, AssetPath: asset.Path, Sha256: asset.BlobHash, Size: asset.Size, MetadataJSON: metadataJSON, SourceKind: "hosted"}
	if err := s.publishHostedFormatAsset(repoName, r, asset, meta, AssetOperationAudit{}); err != nil {
		return nil, s.assets.cleanupFailedWrite(asset.BlobHash, err)
	}
	return &NuGetPackage{ID: id, IDNormalized: idNorm, Version: version, VersionNormalized: versionNorm, Filename: filename, AssetPath: asset.Path, Sha256: asset.BlobHash, Size: asset.Size, MetadataJSON: metadataJSON}, nil
}

func (s *FormatMetadataService) NuGetPackages(repoName, id string) ([]NuGetPackage, error) {
	r, err := s.repo(repoName, "nuget", false)
	if err != nil {
		return nil, err
	}
	if r.Type == "group" {
		cfg, e := r.DecodeConfig()
		if e != nil {
			return nil, e
		}
		seen := map[string]bool{}
		var out []NuGetPackage
		for _, member := range cfg.Members {
			items, e := s.NuGetPackages(member, id)
			if e != nil {
				continue
			}
			for _, item := range items {
				if !seen[item.VersionNormalized] {
					seen[item.VersionNormalized] = true
					item.SourceMember = member
					out = append(out, item)
				}
			}
		}
		sort.Slice(out, func(i, j int) bool { return out[i].VersionNormalized < out[j].VersionNormalized })
		return out, nil
	}
	items, err := s.metadata.List(r.ID, "nuget", NormalizeNuGetID(id))
	if err != nil {
		return nil, err
	}
	if r.Type == "proxy" && len(items) == 0 {
		loaded, loadErr, _ := s.nugetSF.Do("index\x00"+r.Name+"\x00"+NormalizeNuGetID(id), func() (any, error) {
			cached, cachedErr := s.metadata.List(r.ID, "nuget", NormalizeNuGetID(id))
			if cachedErr != nil || len(cached) > 0 {
				return cached, cachedErr
			}
			return s.loadNuGetProxy(r, id)
		})
		if loadErr != nil {
			return nil, loadErr
		}
		items = loaded.([]repository.FormatMetadata)
	}
	out := make([]NuGetPackage, 0, len(items))
	for _, item := range items {
		out = append(out, NuGetPackage{ID: item.NameDisplay, IDNormalized: item.NameNormalized, Version: item.Version, VersionNormalized: item.VersionNormalized, Filename: item.Filename, AssetPath: item.AssetPath, Sha256: item.Sha256, Size: item.Size, MetadataJSON: item.MetadataJSON, SourceKind: item.SourceKind, SourceMember: item.SourceMember})
	}
	return out, nil
}

// ResolveNuGet 解析一个 NuGet 包；proxy 缓存的包与其格式索引元数据必须同一 operation 复制。
func (s *FormatMetadataService) ResolveNuGet(ctx context.Context, repoName, id, version, filename string) (*repository.Asset, io.ReadCloser, error) {
	packages, err := s.NuGetPackages(repoName, id)
	if err != nil {
		return nil, nil, err
	}
	for _, item := range packages {
		if item.VersionNormalized != NormalizeNuGetVersion(version) || !strings.EqualFold(item.Filename, filename) {
			continue
		}
		sourceRepo := repoName
		if item.SourceMember != "" {
			sourceRepo = item.SourceMember
		}
		if item.SourceKind == "proxy" {
			if err := s.cacheNuGetProxyPackage(ctx, sourceRepo, item); err != nil {
				return nil, nil, err
			}
		}
		return s.assets.Get(sourceRepo, item.AssetPath)
	}
	return nil, nil, fmt.Errorf("%w: NuGet 包不存在", ErrNotFound)
}

func (s *FormatMetadataService) cacheNuGetProxyPackage(ctx context.Context, repoName string, item NuGetPackage) error {
	repo, err := s.repo(repoName, "nuget", false)
	if err != nil {
		return err
	}
	if repo.Type != "proxy" {
		return ErrConflict
	}
	if _, rc, err := s.assets.Get(repoName, item.AssetPath); err == nil {
		_ = rc.Close()
		return nil
	} else if !errors.Is(err, ErrNotFound) {
		return err
	}
	_, cacheErr, _ := s.nugetSF.Do("package\x00"+repoName+"\x00"+item.IDNormalized+"\x00"+item.Filename, func() (any, error) {
		if _, rc, err := s.assets.Get(repoName, item.AssetPath); err == nil {
			_ = rc.Close()
			return nil, nil
		} else if !errors.Is(err, ErrNotFound) {
			return nil, err
		}
		stored, err := s.metadata.Get(repo.ID, "nuget", item.IDNormalized, item.Filename)
		if err != nil {
			return nil, err
		}
		asset, metadataJSON, err := s.fetchNuGetProxyPackage(ctx, repo, item)
		if err != nil {
			return nil, err
		}
		cached := *stored
		cached.Sha256, cached.Size, cached.AssetPath, cached.MetadataJSON = asset.BlobHash, asset.Size, asset.Path, metadataJSON
		if err := s.publishHostedFormatAsset(repoName, repo, asset, cached, AssetOperationAudit{}); err != nil {
			return nil, s.assets.cleanupFailedWrite(asset.BlobHash, err)
		}
		return nil, nil
	})
	return cacheErr
}

func (s *FormatMetadataService) loadNuGetProxy(repo *repository.Repository, id string) ([]repository.FormatMetadata, error) {
	if err := s.requireBusinessWrite(); err != nil {
		return nil, err
	}
	idNorm := NormalizeNuGetID(id)
	if idNorm == "" {
		return nil, ErrValidation
	}
	indexPath := "v3-flatcontainer/" + idNorm + "/index.json"
	body, resources, err := s.loadNuGetProxyIndex(repo, indexPath)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Versions []string `json:"versions"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, ErrValidation
	}
	dependencies, err := s.loadNuGetProxyRegistration(repo, resources.registration, idNorm)
	if err != nil {
		return nil, err
	}
	result := make([]repository.FormatMetadata, 0, len(doc.Versions))
	for _, version := range doc.Versions {
		versionNorm := NormalizeNuGetVersion(version)
		if versionNorm == "" {
			continue
		}
		filename := idNorm + "." + versionNorm + ".nupkg"
		metadataJSON := []byte("{}")
		if resources.packageBase != nil {
			packageURL, urlErr := nugetResourceURL(resources.packageBase, idNorm, versionNorm, filename)
			if urlErr != nil {
				return nil, ErrUpstream
			}
			metadataJSON, err = json.Marshal(nugetProxyMetadata{DependencyGroups: dependencies[versionNorm], Source: nugetProxySource{PackageURL: packageURL}})
			if err != nil {
				return nil, err
			}
		}
		item := repository.FormatMetadata{
			RepositoryID: repo.ID, Format: "nuget", NameNormalized: idNorm, NameDisplay: id,
			Version: version, VersionNormalized: versionNorm, Filename: filename,
			AssetPath:    "v3-flatcontainer/" + idNorm + "/" + versionNorm + "/" + filename,
			MetadataJSON: string(metadataJSON), SourceKind: "proxy",
		}
		result = append(result, item)
	}
	if err := s.metadata.PutAll(result); err != nil {
		return nil, err
	}
	return result, nil
}

const maxNuGetProxyDocumentBytes = 4 << 20

type nugetProxyResources struct {
	packageBase  *url.URL
	registration *url.URL
}

type nugetProxySource struct {
	PackageURL string `json:"packageUrl"`
}

type nugetProxyMetadata struct {
	DependencyGroups []nugetpackage.DependencyGroup `json:"dependencyGroups,omitempty"`
	Source           nugetProxySource               `json:"source"`
}

// loadNuGetProxyIndex 优先读取历史本地 flat 索引；未缓存时按 V3 service index 发现资源。
func (s *FormatMetadataService) loadNuGetProxyIndex(repo *repository.Repository, indexPath string) ([]byte, nugetProxyResources, error) {
	if _, cached, err := s.assets.Get(repo.Name, indexPath); err == nil {
		defer func() { _ = cached.Close() }()
		body, readErr := io.ReadAll(io.LimitReader(cached, maxNuGetProxyDocumentBytes+1))
		if readErr != nil || len(body) > maxNuGetProxyDocumentBytes {
			return nil, nugetProxyResources{}, ErrUpstream
		}
		return body, nugetProxyResources{}, nil
	} else if !errors.Is(err, ErrNotFound) {
		return nil, nugetProxyResources{}, err
	}
	resources, err := s.discoverNuGetProxyResources(repo)
	if err != nil {
		_, legacy, legacyErr := s.assets.Resolve(context.Background(), repo.Name, indexPath)
		if legacyErr != nil {
			return nil, nugetProxyResources{}, err
		}
		defer func() { _ = legacy.Close() }()
		body, readErr := io.ReadAll(io.LimitReader(legacy, maxNuGetProxyDocumentBytes+1))
		if readErr != nil || len(body) > maxNuGetProxyDocumentBytes {
			return nil, nugetProxyResources{}, ErrUpstream
		}
		return body, nugetProxyResources{}, nil
	}
	parts := strings.Split(indexPath, "/")
	if len(parts) != 3 || parts[0] != "v3-flatcontainer" || parts[2] != "index.json" {
		return nil, nugetProxyResources{}, ErrUpstream
	}
	indexURL, err := nugetResourceURL(resources.packageBase, parts[1], "index.json")
	if err != nil {
		return nil, nugetProxyResources{}, ErrUpstream
	}
	body, err := s.readNuGetProxyDocument(repo, indexURL)
	if err != nil {
		return nil, nugetProxyResources{}, err
	}
	return body, resources, nil
}

func (s *FormatMetadataService) discoverNuGetProxyResources(repo *repository.Repository) (nugetProxyResources, error) {
	session, indexURL, err := s.nugetProxySession(repo)
	if err != nil {
		return nugetProxyResources{}, mapUpstreamErr(err)
	}
	body, err := readNuGetDocument(session, indexURL.String())
	if err != nil {
		return nugetProxyResources{}, mapUpstreamErr(err)
	}
	var index struct {
		Resources []struct {
			ID   string `json:"@id"`
			Type string `json:"@type"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(body, &index); err != nil {
		return nugetProxyResources{}, ErrUpstream
	}
	resources := nugetProxyResources{}
	for _, resource := range index.Resources {
		resolved, resolveErr := nugetResolveResource(indexURL, resource.ID)
		if resolveErr != nil {
			return nugetProxyResources{}, ErrUpstream
		}
		switch {
		case strings.HasPrefix(strings.ToLower(resource.Type), "packagebaseaddress/"):
			resources.packageBase = resolved
		case strings.HasPrefix(strings.ToLower(resource.Type), "registrationsbaseurl/"):
			resources.registration = resolved
		}
	}
	if resources.packageBase == nil || resources.registration == nil {
		return nugetProxyResources{}, ErrUpstream
	}
	return resources, nil
}

func (s *FormatMetadataService) loadNuGetProxyRegistration(repo *repository.Repository, registration *url.URL, id string) (map[string][]nugetpackage.DependencyGroup, error) {
	if registration == nil {
		return map[string][]nugetpackage.DependencyGroup{}, nil
	}
	registrationURL, err := nugetResourceURL(registration, id, "index.json")
	if err != nil {
		return nil, ErrUpstream
	}
	body, err := s.readNuGetProxyDocument(repo, registrationURL)
	if err != nil {
		return nil, err
	}
	return parseNuGetRegistration(body), nil
}

func (s *FormatMetadataService) readNuGetProxyDocument(repo *repository.Repository, sourceURL string) ([]byte, error) {
	session, _, err := s.nugetProxySession(repo)
	if err != nil {
		return nil, mapUpstreamErr(err)
	}
	return readNuGetDocument(session, sourceURL)
}

func (s *FormatMetadataService) nugetProxySession(repo *repository.Repository) (*upstream.Session, *url.URL, error) {
	if s.upstream == nil {
		return nil, nil, ErrUpstream
	}
	cfg, err := repo.DecodeConfig()
	if err != nil || cfg.RemoteURL == "" {
		return nil, nil, ErrUpstream
	}
	indexURL, err := url.Parse(cfg.RemoteURL)
	if err != nil || indexURL.User != nil || indexURL.RawQuery != "" || indexURL.Fragment != "" {
		return nil, nil, ErrUpstream
	}
	session, err := s.upstream.NewSession(cfg.RemoteURL, cfg.CredentialRef)
	if err != nil {
		return nil, nil, err
	}
	return session, indexURL, nil
}

func (s *FormatMetadataService) fetchNuGetProxyPackage(ctx context.Context, repo *repository.Repository, item NuGetPackage) (*repository.Asset, string, error) {
	sourceURL, err := nugetProxySourceURL(item.MetadataJSON)
	if err != nil {
		asset, fallbackErr := s.assets.fetchProxyAsset(ctx, repo, item.AssetPath)
		return asset, item.MetadataJSON, fallbackErr
	}
	session, _, err := s.nugetProxySession(repo)
	if err != nil {
		return nil, "", mapUpstreamErr(err)
	}
	body, header, err := session.ReadAbsolute(ctx, sourceURL)
	if err != nil {
		return nil, "", mapUpstreamErr(err)
	}
	defer func() { _ = body.Close() }()
	if contentLength, parseErr := strconv.ParseInt(header.Get("Content-Length"), 10, 64); parseErr == nil && contentLength > 128<<20 {
		return nil, "", fmt.Errorf("%w: NuGet 上游包超过大小限制", ErrUpstream)
	}
	asset, err := s.assets.StageBlob(io.LimitReader(body, (128<<20)+1), header.Get("Content-Type"))
	if err != nil {
		return nil, "", err
	}
	if asset.Size > 128<<20 {
		return nil, "", s.assets.cleanupFailedWrite(asset.BlobHash, fmt.Errorf("%w: NuGet 上游包超过大小限制", ErrUpstream))
	}
	file, _, err := s.assets.blobs.Open(asset.BlobHash)
	if err != nil {
		return nil, "", s.assets.cleanupFailedWrite(asset.BlobHash, err)
	}
	readerAt, ok := file.(io.ReaderAt)
	if !ok {
		_ = file.Close()
		return nil, "", s.assets.cleanupFailedWrite(asset.BlobHash, ErrUpstream)
	}
	id, version, metadataJSON, parseErr := nugetpackage.Parse(readerAt, asset.Size)
	_ = file.Close()
	if parseErr != nil || NormalizeNuGetID(id) != item.IDNormalized || NormalizeNuGetVersion(version) != item.VersionNormalized {
		return nil, "", s.assets.cleanupFailedWrite(asset.BlobHash, ErrUpstream)
	}
	asset.Path = item.AssetPath
	return asset, metadataJSON, nil
}

func readNuGetDocument(session *upstream.Session, sourceURL string) ([]byte, error) {
	body, _, err := session.ReadAbsolute(context.Background(), sourceURL)
	if err != nil {
		return nil, err
	}
	defer func() { _ = body.Close() }()
	data, readErr := io.ReadAll(io.LimitReader(body, maxNuGetProxyDocumentBytes+1))
	if readErr != nil || len(data) > maxNuGetProxyDocumentBytes {
		return nil, ErrUpstream
	}
	return data, nil
}

func nugetResolveResource(base *url.URL, raw string) (*url.URL, error) {
	reference, err := url.Parse(raw)
	if err != nil || base == nil {
		return nil, ErrUpstream
	}
	resolved := base.ResolveReference(reference)
	if resolved.Scheme != "http" && resolved.Scheme != "https" || resolved.Host == "" || resolved.User != nil || resolved.RawQuery != "" || resolved.Fragment != "" {
		return nil, ErrUpstream
	}
	return resolved, nil
}

func nugetResourceURL(base *url.URL, segments ...string) (string, error) {
	if base == nil {
		return "", ErrUpstream
	}
	encoded := make([]string, 0, len(segments))
	for _, segment := range segments {
		if segment == "" || strings.ContainsAny(segment, `/\\?#`) {
			return "", ErrUpstream
		}
		encoded = append(encoded, url.PathEscape(segment))
	}
	target := *base
	target.Path = strings.TrimRight(target.Path, "/") + "/" + strings.Join(encoded, "/")
	target.RawPath = ""
	return target.String(), nil
}

func parseNuGetRegistration(data []byte) map[string][]nugetpackage.DependencyGroup {
	var document struct {
		Items []json.RawMessage `json:"items"`
	}
	if json.Unmarshal(data, &document) != nil {
		return map[string][]nugetpackage.DependencyGroup{}
	}
	result := make(map[string][]nugetpackage.DependencyGroup)
	var visit func(json.RawMessage)
	visit = func(raw json.RawMessage) {
		var node struct {
			Items        []json.RawMessage `json:"items"`
			CatalogEntry json.RawMessage   `json:"catalogEntry"`
		}
		if json.Unmarshal(raw, &node) != nil {
			return
		}
		if len(node.CatalogEntry) > 0 {
			var entry struct {
				Version          string                         `json:"version"`
				DependencyGroups []nugetpackage.DependencyGroup `json:"dependencyGroups"`
			}
			if json.Unmarshal(node.CatalogEntry, &entry) == nil && NormalizeNuGetVersion(entry.Version) != "" {
				result[NormalizeNuGetVersion(entry.Version)] = entry.DependencyGroups
			}
		}
		for _, child := range node.Items {
			visit(child)
		}
	}
	for _, item := range document.Items {
		visit(item)
	}
	return result
}

func nugetProxySourceURL(raw string) (string, error) {
	var metadata struct {
		Source nugetProxySource `json:"source"`
	}
	if json.Unmarshal([]byte(raw), &metadata) != nil || metadata.Source.PackageURL == "" {
		return "", ErrUpstream
	}
	parsed, err := url.Parse(metadata.Source.PackageURL)
	if err != nil || parsed.Scheme != "http" && parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", ErrUpstream
	}
	return parsed.String(), nil
}
