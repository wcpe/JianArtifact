package config

import "testing"

// TestSQLiteMaxConnsParsing 覆盖 JIAN_SQLITE_MAX_CONNS 的解析口径。
//
// 本项**无禁用语义**：池上限取 0 在 database/sql 里表示「无上限」，既不是想要的语义，
// 也会让 SQLite 侧的页面缓存副本与文件句柄随负载无界增长。因此缺省、非数字、负数与
// 显式 0 一律回落默认 8（口径同 searchTimeout / autoBlockBase 的「取 0 无意义」）。
//
// 另有**上界**（maxSQLiteMaxConns）：纯数量配置多打一个 0 是现实存在的笔误，
// 超限夹取到上界而不是照单全收——本函数只断言结果值，警告文本由人工核对日志。
func TestSQLiteMaxConnsParsing(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  int
	}{
		{name: "缺省为八", value: "", want: 8},
		{name: "零回落默认", value: "0", want: 8},
		{name: "负数回落默认", value: "-4", want: 8},
		{name: "非数字回落默认", value: "abc", want: 8},
		{name: "带单位回落默认", value: "8条", want: 8},
		{name: "显式一仍生效", value: "1", want: 1},
		{name: "显式值生效", value: "16", want: 16},
		{name: "两侧空白被裁剪", value: "  12  ", want: 12},
		{name: "等于上界不夹取", value: "64", want: 64},
		// 数量级笔误（8 → 80）：夹取到上界而不是照单全收。照收会开出 80 条连接与
		// 文件句柄、每条各带一份 SQLite 页面缓存，而日志里没有任何异常。
		{name: "超过上界被夹取", value: "80", want: 64},
		{name: "远超上界被夹取", value: "9999", want: 64},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(EnvSQLiteMaxConns, tc.value)
			if got := sqliteMaxConns(); got != tc.want {
				t.Fatalf("%s=%q 解析结果=%d，期望=%d", EnvSQLiteMaxConns, tc.value, got, tc.want)
			}
		})
	}
}

// TestLoadCarriesSQLiteMaxConns 验证配置值确实被装配到 Config 上：
// 解析函数的单测绿了但字段没赋值，是这类改动最典型的静默失灵。
func TestLoadCarriesSQLiteMaxConns(t *testing.T) {
	t.Setenv(EnvDataDir, t.TempDir())
	t.Setenv(EnvSQLiteMaxConns, "5")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load：%v", err)
	}
	if cfg.SQLiteMaxConns != 5 {
		t.Fatalf("Config.SQLiteMaxConns = %d，期望 5", cfg.SQLiteMaxConns)
	}
}

// TestLoadDefaultsSQLiteMaxConns 验证未配置时 Config 取默认 8。
func TestLoadDefaultsSQLiteMaxConns(t *testing.T) {
	t.Setenv(EnvDataDir, t.TempDir())
	t.Setenv(EnvSQLiteMaxConns, "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load：%v", err)
	}
	if cfg.SQLiteMaxConns != 8 {
		t.Fatalf("Config.SQLiteMaxConns = %d，期望默认 8", cfg.SQLiteMaxConns)
	}
}
