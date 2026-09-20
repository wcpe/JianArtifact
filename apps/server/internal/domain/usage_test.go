package domain

import (
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
