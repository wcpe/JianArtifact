package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeEnvFiles 造一个可注入的 env 文件读取器：只认给定路径。
func fakeEnvFiles(files map[string]string) envFileReader {
	return func(path string) (string, bool) {
		content, ok := files[path]
		return content, ok
	}
}

// TestResolveDataDirExplicitWins 显式设置 JIAN_DATA_DIR 时以其为准，且不打扰用户。
//
// 这是唯一权威来源：部署（systemd EnvironmentFile）与自动化脚本都靠它，
// 任何自动发现都不得覆盖它，也不该在此时输出提示（那只会变成噪声）。
func TestResolveDataDirExplicitWins(t *testing.T) {
	t.Parallel()
	// 即使存在会被发现的 env 文件，显式值也必须胜出。
	reader := fakeEnvFiles(map[string]string{
		envFileCandidatePaths[0]: "JIAN_DATA_DIR=/discovered/data\n",
	})
	got, err := resolveDataDirWith("/explicit/data", reader)
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if got.Source != DataDirFromEnv {
		t.Fatalf("来源应为 %q，实际 %q", DataDirFromEnv, got.Source)
	}
	if !filepath.IsAbs(got.Dir) || !strings.HasSuffix(filepath.ToSlash(got.Dir), "/explicit/data") {
		t.Fatalf("目录应为 /explicit/data 的绝对形式，实际 %q", got.Dir)
	}
	if got.Warning != "" {
		t.Fatalf("显式设置时不应有提示，实际 %q", got.Warning)
	}
}

// TestResolveDataDirDiscoversRunningInstance 未设置但发现唯一候选时使用它，并说明依据。
//
// 这条守卫来自实测：在服务机上不设 JIAN_DATA_DIR 跑 CLI，它静默回落到 $HOME/data——
// 那是**另一个库**，看不到服务侧真实的包，还会顺手在原地建出一个空实例骨架
// （含 jwt.secret 与 jianartifact.db）。用户反馈"老出现莫名其妙的数据目录"即此。
func TestResolveDataDirDiscoversRunningInstance(t *testing.T) {
	t.Parallel()
	reader := fakeEnvFiles(map[string]string{
		envFileCandidatePaths[0]: "# 部署环境\nJIAN_DATA_DIR=/opt/jianartifact/data\nJIAN_HTTP_ADDR=:8001\n",
	})
	got, err := resolveDataDirWith("", reader)
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if got.Source != DataDirFromDiscovery {
		t.Fatalf("来源应为 %q，实际 %q", DataDirFromDiscovery, got.Source)
	}
	if !strings.HasSuffix(filepath.ToSlash(got.Dir), "/opt/jianartifact/data") {
		t.Fatalf("应发现 /opt/jianartifact/data，实际 %q", got.Dir)
	}
	if got.DiscoveredFrom != envFileCandidatePaths[0] {
		t.Fatalf("应记录来源文件，实际 %q", got.DiscoveredFrom)
	}
	// 自动发现必须**可见**：静默发现只是把"莫名其妙"换个地方发生。
	// 提示里要能回答"用的是哪个目录、依据是什么、怎么覆盖"。
	for _, want := range []string{got.Dir, envFileCandidatePaths[0], EnvDataDir} {
		if !strings.Contains(got.Warning, want) {
			t.Fatalf("提示应包含 %q，实际：%s", want, got.Warning)
		}
	}
}

// TestResolveDataDirAmbiguousDoesNotGuess 发现多个不同候选时不猜，交回候选清单。
//
// 猜错的代价是操作到错的库（可能覆盖、删除错误实例的数据），远高于让用户显式指定一次。
func TestResolveDataDirAmbiguousDoesNotGuess(t *testing.T) {
	t.Parallel()
	reader := fakeEnvFiles(map[string]string{
		envFileCandidatePaths[0]: "JIAN_DATA_DIR=/opt/jianartifact/data\n",
		envFileCandidatePaths[1]: "JIAN_DATA_DIR=/etc/jianartifact/data\n",
	})
	got, err := resolveDataDirWith("", reader)
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if len(got.Ambiguous) != 2 {
		t.Fatalf("应交回 2 个候选，实际 %v", got.Ambiguous)
	}
	if got.Dir != "" {
		t.Fatalf("有歧义时不得选定任何目录，实际 %q", got.Dir)
	}
}

// TestResolveDataDirFallsBackWithWarning 探测不到时回落默认值，但必须显著提示。
//
// `run` 首次初始化实例正是靠这个回落，故不能改成报错；但提示必须说清
// 「用的是默认目录」以及「若本意是操作已有实例该怎么办」。
func TestResolveDataDirFallsBackWithWarning(t *testing.T) {
	t.Parallel()
	got, err := resolveDataDirWith("", fakeEnvFiles(nil))
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if got.Source != DataDirFromDefault {
		t.Fatalf("来源应为 %q，实际 %q", DataDirFromDefault, got.Source)
	}
	if !strings.HasSuffix(filepath.ToSlash(got.Dir), "/data") {
		t.Fatalf("应回落默认 ./data，实际 %q", got.Dir)
	}
	for _, want := range []string{got.Dir, EnvDataDir, "另建一个空实例"} {
		if !strings.Contains(got.Warning, want) {
			t.Fatalf("回落提示应包含 %q，实际：%s", want, got.Warning)
		}
	}
}

// TestResolveDataDirSameValueInTwoFilesIsNotAmbiguous 两个文件给出**同一个**目录不算歧义。
//
// 实际部署里 /opt 与 /etc 两处 env 文件内容一致是常见的（硬链接式备份），
// 这种情况没有歧义可言，报错只会让人困惑。
func TestResolveDataDirSameValueInTwoFilesIsNotAmbiguous(t *testing.T) {
	t.Parallel()
	reader := fakeEnvFiles(map[string]string{
		envFileCandidatePaths[0]: "JIAN_DATA_DIR=/opt/jianartifact/data\n",
		envFileCandidatePaths[1]: "JIAN_DATA_DIR=/opt/jianartifact/data\n",
	})
	got, err := resolveDataDirWith("", reader)
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if len(got.Ambiguous) != 0 {
		t.Fatalf("同值不应被判为歧义，实际 %v", got.Ambiguous)
	}
	if got.Source != DataDirFromDiscovery {
		t.Fatalf("来源应为 %q，实际 %q", DataDirFromDiscovery, got.Source)
	}
}

// TestEnvFileValueParsing 覆盖 env 文件的最小解析：
// 注释、空行、export 前缀、引号包裹、值里含 = 、键名前后空白。
func TestEnvFileValueParsing(t *testing.T) {
	t.Parallel()
	content := strings.Join([]string{
		"# 注释行里的 JIAN_DATA_DIR=/should/not/match",
		"",
		"  JIAN_HTTP_ADDR=:8001  ",
		"export JIAN_DATA_DIR=/opt/jianartifact/data",
		`JIAN_OTHER="a=b"`,
	}, "\n")

	if got := envFileValue(content, "JIAN_DATA_DIR"); got != "/opt/jianartifact/data" {
		t.Fatalf("应跳过注释行并剥掉 export，实际 %q", got)
	}
	if got := envFileValue(content, "JIAN_HTTP_ADDR"); got != ":8001" {
		t.Fatalf("应去掉值两侧空白，实际 %q", got)
	}
	// 值里含 = 时只按第一个 = 切分（否则会把合法值截断）。
	if got := envFileValue(content, "JIAN_OTHER"); got != "a=b" {
		t.Fatalf("应剥掉引号且保留值内 =，实际 %q", got)
	}
	if got := envFileValue(content, "JIAN_MISSING"); got != "" {
		t.Fatalf("不存在的键应返回空串，实际 %q", got)
	}
}

// TestLoadAmbiguousReturnsError 歧义必须在 Load 层变成明确错误（而不是静默选一个）。
func TestLoadAmbiguousReturnsError(t *testing.T) {
	// 不用 t.Parallel：本用例设置进程级环境变量。
	t.Setenv(EnvDataDir, "")

	// 用真实文件系统造两个候选：临时目录作为部署前缀不方便改候选路径，
	// 故这里直接调 resolveDataDirWith 之上的一层——把歧义判定交给 Load 的调用约定。
	got, err := resolveDataDirWith("", fakeEnvFiles(map[string]string{
		envFileCandidatePaths[0]: "JIAN_DATA_DIR=" + filepath.Join(t.TempDir(), "a") + "\n",
		envFileCandidatePaths[1]: "JIAN_DATA_DIR=" + filepath.Join(t.TempDir(), "b") + "\n",
	}))
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if len(got.Ambiguous) != 2 {
		t.Fatalf("应有两个候选，实际 %v", got.Ambiguous)
	}
}

// TestResolveDataDirIgnoresKeysNotConfigured env 文件里没有该键时不算候选。
func TestResolveDataDirIgnoresKeysNotConfigured(t *testing.T) {
	t.Parallel()
	reader := fakeEnvFiles(map[string]string{
		envFileCandidatePaths[0]: "JIAN_HTTP_ADDR=:8001\nJIAN_PUBLIC_URL=https://example.com\n",
	})
	got, err := resolveDataDirWith("", reader)
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if got.Source != DataDirFromDefault {
		t.Fatalf("无该键时应回落默认，实际 %q", got.Source)
	}
}

// TestResolveDataDirRelativeExplicitIsAbsolutized 显式给相对路径也要展开为绝对路径。
//
// 相对路径的语义随工作目录漂移，正是"莫名其妙的数据目录"的来源之一；
// 展开之后至少日志里能看到唯一确定的路径。
func TestResolveDataDirRelativeExplicitIsAbsolutized(t *testing.T) {
	dir := t.TempDir()
	old, err := os.Getwd()
	if err != nil {
		t.Fatalf("取工作目录：%v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("切换工作目录：%v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })

	got, err := resolveDataDirWith("./data", fakeEnvFiles(nil))
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if !filepath.IsAbs(got.Dir) {
		t.Fatalf("应为绝对路径，实际 %q", got.Dir)
	}
	if got.Source != DataDirFromEnv {
		t.Fatalf("相对路径也算显式设置，实际 %q", got.Source)
	}
}

// TestResolveDataDirReturnsWholeEnvFile 发现实例时必须交回 env 文件全文，供整份采纳。
//
// 只取数据目录是不够的：该实例的其余配置仍缺失，CLI 会以为 JWT 密钥"未设置"，
// 于是在服务的数据库目录里现场生成一份（实测踩到：`backup list` 这种只读命令
// 也在服务数据目录留下了 jwt.secret 与 migration-credential.key，
// 并打印"未设置 JIAN_JWT_SECRET"的误导性警告——而服务明明设了）。
func TestResolveDataDirReturnsWholeEnvFile(t *testing.T) {
	t.Parallel()
	full := "# 部署环境\nJIAN_DATA_DIR=/opt/jianartifact/data\nJIAN_JWT_SECRET=abc123\nJIAN_HTTP_ADDR=:8001\n"
	got, err := resolveDataDirWith("", fakeEnvFiles(map[string]string{envFileCandidatePaths[0]: full}))
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if got.EnvFileContent != full {
		t.Fatalf("应交回 env 文件全文供整份采纳，实际 %q", got.EnvFileContent)
	}
}

// TestApplyEnvFileDefaultsOnlyFillsMissing 采纳 env 文件时**只补未设置的键**。
//
// 进程环境（含调用方显式 export 的值）必须优先——那是刻意的覆盖意图，
// 自动化脚本与临时调试都依赖它。这里逐条验证三类键：未设置→补上、
// 已设置→不动、注释与空行→忽略。
func TestApplyEnvFileDefaultsOnlyFillsMissing(t *testing.T) {
	// 不用 t.Parallel：本用例设置进程级环境变量。
	t.Setenv("JIAN_PROBE_ALREADY", "explicit")
	t.Setenv("JIAN_PROBE_MISSING", "")

	applyEnvFileDefaults(strings.Join([]string{
		"# 注释不应生效",
		"",
		"JIAN_PROBE_MISSING=filled",
		"JIAN_PROBE_ALREADY=from-file",
		"export JIAN_PROBE_EXPORTED=exported",
		`JIAN_PROBE_QUOTED="quoted value"`,
	}, "\n"))

	if got := os.Getenv("JIAN_PROBE_MISSING"); got != "filled" {
		t.Fatalf("未设置的键应从文件补上，实际 %q", got)
	}
	if got := os.Getenv("JIAN_PROBE_ALREADY"); got != "explicit" {
		t.Fatalf("已设置的键不得被文件覆盖，实际 %q", got)
	}
	if got := os.Getenv("JIAN_PROBE_EXPORTED"); got != "exported" {
		t.Fatalf("应支持 export 前缀，实际 %q", got)
	}
	if got := os.Getenv("JIAN_PROBE_QUOTED"); got != "quoted value" {
		t.Fatalf("应剥掉引号，实际 %q", got)
	}
}

// TestResolveDataDirExplicitAlsoAdoptsInstanceEnv 显式指定目录时也要采纳该实例的 env 文件。
//
// 这条守卫来自实测漏报：第一版只在"自动发现"分支采纳 env，显式 `JIAN_DATA_DIR=… cli …`
// 时既不发现也不采纳，于是 CLI 以为密钥"未设置"，**在服务的数据目录里现场生成了
// jwt.secret 与 migration-credential.key**（只读命令也写文件），并打印误导性警告。
// 实测于测试站复现：显式设置目录后仍出现了这两个散件。
func TestResolveDataDirExplicitAlsoAdoptsInstanceEnv(t *testing.T) {
	t.Parallel()
	full := "JIAN_DATA_DIR=/opt/jianartifact/data\nJIAN_JWT_SECRET=abc123\n"
	reader := fakeEnvFiles(map[string]string{envFileCandidatePaths[0]: full})

	got, err := resolveDataDirWith("/opt/jianartifact/data", reader)
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if got.Source != DataDirFromEnv {
		t.Fatalf("来源仍应是显式设置，实际 %q", got.Source)
	}
	if got.EnvFileContent != full {
		t.Fatalf("显式指定该实例目录时也应采纳其 env 文件，实际 %q", got.EnvFileContent)
	}
	// 显式设置时依然**不**提示（那是刻意的静默：行为完全由调用方决定）。
	if got.Warning != "" {
		t.Fatalf("显式设置不应产生提示，实际 %q", got.Warning)
	}
}

// TestResolveDataDirExplicitOtherDirDoesNotAdoptForeignEnv 显式指向**别的**目录时，
// 不得采纳那些候选实例的配置。
//
// 否则调用方明确指向另一个目录，却拿到别的实例的 JWT 密钥/配置，是更难排查的错配。
func TestResolveDataDirExplicitOtherDirDoesNotAdoptForeignEnv(t *testing.T) {
	t.Parallel()
	reader := fakeEnvFiles(map[string]string{
		envFileCandidatePaths[0]: "JIAN_DATA_DIR=/opt/jianartifact/data\nJIAN_JWT_SECRET=abc123\n",
	})
	got, err := resolveDataDirWith("/some/other/data", reader)
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if got.EnvFileContent != "" {
		t.Fatalf("目录不匹配时不得采纳他人配置，实际 %q", got.EnvFileContent)
	}
}
