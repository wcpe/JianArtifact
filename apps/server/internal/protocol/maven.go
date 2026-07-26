// Package protocol：Maven 格式端点。见 doc.go 分层说明。
//
// Maven 复用 Raw 的 URL 形态（/repository/:repo/*artifactPath）与内容寻址存储，
// 由 Dispatcher 按仓库 format 分派：制品字节的发布/拉取/删除完全复用 RawHandler；
// MavenHandler 仅覆盖 GET 以补两点 Maven 语义：
//   - 校验和文件（.md5/.sha1/.sha256）缺失时据底层制品字节现算返回；
//   - group 仓库的 maven-metadata.xml 合并各成员的 versions/latest/release（时间戳重算）。
//
// SNAPSHOT 版本解析：当客户端请求 artifact-version-SNAPSHOT.ext 路径时，
// 查询对应目录下 maven-metadata.xml 中 <snapshot> 的 timestamp+buildNumber，
// 将文件名中的 SNAPSHOT 替换为 timestamp-buildNumber 后重新解析（标准 Maven 行为）。
package protocol

import (
	"context"
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
	"sync"
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
// SNAPSHOT 制品优先尝试时间戳解析（跳过慢速的字面路径轮询）；校验和文件缺失则据底层制品现算返回。
func (h *MavenHandler) Get(c *gin.Context) {
	repoName := c.Param("repo")
	if !h.authorize(c, repoName, "read") {
		return
	}
	if h.tryBrowse(c) {
		return
	}
	artPath := cleanArtifactPath(c.Param("artifactPath"))

	repo, err := h.repoSvc.Get(repoName)
	if err != nil {
		writeAssetErr(c, err)
		return
	}
	// 仅对 artifact 级别的 maven-metadata.xml（列版本号）做 group 合并；
	// SNAPSHOT 级别的 metadata（含 <snapshot> timestamp/buildNumber）不合并，直接走 Resolve。
	if repo.Type == "group" && isMavenMetadataFile(artPath) && !isSnapshotMetadata(artPath) {
		h.serveGroupMetadata(c, repo, artPath)
		return
	}

	// SNAPSHOT 制品优化：文件名含 -SNAPSHOT 时先尝试时间戳解析，
	// 避免 group 广播字面 -SNAPSHOT 路径给所有 proxy 成员（不存在且超时慢）。
	if h.isSnapshotArtifact(artPath) {
		if resolved, ok := h.resolveSnapshotPath(c, repoName, artPath); ok {
			asset, rc, err := h.assets.Resolve(c.Request.Context(), repoName, resolved)
			if err == nil {
				defer func() { _ = rc.Close() }()
				writeArtifact(c, asset.ContentType, asset.Size, asset.BlobHash, rc)
				return
			}
		}
		// SNAPSHOT 解析失败，尝试校验和现算
		if ext, ok := checksumExt(artPath); ok && h.serveComputedChecksum(c, repoName, artPath, ext) {
			return
		}
		auth.WriteError(c, http.StatusNotFound, "not_found", "资源不存在")
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

// metadataMemberTimeout 是 group 合并 maven-metadata.xml 时每个成员解析的最大等待时间。
// proxy 成员在此时间内未能从上游取回 metadata 则跳过，防止不可达成员阻塞整条链路。
const metadataMemberTimeout = 5 * time.Second

// serveGroupMetadata 合并 group 各成员的 maven-metadata.xml：并行请求所有成员（限时
// metadataMemberTimeout），合并 versions、重算 latest/release/lastUpdated 后返回；
// 全成员皆无该文件返回 404。并行化使总耗时为 max(成员耗时) 而非 sum。
func (h *MavenHandler) serveGroupMetadata(c *gin.Context, repo *repository.Repository, artPath string) {
	cfg, err := repo.DecodeConfig()
	if err != nil {
		writeAssetErr(c, err)
		return
	}

	// 并行请求所有成员的 metadata
	type memberResult struct {
		data []byte
	}
	results := make(chan memberResult, len(cfg.Members))
	var wg sync.WaitGroup
	for _, member := range cfg.Members {
		wg.Add(1)
		go func(m string) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(c.Request.Context(), metadataMemberTimeout)
			defer cancel()
			_, rc, err := h.assets.Resolve(ctx, m, artPath)
			if err != nil {
				return
			}
			data, rerr := io.ReadAll(rc)
			_ = rc.Close()
			if rerr != nil {
				return
			}
			results <- memberResult{data: data}
		}(member)
	}
	// 等待所有 goroutine 完成后关闭 channel
	go func() {
		wg.Wait()
		close(results)
	}()

	var merged mavenMetadata
	found := false
	for r := range results {
		if merged.merge(r.data) {
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

// isSnapshotArtifact 判断路径是否为 SNAPSHOT 制品请求（文件名含 -SNAPSHOT，但排除 maven-metadata.xml）。
func (h *MavenHandler) isSnapshotArtifact(artPath string) bool {
	if isMavenMetadataFile(artPath) {
		return false
	}
	slash := strings.LastIndex(artPath, "/")
	var filename string
	if slash >= 0 {
		filename = artPath[slash+1:]
	} else {
		filename = artPath
	}
	// 去掉校验和后缀再判断
	for _, ext := range []string{".sha512", ".sha256", ".sha1", ".md5"} {
		filename = strings.TrimSuffix(filename, ext)
	}
	return strings.Contains(filename, "-SNAPSHOT")
}

// resolveSnapshotPath 检测路径是否为 SNAPSHOT 制品请求，若是则读取 maven-metadata.xml
// 解析 <snapshot> 的 timestamp+buildNumber，将文件名中 -SNAPSHOT 替换为 -timestamp-buildNumber。
// 例: .../6.2.4-wcpe-SNAPSHOT/foo-6.2.4-wcpe-SNAPSHOT.jar → .../6.2.4-wcpe-SNAPSHOT/foo-6.2.4-wcpe-20260317.215705-3.jar
func (h *MavenHandler) resolveSnapshotPath(c *gin.Context, repoName, artPath string) (string, bool) {
	// 仅处理文件名中包含 -SNAPSHOT 的路径（不含 maven-metadata.xml 本身）
	slash := strings.LastIndex(artPath, "/")
	var dir, filename string
	if slash >= 0 {
		dir = artPath[:slash]
		filename = artPath[slash+1:]
	} else {
		dir = ""
		filename = artPath
	}
	if !strings.Contains(filename, "-SNAPSHOT") {
		return "", false
	}
	// 也要处理校验和后缀的情况（如 .jar.sha1 请求 SNAPSHOT 解析）
	base := filename
	var csExt string
	for _, ext := range []string{".sha512", ".sha256", ".sha1", ".md5"} {
		if strings.HasSuffix(base, ext) {
			csExt = ext
			base = strings.TrimSuffix(base, ext)
			break
		}
	}

	// 读取同目录下 maven-metadata.xml
	metaPath := dir + "/maven-metadata.xml"
	_, rc, err := h.assets.Resolve(c.Request.Context(), repoName, metaPath)
	if err != nil {
		return "", false
	}
	data, readErr := io.ReadAll(rc)
	_ = rc.Close()
	if readErr != nil {
		return "", false
	}

	// 解析 snapshot 元数据
	var meta snapshotMetadata
	if xmlErr := xml.Unmarshal(data, &meta); xmlErr != nil {
		return "", false
	}
	ts := meta.Versioning.Snapshot.Timestamp
	bn := meta.Versioning.Snapshot.BuildNumber
	if ts == "" || bn == "" {
		return "", false
	}

	// 替换文件名中的 SNAPSHOT → timestamp-buildNumber
	resolved := strings.Replace(base, "-SNAPSHOT", "-"+ts+"-"+bn, 1) + csExt
	if dir != "" {
		return dir + "/" + resolved, true
	}
	return resolved, true
}

// snapshotMetadata 解析 SNAPSHOT 版本目录下的 maven-metadata.xml。
type snapshotMetadata struct {
	XMLName    xml.Name `xml:"metadata"`
	Versioning struct {
		Snapshot struct {
			Timestamp   string `xml:"timestamp"`
			BuildNumber string `xml:"buildNumber"`
		} `xml:"snapshot"`
	} `xml:"versioning"`
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

// isSnapshotMetadata 判定 maven-metadata.xml 是否位于 SNAPSHOT 版本目录下。
// 例: io/izzel/taboolib/bukkit-nms-stable/6.2.4-wcpe-SNAPSHOT/maven-metadata.xml
// 此类 metadata 的结构为 <snapshot>/<timestamp>/<buildNumber>，不适用 group 版本合并。
func isSnapshotMetadata(artPath string) bool {
	i := strings.LastIndex(artPath, "/")
	if i <= 0 {
		return false
	}
	dir := artPath[:i]
	return strings.HasSuffix(dir, "-SNAPSHOT")
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
