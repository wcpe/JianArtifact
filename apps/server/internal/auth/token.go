package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// tokenPrefix 是 API Token 明文的前缀，便于识别与日志中脱敏定位。
const tokenPrefix = "jat_"

// GenerateToken 生成一枚 API Token，返回明文（仅签发时返回一次）与其 sha256 摘要。
// 数据库仅存摘要（AC-05）：鉴权时对呈递的明文取摘要查表比对。
func GenerateToken() (plaintext, digest string) {
	plaintext = tokenPrefix + randomHex(24)
	return plaintext, DigestToken(plaintext)
}

// DigestToken 计算 API Token 明文的 sha256 十六进制摘要。
func DigestToken(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// randomHex 返回 n 字节的密码学随机数的十六进制串（长度 2n）。
func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand 失败属不可恢复的系统级错误。
		panic(fmt.Sprintf("读取随机源失败：%v", err))
	}
	return hex.EncodeToString(b)
}
