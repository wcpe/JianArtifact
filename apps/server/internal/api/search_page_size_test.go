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

// 页码解析口径（fix：page 无上界导致 offset 整数溢出为负）。
//
// 关键回归点：极大的 page 会让 offset = (page-1)*pageSize 溢出成**负数**，而 SQLite 把负
// OFFSET 当 0 —— 表现为「静默返回第一页」，既不报错也不返回空页，调用方无从察觉参数被吞。
// 因此 page 必须有上界，且口径与 page_size 一致：夹取而非 400。
func TestParseSearchPage(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want int
	}{
		{"缺省回落第一页", "", 1},
		{"零值回落第一页", "0", 1},
		{"负数回落第一页", "-5", 1},
		{"非数字回落第一页", "abc", 1},
		{"常规值原样采纳", "3", 3},
		{"恰为上限原样采纳", "10000", 10000},
		{"超上限一档夹取到上限", "10001", 10000},
		{"极大值夹取到上限（不得溢出）", "9223372036854775807", 10000},
		{"超出 int 范围也回落到第一页", "99999999999999999999", 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := parseSearchPage(c.raw); got != c.want {
				t.Errorf("parseSearchPage(%q) = %d, want %d", c.raw, got, c.want)
			}
		})
	}
}

// TestSearchOffsetNeverNegative 直接钉住「offset 不得为负」这个可观测后果：
// 页码与每页条数都取到各自上限时，乘积也必须仍在正数范围。
func TestSearchOffsetNeverNegative(t *testing.T) {
	for _, page := range []string{"1", "10000", "999999999999"} {
		for _, size := range []string{"20", "100", "4294967296"} {
			p := parseSearchPage(page)
			ps := parseSearchPageSize(size)
			offset := (p - 1) * ps
			if offset < 0 {
				t.Errorf("page=%s page_size=%s 得到负 offset=%d（负 OFFSET 在 SQLite 里等同 0，会静默返回第一页）",
					page, size, offset)
			}
		}
	}

	// 共用分页助手同样不得溢出：极大页码 + 极大每页条数。
	hugePage, hugeSize := 1<<62, 1<<40
	limit, offset := pageOffset(&hugePage, &hugeSize)
	if limit != searchPageSizeMax {
		t.Errorf("pageOffset 每页条数 = %d，期望夹取到 %d", limit, searchPageSizeMax)
	}
	if offset < 0 {
		t.Errorf("pageOffset 得到负 offset=%d", offset)
	}
}
