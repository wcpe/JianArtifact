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
