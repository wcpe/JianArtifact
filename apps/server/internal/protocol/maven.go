// Package protocol：Maven 格式端点。见 doc.go 分层说明。
//
// Maven 复用 Raw 的 URL 形态（/repository/:repo/*artifactPath）与内容寻址存储，
// 由 Dispatcher 按仓库 format 分派：制品字节的发布/拉取/删除完全复用 RawHandler；
// MavenHandler 仅覆盖 GET 以补两点 Maven 语义：
//   - 校验和文件（.md5/.sha1/.sha256）缺失时据底层制品字节现算返回；
//   - group 仓库的 maven-metadata.xml 合并各成员的 versions/latest/release（时间戳重算）。
//
// SNAPSHOT 唯一时间戳版本按路径原样存取，不做额外语义解析（边界见 spec）。
package protocol

import (
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"hash"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// Dispatcher 按仓库 format 将 /repository/:repo/*artifactPath 分派到对应格式处理器：
// maven→MavenHandler，其余（含 raw、未知类型与不存在的仓库）→ RawHandler，
// 仓库不存在等由目标处理器内 authorize 统一产出 401/404。
type Dispatcher struct {
	repoSvc *domain.RepositoryService
	raw     *RawHandler
	maven   *MavenHandler
}

// NewDispatcher 构造 Dispatcher。
func NewDispatcher(repoSvc *domain.RepositoryService, raw *RawHandler, maven *MavenHandler) *Dispatcher {
	return &Dispatcher{repoSvc: repoSvc, raw: raw, maven: maven}
}

// route 依据仓库 format 选择处理器；仓库不存在则回退 raw（其 authorize 会产出 404）。
func (d *Dispatcher) route(repoName string) artifactHandler {
	repo, err := d.repoSvc.Get(repoName)
	if err != nil {
		return d.raw
	}
	switch repo.Format {
	case "maven":
		return d.maven
	default:
		return d.raw
	}
}

// Get/Put/Delete 按仓库 format 委派到对应处理器。
func (d *Dispatcher) Get(c *gin.Context)    { d.route(c.Param("repo")).Get(c) }
func (d *Dispatcher) Put(c *gin.Context)    { d.route(c.Param("repo")).Put(c) }
func (d *Dispatcher) Delete(c *gin.Context) { d.route(c.Param("repo")).Delete(c) }

// MavenHandler 处理 Maven 格式仓库。发布/删除与鉴权复用内嵌 RawHandler；
// GET 覆盖以支持校验和缺失现算与 group 的 maven-metadata.xml 合并。
type MavenHandler struct {
	*RawHandler
}

// NewMavenHandler 构造 MavenHandler，复用既有 RawHandler 的发布/删除/鉴权能力。
func NewMavenHandler(raw *RawHandler) *MavenHandler {
	return &MavenHandler{RawHandler: raw}
}

// Get 处理 GET/HEAD：group 的 maven-metadata.xml 走成员合并；普通制品走 Resolve；
// 校验和文件缺失则据底层制品现算返回。
func (h *MavenHandler) Get(c *gin.Context) {
	repoName := c.Param("repo")
	artPath := cleanArtifactPath(c.Param("artifactPath"))
	if !h.authorize(c, repoName, "read") {
		return
	}

	repo, err := h.repoSvc.Get(repoName)
	if err != nil {
		writeAssetErr(c, err)
		return
	}
	if repo.Type == "group" && isMavenMetadataFile(artPath) {
		h.serveGroupMetadata(c, repo, artPath)
		return
	}

	asset, rc, err := h.assets.Resolve(c.Request.Context(), repoName, artPath)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			if ext, ok := checksumExt(artPath); ok && h.serveComputedChecksum(c, repoName, artPath, ext) {
				return
			}
		}
		writeAssetErr(c, err)
		return
	}
	defer func() { _ = rc.Close() }()
	writeArtifact(c, asset.ContentType, asset.Size, asset.BlobHash, rc)
}

// serveComputedChecksum 据底层制品字节现算校验和并以纯文本返回；底层制品不存在返回 false。
func (h *MavenHandler) serveComputedChecksum(c *gin.Context, repoName, artPath, ext string) bool {
	base := strings.TrimSuffix(artPath, ext)
	_, rc, err := h.assets.Resolve(c.Request.Context(), repoName, base)
	if err != nil {
		return false
	}
	defer func() { _ = rc.Close() }()

	var hsh hash.Hash
	switch ext {
	case ".md5":
		hsh = md5.New()
	case ".sha1":
		hsh = sha1.New()
	case ".sha256":
		hsh = sha256.New()
	default:
		return false
	}
	if _, err := io.Copy(hsh, rc); err != nil {
		return false
	}
	sum := hex.EncodeToString(hsh.Sum(nil))
	c.Header("Content-Type", "text/plain; charset=utf-8")
	c.Header("Content-Length", strconv.Itoa(len(sum)))
	if c.Request.Method == http.MethodHead {
		c.Status(http.StatusOK)
		return true
	}
	c.String(http.StatusOK, sum)
	return true
}

// serveGroupMetadata 合并 group 各成员的 maven-metadata.xml：按成员有序解析、并入 versions，
// 重算 latest/release/lastUpdated 后返回；全成员皆无该文件返回 404。
func (h *MavenHandler) serveGroupMetadata(c *gin.Context, repo *repository.Repository, artPath string) {
	cfg, err := repo.DecodeConfig()
	if err != nil {
		writeAssetErr(c, err)
		return
	}
	var merged mavenMetadata
	found := false
	for _, member := range cfg.Members {
		_, rc, err := h.assets.Resolve(c.Request.Context(), member, artPath)
		if err != nil {
			continue
		}
		data, rerr := io.ReadAll(rc)
		_ = rc.Close()
		if rerr != nil {
			continue
		}
		if merged.merge(data) {
			found = true
		}
	}
	if !found {
		auth.WriteError(c, http.StatusNotFound, "not_found", "资源不存在")
		return
	}
	out, err := merged.encode()
	if err != nil {
		auth.WriteError(c, http.StatusInternalServerError, "internal", "内部错误")
		return
	}
	c.Header("Content-Type", "application/xml")
	c.Header("Content-Length", strconv.Itoa(len(out)))
	if c.Request.Method == http.MethodHead {
		c.Status(http.StatusOK)
		return
	}
	c.Data(http.StatusOK, "application/xml", out)
}

// writeArtifact 流式回写制品：设置 Content-Type/Length 与 ETag(=blob sha256)，HEAD 不写 body。
func writeArtifact(c *gin.Context, contentType string, size int64, blobHash string, rc io.Reader) {
	c.Header("Content-Type", contentType)
	c.Header("Content-Length", strconv.FormatInt(size, 10))
	c.Header("ETag", `"`+blobHash+`"`)
	if c.Request.Method == http.MethodHead {
		c.Status(http.StatusOK)
		return
	}
	c.Status(http.StatusOK)
	_, _ = io.Copy(c.Writer, rc)
}

// checksumExt 返回路径的 Maven 校验和后缀（.md5/.sha1/.sha256）及是否命中。
func checksumExt(p string) (string, bool) {
	for _, ext := range []string{".sha256", ".sha1", ".md5"} {
		if strings.HasSuffix(p, ext) {
			return ext, true
		}
	}
	return "", false
}

// isMavenMetadataFile 判定路径是否为 maven-metadata.xml（不含其校验和衍生文件）。
func isMavenMetadataFile(p string) bool {
	return strings.HasSuffix(p, "maven-metadata.xml")
}

// mavenMetadata 是 maven-metadata.xml 的最小结构，用于 group 合并。
type mavenMetadata struct {
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

// merge 解析并并入一份成员 maven-metadata.xml：groupId/artifactId 以首个非空为准，
// versions 按出现顺序去重合并。解析失败返回 false。
func (m *mavenMetadata) merge(data []byte) bool {
	var in mavenMetadata
	if err := xml.Unmarshal(data, &in); err != nil {
		return false
	}
	if m.GroupID == "" {
		m.GroupID = in.GroupID
	}
	if m.ArtifactID == "" {
		m.ArtifactID = in.ArtifactID
	}
	seen := make(map[string]struct{}, len(m.Versioning.Versions.Version))
	for _, v := range m.Versioning.Versions.Version {
		seen[v] = struct{}{}
	}
	for _, v := range in.Versioning.Versions.Version {
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		m.Versioning.Versions.Version = append(m.Versioning.Versions.Version, v)
	}
	return true
}

// encode 重算 latest（合并列表最后一项）、release（最后一个非 SNAPSHOT）、lastUpdated（当前 UTC）
// 后序列化为带声明头的 XML。版本排序采路径原样顺序，不做语义化版本比较（边界见 spec）。
func (m *mavenMetadata) encode() ([]byte, error) {
	versions := m.Versioning.Versions.Version
	if n := len(versions); n > 0 {
		m.Versioning.Latest = versions[n-1]
		m.Versioning.Release = ""
		for i := n - 1; i >= 0; i-- {
			if !strings.HasSuffix(versions[i], "-SNAPSHOT") {
				m.Versioning.Release = versions[i]
				break
			}
		}
	}
	m.Versioning.LastUpdated = time.Now().UTC().Format("20060102150405")
	body, err := xml.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	return append([]byte(xml.Header), body...), nil
}
