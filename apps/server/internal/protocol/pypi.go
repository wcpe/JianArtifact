package protocol

import (
	"encoding/json"
	"errors"
	"html"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
)

// PypiHandler 提供 PyPI PEP 503/691 Simple 与 Twine legacy 上传端点。
type PypiHandler struct {
	*RawHandler
	metadata  *domain.FormatMetadataService
	publicURL string
}

const (
	maxPyPIUploadBytes = 128 << 20
	maxPyPIFieldBytes  = 64 << 10
)

var (
	errPyPIFieldTooLarge = errors.New("PyPI 字段过大")
	errPyPIMultipleFiles = errors.New("PyPI 只能上传一个文件")
	errPyPIFileMissing   = errors.New("PyPI 缺少发布文件")
)

func NewPypiHandler(raw *RawHandler, metadata *domain.FormatMetadataService, publicURL string) *PypiHandler {
	return &PypiHandler{RawHandler: raw, metadata: metadata, publicURL: strings.TrimRight(publicURL, "/")}
}

func (h *PypiHandler) SetPublicURL(publicURL string) { h.publicURL = strings.TrimRight(publicURL, "/") }

// RegisterPypiRoutes 注册 PyPI 协议路径。simple 根与项目页均保留尾斜杠语义。
func RegisterPypiRoutes(r gin.IRouter, h *PypiHandler, mw ...gin.HandlerFunc) {
	g := r.Group("/pypi", mw...)
	g.GET("/:repo/simple/*rest", h.Simple)
	g.HEAD("/:repo/simple/*rest", h.Simple)
	g.GET("/:repo/packages/*rest", h.Package)
	g.HEAD("/:repo/packages/*rest", h.Package)
	g.POST("/:repo/legacy/", h.LegacyUpload)
	g.POST("/:repo/legacy", h.LegacyUpload)
}

func (h *PypiHandler) Simple(c *gin.Context) {
	repo := c.Param("repo")
	if !h.authorize(c, repo, "read") {
		return
	}
	rest := cleanArtifactPath(c.Param("rest"))
	if rest == "" {
		h.simpleRoot(c, repo)
		return
	}
	project := strings.Trim(rest, "/")
	normalized := domain.NormalizePyPIProject(project)
	if normalized == "" {
		auth.WriteError(c, http.StatusBadRequest, "invalid_path", "项目名不能为空")
		return
	}
	if project != normalized || !strings.HasSuffix(c.Request.URL.Path, "/") {
		location := h.baseURL(c) + "/pypi/" + url.PathEscape(repo) + "/simple/" + url.PathEscape(normalized) + "/"
		c.Redirect(http.StatusMovedPermanently, location)
		return
	}
	files, err := h.metadata.PyPIFiles(repo, normalized)
	if err != nil {
		writeAssetErr(c, err)
		return
	}
	if wantsPypiJSON(c) {
		out := map[string]any{"meta": map[string]any{"api-version": "1.0"}, "name": normalized, "files": h.pypiJSONFiles(c, repo, normalized, files)}
		writeJSONHead(c, http.StatusOK, out, "application/vnd.pypi.simple.v1+json")
		return
	}
	var b strings.Builder
	b.WriteString("<!DOCTYPE html><html><body>\n")
	for _, file := range files {
		b.WriteString(`<a href="` + html.EscapeString(h.packageURL(c, repo, normalized, file.Filename)+"#sha256="+file.Sha256) + `"`)
		if file.RequiresPython != "" {
			b.WriteString(` data-requires-python="` + html.EscapeString(file.RequiresPython) + `"`)
		}
		if file.Yanked != "" {
			b.WriteString(` data-yanked="` + html.EscapeString(file.Yanked) + `"`)
		}
		b.WriteString(">" + html.EscapeString(file.Filename) + "</a>\n")
	}
	b.WriteString("</body></html>\n")
	writeDataHead(c, http.StatusOK, "text/html; charset=UTF-8", []byte(b.String()))
}

func (h *PypiHandler) simpleRoot(c *gin.Context, repo string) {
	projects, err := h.metadata.PyPIProjects(repo)
	if err != nil {
		writeAssetErr(c, err)
		return
	}
	var b strings.Builder
	b.WriteString("<!DOCTYPE html><html><body>\n")
	for _, project := range projects {
		b.WriteString(`<a href="` + html.EscapeString(h.baseURL(c)+"/pypi/"+url.PathEscape(repo)+"/simple/"+url.PathEscape(project)+"/") + `">` + html.EscapeString(project) + "</a>\n")
	}
	b.WriteString("</body></html>\n")
	writeDataHead(c, http.StatusOK, "text/html; charset=UTF-8", []byte(b.String()))
}

func (h *PypiHandler) pypiJSONFiles(c *gin.Context, repo, project string, files []domain.PypiFile) []map[string]any {
	out := make([]map[string]any, 0, len(files))
	for _, file := range files {
		entry := map[string]any{"filename": file.Filename, "url": h.packageURL(c, repo, project, file.Filename), "hashes": map[string]string{"sha256": file.Sha256}}
		if file.RequiresPython != "" {
			entry["requires-python"] = file.RequiresPython
		}
		if file.Yanked != "" {
			entry["yanked"] = file.Yanked
		}
		out = append(out, entry)
	}
	return out
}

func wantsPypiJSON(c *gin.Context) bool {
	return strings.Contains(strings.ToLower(c.GetHeader("Accept")), "application/vnd.pypi.simple.v1+json")
}

func (h *PypiHandler) Package(c *gin.Context) {
	repo := c.Param("repo")
	if !h.authorize(c, repo, "read") {
		return
	}
	parts := strings.Split(strings.Trim(cleanArtifactPath(c.Param("rest")), "/"), "/")
	if len(parts) != 2 || parts[0] == "" || !validPypiFilename(parts[1]) {
		auth.WriteError(c, http.StatusBadRequest, "invalid_path", "PyPI 文件路径非法")
		return
	}
	asset, rc, err := h.metadata.ResolvePyPIWithAudit(c.Request.Context(), repo, parts[0], parts[1], h.nativeOperationAudit(c, "pypi.proxy_cache", repo))
	if err != nil {
		writeAssetErr(c, err)
		return
	}
	defer func() { _ = rc.Close() }()
	writeArtifact(c, asset.ContentType, asset.Size, asset.BlobHash, rc)
}

func validPypiFilename(name string) bool {
	return name != "" && !strings.ContainsAny(name, `/\\`) && name != "." && name != ".."
}

// LegacyUpload 解析 Twine 的 multipart 上传；文件正文先流式暂存，拒绝超大表单。
func (h *PypiHandler) LegacyUpload(c *gin.Context) {
	repo := c.Param("repo")
	if !h.authorize(c, repo, "write") {
		h.auditRejected(c, "pypi.publish", repo, "", "authorization_denied")
		return
	}
	if _, err := h.repoSvc.Get(repo); err != nil {
		writeAssetErr(c, err)
		return
	}
	settle, limit, err := h.beginUnresolvedPublish(c, repo)
	if err != nil {
		h.auditRejected(c, "pypi.publish", repo, "", publishRejectionDetail(err))
		writePublishErr(c, err)
		return
	}
	defer func() { settle(false, 0) }()
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxPyPIUploadBytes)
	mr, err := c.Request.MultipartReader()
	if err != nil {
		auth.WriteError(c, http.StatusBadRequest, "invalid_body", "必须使用 multipart/form-data")
		return
	}
	upload, err := readPyPIUpload(mr, limit)
	if err != nil {
		if errors.Is(err, domain.ErrQuotaExceeded) {
			h.auditRejected(c, "pypi.publish", repo, "", "quota_exceeded")
			writePublishErr(c, err)
			return
		}
		auth.WriteError(c, http.StatusBadRequest, "invalid_body", "读取上传内容失败")
		return
	}
	defer upload.Close()
	if upload.filename == "" {
		upload.filename = upload.fields["filename"]
	}
	project, version := upload.fields["name"], upload.fields["version"]
	artifactPath := "pypi/packages/" + domain.NormalizePyPIProject(project) + "/" + upload.filename
	if err := h.validateUnresolvedPublish(c, repo, artifactPath); err != nil {
		h.auditRejected(c, "pypi.publish", repo, artifactPath, publishRejectionDetail(err))
		writePublishErr(c, err)
		return
	}
	if _, err := h.metadata.PublishPyPIWithAudit(repo, project, version, upload.filename, upload.fields["requires_python"], upload.fields["yanked"], upload.file, h.nativeOperationAudit(c, "pypi.publish", repo)); err != nil {
		h.auditRejected(c, "pypi.publish", repo, artifactPath, publishRejectionDetail(err))
		writePublishErr(c, err)
		return
	}
	settle(true, upload.size)
	c.Header("Content-Type", "text/plain; charset=utf-8")
	c.String(http.StatusOK, "OK")
}

type pypiUpload struct {
	fields   map[string]string
	filename string
	file     *os.File
	size     int64
}

func readPyPIUpload(mr *multipart.Reader, limit int64) (*pypiUpload, error) {
	upload := &pypiUpload{fields: map[string]string{}}
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			upload.Close()
			return nil, err
		}
		name := part.FormName()
		if part.FileName() != "" || name == "content" {
			if upload.file != nil {
				_ = part.Close()
				upload.Close()
				return nil, errPyPIMultipleFiles
			}
			upload.filename = part.FileName()
			upload.file, upload.size, err = spoolPyPIFile(part, limit)
			_ = part.Close()
			if err != nil {
				upload.Close()
				return nil, err
			}
			continue
		}
		value, readErr := readPyPIField(part)
		_ = part.Close()
		if readErr != nil {
			upload.Close()
			return nil, readErr
		}
		upload.fields[name] = value
	}
	if upload.file == nil {
		return nil, errPyPIFileMissing
	}
	return upload, nil
}

func readPyPIField(r io.Reader) (string, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxPyPIFieldBytes+1))
	if err != nil {
		return "", err
	}
	if len(data) > maxPyPIFieldBytes {
		return "", errPyPIFieldTooLarge
	}
	return string(data), nil
}

func spoolPyPIFile(r io.Reader, limit int64) (file *os.File, size int64, err error) {
	file, err = os.CreateTemp("", "jianartifact-pypi-*")
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
	if size, err = io.Copy(file, r); err != nil {
		return nil, 0, err
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		return nil, 0, err
	}
	return file, size, nil
}

func (u *pypiUpload) Close() {
	if u.file == nil {
		return
	}
	_ = u.file.Close()
	_ = os.Remove(u.file.Name())
}

func (h *PypiHandler) baseURL(c *gin.Context) string {
	if h.publicURL != "" {
		return h.publicURL
	}
	scheme := "http"
	if c.Request.TLS != nil || strings.EqualFold(c.GetHeader("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return scheme + "://" + c.Request.Host
}

func (h *PypiHandler) packageURL(c *gin.Context, repo, project, filename string) string {
	return h.baseURL(c) + "/pypi/" + url.PathEscape(repo) + "/packages/" + url.PathEscape(project) + "/" + url.PathEscape(filename)
}

func writeDataHead(c *gin.Context, status int, contentType string, data []byte) {
	c.Header("Content-Type", contentType)
	c.Header("Content-Length", strconv.Itoa(len(data)))
	if c.Request.Method == http.MethodHead {
		c.Status(status)
		return
	}
	c.Data(status, contentType, data)
}

func writeJSONHead(c *gin.Context, status int, v any, contentType string) {
	b, err := json.Marshal(v)
	if err != nil {
		auth.WriteError(c, http.StatusInternalServerError, "internal", "序列化响应失败")
		return
	}
	writeDataHead(c, status, contentType, b)
}
