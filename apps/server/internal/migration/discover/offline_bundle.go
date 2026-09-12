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

type bundleRepository struct {
	Name      string   `json:"name"`
	Format    string   `json:"format"`
	Type      string   `json:"type"`
	RemoteURL string   `json:"remoteUrl,omitempty"`
	Members   []string `json:"members,omitempty"`
}

type bundleManifest struct {
	Repositories []bundleRepository `json:"repositories"`
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
	allow := includeSet(cfg.IncludeRepositories)

	// 优先 manifest
	if raw, err := os.ReadFile(manifestPath); err == nil {
		var m bundleManifest
		if err := json.Unmarshal(raw, &m); err != nil {
			return Plan{}, &ErrInvalidConfig{Msg: "manifest.json 解析失败"}
		}
		byName := make(map[string]bundleRepository, len(m.Repositories))
		for _, item := range m.Repositories {
			byName[item.Name] = item
		}
		for _, r := range m.Repositories {
			if len(allow) > 0 && !allow[r.Name] {
				continue
			}
			format, ok := mapNexusFormat(strings.ToLower(r.Format))
			if !ok || !supportedFormat(format) {
				plan.Warnings = append(plan.Warnings, "跳过不支持的 format: "+r.Name+" ("+r.Format+")")
				continue
			}
			typ := normalizeRepoType(r.Type)
			mode := migrationMode(format, typ)
			if mode == "unsupported" {
				plan.Warnings = append(plan.Warnings, "跳过不支持的仓库类型："+r.Name+"（Go modules 仅允许 proxy）")
				continue
			}
			count := int64(0)
			if mode == "assets" {
				count = countFilesUnder(filepath.Join(contentDir, r.Name))
			}
			config := map[string]any{}
			if typ == "group" {
				config["members"] = append([]string(nil), r.Members...)
			}
			if typ == "proxy" && strings.TrimSpace(r.RemoteURL) == "" {
				mode = "unsupported"
				plan.Warnings = append(plan.Warnings, r.Name+": proxy 缺少上游配置，需管理员补充")
			} else if typ == "proxy" {
				if _, err := validateSourceURL(r.RemoteURL); err != nil {
					mode = "unsupported"
					plan.Warnings = append(plan.Warnings, r.Name+": proxy 上游配置无效，需管理员补充")
				} else {
					config["remoteUrl"] = strings.TrimRight(strings.TrimSpace(r.RemoteURL), "/")
				}
			}
			if typ == "group" {
				if warning := validateGroup(r, byName); warning != "" {
					mode = "unsupported"
					plan.Warnings = append(plan.Warnings, r.Name+": "+warning)
				}
			}
			plan.Repositories = append(plan.Repositories, PlanRepository{
				Name:            r.Name,
				Format:          format,
				Type:            typ,
				EstimatedAssets: count,
				Config:          config,
				MigrationMode:   mode,
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
		if len(allow) > 0 && !allow[name] {
			continue
		}
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

func validateGroup(group bundleRepository, byName map[string]bundleRepository) string {
	if len(group.Members) == 0 {
		return "group 缺少成员"
	}
	for _, memberName := range group.Members {
		member, ok := byName[memberName]
		if !ok {
			return "group 成员不存在：" + memberName
		}
		memberFormat, mapped := mapNexusFormat(strings.ToLower(member.Format))
		groupFormat, _ := mapNexusFormat(strings.ToLower(group.Format))
		if !mapped || memberFormat != groupFormat {
			return "group 成员必须全部存在且同 format"
		}
		if memberName == group.Name {
			return "group 不允许自引用"
		}
		if normalizeRepoType(member.Type) == "group" && groupCycle(memberName, group.Name, byName, map[string]bool{}) {
			return "group 存在循环成员"
		}
	}
	return ""
}

func groupCycle(current, target string, byName map[string]bundleRepository, seen map[string]bool) bool {
	if current == target {
		return true
	}
	if seen[current] {
		return false
	}
	seen[current] = true
	item, ok := byName[current]
	if !ok || normalizeRepoType(item.Type) != "group" {
		return false
	}
	for _, member := range item.Members {
		if groupCycle(member, target, byName, seen) {
			return true
		}
	}
	return false
}
