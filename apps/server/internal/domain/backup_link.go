package domain

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strconv"
	"time"
)

// ErrBackupLinkInvalid 表示备份下载令牌无效或已过期。
var ErrBackupLinkInvalid = errors.New("备份下载链接无效或已过期")

// BackupLinkSigner 签发与校验备份包下载令牌。
//
// 为什么需要它：管理接口统一用 `Authorization: Bearer` 认证，普通的 `<a href>` 下载
// 带不上该请求头；而搬迁场景要求新机器能直接粘贴一个地址把包拉过去。因此用一个
// 只对单个包、有明确截止时间的 HMAC 令牌替代会话——既不引入新账号体系，也不给包
// 本身加密（包内不含任何密钥，加密收益低于流程复杂度）。
//
// 令牌绑定 packageId 与 exp：改动任一项都会使签名失效，无法把链接挪作他用或续期。
type BackupLinkSigner struct{ key []byte }

// NewBackupLinkSigner 从启动密钥派生签名密钥。
// 派生而非直接复用：避免同一份密钥在不同用途间产生可关联性。
func NewBackupLinkSigner(secret []byte) *BackupLinkSigner {
	sum := sha256.Sum256(append(append([]byte(nil), secret...), []byte("jianartifact/backup-link/v1")...))
	return &BackupLinkSigner{key: sum[:]}
}

// Sign 为 packageID 签发截止到 exp 的令牌。
func (s *BackupLinkSigner) Sign(packageID string, exp time.Time) string {
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(packageID))
	mac.Write([]byte{'\n'})
	mac.Write([]byte(strconv.FormatInt(exp.Unix(), 10)))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// Verify 校验令牌：签名匹配且尚未过期。
func (s *BackupLinkSigner) Verify(packageID, token string, exp time.Time, now time.Time) error {
	if token == "" {
		return ErrBackupLinkInvalid
	}
	// 用调用方给出的 exp 重算签名：exp 被改动则签名不匹配。
	if !hmac.Equal([]byte(s.Sign(packageID, exp)), []byte(token)) {
		return ErrBackupLinkInvalid
	}
	if !now.Before(exp) {
		return ErrBackupLinkInvalid
	}
	return nil
}
