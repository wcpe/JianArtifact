package discover

import "fmt"

// PersistedSourceConfig 只保留任务恢复所需的安全来源配置。
// 调用方负责先处理在线发现阶段的临时入参。
func PersistedSourceConfig(sourceType string, cfg map[string]any) (map[string]any, error) {
	switch sourceType {
	case "online_rest":
		ref, hasRef := cfg["sourceRef"].(string)
		rawURL, hasURL := cfg["url"].(string)
		if hasRef == hasURL {
			return nil, fmt.Errorf("online_rest 须且只能提供 sourceRef 或 url")
		}
		if hasRef {
			if !IsSourceRef(ref) {
				return nil, fmt.Errorf("来源引用无效")
			}
			return map[string]any{"sourceRef": ref}, nil
		}
		url, err := validateSourceURL(rawURL)
		if err != nil {
			return nil, err
		}
		return map[string]any{"url": url}, nil
	case "offline_dir", "offline_bundle":
		path, ok := cfg["path"].(string)
		if !ok {
			return nil, fmt.Errorf("来源路径无效")
		}
		return map[string]any{"path": path}, nil
	default:
		return nil, fmt.Errorf("不支持的 sourceType %q", sourceType)
	}
}
