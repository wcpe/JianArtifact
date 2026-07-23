package domain

import (
	"fmt"

	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// UsageSnippet 是一段面向客户端的接入说明（标题 + 可选描述 + 可复制的命令 / 配置文本）。
type UsageSnippet struct {
	Title       string
	Description string
	Code        string
}

// buildUsage 依仓库 format 与 type、对外基址组装客户端接入片段。
// hosted 额外给出发布 / 上传片段；proxy/group 仅给出解析 / 下载片段。
func buildUsage(repo *repository.Repository, baseURL string) []UsageSnippet {
	writable := repo.Type == "hosted"
	switch repo.Format {
	case "maven":
		return mavenUsage(repo.Name, baseURL, writable)
	case "npm":
		return npmUsage(repo.Name, baseURL, writable)
	default:
		return rawUsage(repo.Name, baseURL, writable)
	}
}

// rawUsage 组装 Raw 仓库的 curl 上传 / 下载片段。
func rawUsage(name, base string, writable bool) []UsageSnippet {
	repoURL := fmt.Sprintf("%s/repository/%s", base, name)
	snippets := []UsageSnippet{
		{
			Title:       "下载制品（curl）",
			Description: "以 API Token 作口令，用户名任意（公开仓库可匿名读）。",
			Code:        fmt.Sprintf("curl -u <user>:<token> -O %s/path/to/artifact", repoURL),
		},
	}
	if writable {
		snippets = append(snippets, UsageSnippet{
			Title:       "上传制品（curl）",
			Description: "PUT 上传到指定路径（仅 hosted 可写）。",
			Code:        fmt.Sprintf("curl -u <user>:<token> --upload-file ./artifact %s/path/to/artifact", repoURL),
		})
	}
	return snippets
}

// mavenUsage 组装 Maven 仓库的 settings.xml 认证与 pom.xml 解析 / 发布片段。
func mavenUsage(name, base string, writable bool) []UsageSnippet {
	repoURL := fmt.Sprintf("%s/repository/%s", base, name)
	snippets := []UsageSnippet{
		{
			Title:       "认证（~/.m2/settings.xml）",
			Description: "在 <servers> 中配置凭据，server id 与下方仓库 id 保持一致。",
			Code: fmt.Sprintf(`<server>
  <id>%s</id>
  <username><user></username>
  <password><token></password>
</server>`, name),
		},
		{
			Title:       "解析依赖（pom.xml）",
			Description: "在 <repositories> 中声明该仓库用于依赖解析。",
			Code: fmt.Sprintf(`<repository>
  <id>%s</id>
  <url>%s</url>
</repository>`, name, repoURL),
		},
	}
	if writable {
		snippets = append(snippets, UsageSnippet{
			Title:       "发布制品（pom.xml + mvn deploy）",
			Description: "在 <distributionManagement> 声明部署目标后执行 mvn deploy（仅 hosted 可写）。",
			Code: fmt.Sprintf(`<distributionManagement>
  <repository>
    <id>%s</id>
    <url>%s</url>
  </repository>
</distributionManagement>`, name, repoURL),
		})
	}
	return snippets
}

// npmUsage 组装 npm 仓库的 registry 配置与安装 / 发布片段。
func npmUsage(name, base string, writable bool) []UsageSnippet {
	registryURL := fmt.Sprintf("%s/npm/%s/", base, name)
	snippets := []UsageSnippet{
		{
			Title:       "配置 registry",
			Description: "将该仓库设为 npm registry（或写入项目 .npmrc）。",
			Code:        fmt.Sprintf("npm config set registry %s", registryURL),
		},
		{
			Title:       "安装依赖",
			Description: "从该 registry 安装包。",
			Code:        fmt.Sprintf("npm install <package> --registry %s", registryURL),
		},
	}
	if writable {
		snippets = append(snippets, UsageSnippet{
			Title:       "发布包（npm publish）",
			Description: "发布到该仓库（仅 hosted 可写）。",
			Code:        fmt.Sprintf("npm publish --registry %s", registryURL),
		})
	}
	return snippets
}
