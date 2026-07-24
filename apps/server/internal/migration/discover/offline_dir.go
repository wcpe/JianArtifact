package discover

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"strings"
)

// OfflineDir 扫描离线源。支持两种布局：
//
// 1) 验收夹具：
//
//	<root>/repositories/<repo>/.format + content/
//
// 2) Nexus 3.x 原生 blob store（用户真机）：
//
//	<root>/content/vol-*/chap-*/{uuid}.properties
//	  @Bucket.repo-name=...
//	  @BlobStore.blob-name=...
//	同 stem 的 .bytes 为内容
//
// 当存在 content/vol-* 时走 blob store 路径；否则走夹具布局。
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

	// Nexus blob store：path 为 .../blobs/default 或 .../blobs/default/content 的父
	if isNexusBlobStore(root) {
		return discoverNexusBlobStore(root, cfg.IncludeRepositories)
	}

	return discoverFixtureDir(root, cfg.IncludeRepositories)
}

func isNexusBlobStore(root string) bool {
	// 常见：.../blobs/default 下有 content/vol-*
	content := filepath.Join(root, "content")
	if st, err := os.Stat(content); err == nil && st.IsDir() {
		entries, _ := os.ReadDir(content)
		for _, e := range entries {
			if e.IsDir() && strings.HasPrefix(e.Name(), "vol-") {
				return true
			}
		}
	}
	// 直接传入 content 目录
	entries, err := os.ReadDir(root)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), "vol-") {
			return true
		}
	}
	return false
}

func discoverNexusBlobStore(root string, include []string) (Plan, error) {
	contentRoot := root
	if st, err := os.Stat(filepath.Join(root, "content")); err == nil && st.IsDir() {
		contentRoot = filepath.Join(root, "content")
	}
	allow := includeSet(include)
	// 真机 blob 极大：必须带 include，否则拒绝全盘扫描以免卡住
	if len(allow) == 0 {
		return Plan{}, &ErrInvalidConfig{Msg: "Nexus blob store 发现必须指定 includeRepositories（避免全量扫描占满磁盘/时间）"}
	}

	repos := make([]string, 0, len(allow))
	for name := range allow {
		repos = append(repos, name)
	}
	assets, err := EnumerateNexusBlobAssets(contentRoot, repos)
	if err != nil {
		return Plan{}, &ErrInvalidConfig{Msg: "扫描 blob store 失败: " + err.Error()}
	}
	counts := map[string]int64{}
	for _, a := range assets {
		counts[a.Repo]++
	}

	plan := emptyPlan()
	plan.Estimated = false
	for name, n := range counts {
		plan.Repositories = append(plan.Repositories, PlanRepository{
			Name:            name,
			Format:          FormatMaven,
			Type:            "hosted",
			EstimatedAssets: n,
		})
	}
	if len(plan.Repositories) == 0 {
		plan.Warnings = append(plan.Warnings, "blob store 中未匹配到 includeRepositories 内仓库")
	} else {
		plan.Warnings = append(plan.Warnings, "Nexus blob store：format 默认 maven；已跳过 deleted 资产")
	}
	return finalizePlan(plan), nil
}

func discoverFixtureDir(root string, include []string) (Plan, error) {
	reposRoot := filepath.Join(root, "repositories")
	if st, err := os.Stat(reposRoot); err != nil || !st.IsDir() {
		reposRoot = root
	}

	entries, err := os.ReadDir(reposRoot)
	if err != nil {
		return Plan{}, &ErrInvalidConfig{Msg: "无法读取仓库目录"}
	}

	plan := emptyPlan()
	allow := includeSet(include)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
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

// readBlobProperties 解析 Nexus blob .properties（Java Properties 风格 key=value）。
func readBlobProperties(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	// 个别 properties 可能较长
	buf := make([]byte, 0, 64*1024)
	sc.Buffer(buf, 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		i := strings.IndexByte(line, '=')
		if i <= 0 {
			continue
		}
		k := strings.TrimSpace(line[:i])
		v := strings.TrimSpace(line[i+1:])
		out[k] = v
	}
	return out, sc.Err()
}
