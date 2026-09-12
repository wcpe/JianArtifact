package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/nugetpackage"
)

// NuGetHandler 提供 NuGet V3 service index、flat container、registration 与 hosted push。
type NuGetHandler struct {
	*RawHandler
	metadata  *domain.FormatMetadataService
	publicURL string
}

func NewNuGetHandler(raw *RawHandler, metadata *domain.FormatMetadataService, publicURL string) *NuGetHandler {
	return &NuGetHandler{RawHandler: raw, metadata: metadata, publicURL: strings.TrimRight(publicURL, "/")}
}

func RegisterNuGetRoutes(r gin.IRouter, h *NuGetHandler, mw ...gin.HandlerFunc) {
	g := r.Group("/nuget", mw...)
	g.GET("/:repo/*rest", h.Get)
	g.HEAD("/:repo/*rest", h.Get)
	g.PUT("/:repo/api/v2/package", h.Push)
}

func (h *NuGetHandler) Get(c *gin.Context) {
	repo := c.Param("repo")
	if !h.authorize(c, repo, "read") {
		return
	}
	repoPath := strings.Trim(cleanArtifactPath(c.Param("rest")), "/")
	switch {
	case repoPath == "v3/index.json":
		h.serviceIndex(c, repo)
	case strings.HasPrefix(repoPath, "v3-flatcontainer/"):
		h.flatContainer(c, repo, strings.TrimPrefix(repoPath, "v3-flatcontainer/"))
	case strings.HasPrefix(repoPath, "v3-registration5-gz-semver2/"):
		h.registration(c, repo, strings.TrimPrefix(repoPath, "v3-registration5-gz-semver2/"))
	case strings.HasPrefix(repoPath, "query"):
		h.search(c, repo)
	default:
		auth.WriteError(c, http.StatusNotFound, "not_found", "NuGet 资源不存在")
	}
}

func (h *NuGetHandler) serviceIndex(c *gin.Context, repo string) {
	base := h.baseURL(c) + "/nuget/" + url.PathEscape(repo)
	resources := []map[string]string{
		{"@id": base + "/v3-flatcontainer/", "@type": "PackageBaseAddress/3.0.0"},
		{"@id": base + "/v3-registration5-gz-semver2/", "@type": "RegistrationsBaseUrl/3.6.0"},
		{"@id": base + "/query", "@type": "SearchQueryService/3.0.0-rc"},
	}
	r, err := h.repoSvc.Get(repo)
	if err != nil {
		writeAssetErr(c, err)
		return
	}
	if r.Type == "hosted" {
		resources = append(resources, map[string]string{"@id": base + "/api/v2/package", "@type": "PackagePublish/2.0.0"})
	}
	writeJSONHead(c, http.StatusOK, map[string]any{"version": "3.0.0", "resources": resources}, "application/json")
}

func (h *NuGetHandler) flatContainer(c *gin.Context, repo, rest string) {
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) < 2 || parts[0] == "" {
		auth.WriteError(c, http.StatusBadRequest, "invalid_path", "NuGet 包路径非法")
		return
	}
	id := parts[0]
	packages, err := h.metadata.NuGetPackages(repo, id)
	if err != nil {
		writeAssetErr(c, err)
		return
	}
	if len(packages) == 0 {
		writeJSONHead(c, http.StatusOK, map[string]any{"totalHits": 0, "data": []any{}}, "application/json")
		return
	}
	if len(parts) == 2 && parts[1] == "index.json" {
		versions := make([]string, 0, len(packages))
		for _, pkg := range packages {
			versions = append(versions, pkg.VersionNormalized)
		}
		writeJSONHead(c, http.StatusOK, map[string]any{"versions": versions}, "application/json")
		return
	}
	if len(parts) != 3 {
		auth.WriteError(c, http.StatusBadRequest, "invalid_path", "NuGet 包路径非法")
		return
	}
	version, filename := parts[1], parts[2]
	for _, pkg := range packages {
		if pkg.VersionNormalized == strings.ToLower(version) && strings.EqualFold(pkg.Filename, filename) {
			asset, rc, e := h.metadata.ResolveNuGet(c.Request.Context(), repo, id, version, filename)
			if e != nil {
				writeAssetErr(c, e)
				return
			}
			defer func() { _ = rc.Close() }()
			writeArtifact(c, asset.ContentType, asset.Size, asset.BlobHash, rc)
			return
		}
	}
	auth.WriteError(c, http.StatusNotFound, "not_found", "NuGet 包不存在")
}

func (h *NuGetHandler) registration(c *gin.Context, repo, rest string) {
	id := strings.TrimSuffix(strings.Trim(rest, "/"), "/index.json")
	if id == "" {
		auth.WriteError(c, http.StatusBadRequest, "invalid_path", "NuGet 包 ID 不能为空")
		return
	}
	packages, err := h.metadata.NuGetPackages(repo, id)
	if err != nil {
		writeAssetErr(c, err)
		return
	}
	items := make([]map[string]any, 0, len(packages))
	base := h.baseURL(c) + "/nuget/" + url.PathEscape(repo)
	for _, pkg := range packages {
		packageContent := base + "/v3-flatcontainer/" + url.PathEscape(pkg.IDNormalized) + "/" + url.PathEscape(pkg.VersionNormalized) + "/" + url.PathEscape(pkg.Filename)
		catalogEntry := map[string]any{"id": pkg.ID, "version": pkg.Version, "listed": true, "packageContent": packageContent}
		var metadata nugetpackage.Metadata
		if json.Unmarshal([]byte(pkg.MetadataJSON), &metadata) == nil && len(metadata.DependencyGroups) > 0 {
			catalogEntry["dependencyGroups"] = metadata.DependencyGroups
		}
		items = append(items, map[string]any{
			"catalogEntry":   catalogEntry,
			"packageContent": packageContent,
		})
	}
	writeJSONHead(c, http.StatusOK, map[string]any{"count": len(items), "items": items}, "application/json")
}

func (h *NuGetHandler) search(c *gin.Context, repo string) {
	id := c.Query("q")
	if id == "" {
		id = c.Query("id")
	}
	if id == "" {
		writeJSONHead(c, http.StatusOK, map[string]any{"totalHits": 0, "data": []any{}}, "application/json")
		return
	}
	packages, err := h.metadata.NuGetPackages(repo, id)
	if err != nil {
		writeAssetErr(c, err)
		return
	}
	if len(packages) == 0 {
		writeJSONHead(c, http.StatusOK, map[string]any{"totalHits": 0, "data": []any{}}, "application/json")
		return
	}
	versions := make([]string, 0, len(packages))
	for _, pkg := range packages {
		versions = append(versions, pkg.Version)
	}
	writeJSONHead(c, http.StatusOK, map[string]any{"totalHits": len(packages), "data": []any{map[string]any{"id": id, "version": versions[len(versions)-1], "versions": versions}}}, "application/json")
}

func (h *NuGetHandler) Push(c *gin.Context) {
	repo := c.Param("repo")
	if !h.authorize(c, repo, "write") {
		h.auditRejected(c, "nuget.publish", repo, "", "authorization_denied")
		return
	}
	if r, err := h.repoSvc.Get(repo); err != nil {
		writeAssetErr(c, err)
		return
	} else if r.Format != "nuget" || r.Type != "hosted" {
		writeAssetErr(c, domain.ErrConflict)
		return
	}
	settle, limit, err := h.beginUnresolvedPublish(c, repo)
	if err != nil {
		h.auditRejected(c, "nuget.publish", repo, "", publishRejectionDetail(err))
		writePublishErr(c, err)
		return
	}
	defer func() { settle(false, 0) }()
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 128<<20)
	packageFile, packageSize, err := spoolNuGetPackage(c, limit)
	if err != nil {
		if errors.Is(err, domain.ErrQuotaExceeded) {
			h.auditRejected(c, "nuget.publish", repo, "", "quota_exceeded")
			writePublishErr(c, err)
			return
		}
		auth.WriteError(c, http.StatusBadRequest, "invalid_body", "读取 nupkg 失败")
		return
	}
	defer func() {
		_ = packageFile.Close()
		_ = os.Remove(packageFile.Name())
	}()
	id, version, metadataJSON, err := parseNuPkgReaderAt(packageFile, packageSize)
	if err != nil {
		auth.WriteError(c, http.StatusBadRequest, "invalid_package", err.Error())
		return
	}
	filename := strings.ToLower(id) + "." + strings.ToLower(version) + ".nupkg"
	artifactPath := "nuget/" + strings.ToLower(id) + "/" + strings.ToLower(version) + "/" + filename
	if err := h.validateUnresolvedPublish(c, repo, artifactPath); err != nil {
		h.auditRejected(c, "nuget.publish", repo, artifactPath, publishRejectionDetail(err))
		writePublishErr(c, err)
		return
	}
	if _, err := packageFile.Seek(0, io.SeekStart); err != nil {
		auth.WriteError(c, http.StatusInternalServerError, "internal", "读取 nupkg 失败")
		return
	}
	if _, err := h.metadata.PublishNuGet(repo, id, version, filename, metadataJSON, packageFile); err != nil {
		h.auditRejected(c, "nuget.publish", repo, artifactPath, publishRejectionDetail(err))
		writePublishErr(c, err)
		return
	}
	settle(true, packageSize)
	h.auditPublish(c, "nuget.publish", repo, artifactPath, packageSize)
	c.Status(http.StatusCreated)
}

func spoolNuGetPackage(c *gin.Context, limit int64) (*os.File, int64, error) {
	if !strings.HasPrefix(strings.ToLower(c.GetHeader("Content-Type")), "multipart/") {
		return spoolNuGetReader(c.Request.Body, limit)
	}
	mr, err := c.Request.MultipartReader()
	if err != nil {
		return nil, 0, err
	}
	for {
		part, nextErr := mr.NextPart()
		if nextErr == io.EOF {
			break
		}
		if nextErr != nil {
			return nil, 0, nextErr
		}
		if part.FileName() != "" || part.FormName() == "package" {
			file, size, readErr := spoolNuGetReader(part, limit)
			closeErr := part.Close()
			if readErr != nil {
				return nil, 0, readErr
			}
			if closeErr != nil {
				_ = file.Close()
				_ = os.Remove(file.Name())
				return nil, 0, closeErr
			}
			return file, size, nil
		}
		_ = part.Close()
	}
	return nil, 0, io.ErrUnexpectedEOF
}

// spoolNuGetReader 将待校验包写入临时文件，避免协议层聚合完整 nupkg 到内存。
func spoolNuGetReader(r io.Reader, limit int64) (file *os.File, size int64, err error) {
	file, err = os.CreateTemp("", "jianartifact-nupkg-*")
	if err != nil {
		return nil, 0, err
	}
	tempFile, name := file, file.Name()
	defer func() {
		if err != nil {
			_ = tempFile.Close()
			_ = os.Remove(name)
		}
	}()
	if limit > 0 {
		r = &quotaReader{r: r, left: limit, limit: limit}
	}
	size, err = io.Copy(file, r)
	if err != nil {
		return nil, 0, err
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		return nil, 0, err
	}
	return file, size, nil
}

func parseNuPkg(data []byte) (id, version, metadata string, err error) {
	if len(data) == 0 {
		return "", "", "", io.ErrUnexpectedEOF
	}
	return parseNuPkgReaderAt(bytes.NewReader(data), int64(len(data)))
}

func parseNuPkgReaderAt(source io.ReaderAt, size int64) (id, version, metadata string, err error) {
	return nugetpackage.Parse(source, size)
}

func (h *NuGetHandler) baseURL(c *gin.Context) string {
	if h.publicURL != "" {
		return h.publicURL
	}
	scheme := "http"
	if c.Request.TLS != nil || strings.EqualFold(c.GetHeader("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return scheme + "://" + c.Request.Host
}
