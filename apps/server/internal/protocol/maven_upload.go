// FR-73: Maven 网页上传端点（非契约，面向管理端表单）。
//
// POST /api/v1/repositories/:name/maven-upload（multipart：groupId/artifactId/version/
// packaging/file）。服务端自动生成最小 pom.xml、各文件 .md5/.sha1，并读-改-写 artifact 级
// maven-metadata.xml（复用 mavenMetadata merge/encode）及其校验和，使 mvn 客户端可直接解析。
// 仅限 release 版本（SNAPSHOT 需 timestamp/buildNumber 语义，属客户端 deploy 职责）。
package protocol

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
)

// gavPattern 限定 GAV 与 packaging 的合法字符（拒绝路径分隔符与空白）。
var gavPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// validGavField 校验单个 GAV 字段：非空、字符合法、非全点（. / .. 拼路径会穿越）。
func validGavField(s string) bool {
	if s == "" || !gavPattern.MatchString(s) {
		return false
	}
	return strings.Trim(s, ".") != ""
}

// validGroupID 在 validGavField 基础上要求每个点分段非空（防 com..example 产生空目录段）。
func validGroupID(s string) bool {
	if !validGavField(s) {
		return false
	}
	for _, seg := range strings.Split(s, ".") {
		if seg == "" || strings.Trim(seg, ".") == "" {
			return false
		}
	}
	return true
}

// mainContentType 按 packaging 推断主文件 Content-Type。
func mainContentType(packaging string) string {
	switch packaging {
	case "jar", "war":
		return "application/java-archive"
	case "pom":
		return "application/xml"
	default:
		return "application/octet-stream"
	}
}

// buildMinimalPom 生成最小 pom 骨架；packaging=jar 时省略 <packaging>（与 Maven 默认一致）。
// GAV 已限定为 [A-Za-z0-9._-]，无需 XML 转义。
func buildMinimalPom(groupID, artifactID, version, packaging string) string {
	var b strings.Builder
	b.WriteString(xml.Header)
	b.WriteString("<project xmlns=\"http://maven.apache.org/POM/4.0.0\">\n")
	b.WriteString("  <modelVersion>4.0.0</modelVersion>\n")
	fmt.Fprintf(&b, "  <groupId>%s</groupId>\n", groupID)
	fmt.Fprintf(&b, "  <artifactId>%s</artifactId>\n", artifactID)
	fmt.Fprintf(&b, "  <version>%s</version>\n", version)
	if packaging != "jar" {
		fmt.Fprintf(&b, "  <packaging>%s</packaging>\n", packaging)
	}
	b.WriteString("</project>\n")
	return b.String()
}

// UploadForm 处理管理端 Maven 表单上传。校验顺序：字段 400 → SNAPSHOT 400 →
// 鉴权 write（401/403/404）→ 仓库须 maven hosted（409）。多文件顺序写入不引入事务，
// 中途失败可能留下部分文件——重传即自愈（Upsert 覆盖），与 mvn deploy 多次 PUT 行为一致。
func (h *MavenHandler) UploadForm(c *gin.Context) {
	repoName := c.Param("name")
	groupID := strings.TrimSpace(c.PostForm("groupId"))
	artifactID := strings.TrimSpace(c.PostForm("artifactId"))
	version := strings.TrimSpace(c.PostForm("version"))
	packaging := strings.TrimSpace(c.PostForm("packaging"))
	if packaging == "" {
		packaging = "jar"
	}

	if !validGroupID(groupID) || !validGavField(artifactID) || !validGavField(version) || !validGavField(packaging) {
		auth.WriteError(c, http.StatusBadRequest, "invalid_gav", "groupId/artifactId/version/packaging 为空或含非法字符")
		return
	}
	if strings.Contains(strings.ToUpper(version), "-SNAPSHOT") {
		auth.WriteError(c, http.StatusBadRequest, "snapshot_not_supported", "网页上传仅限 release 版本，SNAPSHOT 请使用 mvn deploy 发布")
		return
	}
	fileHeader, err := c.FormFile("file")
	if err != nil {
		auth.WriteError(c, http.StatusBadRequest, "missing_file", "缺少上传文件")
		return
	}

	if !h.authorize(c, repoName, "write") {
		return
	}
	repo, err := h.repoSvc.Get(repoName)
	if err != nil {
		writeAssetErr(c, err)
		return
	}
	if repo.Format != "maven" || repo.Type != "hosted" {
		auth.WriteError(c, http.StatusConflict, "not_maven_hosted", "仅 Maven hosted 仓库支持网页上传")
		return
	}

	artifactDir := strings.ReplaceAll(groupID, ".", "/") + "/" + artifactID
	versionDir := artifactDir + "/" + version

	// putWithChecksums 写入一个文件及其 .md5/.sha1（校验和直接取 Put 返回的 Asset 列，无需重算）。
	var files []string
	putWithChecksums := func(path, contentType string, r io.Reader) error {
		asset, perr := h.assets.Put(repoName, path, r, contentType)
		if perr != nil {
			return perr
		}
		files = append(files, path)
		for _, cs := range []struct{ ext, sum string }{{".md5", asset.Md5}, {".sha1", asset.Sha1}} {
			if _, perr := h.assets.Put(repoName, path+cs.ext, strings.NewReader(cs.sum), "text/plain"); perr != nil {
				return perr
			}
			files = append(files, path+cs.ext)
		}
		return nil
	}

	// 1) 主文件 + 校验和。
	f, err := fileHeader.Open()
	if err != nil {
		auth.WriteError(c, http.StatusBadRequest, "invalid_file", "读取上传文件失败")
		return
	}
	defer func() { _ = f.Close() }()
	mainPath := versionDir + "/" + artifactID + "-" + version + "." + packaging
	if err := putWithChecksums(mainPath, mainContentType(packaging), f); err != nil {
		writeAssetErr(c, err)
		return
	}

	// 2) 生成 pom + 校验和（packaging=pom 时主文件即 pom，不生成骨架避免覆盖）。
	if packaging != "pom" {
		pom := buildMinimalPom(groupID, artifactID, version, packaging)
		pomPath := versionDir + "/" + artifactID + "-" + version + ".pom"
		if err := putWithChecksums(pomPath, "application/xml", strings.NewReader(pom)); err != nil {
			writeAssetErr(c, err)
			return
		}
	}

	// 3) artifact 级 maven-metadata.xml 读-改-写 + 校验和（无锁：管理端低频操作，
	// 与客户端 deploy 一致不加并发保护，见 spec 风险节）。
	metaPath := artifactDir + "/maven-metadata.xml"
	var meta mavenMetadata
	if _, rc, rerr := h.assets.Resolve(c.Request.Context(), repoName, metaPath); rerr == nil {
		data, readErr := io.ReadAll(rc)
		_ = rc.Close()
		if readErr == nil {
			meta.merge(data)
		}
	}
	if meta.GroupID == "" {
		meta.GroupID = groupID
	}
	if meta.ArtifactID == "" {
		meta.ArtifactID = artifactID
	}
	hasVersion := false
	for _, v := range meta.Versioning.Versions.Version {
		if v == version {
			hasVersion = true
			break
		}
	}
	if !hasVersion {
		meta.Versioning.Versions.Version = append(meta.Versioning.Versions.Version, version)
	}
	out, err := meta.encode()
	if err != nil {
		auth.WriteError(c, http.StatusInternalServerError, "internal", "生成 maven-metadata.xml 失败")
		return
	}
	if err := putWithChecksums(metaPath, "application/xml", bytes.NewReader(out)); err != nil {
		writeAssetErr(c, err)
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"repository": repoName,
		"groupId":    groupID,
		"artifactId": artifactID,
		"version":    version,
		"files":      files,
	})
}
