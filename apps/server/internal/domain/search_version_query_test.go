package domain

import (
	"reflect"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// 本文件钉住 FR-150 §3.6 的 `version:` 语法解析。既有 TestParseSearchQuery 的断言不动，
// 这里只针对新分支补用例。

// TestParseSearchQueryVersions 覆盖 version:/-version: 的解析口径，
// 以及最关键的一条：**版本值绝不能落进 IncludeTerms**（否则失去"严格相等"语义）。
func TestParseSearchQueryVersions(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		q    string
		want parsedSearch
	}{
		{
			name: "单个版本限定：值只进版本条件",
			q:    "version:1.17.12",
			want: parsedSearch{
				versions: []string{"1.17.12"},
				filter:   repository.SearchFilter{IncludeVersions: []string{"1.17.12"}},
			},
		},
		{
			name: "多个版本限定为 OR",
			q:    "version:1.0.0 version:2.0.0",
			want: parsedSearch{
				versions: []string{"1.0.0", "2.0.0"},
				filter:   repository.SearchFilter{IncludeVersions: []string{"1.0.0", "2.0.0"}},
			},
		},
		{
			name: "排除版本",
			q:    "-version:1.0.0 -version:1.0.1",
			want: parsedSearch{
				notVersions: []string{"1.0.0", "1.0.1"},
				filter:      repository.SearchFilter{ExcludeVersions: []string{"1.0.0", "1.0.1"}},
			},
		},
		{
			name: "空值静默丢弃",
			q:    "version: -version: pkg",
			want: parsedSearch{
				filter: repository.SearchFilter{IncludeTerms: []string{"pkg"}},
			},
		},
		{
			name: "版本条件与普通词、ext 共存",
			q:    "foo version:1.17.12 ext:jar -ext:sha1",
			want: parsedSearch{
				versions: []string{"1.17.12"},
				filter: repository.SearchFilter{
					IncludeTerms:    []string{"foo"},
					IncludeExts:     []string{"jar"},
					ExcludeExts:     []string{"sha1"},
					IncludeVersions: []string{"1.17.12"},
				},
			},
		},
		{
			name: "大小写前缀也识别（与 ext: 同级）",
			q:    "VERSION:1.17.12 -Version:2.0.0",
			want: parsedSearch{
				versions:    []string{"1.17.12"},
				notVersions: []string{"2.0.0"},
				filter: repository.SearchFilter{
					IncludeVersions: []string{"1.17.12"},
					ExcludeVersions: []string{"2.0.0"},
				},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseSearchQuery(tc.q)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("parseSearchQuery(%q)\n got  %+v\n want %+v", tc.q, got, tc.want)
			}
			// 关键回归：版本值不得作为子串词参与路径匹配。
			for _, term := range got.filter.IncludeTerms {
				if term == "1.17.12" || term == "1.0.0" || term == "2.0.0" {
					t.Fatalf("版本值 %q 落进了 IncludeTerms，会退化为子串匹配", term)
				}
			}
		})
	}
}
