package domain

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base32"
	"encoding/binary"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// RFC 6238 TOTP：6 位、30 秒步长；密钥为随机 160bit 种子（Base32 无填充编码）。
// 不引入外部二维码/OTP 依赖（规格的依赖审查要求），HMAC-SHA1 动态口令在本地实现。
const (
	totpDigits       = 6
	totpStepSeconds  = 30
	totpSecretBytes  = 20
	totpCodeModulo   = 1_000_000
	recoveryCodePart = 5                                 // 每枚恢复码两段 5 字符
	recoveryAlphabet = "abcdefghjkmnpqrstuvwxyz23456789" // 去掉易混淆字符
)

// ErrSecurityFactorKey 表示实例因子密钥缺失或损坏；不降级为明文存储。
var ErrSecurityFactorKey = errors.New("因子加密密钥不可用")

func totpCode(secret []byte, counter int64) (string, error) {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(counter))
	mac := hmac.New(sha1.New, secret)
	if _, err := mac.Write(buf[:]); err != nil {
		return "", err
	}
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 0x0f
	value := binary.BigEndian.Uint32(sum[offset:offset+4]) & 0x7fffffff
	return fmt.Sprintf("%0*d", totpDigits, value%totpCodeModulo), nil
}

// totpMatchStep 在当前步 ±1 窗口内返回匹配的步号；不泄露是否存在账号。
// 返回 -1 表示窗口内无匹配。
func totpMatchStep(secret []byte, code string, now int64) int64 {
	current := now / totpStepSeconds
	for delta := int64(0); delta <= 1; delta++ {
		for _, step := range []int64{current, current - delta, current + delta} {
			if step < 0 {
				continue
			}
			expect, err := totpCode(secret, step)
			if err == nil && hmac.Equal([]byte(expect), []byte(code)) {
				return step
			}
		}
	}
	return -1
}

func newTotpSecret() ([]byte, string, error) {
	secret := make([]byte, totpSecretBytes)
	if _, err := rand.Read(secret); err != nil {
		return nil, "", err
	}
	return secret, base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(secret), nil
}

// totpEnrollmentURI 生成 otpauth:// 登记串；只在该流程一次展示。
func totpEnrollmentURI(issuer, account, secretBase32 string) string {
	q := url.Values{}
	q.Set("secret", secretBase32)
	q.Set("issuer", issuer)
	q.Set("algorithm", "SHA1")
	q.Set("digits", "6")
	q.Set("period", "30")
	return "otpauth://totp/" + url.PathEscape(issuer+":"+account) + "?" + q.Encode()
}

// securityFactorCipher 用实例专用 AES-256-GCM 密钥做种子带认证加密；
// 绑定因子 ID 为附加数据，密文不可跨因子重放。
type securityFactorCipher struct {
	key       [32]byte
	keyVer    int64
	available bool
}

func newSecurityFactorCipher(key []byte, keyVersion int64) (*securityFactorCipher, error) {
	if len(key) != 32 || keyVersion <= 0 {
		return nil, ErrSecurityFactorKey
	}
	c := &securityFactorCipher{keyVer: keyVersion, available: true}
	copy(c.key[:], key)
	return c, nil
}

func (c *securityFactorCipher) seal(factorID string, secret []byte) (string, error) {
	block, err := aes.NewCipher(c.key[:])
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	out := gcm.Seal(nonce, nonce, secret, []byte("totp\x00"+factorID))
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(out), nil
}

func (c *securityFactorCipher) open(factorID, ciphertext string) ([]byte, error) {
	raw, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(ciphertext)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errFactorCiphertext, err)
	}
	block, err := aes.NewCipher(c.key[:])
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(raw) < gcm.NonceSize()+gcm.Overhead() {
		return nil, errors.New("因子密文不完整")
	}
	secret, err := gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], []byte("totp\x00"+factorID))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errFactorCiphertext, err)
	}
	return secret, nil
}

// errFactorCiphertext 把密文/校验失败归一：不向调用方区分「密钥问题」
// 与「被篡改的存储」——两者都不放行。
var errFactorCiphertext = errors.New("因子密文不可用")

// hashChallengeToken / hashRecoveryCode：高熵不透明串与恢复码各自独立 SHA-256 校验值。
// 输入均为服务端生成的 ≥128bit 随机串，SHA-256 足够且不引入口令哈希误用。
func hashChallengeToken(token string) string {
	sum := sha256.Sum256([]byte("challenge\x00" + token))
	return fmt.Sprintf("%x", sum[:])
}

func hashRecoveryCode(code string) string {
	sum := sha256.Sum256([]byte("recovery\x00" + strings.ToLower(strings.ReplaceAll(code, "-", ""))))
	return fmt.Sprintf("%x", sum[:])
}

// newRecoveryCodes 生成 n 枚一次性恢复码（形如 xxxxx-xxxxx），返回明文与校验值行。
func newRecoveryCodes(n int) ([]string, error) {
	const partLen = recoveryCodePart
	codes := make([]string, 0, n)
	alpha := recoveryAlphabet
	for i := 0; i < n; i++ {
		buf := make([]byte, partLen*2)
		if _, err := rand.Read(buf); err != nil {
			return nil, err
		}
		var b strings.Builder
		for j, v := range buf {
			if j == partLen {
				b.WriteByte('-')
			}
			b.WriteByte(alpha[int(v)%len(alpha)])
		}
		codes = append(codes, b.String())
	}
	return codes, nil
}
