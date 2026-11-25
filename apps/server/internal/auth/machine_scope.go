package auth

import "encoding/json"

// 机器 Token 用途：凭据的功能边界，与实时 ACL 取交集后再裁决请求。
// LegacyTokenSunsetDate 是未限用途旧式令牌停止签发的弃用时间盒日期。
// 当前为向后兼容期：签发照常但回 Warning 头警告（见 api/token_handlers.go）；
// 到期的硬禁落地前，npm 登录等协议签发路径必须先迁移到受限凭据，
// 否则到期日一到协议登录会断——因此硬禁是一个独立排期项，不在本期范围。
const LegacyTokenSunsetDate = "2027-03-01"

// TokenPurposeLegacy 是过渡期的未限用途旧凭据（早期签发、无 scope），
// 只保留既有协议承载能力，仍不能越过通道限制新增的管理面入口。
const (
	TokenPurposeLegacy          = ""
	TokenPurposeArtifactRead    = "artifact_read"
	TokenPurposeArtifactPublish = "artifact_publish"
)

// 机器 Token 允许的协议动作；取值与仓储层 ACL 动作常量一致。
const (
	MachineActionRead    = "read"
	MachineActionPublish = "publish"
)

// MachineTokenScope 是一条机器凭据作用域：仓库 + 规范路径段前缀 + 动作集合。
type MachineTokenScope struct {
	RepositoryID int64    `json:"repository_id"`
	PathPrefix   string   `json:"path_prefix"`
	Actions      []string `json:"actions"`
}

// ParseMachineScopes 反序列化持久化的作用域列表；空串表示未限用途旧凭据。
// 存储内容损坏时返回错误，调用方应按凭据失效处理而不是降级放行。
func ParseMachineScopes(raw string) ([]MachineTokenScope, error) {
	if raw == "" {
		return nil, nil
	}
	var scopes []MachineTokenScope
	if err := json.Unmarshal([]byte(raw), &scopes); err != nil {
		return nil, err
	}
	return scopes, nil
}

// MachineScopeAllowed 判断任一作用域是否同时命中仓库、路径与动作。
// pathPrefix 按规范段边界匹配：foo 命中 foo 与 foo/bar，不命中 foobar；
// 空前缀覆盖仓库根，即仓库下全部路径与仓库级操作。
func MachineScopeAllowed(scopes []MachineTokenScope, repoID int64, path, action string) bool {
	for _, s := range scopes {
		if s.RepositoryID != repoID {
			continue
		}
		if !machinePathMatch(s.PathPrefix, path) {
			continue
		}
		for _, a := range s.Actions {
			if a == action {
				return true
			}
		}
	}
	return false
}

func machinePathMatch(prefix, path string) bool {
	if prefix == "" {
		return true
	}
	return path == prefix || len(path) > len(prefix) && path[len(prefix)] == '/' && path[:len(prefix)] == prefix
}

// MachinePurposeAllows 表示该用途本身是否可能覆盖动作（不考虑具体 scope 行）。
// 未限用途旧凭据放行全部协议动作；scope 层另行收窄。
func MachinePurposeAllows(purpose, action string) bool {
	switch purpose {
	case TokenPurposeLegacy:
		return action == MachineActionRead || action == MachineActionPublish || action == "write"
	case TokenPurposeArtifactRead:
		return action == MachineActionRead
	case TokenPurposeArtifactPublish:
		return action == MachineActionRead || action == MachineActionPublish
	default:
		return false
	}
}
