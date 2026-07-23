package discover

import (
	"context"
	"os"
	"path/filepath"
	"strings"
)

// OfflineDir 扫描 Nexus 原生存储目录的简化夹具布局（验收用）：
//
//	<root>/
//	  repositories/
//	    <repo-name>/
//	      .format          # 内容为 raw|maven|maven2|npm|docker…
//	      content/         # 制品文件树
//
// 若无 .format，则 format 默认 raw 并 warning。
// 真实 3.70 blob store 布局可在后续用录制样例替换，不静默猜。
type OfflineDir struct{}

// Discover 实现 Source。
func (OfflineDir) Discover(ctx context.Context, cfg Config) (Plan, error) {
	_ = ctx
	if err := requirePath(cfg.Path); err != nil {
		return Plan{}, err
	}
	root := cfg.Path
	st, err := os.Stat(root)
	if err != nil {
		if os.IsNotExist(err) {
			return Plan{}, &ErrInvalidConfig{Msg: "离线目录不存在"}
		}
		return Plan{}, &ErrInvalidConfig{Msg: "无法访问离线目录"}
	}
	if !st.IsDir() {
		return Plan{}, &ErrInvalidConfig{Msg: "离线路径须为目录"}
	}

	reposRoot := filepath.Join(root, "repositories")
	if st, err := os.Stat(reposRoot); err != nil || !st.IsDir() {
		// 兼容：直接把 path 当作 repositories 父级的平铺
		reposRoot = root
	}

	entries, err := os.ReadDir(reposRoot)
	if err != nil {
		return Plan{}, &ErrInvalidConfig{Msg: "无法读取仓库目录"}
	}

	plan := emptyPlan()
	allow := includeSet(cfg.IncludeRepositories)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		// 跳过常见非仓库名
		name := e.Name()
		if name == "content" || name == "blobs" || strings.HasPrefix(name, ".") {
			continue
		}
		if len(allow) > 0 && !allow[name] {
			continue
		}
		repoDir := filepath.Join(reposRoot, name)
		format := readFormatFile(filepath.Join(repoDir, ".format"))
		if format == "" {
			format = FormatRaw
			plan.Warnings = append(plan.Warnings, name+": 无 .format，默认 raw")
		}
		mapped, ok := mapNexusFormat(strings.ToLower(format))
		if !ok || !supportedFormat(mapped) {
			plan.Warnings = append(plan.Warnings, "跳过不支持的 format: "+name+" ("+format+")")
			continue
		}
		content := filepath.Join(repoDir, "content")
		if _, err := os.Stat(content); err != nil {
			content = repoDir
		}
		count := countFilesUnder(content)
		// 不把 .format 计入资产
		if _, err := os.Stat(filepath.Join(repoDir, ".format")); err == nil && content == repoDir {
			count--
			if count < 0 {
				count = 0
			}
		}
		plan.Repositories = append(plan.Repositories, PlanRepository{
			Name:            name,
			Format:          mapped,
			Type:            "hosted",
			EstimatedAssets: count,
		})
	}
	plan.Estimated = false
	return finalizePlan(plan), nil
}

func readFormatFile(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
