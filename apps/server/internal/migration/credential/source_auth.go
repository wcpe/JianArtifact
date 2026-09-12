// Package credential 定义在线迁移来源认证及其任务密文存储。
package credential

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const (
	TypeAnonymous = "anonymous"
	TypeBasic     = "basic"
	TypeBearer    = "bearer"
)

// SourceAuth 是仅在请求处理或任务运行时保存明文的 Nexus 来源认证。
// Basic 同时支持用户名/密码及 Nexus User Token 的名称/Passcode。
type SourceAuth struct {
	Type     string `json:"type"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	Token    string `json:"token,omitempty"`
}

// Validate 校验认证联合体，避免类型与认证头推断产生歧义。
func (a SourceAuth) Validate() error {
	switch a.Type {
	case TypeAnonymous:
		if a.Username != "" || a.Password != "" || a.Token != "" {
			return errors.New("匿名认证不能携带认证材料")
		}
	case TypeBasic:
		if strings.TrimSpace(a.Username) == "" || a.Password == "" || a.Token != "" {
			return errors.New("Basic 认证须包含用户名和口令，且不能携带令牌")
		}
	case TypeBearer:
		if a.Username != "" || a.Password != "" || strings.TrimSpace(a.Token) == "" {
			return errors.New("Bearer 认证须仅包含令牌")
		}
	default:
		return errors.New("来源认证类型无效")
	}
	return nil
}

// Apply 仅将已校验的认证材料写入当前请求。
func (a SourceAuth) Apply(req *http.Request) {
	switch a.Type {
	case TypeBasic:
		req.SetBasicAuth(a.Username, a.Password)
	case TypeBearer:
		req.Header.Set("Authorization", "Bearer "+a.Token)
	}
}

// FromLegacy 将已有 credentialRef 解析结果保持原有的冒号兼容规则。
func FromLegacy(raw string) SourceAuth {
	if raw == "" {
		return SourceAuth{Type: TypeAnonymous}
	}
	if strings.Contains(raw, ":") {
		parts := strings.SplitN(raw, ":", 2)
		return SourceAuth{Type: TypeBasic, Username: parts[0], Password: parts[1]}
	}
	return SourceAuth{Type: TypeBearer, Token: raw}
}

// Sealer 使用 AES-256-GCM 保存任务认证材料。密文布局为 nonce || sealed JSON。
type Sealer struct {
	aead cipher.AEAD
}

// NewSealer 以独立的 32 字节迁移凭据密钥构造密封器。
func NewSealer(key []byte) (*Sealer, error) {
	if len(key) != 32 {
		return nil, errors.New("迁移凭据密钥必须为 32 字节")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("初始化迁移凭据加密：%w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("初始化迁移凭据认证加密：%w", err)
	}
	return &Sealer{aead: aead}, nil
}

// Seal 将非匿名认证加密，并用任务安全来源配置作为附加认证数据。
func (s *Sealer) Seal(auth SourceAuth, additionalData []byte) ([]byte, error) {
	if s == nil || s.aead == nil {
		return nil, errors.New("迁移凭据加密未配置")
	}
	if err := auth.Validate(); err != nil {
		return nil, err
	}
	if auth.Type == TypeAnonymous {
		return nil, errors.New("匿名认证无需加密保存")
	}
	payload, err := json.Marshal(auth)
	if err != nil {
		return nil, fmt.Errorf("序列化迁移认证：%w", err)
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("生成迁移凭据随机数：%w", err)
	}
	return s.aead.Seal(nonce, nonce, payload, additionalData), nil
}

// Open 解密并再次校验任务认证材料。
func (s *Sealer) Open(ciphertext, additionalData []byte) (SourceAuth, error) {
	if s == nil || s.aead == nil {
		return SourceAuth{}, errors.New("迁移凭据加密未配置")
	}
	if len(ciphertext) < s.aead.NonceSize()+s.aead.Overhead() {
		return SourceAuth{}, errors.New("迁移凭据密文无效")
	}
	nonce := ciphertext[:s.aead.NonceSize()]
	payload, err := s.aead.Open(nil, nonce, ciphertext[s.aead.NonceSize():], additionalData)
	if err != nil {
		return SourceAuth{}, errors.New("迁移凭据无法解密")
	}
	var auth SourceAuth
	if err := json.Unmarshal(payload, &auth); err != nil {
		return SourceAuth{}, errors.New("迁移凭据格式无效")
	}
	if err := auth.Validate(); err != nil || auth.Type == TypeAnonymous {
		return SourceAuth{}, errors.New("迁移凭据内容无效")
	}
	return auth, nil
}
