package domain

import (
	"reflect"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// TestParseSearchQuery 覆盖高级搜索表达式的各类 token 解析。
func TestParseSearchQuery(t *testing.T) {
	cases := []struct {
		name string
		q    string
		want parsedSearch
	}{
		{
			name: "普通多词为 AND 包含",
			q:    "spring core",
			want: parsedSearch{filter: repository.SearchFilter{IncludeTerms: []string{"spring", "core"}}},
		},
		{
			name: "负筛选关键词",
			q:    "spring -sources -javadoc",
			want: parsedSearch{filter: repository.SearchFilter{
				IncludeTerms: []string{"spring"},
				ExcludeTerms: []string{"sources", "javadoc"},
			}},
		},
		{
			name: "引号短语保留空格",
			q:    `"my lib" -"backup dir"`,
			want: parsedSearch{filter: repository.SearchFilter{
				IncludeTerms: []string{"my lib"},
				ExcludeTerms: []string{"backup dir"},
			}},
		},
		{
			name: "repo 与 format 限定及排除",
			q:    "guava repo:maven-releases -repo:legacy format:maven -format:raw",
			want: parsedSearch{
				filter:     repository.SearchFilter{IncludeTerms: []string{"guava"}},
				repos:      []string{"maven-releases"},
				notRepos:   []string{"legacy"},
				formats:    []string{"maven"},
				notFormats: []string{"raw"},
			},
		},
		{
			name: "ext 归一化（去点、小写）与排除",
			q:    "log4j ext:.JAR -ext:sha1 -ext:md5",
			want: parsedSearch{filter: repository.SearchFilter{
				IncludeTerms: []string{"log4j"},
				IncludeExts:  []string{"jar"},
				ExcludeExts:  []string{"sha1", "md5"},
			}},
		},
		{
			name: "空值 token 静默丢弃、单独连字符按词处理",
			q:    "repo: ext: format: - x",
			want: parsedSearch{filter: repository.SearchFilter{IncludeTerms: []string{"-", "x"}}},
		},
		{
			name: "仅过滤条件无关键词",
			q:    "format:maven -ext:sha1",
			want: parsedSearch{
				filter:  repository.SearchFilter{ExcludeExts: []string{"sha1"}},
				formats: []string{"maven"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseSearchQuery(tc.q)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("parseSearchQuery(%q)\n got  %+v\n want %+v", tc.q, got, tc.want)
			}
		})
	}
}

// TestParsedSearchHasRepoMeta 验证 hasRepoMeta 判定。
func TestParsedSearchHasRepoMeta(t *testing.T) {
	if parseSearchQuery("spring -sources ext:jar").hasRepoMeta() {
		t.Error("纯路径条件不应需要仓库元数据")
	}
	for _, q := range []string{"repo:a", "-repo:a", "format:maven", "-format:raw"} {
		if !parseSearchQuery(q).hasRepoMeta() {
			t.Errorf("%q 应需要仓库元数据", q)
		}
	}
}
