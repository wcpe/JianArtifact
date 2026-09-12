package domain

import (
	"errors"
	"testing"
	"time"
)

func TestBackupLinkSignerRoundTrip(t *testing.T) {
	signer := NewBackupLinkSigner([]byte("test-secret-for-backup-link"))
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	exp := now.Add(30 * time.Minute)

	token := signer.Sign("bk-20260910-120000-abcdef", exp)
	if token == "" {
		t.Fatal("签名不能为空")
	}
	if err := signer.Verify("bk-20260910-120000-abcdef", token, exp, now); err != nil {
		t.Fatalf("有效令牌应通过：%v", err)
	}
}

// TestBackupLinkSignerRejectsTampering 覆盖令牌被挪用到别的包或延期使用。
func TestBackupLinkSignerRejectsTampering(t *testing.T) {
	signer := NewBackupLinkSigner([]byte("test-secret-for-backup-link"))
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	exp := now.Add(30 * time.Minute)
	token := signer.Sign("bk-aaa", exp)

	cases := []struct {
		name      string
		packageID string
		token     string
		exp       time.Time
		now       time.Time
	}{
		{"换包名", "bk-bbb", token, exp, now},
		{"延长有效期", "bk-aaa", token, exp.Add(time.Hour), now},
		{"缩短有效期", "bk-aaa", token, exp.Add(-time.Minute), now},
		{"已过期", "bk-aaa", token, exp, exp.Add(time.Second)},
		{"空令牌", "bk-aaa", "", exp, now},
		{"伪造令牌", "bk-aaa", "ZmFrZXRva2Vu", exp, now},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := signer.Verify(tc.packageID, tc.token, tc.exp, tc.now); !errors.Is(err, ErrBackupLinkInvalid) {
				t.Fatalf("应返回 ErrBackupLinkInvalid，实际 %v", err)
			}
		})
	}
}

// TestBackupLinkSignerKeyIsDerived 验证签名密钥是从启动密钥派生的，
// 而不是直接复用原值：换一个启动密钥必须导致旧令牌失效。
func TestBackupLinkSignerKeyIsDerived(t *testing.T) {
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	exp := now.Add(time.Hour)
	token := NewBackupLinkSigner([]byte("secret-one")).Sign("bk-aaa", exp)
	if err := NewBackupLinkSigner([]byte("secret-two")).Verify("bk-aaa", token, exp, now); !errors.Is(err, ErrBackupLinkInvalid) {
		t.Fatalf("换密钥后旧令牌应失效，实际 %v", err)
	}
}

// TestBackupLinkSignerBoundaryAtExpiry 验证到期瞬间即失效（用 now.Before(exp) 判定）。
func TestBackupLinkSignerBoundaryAtExpiry(t *testing.T) {
	signer := NewBackupLinkSigner([]byte("k"))
	exp := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	token := signer.Sign("bk-aaa", exp)

	if err := signer.Verify("bk-aaa", token, exp, exp.Add(-time.Nanosecond)); err != nil {
		t.Fatalf("到期前一刻应有效：%v", err)
	}
	if err := signer.Verify("bk-aaa", token, exp, exp); !errors.Is(err, ErrBackupLinkInvalid) {
		t.Fatalf("到期时刻应失效，实际 %v", err)
	}
}
