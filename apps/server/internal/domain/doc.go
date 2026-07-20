// Package domain 承载业务规则：认证会话、用户管理、仓库与 ACL。
//
// 分层（见 internal/doc.go）：domain -> repository, auth。domain 编排持久化与
// 认证原语，暴露给 api 层调用；不直接触碰 SQLite 或 HTTP。
package domain

import "errors"

// 领域错误。api 层据此映射 HTTP 状态码。
var (
	// ErrInvalidCredentials 登录口令或用户名不符 / 账号被停用。
	ErrInvalidCredentials = errors.New("用户名或口令错误")
	// ErrAlreadyInitialized 实例已初始化，自举端点关闭。
	ErrAlreadyInitialized = errors.New("实例已初始化")
	// ErrConflict 唯一约束冲突（如用户名 / 仓库名重复）。
	ErrConflict = errors.New("资源已存在")
	// ErrNotFound 目标资源不存在。
	ErrNotFound = errors.New("资源不存在")
	// ErrValidation 入参不满足业务约束。
	ErrValidation = errors.New("参数校验失败")
)
