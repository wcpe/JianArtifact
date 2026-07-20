package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// argon2id 参数（固定，随哈希编码一并存储，便于未来调参不影响旧哈希校验）。
const (
	argonTime    = 1
	argonMemory  = 64 * 1024 // 64 MiB
	argonThreads = 4
	argonKeyLen  = 32
	argonSaltLen = 16
)

// ErrPasswordMismatch 表示口令与哈希不匹配。
var ErrPasswordMismatch = errors.New("口令不匹配")

// HashPassword 用 argon2id 派生口令哈希，返回 PHC 风格编码串：
// $argon2id$v=19$m=65536,t=1,p=4$<salt-b64>$<hash-b64>。
func HashPassword(password string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("生成盐值：%w", err)
	}
	hash := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	b64 := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		b64.EncodeToString(salt), b64.EncodeToString(hash)), nil
}

// VerifyPassword 以恒定时间比较口令与已编码哈希；不匹配返回 ErrPasswordMismatch。
func VerifyPassword(password, encoded string) error {
	salt, hash, params, err := decodeHash(encoded)
	if err != nil {
		return err
	}
	computed := argon2.IDKey([]byte(password), salt, params.time, params.memory, params.threads, uint32(len(hash)))
	if subtle.ConstantTimeCompare(hash, computed) != 1 {
		return ErrPasswordMismatch
	}
	return nil
}

type argonParams struct {
	memory  uint32
	time    uint32
	threads uint8
}

// decodeHash 解析 PHC 编码串，取回盐、哈希与参数。
func decodeHash(encoded string) (salt, hash []byte, params argonParams, err error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return nil, nil, params, errors.New("非法的口令哈希格式")
	}
	var version int
	if _, err = fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return nil, nil, params, fmt.Errorf("解析哈希版本：%w", err)
	}
	if version != argon2.Version {
		return nil, nil, params, fmt.Errorf("不支持的 argon2 版本 %d", version)
	}
	if _, err = fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &params.memory, &params.time, &params.threads); err != nil {
		return nil, nil, params, fmt.Errorf("解析哈希参数：%w", err)
	}
	b64 := base64.RawStdEncoding
	if salt, err = b64.DecodeString(parts[4]); err != nil {
		return nil, nil, params, fmt.Errorf("解码盐值：%w", err)
	}
	if hash, err = b64.DecodeString(parts[5]); err != nil {
		return nil, nil, params, fmt.Errorf("解码哈希：%w", err)
	}
	return salt, hash, params, nil
}
