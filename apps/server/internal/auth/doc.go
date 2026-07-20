// Package auth 提供认证授权原语：argon2id 口令哈希、JWT HS256 会话、
// API Token 摘要，以及 Gin 鉴权中间件。
//
// 分层（见 internal/doc.go）：auth -> persistence。会话为无状态 JWT，
// 登出通过 revoked_token 黑名单实现；API Token 仅存 sha256 摘要，明文不落库。
// 明文口令与明文 Token 不写入数据库、不进日志（NFR-06、AC-05）。
package auth
