package domain

import (
	"fmt"
	"strings"

	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// 使用片段的分组取值集合（取值须与 api/openapi.yaml 的 UsageSnippet.group enum 一致）。
//
// Group 是**结构化标记**——供界面按「认证 / 解析依赖 / 发布制品 / 其他」折叠分区，
// 与 Title / Description 不同：**它不是给人看的文案，因此不参与本地化**，
// 不要把它加进下方 usageTexts 语言表。
const (
	// UsageGroupAuth 认证：接入前的凭据 / registry 等一次性准备。
	UsageGroupAuth = "auth"
	// UsageGroupResolve 解析依赖：声明仓库以拉取依赖、下载制品。
	UsageGroupResolve = "resolve"
	// UsageGroupPublish 发布制品：向上传 / 发布到该仓库。
	UsageGroupPublish = "publish"
	// UsageGroupOther 其他：不属于上述用途的片段。
	UsageGroupOther = "other"
)

// 使用片段的具体工具 / 使用方式取值集合（取值须与 api/openapi.yaml 的 UsageSnippet.tool
// 说明中列出的当前取值一致）。
//
// Tool 与 Group 一样是**结构化标记**——供界面在同一个分组内下拉切换「用哪种工具 / 客户端」，
// 而不是给人看的文案：**它不参与本地化**，因此不要把它加进下方 usageTexts 语言表。
// 这里用自由字符串常量而非枚举：将来新增格式（如不同客户端）时只需追加片段，
// 不必改动契约里的枚举定义。
const (
	// UsageToolMaven Maven（pom.xml / settings.xml / mvn deploy 等 Maven 原生方式）。
	UsageToolMaven = "maven"
	// UsageToolGradle Gradle（Groovy DSL：build.gradle / gradle.properties）。
	UsageToolGradle = "gradle"
	// UsageToolGradleKts Gradle（Kotlin DSL：build.gradle.kts / gradle.properties）。
	// 两种 DSL 的构建文件与语法不同，故拆成两个工具，界面可分别给出对应写法。
	UsageToolGradleKts = "gradle-kts"
	// UsageToolSbt sbt（~/.sbt/repositories）。
	UsageToolSbt = "sbt"
	// UsageToolIvy Ivy（ivy.xml 的 resolvers 声明）。
	UsageToolIvy = "ivy"
	// UsageToolAnt Ant（build.xml 手动 get 取件；Ant 无依赖解析模型）。
	UsageToolAnt = "ant"
	// UsageToolNpm npm 客户端（registry 配置、安装与发布）。
	UsageToolNpm = "npm"
	// UsageToolPnpm pnpm 客户端。
	UsageToolPnpm = "pnpm"
	// UsageToolYarn Yarn 客户端。
	UsageToolYarn = "yarn"
	// UsageToolBun bun 客户端。
	UsageToolBun = "bun"
	// UsageToolCurl curl 命令行。
	UsageToolCurl = "curl"
	// UsageToolWget wget 命令行。
	UsageToolWget = "wget"
)

// UsageSnippet 是一段面向客户端的接入说明（标题 + 可选描述 + 可复制的命令 / 配置文本）。
//
// Group 为结构化分组标记，取值为上面四个 UsageGroup* 常量之一，
// 供界面按「认证 / 解析依赖 / 发布制品 / 其他」折叠分区使用；它不是给人看的文案，**不参与本地化**。
//
// Tool 为结构化工具标记，取值为上面 UsageTool* 常量之一，供界面在分组内下拉切换具体工具
// （Maven / Gradle / Gradle Kotlin DSL / sbt / Ivy / Ant / npm / pnpm / Yarn / bun / curl / wget 等）；
// 它同样不是文案，**不参与本地化**。
type UsageSnippet struct {
	Title       string
	Description string
	Code        string
	Group       string
	Tool        string
}

// usageTexts 是一套语言下的接入说明文案。
//
// 使用说明与仓库格式 / 类型绑定，只能由后端组装（它知道协议路径与对外基址）；
// 但**标题与描述是给人看的文案**，故按请求语言返回，与界面语言策略一致
// （见 api 层对 Accept-Language 的解析）：命令与配置片段本身与语言无关，不参与本地化。
//
// 每个字段都必须在每种语言下都有值——空标题会让界面出现空白分区，
// 故 usage_test.go 会逐字段校验两套语言均非空。
type usageTexts struct {
	rawDownloadTitle      string
	rawDownloadDesc       string
	rawDownloadTokenTitle string
	rawDownloadTokenDesc  string
	rawDownloadWgetTitle  string
	rawDownloadWgetDesc   string
	rawUploadTitle        string
	rawUploadDesc         string
	rawUploadTokenTitle   string
	rawUploadTokenDesc    string

	mavenAuthTitle       string
	mavenAuthDesc        string
	mavenAuthGradleTitle string
	mavenAuthGradleDesc  string
	mavenPomTitle        string
	mavenPomDesc         string
	// Gradle 的两种 DSL 各一份解析文案：Groovy（build.gradle）与 Kotlin（build.gradle.kts）。
	mavenGradleGroovyTitle string
	mavenGradleGroovyDesc  string
	mavenGradleKtsTitle    string
	mavenGradleKtsDesc     string
	mavenSbtTitle          string
	mavenSbtDesc           string
	mavenOneLinerTitle     string
	mavenOneLinerDesc      string
	mavenIvyTitle          string
	mavenIvyDesc           string
	mavenAntTitle          string
	mavenAntDesc           string

	mavenDeployTitle             string
	mavenDeployDesc              string
	mavenDeployGradleGroovyTitle string
	mavenDeployGradleGroovyDesc  string
	mavenDeployGradleKtsTitle    string
	mavenDeployGradleKtsDesc     string
	mavenDeployOneLinerTitle     string
	mavenDeployOneLinerDesc      string

	npmAuthTitle          string
	npmAuthDesc           string
	npmRegistryTitle      string
	npmRegistryDesc       string
	npmInstallTitle       string
	npmInstallDesc        string
	npmPnpmTitle          string
	npmPnpmDesc           string
	npmYarnTitle          string
	npmYarnDesc           string
	npmBunTitle           string
	npmBunDesc            string
	npmPublishTitle       string
	npmPublishDesc        string
	npmPublishScopedTitle string
	npmPublishScopedDesc  string
}

var usageTextsByLang = map[string]usageTexts{
	"zh": {
		rawDownloadTitle:      "下载制品（curl）",
		rawDownloadDesc:       "以 API Token 作口令，用户名任意（公开仓库可匿名读）。",
		rawDownloadTokenTitle: "下载制品（curl + Bearer Token）",
		rawDownloadTokenDesc:  "把 API Token 放进 Authorization 头，适合不方便用 Basic 认证的脚本。",
		rawDownloadWgetTitle:  "下载制品（wget）",
		rawDownloadWgetDesc:   "wget 等价写法：用 --header 传 Authorization。",
		rawUploadTitle:        "上传制品（curl）",
		rawUploadDesc:         "PUT 上传到指定路径（仅 hosted 可写）。",
		rawUploadTokenTitle:   "上传制品（curl + Bearer Token）",
		rawUploadTokenDesc:    "以 PUT + Authorization 头上传（仅 hosted 可写）。",

		mavenAuthTitle:         "认证（~/.m2/settings.xml）",
		mavenAuthDesc:          "在 <servers> 中配置凭据，server id 与下方仓库 id 保持一致。",
		mavenAuthGradleTitle:   "认证（~/.gradle/gradle.properties）",
		mavenAuthGradleDesc:    "把凭据集中放在 Gradle 用户级属性文件，构建脚本改用 repoUser / repoPassword 引用。",
		mavenPomTitle:          "解析依赖（pom.xml）",
		mavenPomDesc:           "在 <repositories> 中声明该仓库用于依赖解析。",
		mavenGradleGroovyTitle: "解析依赖（Gradle）",
		mavenGradleGroovyDesc:  "在 build.gradle 的 repositories 块中添加仓库（Groovy DSL）。",
		mavenGradleKtsTitle:    "解析依赖（Gradle Kotlin DSL）",
		mavenGradleKtsDesc:     "在 build.gradle.kts 的 repositories 块中添加仓库（Kotlin DSL）。",
		mavenSbtTitle:          "解析依赖（sbt）",
		mavenSbtDesc:           "在 ~/.sbt/repositories 中声明该仓库；私有仓库另需在 ~/.sbt/1.0/credentials.sbt 补凭据。",
		mavenOneLinerTitle:     "解析依赖（一行式 mvn 参数）",
		mavenOneLinerDesc:      "不改 pom.xml，按坐标从该仓库取单个制品，适合脚本或一次性排查。",
		mavenIvyTitle:          "解析依赖（Ivy）",
		mavenIvyDesc:           "在 ivy.xml 的 resolvers 中声明该仓库；m2compatible 让其按 Maven 2 目录布局取件。",
		mavenAntTitle:          "解析依赖（Ant）",
		mavenAntDesc:           "Ant 无依赖解析模型，需用 get 任务手动按 URL 取件。",

		mavenDeployTitle:             "发布制品（pom.xml + mvn deploy）",
		mavenDeployDesc:              "在 <distributionManagement> 声明部署目标后执行 mvn deploy（仅 hosted 可写）。",
		mavenDeployGradleGroovyTitle: "发布制品（Gradle）",
		mavenDeployGradleGroovyDesc:  "在 build.gradle 的 publishing 块中配置部署仓库（Groovy DSL，仅 hosted 可写）。",
		mavenDeployGradleKtsTitle:    "发布制品（Gradle Kotlin DSL）",
		mavenDeployGradleKtsDesc:     "在 build.gradle.kts 的 publishing 块中配置部署仓库（Kotlin DSL，仅 hosted 可写）。",
		mavenDeployOneLinerTitle:     "发布制品（一行式 mvn deploy）",
		mavenDeployOneLinerDesc:      "用 -DaltDeploymentRepository 覆盖部署目标，无需改动 pom.xml（仅 hosted 可写）。",

		npmAuthTitle:          "认证（.npmrc）",
		npmAuthDesc:           "在项目或用户级 .npmrc 中同时写入 registry 与 _authToken（token 即 API Token）。",
		npmRegistryTitle:      "配置 registry",
		npmRegistryDesc:       "将该仓库设为 npm 全局 registry（写入用户级 .npmrc）。",
		npmInstallTitle:       "安装依赖（npm）",
		npmInstallDesc:        "从该 registry 安装包。",
		npmPnpmTitle:          "安装依赖（pnpm）",
		npmPnpmDesc:           "pnpm 读取同一 registry 配置，设置一次后即可安装。",
		npmYarnTitle:          "安装依赖（Yarn）",
		npmYarnDesc:           "Yarn 1.x 用 yarn config set registry；Yarn 2+ 写入 .yarnrc.yml 的 npmRegistryServer。",
		npmBunTitle:           "安装依赖（bun）",
		npmBunDesc:            "bun 读取 .npmrc / bunfig.toml，registry 沿用既有配置。",
		npmPublishTitle:       "发布包（npm publish）",
		npmPublishDesc:        "发布到该仓库（仅 hosted 可写）。",
		npmPublishScopedTitle: "发布作用域包（publishConfig）",
		npmPublishScopedDesc:  "在 package.json 固定发布目标，作用域包（@scope/name）无需每次带 --registry（仅 hosted 可写）。",
	},
	"en": {
		rawDownloadTitle:      "Download artifact (curl)",
		rawDownloadDesc:       "Use an API token as the password; the username is arbitrary (public repositories allow anonymous reads).",
		rawDownloadTokenTitle: "Download artifact (curl + bearer token)",
		rawDownloadTokenDesc:  "Send the API token in the Authorization header when Basic auth is awkward (for example in scripts).",
		rawDownloadWgetTitle:  "Download artifact (wget)",
		rawDownloadWgetDesc:   "wget equivalent: pass Authorization through --header.",
		rawUploadTitle:        "Upload artifact (curl)",
		rawUploadDesc:         "Upload to the given path with PUT (hosted repositories only).",
		rawUploadTokenTitle:   "Upload artifact (curl + bearer token)",
		rawUploadTokenDesc:    "Upload with PUT and an Authorization header (hosted repositories only).",

		mavenAuthTitle:         "Authentication (~/.m2/settings.xml)",
		mavenAuthDesc:          "Configure credentials in <servers>; the server id must match the repository id below.",
		mavenAuthGradleTitle:   "Authentication (~/.gradle/gradle.properties)",
		mavenAuthGradleDesc:    "Keep credentials in Gradle's user-level properties file and reference them as repoUser / repoPassword from the build script.",
		mavenPomTitle:          "Resolve dependencies (pom.xml)",
		mavenPomDesc:           "Declare this repository in <repositories> for dependency resolution.",
		mavenGradleGroovyTitle: "Resolve dependencies (Gradle)",
		mavenGradleGroovyDesc:  "Add the repository to the repositories block in build.gradle (Groovy DSL).",
		mavenGradleKtsTitle:    "Resolve dependencies (Gradle Kotlin DSL)",
		mavenGradleKtsDesc:     "Add the repository to the repositories block in build.gradle.kts (Kotlin DSL).",
		mavenSbtTitle:          "Resolve dependencies (sbt)",
		mavenSbtDesc:           "Declare this repository in ~/.sbt/repositories; private repositories also need credentials in ~/.sbt/1.0/credentials.sbt.",
		mavenOneLinerTitle:     "Resolve dependencies (one-liner mvn flags)",
		mavenOneLinerDesc:      "Fetch a single artifact by coordinates without editing pom.xml, handy for scripts and one-off checks.",
		mavenIvyTitle:          "Resolve dependencies (Ivy)",
		mavenIvyDesc:           "Declare this repository in the resolvers of ivy.xml; m2compatible makes it resolve using the Maven 2 layout.",
		mavenAntTitle:          "Resolve dependencies (Ant)",
		mavenAntDesc:           "Ant has no dependency-resolution model, so fetch artifacts manually by URL with the get task.",

		mavenDeployTitle:             "Publish artifacts (pom.xml + mvn deploy)",
		mavenDeployDesc:              "Declare the deployment target in <distributionManagement>, then run mvn deploy (hosted repositories only).",
		mavenDeployGradleGroovyTitle: "Publish artifacts (Gradle)",
		mavenDeployGradleGroovyDesc:  "Configure the publishing repository in the publishing block of build.gradle (Groovy DSL, hosted repositories only).",
		mavenDeployGradleKtsTitle:    "Publish artifacts (Gradle Kotlin DSL)",
		mavenDeployGradleKtsDesc:     "Configure the publishing repository in the publishing block of build.gradle.kts (Kotlin DSL, hosted repositories only).",
		mavenDeployOneLinerTitle:     "Publish artifacts (one-liner mvn deploy)",
		mavenDeployOneLinerDesc:      "Override the deployment target with -DaltDeploymentRepository, no pom.xml change needed (hosted repositories only).",

		npmAuthTitle:          "Authentication (.npmrc)",
		npmAuthDesc:           "Write both the registry and _authToken into a project- or user-level .npmrc (the token is your API token).",
		npmRegistryTitle:      "Configure registry",
		npmRegistryDesc:       "Set this repository as the global npm registry (written to the user-level .npmrc).",
		npmInstallTitle:       "Install dependencies (npm)",
		npmInstallDesc:        "Install packages from this registry.",
		npmPnpmTitle:          "Install dependencies (pnpm)",
		npmPnpmDesc:           "pnpm reads the same registry setting; configure it once, then install.",
		npmYarnTitle:          "Install dependencies (Yarn)",
		npmYarnDesc:           "Yarn 1.x uses yarn config set registry; Yarn 2+ sets npmRegistryServer in .yarnrc.yml.",
		npmBunTitle:           "Install dependencies (bun)",
		npmBunDesc:            "bun reads .npmrc / bunfig.toml and reuses the existing registry configuration.",
		npmPublishTitle:       "Publish package (npm publish)",
		npmPublishDesc:        "Publish to this repository (hosted repositories only).",
		npmPublishScopedTitle: "Publish a scoped package (publishConfig)",
		npmPublishScopedDesc:  "Pin the publish target in package.json so scoped packages (@scope/name) need no --registry flag (hosted repositories only).",
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

// rawUsage 组装 Raw 仓库的下载 / 上传片段：
// 下载给出 Basic 与 Bearer 两种认证写法并附 wget 等价命令，上传同样给出 curl 的两种写法。
func rawUsage(name, base string, writable bool, t usageTexts) []UsageSnippet {
	repoURL := fmt.Sprintf("%s/repository/%s", base, name)
	snippets := []UsageSnippet{
		{
			Title:       t.rawDownloadTitle,
			Description: t.rawDownloadDesc,
			Code:        fmt.Sprintf("curl -u <user>:<token> -O %s/path/to/artifact", repoURL),
			Group:       UsageGroupResolve,
			Tool:        UsageToolCurl,
		},
		{
			Title:       t.rawDownloadTokenTitle,
			Description: t.rawDownloadTokenDesc,
			Code:        fmt.Sprintf("curl -H \"Authorization: Bearer <token>\" -O %s/path/to/artifact", repoURL),
			Group:       UsageGroupResolve,
			Tool:        UsageToolCurl,
		},
		{
			Title:       t.rawDownloadWgetTitle,
			Description: t.rawDownloadWgetDesc,
			Code:        fmt.Sprintf("wget --header=\"Authorization: Bearer <token>\" %s/path/to/artifact", repoURL),
			Group:       UsageGroupResolve,
			Tool:        UsageToolWget,
		},
	}
	if writable {
		snippets = append(snippets, UsageSnippet{
			Title:       t.rawUploadTitle,
			Description: t.rawUploadDesc,
			Code:        fmt.Sprintf("curl -u <user>:<token> --upload-file ./artifact %s/path/to/artifact", repoURL),
			Group:       UsageGroupPublish,
			Tool:        UsageToolCurl,
		})
		snippets = append(snippets, UsageSnippet{
			Title:       t.rawUploadTokenTitle,
			Description: t.rawUploadTokenDesc,
			Code:        fmt.Sprintf("curl -H \"Authorization: Bearer <token>\" --upload-file ./artifact %s/path/to/artifact", repoURL),
			Group:       UsageGroupPublish,
			Tool:        UsageToolCurl,
		})
	}
	return snippets
}

// mavenUsage 组装 Maven 仓库的接入片段：
// 认证给出 settings.xml 与 Gradle 属性文件两种凭据存放方式（属性文件在两种 Gradle DSL 下各给一份）；
// 解析给出 pom.xml、Gradle（Groovy / Kotlin DSL）、sbt、一行式 mvn 参数、Ivy 与 Ant；
// 发布给出 pom.xml、Gradle（Groovy / Kotlin DSL）与一行式 mvn deploy。
func mavenUsage(name, base string, writable bool, t usageTexts) []UsageSnippet {
	repoURL := fmt.Sprintf("%s/repository/%s", base, name)
	snippets := []UsageSnippet{
		{
			Title:       t.mavenAuthTitle,
			Description: t.mavenAuthDesc,
			Group:       UsageGroupAuth,
			Tool:        UsageToolMaven,
			Code: fmt.Sprintf(`<server>
  <id>%s</id>
  <username><user></username>
  <password><token></password>
</server>`, name),
		},
		// gradle.properties 与 DSL 无关：Groovy 与 Kotlin 两种 DSL 都用同一份用户级凭据文件。
		// 为了让「选中某个 Gradle 变体」时的视图自包含（选了 Kotlin 也能看到凭据怎么配），
		// 该片段在两个 Gradle 工具下**各出现一份**（内容相同），而不是只挂在其中一个下面。
		{
			Title:       t.mavenAuthGradleTitle,
			Description: t.mavenAuthGradleDesc,
			Group:       UsageGroupAuth,
			Tool:        UsageToolGradle,
			Code: `# ~/.gradle/gradle.properties
repoUser=<user>
repoPassword=<token>`,
		},
		{
			Title:       t.mavenAuthGradleTitle,
			Description: t.mavenAuthGradleDesc,
			Group:       UsageGroupAuth,
			Tool:        UsageToolGradleKts,
			Code: `# ~/.gradle/gradle.properties
repoUser=<user>
repoPassword=<token>`,
		},
		{
			Title:       t.mavenPomTitle,
			Description: t.mavenPomDesc,
			Group:       UsageGroupResolve,
			Tool:        UsageToolMaven,
			Code: fmt.Sprintf(`<repository>
  <id>%s</id>
  <url>%s</url>
</repository>`, name, repoURL),
		},
		{
			Title:       t.mavenGradleGroovyTitle,
			Description: t.mavenGradleGroovyDesc,
			Group:       UsageGroupResolve,
			Tool:        UsageToolGradle,
			Code: fmt.Sprintf(`// build.gradle
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
		{
			Title:       t.mavenGradleKtsTitle,
			Description: t.mavenGradleKtsDesc,
			Group:       UsageGroupResolve,
			Tool:        UsageToolGradleKts,
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
		{
			Title:       t.mavenSbtTitle,
			Description: t.mavenSbtDesc,
			Group:       UsageGroupResolve,
			Tool:        UsageToolSbt,
			Code: fmt.Sprintf(`# ~/.sbt/repositories
[repositories]
  local
  %s: %s`, name, repoURL),
		},
		{
			Title:       t.mavenOneLinerTitle,
			Description: t.mavenOneLinerDesc,
			Group:       UsageGroupResolve,
			// 一行式 mvn 与 pom.xml 都是 Maven 这一种工具，归同一 tool——
			// 界面的工具下拉是「你用哪个构建工具」，不该把同工具的两种用法拆成两个选项；
			// 选中 Maven 时这两段一起展示。
			Tool: UsageToolMaven,
			Code: fmt.Sprintf("mvn dependency:get -Dartifact=<groupId>:<artifactId>:<version> -DremoteRepositories=%s", repoURL),
		},
		{
			Title:       t.mavenIvyTitle,
			Description: t.mavenIvyDesc,
			Group:       UsageGroupResolve,
			Tool:        UsageToolIvy,
			Code: fmt.Sprintf(`<!-- ivy.xml -->
<resolvers>
  <ibiblio name="%s" m2compatible="true" root="%s/"/>
</resolvers>`, name, repoURL),
		},
		{
			Title:       t.mavenAntTitle,
			Description: t.mavenAntDesc,
			Group:       UsageGroupResolve,
			Tool:        UsageToolAnt,
			Code: fmt.Sprintf(`<!-- build.xml -->
<get src="%s/path/to/artifact" dest="lib/artifact.jar"/>`, repoURL),
		},
	}
	if writable {
		snippets = append(snippets, UsageSnippet{
			Title:       t.mavenDeployTitle,
			Description: t.mavenDeployDesc,
			Group:       UsageGroupPublish,
			Tool:        UsageToolMaven,
			Code: fmt.Sprintf(`<distributionManagement>
  <repository>
    <id>%s</id>
    <url>%s</url>
  </repository>
</distributionManagement>`, name, repoURL),
		})
		snippets = append(snippets, UsageSnippet{
			Title:       t.mavenDeployGradleGroovyTitle,
			Description: t.mavenDeployGradleGroovyDesc,
			Group:       UsageGroupPublish,
			Tool:        UsageToolGradle,
			Code: fmt.Sprintf(`// build.gradle
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
		snippets = append(snippets, UsageSnippet{
			Title:       t.mavenDeployGradleKtsTitle,
			Description: t.mavenDeployGradleKtsDesc,
			Group:       UsageGroupPublish,
			Tool:        UsageToolGradleKts,
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
		snippets = append(snippets, UsageSnippet{
			Title:       t.mavenDeployOneLinerTitle,
			Description: t.mavenDeployOneLinerDesc,
			Group:       UsageGroupPublish,
			// 同上：一行式 mvn deploy 与 pom.xml 都属 Maven 工具。
			Tool: UsageToolMaven,
			Code: fmt.Sprintf("mvn deploy -DaltDeploymentRepository=%s::default::%s", name, repoURL),
		})
	}
	return snippets
}

// npmRegistryAuthLine 由 registry 地址推导 .npmrc 的认证行。
// npm 要求形如 //<host><path>:<key>=<token>：去掉协议、保留路径及其末尾斜杠，
// 因此这里直接用 registry 地址裁剪得到，避免硬编码主机名。
func npmRegistryAuthLine(registryURL string) string {
	hostAndPath := strings.TrimPrefix(strings.TrimPrefix(registryURL, "https://"), "http://")
	return fmt.Sprintf("//%s:_authToken=<token>", hostAndPath)
}

// npmUsage 组装 npm 仓库的接入片段：
// 认证给出全局 registry 配置与 .npmrc 写法；解析给出 npm / pnpm / Yarn / bun 四种客户端的等价配置；
// 发布给出命令行与 package.json 的 publishConfig 两种方式。
func npmUsage(name, base string, writable bool, t usageTexts) []UsageSnippet {
	registryURL := fmt.Sprintf("%s/npm/%s/", base, name)
	snippets := []UsageSnippet{
		{
			Title:       t.npmRegistryTitle,
			Description: t.npmRegistryDesc,
			Code:        fmt.Sprintf("npm config set registry %s", registryURL),
			Group:       UsageGroupAuth,
			Tool:        UsageToolNpm,
		},
		{
			Title:       t.npmAuthTitle,
			Description: t.npmAuthDesc,
			Group:       UsageGroupAuth,
			Tool:        UsageToolNpm,
			Code:        fmt.Sprintf("# .npmrc\nregistry=%s\n%s", registryURL, npmRegistryAuthLine(registryURL)),
		},
		{
			Title:       t.npmInstallTitle,
			Description: t.npmInstallDesc,
			Code:        fmt.Sprintf("npm install <package> --registry %s", registryURL),
			Group:       UsageGroupResolve,
			Tool:        UsageToolNpm,
		},
		{
			Title:       t.npmPnpmTitle,
			Description: t.npmPnpmDesc,
			Code:        fmt.Sprintf("pnpm config set registry %s\npnpm add <package>", registryURL),
			Group:       UsageGroupResolve,
			Tool:        UsageToolPnpm,
		},
		{
			Title:       t.npmYarnTitle,
			Description: t.npmYarnDesc,
			Group:       UsageGroupResolve,
			Tool:        UsageToolYarn,
			Code: fmt.Sprintf(`# Yarn 1.x
yarn config set registry %s
# Yarn 2+：写入 .yarnrc.yml
npmRegistryServer: "%s"`, registryURL, registryURL),
		},
		{
			Title:       t.npmBunTitle,
			Description: t.npmBunDesc,
			// bun 读 .npmrc / bunfig.toml，registry 沿用既有配置，故只给安装命令。
			Code:  "bun add <package>",
			Group: UsageGroupResolve,
			Tool:  UsageToolBun,
		},
	}
	if writable {
		snippets = append(snippets, UsageSnippet{
			Title:       t.npmPublishTitle,
			Description: t.npmPublishDesc,
			Code:        fmt.Sprintf("npm publish --registry %s", registryURL),
			Group:       UsageGroupPublish,
			Tool:        UsageToolNpm,
		})
		snippets = append(snippets, UsageSnippet{
			Title:       t.npmPublishScopedTitle,
			Description: t.npmPublishScopedDesc,
			Group:       UsageGroupPublish,
			Tool:        UsageToolNpm,
			Code: fmt.Sprintf(`// package.json
"publishConfig": {
  "registry": "%s"
}`, registryURL),
		})
	}
	return snippets
}
