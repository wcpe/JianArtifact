package domain

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// 使用片段的标题 / 描述随请求语言本地化，命令片段本身与语言无关。
func TestBuildUsageLocalizesTextsNotCommands(t *testing.T) {
	repo := &repository.Repository{Name: "demo", Format: "raw", Type: "hosted"}

	zh := buildUsage(repo, "https://example.test", "zh")
	en := buildUsage(repo, "https://example.test", "en")

	if len(zh) == 0 || len(en) == 0 {
		t.Fatal("两种语言都应产出使用片段")
	}
	if len(zh) != len(en) {
		t.Fatalf("两种语言的片段数量应一致：zh=%d en=%d", len(zh), len(en))
	}
	if zh[0].Title == en[0].Title {
		t.Errorf("标题应随语言变化，实际都是 %q", zh[0].Title)
	}
	if zh[0].Code != en[0].Code {
		t.Errorf("命令片段不应随语言变化：\nzh=%q\nen=%q", zh[0].Code, en[0].Code)
	}
	if !strings.Contains(en[0].Title, "Download") {
		t.Errorf("英文标题应含 Download，实际 %q", en[0].Title)
	}
	if !strings.Contains(zh[0].Title, "下载") {
		t.Errorf("中文标题应含「下载」，实际 %q", zh[0].Title)
	}
}

// 未收录的语言与空语言一律回退中文（与前端策略一致：只有明确 en 才落英文）。
func TestBuildUsageFallsBackToChinese(t *testing.T) {
	repo := &repository.Repository{Name: "demo", Format: "raw", Type: "hosted"}
	zh := buildUsage(repo, "https://example.test", "zh")[0]

	for _, lang := range []string{"", "ja", "de-DE", "fr"} {
		if got := buildUsage(repo, "https://example.test", lang)[0]; got.Title != zh.Title {
			t.Errorf("语言 %q 应回退中文标题，实际 %q", lang, got.Title)
		}
	}
}

// maven / npm 两类也走同一套语言选择，避免只覆盖 raw 的假绿。
func TestBuildUsageLocalizesAllFormats(t *testing.T) {
	for _, format := range []string{"maven", "npm", "raw"} {
		repo := &repository.Repository{Name: "demo", Format: format, Type: "hosted"}
		zh := buildUsage(repo, "https://example.test", "zh")
		en := buildUsage(repo, "https://example.test", "en")
		if len(zh) == 0 || len(en) == 0 {
			t.Fatalf("%s：两种语言都应产出片段", format)
		}
		if zh[0].Title == en[0].Title {
			t.Errorf("%s：标题应随语言变化，实际都是 %q", format, zh[0].Title)
		}
	}
}

// 每个片段都必须带合法 group，且按用途归类正确（认证 / 解析依赖 / 发布制品 / 其他）。
// 顺序由组装函数决定，故按索引断言；hosted 才有发布片段，proxy/group 只有解析片段。
// 同组内保持「先基础后进阶」的顺序。
func TestBuildUsageAssignsGroups(t *testing.T) {
	valid := map[string]bool{
		UsageGroupAuth:    true,
		UsageGroupResolve: true,
		UsageGroupPublish: true,
		UsageGroupOther:   true,
	}
	cases := []struct {
		format   string
		writable bool
		want     []string
	}{
		{"raw", true, []string{UsageGroupResolve, UsageGroupResolve, UsageGroupResolve, UsageGroupPublish, UsageGroupPublish}},
		{"raw", false, []string{UsageGroupResolve, UsageGroupResolve, UsageGroupResolve}},
		{"maven", true, []string{
			UsageGroupAuth, UsageGroupAuth, UsageGroupAuth,
			UsageGroupResolve, UsageGroupResolve, UsageGroupResolve, UsageGroupResolve, UsageGroupResolve, UsageGroupResolve, UsageGroupResolve,
			UsageGroupPublish, UsageGroupPublish, UsageGroupPublish, UsageGroupPublish,
		}},
		{"maven", false, []string{
			UsageGroupAuth, UsageGroupAuth, UsageGroupAuth,
			UsageGroupResolve, UsageGroupResolve, UsageGroupResolve, UsageGroupResolve, UsageGroupResolve, UsageGroupResolve, UsageGroupResolve,
		}},
		{"npm", true, []string{
			UsageGroupAuth, UsageGroupAuth,
			UsageGroupResolve, UsageGroupResolve, UsageGroupResolve, UsageGroupResolve,
			UsageGroupPublish, UsageGroupPublish,
		}},
		{"npm", false, []string{
			UsageGroupAuth, UsageGroupAuth,
			UsageGroupResolve, UsageGroupResolve, UsageGroupResolve, UsageGroupResolve,
		}},
	}
	for _, tc := range cases {
		repoType := "proxy"
		if tc.writable {
			repoType = "hosted"
		}
		repo := &repository.Repository{Name: "demo", Format: tc.format, Type: repoType}
		got := buildUsage(repo, "https://example.test", "zh")
		if len(got) != len(tc.want) {
			t.Fatalf("%s/%s：片段数量应为 %d，实际 %d", tc.format, repoType, len(tc.want), len(got))
		}
		for i, snippet := range got {
			if snippet.Group == "" || !valid[snippet.Group] {
				t.Errorf("%s/%s 第 %d 段 group 非法或为空：%q", tc.format, repoType, i, snippet.Group)
			}
			if snippet.Group != tc.want[i] {
				t.Errorf("%s/%s 第 %d 段 group 应为 %q，实际 %q", tc.format, repoType, i, tc.want[i], snippet.Group)
			}
		}
	}
}

// group 是结构化标记、不参与本地化：同一仓库在不同语言下分组必须完全一致。
func TestBuildUsageGroupIndependentOfLanguage(t *testing.T) {
	for _, format := range []string{"maven", "npm", "raw"} {
		repo := &repository.Repository{Name: "demo", Format: format, Type: "hosted"}
		zh := buildUsage(repo, "https://example.test", "zh")
		en := buildUsage(repo, "https://example.test", "en")
		if len(zh) != len(en) {
			t.Fatalf("%s：两种语言片段数量应一致：zh=%d en=%d", format, len(zh), len(en))
		}
		for i := range zh {
			if zh[i].Group != en[i].Group {
				t.Errorf("%s 第 %d 段分组不应随语言变化：zh=%q en=%q", format, i, zh[i].Group, en[i].Group)
			}
		}
	}
}

// 只读仓库（proxy/group）不得出现任何发布类片段——否则会误导用户去上传。
func TestBuildUsageReadOnlyHasNoPublishSnippets(t *testing.T) {
	for _, format := range []string{"maven", "npm", "raw"} {
		for _, repoType := range []string{"proxy", "group"} {
			repo := &repository.Repository{Name: "demo", Format: format, Type: repoType}
			got := buildUsage(repo, "https://example.test", "zh")
			if len(got) == 0 {
				t.Fatalf("%s/%s：只读仓库也应给出解析片段", format, repoType)
			}
			for i, snippet := range got {
				if snippet.Group == UsageGroupPublish {
					t.Errorf("%s/%s 第 %d 段不应出现发布片段：%q", format, repoType, i, snippet.Title)
				}
			}
		}
	}
	// 可写仓库必须有发布片段，否则上面的断言在「根本没有发布片段」时会假绿。
	hosted := &repository.Repository{Name: "demo", Format: "maven", Type: "hosted"}
	if !hasUsageGroup(buildUsage(hosted, "https://example.test", "zh"), UsageGroupPublish) {
		t.Error("hosted 仓库应包含发布片段")
	}
}

// 每个片段都要有可展示的标题 / 描述与可复制文本，且 group / tool 落在预期集合内；
// 空字段会让界面出现空白卡片或无法做工具切换，故对所有格式 × 类型 × 语言逐一校验。
func TestBuildUsageSnippetsAreComplete(t *testing.T) {
	valid := map[string]bool{
		UsageGroupAuth:    true,
		UsageGroupResolve: true,
		UsageGroupPublish: true,
		UsageGroupOther:   true,
	}
	validTools := map[string]bool{
		UsageToolMaven:     true,
		UsageToolGradle:    true,
		UsageToolGradleKts: true,
		UsageToolSbt:       true,
		UsageToolIvy:       true,
		UsageToolAnt:       true,
		UsageToolNpm:       true,
		UsageToolPnpm:      true,
		UsageToolYarn:      true,
		UsageToolBun:       true,
		UsageToolCurl:      true,
		UsageToolWget:      true,
	}
	for _, format := range []string{"maven", "npm", "raw"} {
		for _, repoType := range []string{"hosted", "proxy", "group"} {
			repo := &repository.Repository{Name: "demo", Format: format, Type: repoType}
			for _, lang := range []string{"zh", "en"} {
				for i, snippet := range buildUsage(repo, "https://example.test", lang) {
					where := fmt.Sprintf("%s/%s/%s 第 %d 段", format, repoType, lang, i)
					if strings.TrimSpace(snippet.Title) == "" {
						t.Errorf("%s：标题为空", where)
					}
					if strings.TrimSpace(snippet.Description) == "" {
						t.Errorf("%s：描述为空", where)
					}
					if strings.TrimSpace(snippet.Code) == "" {
						t.Errorf("%s：命令 / 配置文本为空", where)
					}
					if !valid[snippet.Group] {
						t.Errorf("%s：group 非法或为空：%q", where, snippet.Group)
					}
					if !validTools[snippet.Tool] {
						t.Errorf("%s：tool 非法或为空：%q", where, snippet.Tool)
					}
				}
			}
		}
	}
}

// 每个片段都必须带具体的工具标记，且归类正确
// （maven / gradle / gradle-kts / sbt / ivy / ant / npm / pnpm / yarn / bun / curl / wget）。
// 顺序由组装函数决定，故按索引断言；hosted 才有发布片段，proxy/group 只有解析片段。
func TestBuildUsageAssignsTools(t *testing.T) {
	cases := []struct {
		format   string
		writable bool
		want     []string
	}{
		// raw：解析给 curl×2 + wget×1，发布给 curl×2。
		{"raw", true, []string{UsageToolCurl, UsageToolCurl, UsageToolWget, UsageToolCurl, UsageToolCurl}},
		{"raw", false, []string{UsageToolCurl, UsageToolCurl, UsageToolWget}},
		// maven：认证 maven + gradle + gradle-kts；
		// 解析 maven(pom) + gradle(Groovy) + gradle-kts + sbt + maven(一行式) + ivy + ant；
		// 发布 maven(pom) + gradle(Groovy) + gradle-kts + maven(一行式)。
		// 一行式与 pom.xml 同属 Maven 工具——工具下拉是「用哪个构建工具」，不把同工具的两种用法拆成两个选项。
		{"maven", true, []string{
			UsageToolMaven, UsageToolGradle, UsageToolGradleKts,
			UsageToolMaven, UsageToolGradle, UsageToolGradleKts, UsageToolSbt, UsageToolMaven, UsageToolIvy, UsageToolAnt,
			UsageToolMaven, UsageToolGradle, UsageToolGradleKts, UsageToolMaven,
		}},
		{"maven", false, []string{
			UsageToolMaven, UsageToolGradle, UsageToolGradleKts,
			UsageToolMaven, UsageToolGradle, UsageToolGradleKts, UsageToolSbt, UsageToolMaven, UsageToolIvy, UsageToolAnt,
		}},
		// npm：认证 npm×2；解析 npm + pnpm + yarn + bun；发布 npm×2。
		{"npm", true, []string{
			UsageToolNpm, UsageToolNpm,
			UsageToolNpm, UsageToolPnpm, UsageToolYarn, UsageToolBun,
			UsageToolNpm, UsageToolNpm,
		}},
		{"npm", false, []string{
			UsageToolNpm, UsageToolNpm,
			UsageToolNpm, UsageToolPnpm, UsageToolYarn, UsageToolBun,
		}},
	}
	for _, tc := range cases {
		repoType := "proxy"
		if tc.writable {
			repoType = "hosted"
		}
		repo := &repository.Repository{Name: "demo", Format: tc.format, Type: repoType}
		got := buildUsage(repo, "https://example.test", "zh")
		if len(got) != len(tc.want) {
			t.Fatalf("%s/%s：片段数量应为 %d，实际 %d", tc.format, repoType, len(tc.want), len(got))
		}
		for i, snippet := range got {
			if snippet.Tool != tc.want[i] {
				t.Errorf("%s/%s 第 %d 段 tool 应为 %q，实际 %q", tc.format, repoType, i, tc.want[i], snippet.Tool)
			}
		}
	}
}

// tool 是结构化标记、不参与本地化：同一仓库在不同语言下工具归类必须完全一致。
func TestBuildUsageToolIndependentOfLanguage(t *testing.T) {
	for _, format := range []string{"maven", "npm", "raw"} {
		repo := &repository.Repository{Name: "demo", Format: format, Type: "hosted"}
		zh := buildUsage(repo, "https://example.test", "zh")
		en := buildUsage(repo, "https://example.test", "en")
		if len(zh) != len(en) {
			t.Fatalf("%s：两种语言片段数量应一致：zh=%d en=%d", format, len(zh), len(en))
		}
		for i := range zh {
			if zh[i].Tool != en[i].Tool {
				t.Errorf("%s 第 %d 段工具不应随语言变化：zh=%q en=%q", format, i, zh[i].Tool, en[i].Tool)
			}
		}
	}
}

// usageTexts 的每个字段都必须在每种语言下都有值：漏翻译会让界面出现空标题 / 空描述。
// 用反射遍历全部字段，新增文案字段时自动纳入校验，无需手工补断言。
func TestUsageTextsCompleteForEveryLanguage(t *testing.T) {
	textType := reflect.TypeOf(usageTexts{})
	for _, lang := range []string{"zh", "en"} {
		texts, ok := usageTextsByLang[lang]
		if !ok {
			t.Fatalf("缺少语言 %q 的文案表", lang)
		}
		value := reflect.ValueOf(texts)
		for i := 0; i < textType.NumField(); i++ {
			if strings.TrimSpace(value.Field(i).String()) == "" {
				t.Errorf("语言 %q 的文案键 %s 为空", lang, textType.Field(i).Name)
			}
		}
	}
}

// 用户明确要求「使用说明要有多种工具」：按标题抽查各格式覆盖到的工具，
// 避免后续重构把这些片段悄悄删掉（只动标题会被发现，但删整段不会）。
func TestBuildUsageCoversMultipleTools(t *testing.T) {
	cases := []struct {
		format  string
		keyword string // 应出现在某段标题里的工具 / 文件标志
	}{
		{"maven", "settings.xml"},
		{"maven", "gradle.properties"},
		{"maven", "pom.xml"},
		{"maven", "Gradle"},
		{"maven", "Gradle Kotlin DSL"},
		{"maven", "sbt"},
		{"maven", "mvn 参数"},
		{"maven", "Ivy"},
		{"maven", "Ant"},
		{"maven", "mvn deploy"},
		{"npm", "配置 registry"},
		{"npm", ".npmrc"},
		{"npm", "（npm）"},
		{"npm", "pnpm"},
		{"npm", "Yarn"},
		{"npm", "（bun）"},
		{"npm", "publishConfig"},
		{"raw", "curl"},
		{"raw", "wget"},
	}
	for _, tc := range cases {
		repo := &repository.Repository{Name: "demo", Format: tc.format, Type: "hosted"}
		titles := make([]string, 0, 4)
		for _, snippet := range buildUsage(repo, "https://example.test", "zh") {
			titles = append(titles, snippet.Title)
		}
		joined := strings.Join(titles, " | ")
		if !strings.Contains(joined, tc.keyword) {
			t.Errorf("%s：标题中应包含 %q，实际 %q", tc.format, tc.keyword, joined)
		}
	}
}

// 两种 Gradle 变体（gradle / gradle-kts）都必须带 gradle.properties 认证片段，且内容一致——
// gradle.properties 与 DSL 无关，在两个工具下各给一份是为了让任一 Gradle 变体的视图自包含。
func TestBuildUsageGradleVariantsShareAuthSnippet(t *testing.T) {
	repo := &repository.Repository{Name: "demo", Format: "maven", Type: "hosted"}
	snippets := buildUsage(repo, "https://example.test", "zh")
	var gradleAuth, ktsAuth *UsageSnippet
	for index := range snippets {
		snippet := snippets[index]
		if snippet.Group != UsageGroupAuth || !strings.Contains(snippet.Code, "gradle.properties") {
			continue
		}
		switch snippet.Tool {
		case UsageToolGradle:
			gradleAuth = &snippet
		case UsageToolGradleKts:
			ktsAuth = &snippet
		}
	}
	if gradleAuth == nil {
		t.Fatal("gradle 变体缺少 gradle.properties 认证片段")
	}
	if ktsAuth == nil {
		t.Fatal("gradle-kts 变体缺少 gradle.properties 认证片段")
	}
	if gradleAuth.Code != ktsAuth.Code {
		t.Errorf("两种 Gradle 变体的认证片段代码应一致：\ngradle=%q\ngradle-kts=%q", gradleAuth.Code, ktsAuth.Code)
	}
}

// hasUsageGroup 判断片段列表里是否存在指定分组的片段。
func hasUsageGroup(snippets []UsageSnippet, group string) bool {
	for _, snippet := range snippets {
		if snippet.Group == group {
			return true
		}
	}
	return false
}
