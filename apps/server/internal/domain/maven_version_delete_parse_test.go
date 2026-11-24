package domain

import (
	"strings"
	"testing"
)

// TestParseSnapshotPointedBuildCoversFragmentedAndOversize 钉住两个解析边界：
// 1) XML 注释 / CDATA 把一个值切成多个 CharData 片段时，不能取首段截断值；
// 2) 大于 4MiB（历史上直接拒绝）的合法 metadata 依然能读出指向构建；
// 3) 输入在字段中途被限长截断时按部分值返回，由调用方判定「指向不明→不删」。
func TestParseSnapshotPointedBuildCoversFragmentedAndOversize(t *testing.T) {
	cases := []struct {
		name string
		body string
		ts   string
		bn   string
	}{
		{
			name: "常规",
			body: `<metadata><versioning><snapshot><timestamp>20240101.120000</timestamp><buildNumber>7</buildNumber></snapshot></versioning></metadata>`,
			ts:   "20240101.120000",
			bn:   "7",
		},
		{
			name: "注释切开片段",
			body: `<metadata><versioning><snapshot><timestamp>20240101<!--mid-->.120000</timestamp><buildNumber><![CDATA[7]]></buildNumber></snapshot></versioning></metadata>`,
			ts:   "20240101.120000",
			bn:   "7",
		},
		{
			name: "CDATA 与混排",
			body: `<metadata><versioning><snapshot><timestamp><![CDATA[2024]]>0101<!--x-->.120000</timestamp><buildNumber>1<!--y-->2</buildNumber></snapshot></versioning></metadata>`,
			ts:   "20240101.120000",
			bn:   "12",
		},
		{
			name: "字段中途截断",
			body: `<metadata><versioning><snapshot><timestamp>20240`,
			ts:   "",
			bn:   "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts, bn := parseSnapshotPointedBuild(strings.NewReader(tc.body))
			if ts != tc.ts || bn != tc.bn {
				t.Fatalf("得 (%q,%q)，应 (%q,%q)", ts, bn, tc.ts, tc.bn)
			}
		})
	}

	// >4MiB 合法 metadata：历史实现按大小上限整块跳过会把指向存活构建的元数据
	// 误判成「指向不明」而删元数据，剩余构建直接 404。限长流式解析不应有此问题。
	t.Run("超大合法元数据", func(t *testing.T) {
		padding := strings.Repeat("<!--"+strings.Repeat("x", 4096)+"-->", 1200) // ≈5MiB
		body := `<metadata><versioning><snapshot>` + padding +
			`<timestamp>20240101.120000</timestamp><buildNumber>42</buildNumber></snapshot></versioning></metadata>`
		ts, bn := parseSnapshotPointedBuild(strings.NewReader(body))
		if ts != "20240101.120000" || bn != "42" {
			t.Fatalf("超大元数据应读出指向构建：(%q,%q)", ts, bn)
		}
	})
}
