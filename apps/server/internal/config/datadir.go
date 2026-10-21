package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// 数据目录解析（FR-40 追加六）。
//
// 要解决的问题（实测踩到、且用户反馈"老出现莫名其妙的数据目录"）：
// CLI 是**独立进程**，读不到 systemd 单元的 EnvironmentFile，于是 JIAN_DATA_DIR 缺失时
// 静默回落到 defaultDataDir = "./data"。后果有两层：
//  1. CLI 操作的是**另一个库**——看不到服务侧真实的包，报"归档不存在"，看着像功能缺陷；
//  2. Load() 会 MkdirAll 数据目录并生成 jwt.secret / jianartifact.db，
//     所以一次误跑就顺手在原地建出一个**完整的实例骨架**。
//
// 难点：不能简单地"缺就报错"，因为 `run` 子命令首次启动实例时正是靠这个回落来初始化的。
// 因此必须区分「我要初始化一个新实例」与「我要操作一个已有实例」。本文件的做法是：
//   - JIAN_DATA_DIR 显式设置 → 直接用（唯一权威，行为与既有部署完全一致）；
//   - 未设置时**主动探测正在运行的实例**（systemd 单元的 EnvironmentFile、常见部署路径），
//     找到唯一候选就用它，找到多个就列出来让人选（不猜）；
//   - 探测不到 → 保持回落，但**打印警告**说明用的是哪个目录、为什么。
//
// 关键原则：**自动发现可以是自动的，但绝不能是静默的**。任何一次解析都要能回答
// 「这次用的是哪个目录、依据是什么」，否则只是把"莫名其妙"换了个地方发生。

// EnvDataDirKeysInEnvFile 是部署 env 文件里可能承载数据目录的键名。
// 目前只有 JIAN_DATA_DIR 一个，保留切片是为了将来支持别名时不必改动探测逻辑。
var EnvDataDirKeysInEnvFile = []string{"JIAN_DATA_DIR"}

// DataDirSource 说明本次数据目录是怎么定出来的，用于向用户打印依据。
type DataDirSource string

const (
	// DataDirFromEnv：JIAN_DATA_DIR 已显式设置。
	DataDirFromEnv DataDirSource = "显式设置"
	// DataDirFromDiscovery：从正在运行的实例配置中发现。
	DataDirFromDiscovery DataDirSource = "自动发现"
	// DataDirFromDefault：探测不到，回落默认值。
	DataDirFromDefault DataDirSource = "默认回落"
)

// DataDirResolution 是一次数据目录解析的结果。
type DataDirResolution struct {
	// Dir 是最终使用的数据目录（已展开为绝对路径）。
	Dir string
	// Source 说明依据。
	Source DataDirSource
	// DiscoveredFrom 非空时说明是从哪个文件/来源发现的值（Source=Discovery 时）。
	DiscoveredFrom string
	// Ambiguous 非空表示发现了多个候选，此时**不使用任何一个**，由调用方决定如何提示。
	// 猜错的代价（操作到错的库）远高于让用户显式指定一次。
	Ambiguous []string
	// Warning 是需要向用户提示的完整说明；为空表示无需提示（显式设置时）。
	Warning string
	// EnvFileContent 非空时是"发现该实例所用的那份 env 文件"的原文，供调用方整份采纳。
	//
	// 为什么必须整份采纳而不是只取 JIAN_DATA_DIR：只取目录会让该实例的其余配置仍然缺失，
	// 于是 CLI 会以为 JWT 密钥/迁移凭据密钥"未设置"，**在服务的数据目录里现场生成一份**
	// （实测踩到：`backup list` 这种只读命令也在服务目录留下了 jwt.secret 与
	// migration-credential.key，并打印"未设置 JIAN_JWT_SECRET"的误导性警告——而服务明明设了）。
	// 采纳整份文件后，CLI 在该实例上的行为与服务一致，且不产生任何多余写入。
	EnvFileContent string
}

// envFileCandidatePaths 是探测"正在运行的实例"时按序查找的部署 env 文件位置。
//
// 取自本仓库的部署形态（见 deploy/ 与 docs/OPERATIONS.md）：systemd 单元的
// EnvironmentFile 指向 ${DEPLOY_DIR}/jianartifact.env，默认部署目录是 /opt/jianartifact。
// 路径写死是有意的——探测是**尽力而为的便利**，不是权威来源；权威永远是 JIAN_DATA_DIR。
var envFileCandidatePaths = []string{
	"/opt/jianartifact/jianartifact.env",
	"/etc/jianartifact/jianartifact.env",
	"/var/lib/jianartifact/jianartifact.env",
}

// envFileReader 抽象"读一个 env 文件的内容"，便于测试注入而不依赖真实部署目录。
type envFileReader func(path string) (string, bool)

func defaultEnvFileReader(path string) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	return string(data), true
}

// ResolveDataDir 解析本次进程应当使用的数据目录。
//
// explicit 非空即视为显式指定（调用方通常传 os.Getenv(EnvDataDir)）；
// 否则尝试自动发现，最后回落默认值。返回值保证 Dir 是绝对路径。
func ResolveDataDir(explicit string) (DataDirResolution, error) {
	return resolveDataDirWith(explicit, defaultEnvFileReader)
}

// resolveDataDirWith 是 ResolveDataDir 的可注入实现（测试用）。
func resolveDataDirWith(explicit string, readEnvFile envFileReader) (DataDirResolution, error) {
	if strings.TrimSpace(explicit) != "" {
		abs, err := absDataDir(explicit)
		if err != nil {
			return DataDirResolution{}, err
		}
		// 显式设置时**不**打印提示（行为完全由调用方决定，多余提示只会变成噪声），
		// 但仍要尝试找到该实例的 env 文件并采纳它——见下方说明。
		content, from := findEnvFileForDir(abs, readEnvFile)
		return DataDirResolution{
			Dir:            abs,
			Source:         DataDirFromEnv,
			DiscoveredFrom: from,
			EnvFileContent: content,
		}, nil
	}

	found := map[string]string{}    // 目录 -> 发现它的来源文件
	contents := map[string]string{} // 来源文件 -> 文件原文（供整份采纳）
	for _, path := range envFileCandidatePaths {
		content, ok := readEnvFile(path)
		if !ok {
			continue
		}
		for _, key := range EnvDataDirKeysInEnvFile {
			if v := envFileValue(content, key); v != "" {
				// 同一目录被多个文件声明时，保留先扫到的那个来源（候选路径已按优先级排序）。
				if _, seen := found[v]; !seen {
					found[v] = path
					contents[path] = content
				}
				break
			}
		}
	}

	// 多个**不同**的目录 → 不猜，交回候选让调用方提示用户。
	dirs := make([]string, 0, len(found))
	for d := range found {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	if len(dirs) > 1 {
		return DataDirResolution{Ambiguous: dirs}, nil
	}

	if len(dirs) == 1 {
		abs, err := absDataDir(dirs[0])
		if err != nil {
			return DataDirResolution{}, err
		}
		from := found[dirs[0]]
		return DataDirResolution{
			Dir:            abs,
			Source:         DataDirFromDiscovery,
			DiscoveredFrom: from,
			EnvFileContent: contents[from],
			Warning: fmt.Sprintf(
				"未设置 %s；已从 %s 自动发现正在运行的实例数据目录：%s\n"+
					"    （如需操作别的目录，请显式设置 %s）",
				EnvDataDir, from, abs, EnvDataDir),
		}, nil
	}

	abs, err := absDataDir(defaultDataDir)
	if err != nil {
		return DataDirResolution{}, err
	}
	return DataDirResolution{
		Dir:    abs,
		Source: DataDirFromDefault,
		Warning: fmt.Sprintf(
			"未设置 %s，且未发现正在运行的实例配置；本次使用默认目录：%s\n"+
				"    ⚠ 若你的本意是操作某个**已有**实例，请显式设置 %s 指向它的数据目录——\n"+
				"      否则本次会在当前目录下另建一个空实例（含 jwt.secret 与 jianartifact.db），\n"+
				"      并且看不到该实例里已有的备份包。",
			EnvDataDir, abs, EnvDataDir),
	}, nil
}

// findEnvFileForDir 找出"声明了该数据目录"的那份实例 env 文件及其原文。
//
// 为什么显式指定目录时也要找：调用方往往只设 JIAN_DATA_DIR（例如 `JIAN_DATA_DIR=… cli …`），
// 而该实例的其余配置（JWT 密钥、迁移凭据密钥…）仍缺失。那样 CLI 会以为密钥"未设置"，
// **在服务的数据目录里现场生成一份散件**，并对只读命令打印误导性警告——实测踩到：
// 显式设置目录后仍生成了 jwt.secret / migration-credential.key。
// 找到就采纳，既消除散件，也让 CLI 的行为与服务一致。
//
// 只在"该文件声明的目录 == 显式指定的目录"时采纳：否则调用方显式指向另一个目录，
// 却拿到别的实例的配置，那是更难排查的错配。
func findEnvFileForDir(absDir string, readEnvFile envFileReader) (content, from string) {
	for _, path := range envFileCandidatePaths {
		c, ok := readEnvFile(path)
		if !ok {
			continue
		}
		for _, key := range EnvDataDirKeysInEnvFile {
			v := envFileValue(c, key)
			if v == "" {
				continue
			}
			candidate, err := absDataDir(v)
			if err != nil {
				continue
			}
			if candidate == absDir {
				return c, path
			}
		}
	}
	return "", ""
}

// applyEnvFileDefaults 把发现的 env 文件里的键值补进当前进程环境。
//
// **只补未设置的键**：进程环境（含调用方显式 export 的值）始终优先——那是刻意的覆盖意图，
// 自动化脚本与临时调试都依赖它。
//
// 为什么需要这一步：CLI 读不到 systemd 单元的 EnvironmentFile，若只采纳数据目录，
// 其余配置仍然缺失，于是 CLI 会以为密钥"未设置"而在服务的数据目录里现场生成一份
// （实测踩到：`backup list` 也在服务目录留下了 jwt.secret / migration-credential.key，
// 并打印"未设置 JIAN_JWT_SECRET"的误导性警告——而服务明明设了）。
func applyEnvFileDefaults(content string) {
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		name, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		if name == "" || os.Getenv(name) != "" {
			continue // 已设置的一律不动
		}
		_ = os.Setenv(name, unquoteEnvValue(value))
	}
}

// unquoteEnvValue 去掉值两侧成对引号（最小 shell 语义，与 envFileValue 一致）。
func unquoteEnvValue(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 2 {
		if (value[0] == '"' && value[len(value)-1] == '"') ||
			(value[0] == '\'' && value[len(value)-1] == '\'') {
			value = value[1 : len(value)-1]
		}
	}
	return strings.TrimSpace(value)
}

// envFileValue 从 env 文件内容里取某个键的值。
//
// 只做最小解析：忽略注释与空行、去掉可选的 export 前缀、剥掉包裹的引号。
// 刻意不追求完整 shell 语义——这是一次尽力而为的探测，权威来源始终是环境变量本身。
func envFileValue(content, key string) string {
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		name, value, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(name) != key {
			continue
		}
		// 引号剥离复用同一个函数：两处各写一遍迟早会漂移（这个文件里已有前车之鉴）。
		return unquoteEnvValue(value)
	}
	return ""
}

// absDataDir 把数据目录展开为绝对路径。
func absDataDir(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("解析 data 目录 %q：%w", dir, err)
	}
	return abs, nil
}
