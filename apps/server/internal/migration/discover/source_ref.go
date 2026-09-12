package discover

import (
	"fmt"
	"os"
	"strings"
)

// SanitizeSourceConfig 删除认证材料，只保留任务恢复所需的来源定位与筛选项。
func SanitizeSourceConfig(sourceType string, cfg map[string]any) (map[string]any, error) {
	out := make(map[string]any, len(cfg)+1)
	for k, v := range cfg {
		if k != "credential" && k != "sourceAuth" && k != "password" && k != "token" {
			out[k] = v
		}
	}
	if sourceType != "online_rest" {
		return out, nil
	}
	ref, hasRef := out["sourceRef"].(string)
	rawURL, hasURL := out["url"].(string)
	if hasRef == hasURL {
		return nil, fmt.Errorf("online_rest 须且只能提供 sourceRef 或 url")
	}
	if hasRef {
		if !IsSourceRef(ref) {
			return nil, fmt.Errorf("来源引用无效")
		}
		out["sourceRef"] = ref
		return out, nil
	}
	url, err := validateSourceURL(rawURL)
	if err != nil {
		return nil, err
	}
	out["url"] = url
	return out, nil
}

// IsSourceRef 仅接受专用环境变量命名空间中的逻辑名称。
func IsSourceRef(ref string) bool {
	if len(ref) == 0 || len(ref) > 63 {
		return false
	}
	for i := range len(ref) {
		c := ref[i]
		if c >= 'A' && c <= 'Z' || c == '_' {
			continue
		}
		if i > 0 && c >= '0' && c <= '9' {
			continue
		}
		return false
	}
	return ref[0] >= 'A' && ref[0] <= 'Z'
}

// ResolveSourceRef 从专用部署配置解析来源地址，地址不进入任务与报告。
func ResolveSourceRef(ref string) (string, error) {
	if !IsSourceRef(ref) {
		return "", fmt.Errorf("来源引用不可用")
	}
	raw, ok := os.LookupEnv("JIAN_MIGRATION_SOURCE_" + ref)
	if !ok || strings.TrimSpace(raw) == "" {
		return "", fmt.Errorf("来源引用不可用")
	}
	resolved, err := validateSourceURL(raw)
	if err != nil {
		return "", fmt.Errorf("来源引用不可用")
	}
	return resolved, nil
}
