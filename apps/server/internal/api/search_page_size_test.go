package api

import "testing"

// 搜索分页解析口径的回归守卫（fix：仓库内搜索「只搜到前几个结果」的根因）。
//
// 关键回归点：**超上限必须夹取到上限**，而不是静默回落默认 20——
// 此前实现是「n <= 100 才采纳，否则整个忽略」，请求 200 条会被静默丢弃、
// 回落默认 20 条，调用方（前端 page_size=200）与用户都无任何信号，
// 表现为「搜索结果永远只有前 20 条」。
func TestParseSearchPageSize(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want int
	}{
		{"缺省回落默认", "", 20},
		{"零值回落默认", "0", 20},
		{"负数回落默认", "-5", 20},
		{"非数字回落默认", "abc", 20},
		{"常规值原样采纳", "50", 50},
		{"恰为上限原样采纳", "100", 100},
		{"超上限一档夹取到上限", "101", 100},
		{"远超上限夹取到上限", "5000", 100},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := parseSearchPageSize(c.raw); got != c.want {
				t.Errorf("parseSearchPageSize(%q) = %d, want %d", c.raw, got, c.want)
			}
		})
	}
}
