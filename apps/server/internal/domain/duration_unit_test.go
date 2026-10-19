package domain

import (
	"testing"
)

// TestParseDurationExpression 覆盖人类表达 → 秒的换算，含全部单位与边界。
func TestParseDurationExpression(t *testing.T) {
	t.Parallel()
	const max = 30 * 24 * 3600 // 与 settings_handlers 的上限一致

	tests := []struct {
		name    string
		input   string
		want    int64
		wantErr bool
	}{
		// 裸数字 = 秒：既有配置的含义不能被改变，否则升级后间隔会静默变成另一个值。
		{"裸数字按秒", "30", 30, false},
		{"零按关闭", "0", 0, false},
		{"秒后缀", "45s", 45, false},
		{"sec", "45sec", 45, false},
		{"seconds", "45seconds", 45, false},
		{"分钟", "5m", 300, false},
		{"min", "5min", 300, false},
		{"minutes", "5minutes", 300, false},
		{"小时", "2h", 7200, false},
		{"hour", "1hour", 3600, false},
		{"天", "1d", 86400, false},
		{"days", "7days", 7 * 86400, false},
		{"周", "1w", 7 * 86400, false},
		{"月（mo 而非 m）", "1mo", 30 * 86400, false},
		{"months 超上限被钳制", "2months", max, false},
		{"年 超上限被钳制", "1y", max, false},
		{"大写与小写等价", "1D", 86400, false},
		{"前后空白", "  1h  ", 3600, false},
		// 0 恒等于关闭，不受单位影响——否则"0 个月"会成为一个既非开也非关的怪状态。
		{"零带单位仍是关闭", "0d", 0, false},
		// 上限钳制而不是报错：用户填"100 年"的意图是"尽长时间"。
		{"超上限钳制", "100y", max, false},
		{"恰好上限不钳制", "30d", max, false},
		// 错误分支
		{"空串", "", 0, true},
		{"非数字开头", "abc", 0, true},
		{"未知单位", "5x", 0, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseDurationExpression(tc.input, max)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("%q 应报错，实得 %d", tc.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("%q 解析失败：%v", tc.input, err)
			}
			if got != tc.want {
				t.Fatalf("%q = %d，期望 %d", tc.input, got, tc.want)
			}
		})
	}
}

// TestParseDurationExpressionDistinguishesMinuteFromMonth 守护 m 与 mo 的歧义。
//
// 这是本功能最危险的坑：把 "1m" 误解成一个月会有 43200 倍的偏差。
// 单独成例而不是塞进表里，是为了让失败信息直接说明"这是分钟不是月"。
func TestParseDurationExpressionDistinguishesMinuteFromMonth(t *testing.T) {
	t.Parallel()
	const max = 30 * 24 * 3600
	minute, err := ParseDurationExpression("1m", max)
	if err != nil {
		t.Fatalf("1m：%v", err)
	}
	if minute != 60 {
		t.Fatalf("1m 应是 60 秒（分钟），实得 %d", minute)
	}
	month, err := ParseDurationExpression("1mo", max)
	if err != nil {
		t.Fatalf("1mo：%v", err)
	}
	if month != 30*86400 {
		t.Fatalf("1mo 应是 30 天，实得 %d", month)
	}
	if minute*1000 >= month {
		t.Fatal("分钟与月必须相差数个数量级，否则说明单位映射写反了")
	}
}

// TestDurationToPartsPicksLargestDivisibleUnit 覆盖回填：秒应显示成最合适的单位。
func TestDurationToPartsPicksLargestDivisibleUnit(t *testing.T) {
	t.Parallel()
	tests := []struct {
		seconds int64
		wantVal int64
		wantUnt DurationUnit
	}{
		{0, 0, DurationSeconds},
		{30, 30, DurationSeconds},
		{60, 1, DurationMinutes},
		{300, 5, DurationMinutes},
		{3600, 1, DurationHours},
		{7200, 2, DurationHours},
		{86400, 1, DurationDays},
		{7 * 86400, 7, DurationDays},    // 恰 7 天：天优先于周，用户认得出自己填的值
		{14 * 86400, 14, DurationDays},  // 天优先于周：14 天即 2 周，显示成天更贴近用户输入
		{90 * 86400, 3, DurationMonths}, // 90 天 = 3 个月，月优先于天
		{30 * 86400, 1, DurationMonths},
		{365 * 86400, 1, DurationYears},
		// 不能整除时回落到秒，绝不产生"1.5 天"这种小数表达。
		{90, 90, DurationSeconds},
		{3661, 3661, DurationSeconds},
	}
	for _, tc := range tests {
		gotVal, gotUnt := DurationToParts(tc.seconds)
		if gotVal != tc.wantVal || gotUnt != tc.wantUnt {
			t.Fatalf("%d 秒应拆成 %d %s，实得 %d %s",
				tc.seconds, tc.wantVal, tc.wantUnt, gotVal, gotUnt)
		}
	}
}

// TestComposeDurationRoundTrips 覆盖"数值 + 单位"→秒→再拆回来的往返一致性。
func TestComposeDurationRoundTrips(t *testing.T) {
	t.Parallel()
	const max = 30 * 24 * 3600
	cases := []struct {
		value int64
		unit  DurationUnit
		want  int64
	}{
		{30, DurationSeconds, 30},
		{5, DurationMinutes, 300},
		{2, DurationHours, 7200},
		{1, DurationDays, 86400},
		{30, DurationDays, max},
		{0, DurationDays, 0},
	}
	for _, tc := range cases {
		got, err := ComposeDuration(tc.value, tc.unit, max)
		if err != nil {
			t.Fatalf("%d%s：%v", tc.value, tc.unit, err)
		}
		if got != tc.want {
			t.Fatalf("%d%s = %d，期望 %d", tc.value, tc.unit, got, tc.want)
		}
		// 非零且不超上限时，拆回来应还原成同一组（值, 单位）。
		if got > 0 && got < max {
			backVal, backUnit := DurationToParts(got)
			recomposed, err := ComposeDuration(backVal, backUnit, max)
			if err != nil {
				t.Fatalf("回拆后重组：%v", err)
			}
			if recomposed != got {
				t.Fatalf("往返不一致：%d → (%d,%s) → %d", got, backVal, backUnit, recomposed)
			}
		}
	}
}

// TestComposeDurationRejectsNegative 覆盖负数拒绝（0 合法，负数不是）。
func TestComposeDurationRejectsNegative(t *testing.T) {
	t.Parallel()
	if _, err := ComposeDuration(-1, DurationHours, 0); err == nil {
		t.Fatal("负数应被拒绝")
	}
	if got, err := ComposeDuration(0, DurationHours, 0); err != nil || got != 0 {
		t.Fatalf("0 应是合法关闭值：got=%d err=%v", got, err)
	}
}

// TestComposeDurationNoOverflow 覆盖极端输入的溢出保护：
// 不许因乘法溢出而返回一个负数或小的正数（那会把"很久"变成"很频繁"）。
func TestComposeDurationNoOverflow(t *testing.T) {
	t.Parallel()
	const max = 30 * 24 * 3600
	got, err := ComposeDuration(1<<62, DurationYears, max)
	if err != nil {
		t.Fatalf("超大输入不应报错（应钳制）：%v", err)
	}
	if got != max {
		t.Fatalf("超大输入应钳制到上限 %d，实得 %d", max, got)
	}
}

// TestFormatDurationHuman 覆盖中文渲染（日志与摘要用）。
func TestFormatDurationHuman(t *testing.T) {
	t.Parallel()
	tests := []struct {
		seconds int64
		want    string
	}{
		{0, "关闭"},
		{30, "30 秒"},
		{300, "5 分钟"},
		{7200, "2 小时"},
		{86400, "1 天"},
		{30 * 86400, "1 个月"},
		{365 * 86400, "1 年"},
	}
	for _, tc := range tests {
		if got := FormatDurationHuman(tc.seconds); got != tc.want {
			t.Fatalf("%d 应渲染为 %q，实得 %q", tc.seconds, tc.want, got)
		}
	}
}
