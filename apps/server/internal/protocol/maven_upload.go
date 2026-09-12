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
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
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
// 鉴权 write（401/403/404）→ 仓库须 maven hosted（409）。主文件、校验和、POM 和
// 元数据统一暂存，最后作为一个资产操作公开，失败时不暴露半套 Maven 版本。
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
	cfg, err := repo.DecodeConfig()
	if err != nil {
		writeAssetErr(c, err)
		return
	}
	if cfg.ImmutableRelease {
		existing, listErr := h.assets.ListAssetsByPrefix(repoName, versionDir+"/", 1)
		if listErr != nil {
			writeAssetErr(c, listErr)
			return
		}
		if len(existing) > 0 {
			auth.WriteError(c, http.StatusConflict, "immutable_release", "不可变 Release 不允许覆盖")
			return
		}
	}

	// stageWithChecksums 暂存一个文件及其 .md5/.sha1，所有资产准备完成后一次提交。
	var files []string
	planned := make([]*repository.Asset, 0, 9)
	committed := false
	defer func() {
		if !committed {
			_ = h.assets.DiscardStagedAssets(planned)
		}
	}()
	stageWithChecksums := func(path, contentType string, r io.Reader) error {
		asset, perr := h.assets.StageBlob(r, contentType)
		if perr != nil {
			return perr
		}
		asset.Path = path
		planned = append(planned, asset)
		files = append(files, path)
		for _, cs := range []struct{ ext, sum string }{{".md5", asset.Md5}, {".sha1", asset.Sha1}} {
			checksum, perr := h.assets.StageBlob(strings.NewReader(cs.sum), "text/plain")
			if perr != nil {
				return perr
			}
			checksum.Path = path + cs.ext
			planned = append(planned, checksum)
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
	if err := stageWithChecksums(mainPath, mainContentType(packaging), f); err != nil {
		writeAssetErr(c, err)
		return
	}

	// 2) 生成 pom + 校验和（packaging=pom 时主文件即 pom，不生成骨架避免覆盖）。
	if packaging != "pom" {
		pom := buildMinimalPom(groupID, artifactID, version, packaging)
		pomPath := versionDir + "/" + artifactID + "-" + version + ".pom"
		if err := stageWithChecksums(pomPath, "application/xml", strings.NewReader(pom)); err != nil {
			writeAssetErr(c, err)
			return
		}
	}

	// 3) artifact 级 maven-metadata.xml 读-改-写 + 校验和。
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
	if err := stageWithChecksums(metaPath, "application/xml", bytes.NewReader(out)); err != nil {
		writeAssetErr(c, err)
		return
	}
	if _, err := h.assets.PublishAssets(repoName, planned); err != nil {
		writeAssetErr(c, err)
		return
	}
	committed = true

	c.JSON(http.StatusCreated, gin.H{
		"repository": repoName,
		"groupId":    groupID,
		"artifactId": artifactID,
		"version":    version,
		"files":      files,
	})
}
