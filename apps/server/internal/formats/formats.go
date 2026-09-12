// Package formats 定义实例级格式能力集合。
//
// 该包不依赖领域层或协议层，供配置、仓库校验和 HTTP 装配共享同一份格式注册表。
package formats

import (
	"fmt"
	"slices"
	"strings"
)

// Known 是当前版本登记的全部格式标识。
var Known = []string{"raw", "maven", "npm", "docker", "cargo", "pypi", "gomod", "nuget"}

// Set 是进程启动时解析出的不可变格式能力集合。
type Set struct{ enabled map[string]struct{} }

// Default 返回兼容既有部署的默认格式集合。
func Default() Set { return New("raw", "maven", "npm") }

// New 创建格式集合并归一化格式名。调用方应先完成 Parse 校验。
func New(names ...string) Set {
	m := make(map[string]struct{}, len(names))
	for _, name := range names {
		name = strings.ToLower(strings.TrimSpace(name))
		if name != "" {
			m[name] = struct{}{}
		}
	}
	return Set{enabled: m}
}

// Parse 解析逗号分隔的启动配置。空字符串表示显式禁用全部格式。
func Parse(raw string) (Set, error) {
	if strings.TrimSpace(raw) == "" {
		return New(), nil
	}
	seen := make(map[string]struct{})
	for _, item := range strings.Split(raw, ",") {
		name := strings.ToLower(strings.TrimSpace(item))
		if name == "" {
			return Set{}, fmt.Errorf("格式标识不能为空")
		}
		if !slices.Contains(Known, name) {
			return Set{}, fmt.Errorf("未知格式 %q", name)
		}
		seen[name] = struct{}{}
	}
	return Set{enabled: seen}, nil
}

// Has 判断格式是否启用。
func (s Set) Has(name string) bool {
	_, ok := s.enabled[strings.ToLower(strings.TrimSpace(name))]
	return ok
}

// List 返回稳定排序后的启用格式名。
func (s Set) List() []string {
	result := make([]string, 0, len(s.enabled))
	for name := range s.enabled {
		result = append(result, name)
	}
	slices.Sort(result)
	return result
}

// Any 判断是否至少启用一个格式。
func (s Set) Any() bool { return len(s.enabled) > 0 }

// AllPrefixes 返回所有格式协议保留的 URL 前缀，供静态 SPA 回退拒绝表使用。
func AllPrefixes() []string {
	return []string{"/repository", "/npm", "/v2", "/cargo", "/pypi", "/go", "/nuget"}
}
