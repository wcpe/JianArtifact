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

// usageTexts 是一套语言下的接入说明文案。
//
// 使用说明与仓库格式 / 类型绑定，只能由后端组装（它知道协议路径与对外基址）；
// 但**标题与描述是给人看的文案**，故按请求语言返回，与界面语言策略一致
// （见 api 层对 Accept-Language 的解析）：命令与配置片段本身与语言无关，不参与本地化。
type usageTexts struct {
	rawDownloadTitle string
	rawDownloadDesc  string
	rawUploadTitle   string
	rawUploadDesc    string

	mavenAuthTitle   string
	mavenAuthDesc    string
	mavenPomTitle    string
	mavenPomDesc     string
	mavenGradleTitle string
	mavenGradleDesc  string

	mavenDeployTitle       string
	mavenDeployDesc        string
	mavenDeployGradleTitle string
	mavenDeployGradleDesc  string

	npmRegistryTitle string
	npmRegistryDesc  string
	npmInstallTitle  string
	npmInstallDesc   string
	npmPublishTitle  string
	npmPublishDesc   string
}

var usageTextsByLang = map[string]usageTexts{
	"zh": {
		rawDownloadTitle: "下载制品（curl）",
		rawDownloadDesc:  "以 API Token 作口令，用户名任意（公开仓库可匿名读）。",
		rawUploadTitle:   "上传制品（curl）",
		rawUploadDesc:    "PUT 上传到指定路径（仅 hosted 可写）。",

		mavenAuthTitle:   "认证（~/.m2/settings.xml）",
		mavenAuthDesc:    "在 <servers> 中配置凭据，server id 与下方仓库 id 保持一致。",
		mavenPomTitle:    "解析依赖（pom.xml）",
		mavenPomDesc:     "在 <repositories> 中声明该仓库用于依赖解析。",
		mavenGradleTitle: "解析依赖（Gradle）",
		mavenGradleDesc:  "在 build.gradle(.kts) 的 repositories 块中添加仓库。",

		mavenDeployTitle:       "发布制品（pom.xml + mvn deploy）",
		mavenDeployDesc:        "在 <distributionManagement> 声明部署目标后执行 mvn deploy（仅 hosted 可写）。",
		mavenDeployGradleTitle: "发布制品（Gradle）",
		mavenDeployGradleDesc:  "在 build.gradle(.kts) 的 publishing 块中配置部署仓库。",

		npmRegistryTitle: "配置 registry",
		npmRegistryDesc:  "将该仓库设为 npm registry（或写入项目 .npmrc）。",
		npmInstallTitle:  "安装依赖",
		npmInstallDesc:   "从该 registry 安装包。",
		npmPublishTitle:  "发布包（npm publish）",
		npmPublishDesc:   "发布到该仓库（仅 hosted 可写）。",
	},
	"en": {
		rawDownloadTitle: "Download artifact (curl)",
		rawDownloadDesc:  "Use an API token as the password; the username is arbitrary (public repositories allow anonymous reads).",
		rawUploadTitle:   "Upload artifact (curl)",
		rawUploadDesc:    "Upload to the given path with PUT (hosted repositories only).",

		mavenAuthTitle:   "Authentication (~/.m2/settings.xml)",
		mavenAuthDesc:    "Configure credentials in <servers>; the server id must match the repository id below.",
		mavenPomTitle:    "Resolve dependencies (pom.xml)",
		mavenPomDesc:     "Declare this repository in <repositories> for dependency resolution.",
		mavenGradleTitle: "Resolve dependencies (Gradle)",
		mavenGradleDesc:  "Add the repository to the repositories block in build.gradle(.kts).",

		mavenDeployTitle:       "Publish artifacts (pom.xml + mvn deploy)",
		mavenDeployDesc:        "Declare the deployment target in <distributionManagement>, then run mvn deploy (hosted repositories only).",
		mavenDeployGradleTitle: "Publish artifacts (Gradle)",
		mavenDeployGradleDesc:  "Configure the publishing repository in the publishing block of build.gradle(.kts).",

		npmRegistryTitle: "Configure registry",
		npmRegistryDesc:  "Set this repository as the npm registry (or write it to the project .npmrc).",
		npmInstallTitle:  "Install dependencies",
		npmInstallDesc:   "Install packages from this registry.",
		npmPublishTitle:  "Publish package (npm publish)",
		npmPublishDesc:   "Publish to this repository (hosted repositories only).",
	},
}

// usageTextsFor 取对应语言的文案；未收录的语言回退中文——与界面语言策略一致
// （只有明确 en 才落英文，其余语言兜底中文，避免把看不懂英文的访客推进英文内容）。
func usageTextsFor(lang string) usageTexts {
	if texts, ok := usageTextsByLang[lang]; ok {
		return texts
	}
	return usageTextsByLang["zh"]
}

// buildUsage 依仓库 format 与 type、对外基址组装客户端接入片段，文案按 lang 本地化。
// hosted 额外给出发布 / 上传片段；proxy/group 仅给出解析 / 下载片段。
func buildUsage(repo *repository.Repository, baseURL, lang string) []UsageSnippet {
	writable := repo.Type == "hosted"
	texts := usageTextsFor(lang)
	switch repo.Format {
	case "maven":
		return mavenUsage(repo.Name, baseURL, writable, texts)
	case "npm":
		return npmUsage(repo.Name, baseURL, writable, texts)
	default:
		return rawUsage(repo.Name, baseURL, writable, texts)
	}
}

// rawUsage 组装 Raw 仓库的 curl 上传 / 下载片段。
func rawUsage(name, base string, writable bool, t usageTexts) []UsageSnippet {
	repoURL := fmt.Sprintf("%s/repository/%s", base, name)
	snippets := []UsageSnippet{
		{
			Title:       t.rawDownloadTitle,
			Description: t.rawDownloadDesc,
			Code:        fmt.Sprintf("curl -u <user>:<token> -O %s/path/to/artifact", repoURL),
		},
	}
	if writable {
		snippets = append(snippets, UsageSnippet{
			Title:       t.rawUploadTitle,
			Description: t.rawUploadDesc,
			Code:        fmt.Sprintf("curl -u <user>:<token> --upload-file ./artifact %s/path/to/artifact", repoURL),
		})
	}
	return snippets
}

// mavenUsage 组装 Maven 仓库的 settings.xml 认证与 pom.xml / Gradle 解析 / 发布片段。
func mavenUsage(name, base string, writable bool, t usageTexts) []UsageSnippet {
	repoURL := fmt.Sprintf("%s/repository/%s", base, name)
	snippets := []UsageSnippet{
		{
			Title:       t.mavenAuthTitle,
			Description: t.mavenAuthDesc,
			Code: fmt.Sprintf(`<server>
  <id>%s</id>
  <username><user></username>
  <password><token></password>
</server>`, name),
		},
		{
			Title:       t.mavenPomTitle,
			Description: t.mavenPomDesc,
			Code: fmt.Sprintf(`<repository>
  <id>%s</id>
  <url>%s</url>
</repository>`, name, repoURL),
		},
		{
			Title:       t.mavenGradleTitle,
			Description: t.mavenGradleDesc,
			Code: fmt.Sprintf(`// build.gradle.kts
repositories {
    maven {
        url = uri("%s")
        credentials {
            username = "<user>"
            password = "<token>"
        }
    }
}`, repoURL),
		},
	}
	if writable {
		snippets = append(snippets, UsageSnippet{
			Title:       t.mavenDeployTitle,
			Description: t.mavenDeployDesc,
			Code: fmt.Sprintf(`<distributionManagement>
  <repository>
    <id>%s</id>
    <url>%s</url>
  </repository>
</distributionManagement>`, name, repoURL),
		})
		snippets = append(snippets, UsageSnippet{
			Title:       t.mavenDeployGradleTitle,
			Description: t.mavenDeployGradleDesc,
			Code: fmt.Sprintf(`// build.gradle.kts
publishing {
    repositories {
        maven {
            url = uri("%s")
            credentials {
                username = "<user>"
                password = "<token>"
            }
        }
    }
}`, repoURL),
		})
	}
	return snippets
}

// npmUsage 组装 npm 仓库的 registry 配置与安装 / 发布片段。
func npmUsage(name, base string, writable bool, t usageTexts) []UsageSnippet {
	registryURL := fmt.Sprintf("%s/npm/%s/", base, name)
	snippets := []UsageSnippet{
		{
			Title:       t.npmRegistryTitle,
			Description: t.npmRegistryDesc,
			Code:        fmt.Sprintf("npm config set registry %s", registryURL),
		},
		{
			Title:       t.npmInstallTitle,
			Description: t.npmInstallDesc,
			Code:        fmt.Sprintf("npm install <package> --registry %s", registryURL),
		},
	}
	if writable {
		snippets = append(snippets, UsageSnippet{
			Title:       t.npmPublishTitle,
			Description: t.npmPublishDesc,
			Code:        fmt.Sprintf("npm publish --registry %s", registryURL),
		})
	}
	return snippets
}
