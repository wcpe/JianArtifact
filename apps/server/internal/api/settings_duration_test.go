package api

import (
	"testing"
)

// TestResolveBackupAutoInterval 覆盖"表达式优先于秒字段"的取值规则。
//
// 这是 的接口语义：前端提交的是 "1d" 这样的表达式，
// 而秒字段只为兼容既有调用方保留。两者同时给出时表达式必须赢——
// 否则界面上的单位选择会被静默忽略，用户看到的与实际生效的不一致。
func TestResolveBackupAutoInterval(t *testing.T) {
	t.Parallel()

	intPtr := func(v int) *int { return &v }
	strPtr := func(v string) *string { return &v }

	tests := []struct {
		name      string
		req       SettingsRequest
		want      *int
		wantError bool
	}{
		{
			name: "只给秒字段时原样采用",
			req:  SettingsRequest{BackupAutoInterval: intPtr(60)},
			want: intPtr(60),
		},
		{
			name: "只给表达式时换算成秒",
			req:  SettingsRequest{BackupAutoIntervalExpr: strPtr("5m")},
			want: intPtr(300),
		},
		{
			name: "两者都给时表达式优先",
			req: SettingsRequest{
				BackupAutoInterval:     intPtr(999),
				BackupAutoIntervalExpr: strPtr("2h"),
			},
			want: intPtr(7200),
		},
		{
			name: "都不给时不改这一项",
			req:  SettingsRequest{},
			want: nil,
		},
		{
			name: "表达式为 0 表示关闭",
			req:  SettingsRequest{BackupAutoIntervalExpr: strPtr("0")},
			want: intPtr(0),
		},
		{
			name: "裸数字按秒（保持与旧行为一致）",
			req:  SettingsRequest{BackupAutoIntervalExpr: strPtr("45")},
			want: intPtr(45),
		},
		{
			name:      "非法表达式报错",
			req:       SettingsRequest{BackupAutoIntervalExpr: strPtr("abc")},
			wantError: true,
		},
		{
			name:      "未知单位报错",
			req:       SettingsRequest{BackupAutoIntervalExpr: strPtr("5x")},
			wantError: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := resolveBackupAutoInterval(tc.req)
			if tc.wantError {
				if err == nil {
					t.Fatalf("应报错，实得 %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("不应报错：%v", err)
			}
			if tc.want == nil {
				if got != nil {
					t.Fatalf("期望不修改，实得 %d", *got)
				}
				return
			}
			if got == nil {
				t.Fatalf("期望 %d，实得 nil", *tc.want)
			}
			if *got != *tc.want {
				t.Fatalf("期望 %d，实得 %d", *tc.want, *got)
			}
		})
	}
}

// TestValidateBackupSettingsWithExpr 覆盖校验与表达式的配合：
// 有表达式时不再对秒字段重复校验（否则同一个值会报两次错）。
func TestValidateBackupSettingsWithExpr(t *testing.T) {
	t.Parallel()

	intPtr := func(v int) *int { return &v }
	strPtr := func(v string) *string { return &v }

	tests := []struct {
		name      string
		req       SettingsRequest
		wantError bool
	}{
		{
			name: "合法表达式通过",
			req:  SettingsRequest{BackupAutoIntervalExpr: strPtr("1d")},
		},
		{
			name: "超上限的表达式被钳制而非报错",
			req:  SettingsRequest{BackupAutoIntervalExpr: strPtr("100y")},
		},
		{
			name:      "非法表达式被拒",
			req:       SettingsRequest{BackupAutoIntervalExpr: strPtr("5x")},
			wantError: true,
		},
		{
			// 关键：秒字段超限但表达式合法时不该报错——表达式已覆盖语义。
			name: "有合法表达式时忽略秒字段的上限",
			req: SettingsRequest{
				BackupAutoInterval:     intPtr(99999999),
				BackupAutoIntervalExpr: strPtr("1h"),
			},
		},
		{
			name:      "只给秒字段且超上限被拒",
			req:       SettingsRequest{BackupAutoInterval: intPtr(99999999)},
			wantError: true,
		},
		{
			name:      "负数秒被拒",
			req:       SettingsRequest{BackupAutoInterval: intPtr(-1)},
			wantError: true,
		},
		{
			name:      "负份数被拒",
			req:       SettingsRequest{BackupKeepLast: intPtr(-1)},
			wantError: true,
		},
		{
			name: "0 是合法取值",
			req:  SettingsRequest{BackupAutoInterval: intPtr(0), BackupKeepLast: intPtr(0)},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validateBackupSettings(tc.req)
			if tc.wantError && err == nil {
				t.Fatal("应报错")
			}
			if !tc.wantError && err != nil {
				t.Fatalf("不应报错：%v", err)
			}
		})
	}
}

// TestDisplayedIntervalIsTreatedAsAbsent 钉住「读-改-写」可用性。
//
// GET /api/v1/settings 返回的 backupAutoIntervalExpr 是**给人看的中文串**
// （"30 秒" / "2 天" / "关闭"），而 PUT 的同名字段是**输入**。二者共用字段名，
// 于是「把 GET 结果原样 PUT 回去」这种最自然的用法曾经必然 400
// （未知的时间单位后缀 "秒"）——脚本化与第三方客户端一定会踩到。
//
// 修复口径：识别出回显格式时视为「未提供本字段」，交由秒字段决定。
// 本用例同时钉住反方向：任意真正的错误输入（"abc 天"、"30 光年"）仍然报错，
// 不能因为加了这个判断而把错误输入静默吞掉。
func TestDisplayedIntervalIsTreatedAsAbsent(t *testing.T) {
	t.Parallel()

	intPtr := func(v int) *int { return &v }
	strPtr := func(v string) *string { return &v }

	t.Run("回显值视为未提供，回落到秒字段", func(t *testing.T) {
		t.Parallel()
		for _, displayed := range []string{"30 秒", "5 分钟", "2 小时", "1 天", "2 周", "1 个月", "1 年", "关闭"} {
			req := SettingsRequest{
				BackupAutoIntervalExpr: strPtr(displayed),
				BackupAutoInterval:     intPtr(7200),
			}
			if err := validateBackupSettings(req); err != nil {
				t.Fatalf("回显值 %q 不应报错：%v", displayed, err)
			}
			got, err := resolveBackupAutoInterval(req)
			if err != nil {
				t.Fatalf("回显值 %q 解析应回落到秒字段：%v", displayed, err)
			}
			if got == nil || *got != 7200 {
				t.Fatalf("回显值 %q 应回落到秒字段 7200，实际 %v", displayed, got)
			}
		}
	})

	t.Run("真正的错误输入仍然报错", func(t *testing.T) {
		t.Parallel()
		for _, bad := range []string{"abc 天", "30 光年", "天", " ", "1.5 天"} {
			req := SettingsRequest{BackupAutoIntervalExpr: strPtr(bad), BackupAutoInterval: intPtr(60)}
			if err := validateBackupSettings(req); err == nil {
				t.Fatalf("输入 %q 应报错，却被接受", bad)
			}
		}
	})

	t.Run("合法英文表达式不受影响", func(t *testing.T) {
		t.Parallel()
		cases := map[string]int{"30": 30, "5m": 300, "2h": 7200, "1d": 86400}
		for expr, want := range cases {
			req := SettingsRequest{BackupAutoIntervalExpr: strPtr(expr), BackupAutoInterval: intPtr(1)}
			got, err := resolveBackupAutoInterval(req)
			if err != nil {
				t.Fatalf("表达式 %q 解析失败：%v", expr, err)
			}
			if got == nil || *got != want {
				t.Fatalf("表达式 %q 应换算为 %d，实际 %v", expr, want, got)
			}
		}
	})
}
