package discover

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// OfflineBundle 扫描自有离线包：
//
//	bundle/
//	  manifest.json   # { "repositories": [ { "name", "format", "type" } ] }
//	  content/
//	    <repo>/<path...>
type OfflineBundle struct{}

type bundleManifest struct {
	Repositories []struct {
		Name   string `json:"name"`
		Format string `json:"format"`
		Type   string `json:"type"`
	} `json:"repositories"`
}

// Discover 实现 Source。
func (OfflineBundle) Discover(ctx context.Context, cfg Config) (Plan, error) {
	_ = ctx
	if err := requirePath(cfg.Path); err != nil {
		return Plan{}, err
	}
	root := cfg.Path
	st, err := os.Stat(root)
	if err != nil {
		if os.IsNotExist(err) {
			return Plan{}, &ErrInvalidConfig{Msg: "离线包路径不存在"}
		}
		return Plan{}, &ErrInvalidConfig{Msg: "无法访问离线包路径"}
	}
	if !st.IsDir() {
		return Plan{}, &ErrInvalidConfig{Msg: "离线包路径须为目录"}
	}

	plan := emptyPlan()
	contentDir := filepath.Join(root, "content")
	manifestPath := filepath.Join(root, "manifest.json")

	// 优先 manifest
	if raw, err := os.ReadFile(manifestPath); err == nil {
		var m bundleManifest
		if err := json.Unmarshal(raw, &m); err != nil {
			return Plan{}, &ErrInvalidConfig{Msg: "manifest.json 解析失败"}
		}
		for _, r := range m.Repositories {
			format, ok := mapNexusFormat(strings.ToLower(r.Format))
			if !ok || !supportedFormat(format) {
				plan.Warnings = append(plan.Warnings, "跳过不支持的 format: "+r.Name+" ("+r.Format+")")
				continue
			}
			typ := r.Type
			if typ == "" {
				typ = "hosted"
			}
			count := countFilesUnder(filepath.Join(contentDir, r.Name))
			plan.Repositories = append(plan.Repositories, PlanRepository{
				Name:            r.Name,
				Format:          format,
				Type:            typ,
				EstimatedAssets: count,
			})
		}
		plan.Estimated = false
		return finalizePlan(plan), nil
	}

	// 无 manifest：扫描 content/* 一级目录为仓库，format 默认 raw
	if _, err := os.Stat(contentDir); err != nil {
		return Plan{}, &ErrInvalidConfig{Msg: "缺少 manifest.json 且 content/ 不存在"}
	}
	entries, err := os.ReadDir(contentDir)
	if err != nil {
		return Plan{}, &ErrInvalidConfig{Msg: "无法读取 content/"}
	}
	plan.Warnings = append(plan.Warnings, "无 manifest.json，仓库 format 默认 raw")
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		count := countFilesUnder(filepath.Join(contentDir, name))
		plan.Repositories = append(plan.Repositories, PlanRepository{
			Name:            name,
			Format:          FormatRaw,
			Type:            "hosted",
			EstimatedAssets: count,
		})
	}
	plan.Estimated = false
	return finalizePlan(plan), nil
}

func countFilesUnder(dir string) int64 {
	var n int64
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			n++
		}
		return nil
	})
	return n
}
