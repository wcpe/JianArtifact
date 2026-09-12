package credential_test

import (
	"bytes"
	"net/http"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/migration/credential"
)

func TestSourceAuthSealRoundTripAndBindsSource(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	sealer, err := credential.NewSealer(key)
	if err != nil {
		t.Fatalf("构造密封器：%v", err)
	}
	auth := credential.SourceAuth{Type: credential.TypeBasic, Username: "nexus-user", Password: "nexus-pass"}
	ciphertext, err := sealer.Seal(auth, []byte("online_rest:{\"url\":\"https://nexus.example\"}"))
	if err != nil {
		t.Fatalf("加密认证：%v", err)
	}
	if bytes.Contains(ciphertext, []byte(auth.Password)) {
		t.Fatal("密文不得包含明文口令")
	}
	got, err := sealer.Open(ciphertext, []byte("online_rest:{\"url\":\"https://nexus.example\"}"))
	if err != nil {
		t.Fatalf("解密认证：%v", err)
	}
	if got != auth {
		t.Fatalf("解密认证 = %#v，期望 %#v", got, auth)
	}
	if _, err := sealer.Open(ciphertext, []byte("online_rest:{\"url\":\"https://other.example\"}")); err == nil {
		t.Fatal("来源变化时密文必须不可解密")
	}
}

func TestSourceAuthUsesExplicitBearerScheme(t *testing.T) {
	auth := credential.SourceAuth{Type: credential.TypeBearer, Token: "jwt:has-colon"}
	if err := auth.Validate(); err != nil {
		t.Fatalf("Bearer 应有效：%v", err)
	}
	req, err := http.NewRequest(http.MethodGet, "https://nexus.example", nil)
	if err != nil {
		t.Fatal(err)
	}
	auth.Apply(req)
	if got, want := req.Header.Get("Authorization"), "Bearer jwt:has-colon"; got != want {
		t.Fatalf("Authorization = %q，期望 %q", got, want)
	}
}

func TestSourceAuthRejectsIncompleteOrMixedSecrets(t *testing.T) {
	for _, auth := range []credential.SourceAuth{
		{Type: credential.TypeBasic, Username: "only-user"},
		{Type: credential.TypeBearer},
		{Type: credential.TypeAnonymous, Token: "must-not-be-accepted"},
		{Type: "unknown", Token: "value"},
	} {
		if err := auth.Validate(); err == nil {
			t.Fatalf("非法认证应拒绝：%#v", auth)
		}
	}
}
