package auth

import (
	"testing"
	"time"
)

func TestHashAndVerifyPassword(t *testing.T) {
	hash, err := HashPassword("s3cret-pw")
	if err != nil {
		t.Fatalf("HashPassword：%v", err)
	}
	if hash == "s3cret-pw" {
		t.Fatal("哈希不应等于明文")
	}
	if err := VerifyPassword("s3cret-pw", hash); err != nil {
		t.Errorf("正确口令应校验通过：%v", err)
	}
	if err := VerifyPassword("wrong-pw", hash); err != ErrPasswordMismatch {
		t.Errorf("错误口令应返回 ErrPasswordMismatch，实际：%v", err)
	}
}

func TestHashPasswordUniqueSalt(t *testing.T) {
	h1, _ := HashPassword("same")
	h2, _ := HashPassword("same")
	if h1 == h2 {
		t.Error("相同口令应因随机盐产生不同哈希")
	}
}

func TestJWTIssueAndParse(t *testing.T) {
	m := NewJWTManager([]byte("test-secret-key"))
	token, jti, exp, err := m.Issue(42, "admin")
	if err != nil {
		t.Fatalf("Issue：%v", err)
	}
	if jti == "" || !exp.After(time.Now()) {
		t.Fatal("应返回非空 jti 与将来的过期时间")
	}
	claims, err := m.Parse(token)
	if err != nil {
		t.Fatalf("Parse：%v", err)
	}
	if claims.Subject != "42" || claims.Role != "admin" || claims.ID != jti {
		t.Errorf("claims 不符：sub=%s role=%s jti=%s", claims.Subject, claims.Role, claims.ID)
	}
}

func TestJWTRejectsWrongSecret(t *testing.T) {
	token, _, _, _ := NewJWTManager([]byte("secret-a")).Issue(1, "user")
	if _, err := NewJWTManager([]byte("secret-b")).Parse(token); err != ErrInvalidToken {
		t.Errorf("异密钥应拒绝，实际：%v", err)
	}
}

func TestJWTRejectsExpired(t *testing.T) {
	m := NewJWTManager([]byte("secret"))
	m.now = func() time.Time { return time.Now().Add(-24 * time.Hour) } // 令签发的令牌已过期
	token, _, _, _ := m.Issue(1, "user")
	m.now = time.Now
	if _, err := m.Parse(token); err != ErrInvalidToken {
		t.Errorf("过期令牌应拒绝，实际：%v", err)
	}
}

func TestGenerateTokenDigest(t *testing.T) {
	plain, digest := GenerateToken()
	if len(plain) < 8 || plain[:4] != "jat_" {
		t.Errorf("明文应带 jat_ 前缀：%s", plain)
	}
	if digest == plain {
		t.Fatal("摘要不应等于明文")
	}
	if DigestToken(plain) != digest {
		t.Error("DigestToken 应对同一明文稳定")
	}
}
