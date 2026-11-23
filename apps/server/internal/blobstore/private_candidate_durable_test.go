package blobstore

import (
	"errors"
	"strings"
	"testing"
)

func TestRegisteredPrivateCandidateSurvivesStoreReopen(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root)
	key := "37b1ecca667366207a8f8619432c7c37"
	input := "普通私有制品"
	candidate, err := store.StageRegisteredPrivateCandidate(t.Context(), key, strings.NewReader(input), int64(len(input)))
	if err != nil {
		t.Fatal(err)
	}
	reopened := NewStore(root)
	if err := reopened.VerifyRegisteredPrivateCandidate(t.Context(), key, candidate.SHA256(), candidate.Size()); err != nil {
		t.Fatal(err)
	}
	if reopened.Exists(candidate.SHA256()) {
		t.Fatal("已登记字节不属于活动树")
	}
	if _, err := reopened.StageRegisteredPrivateCandidate(t.Context(), key, strings.NewReader("别的字节"), 99); err == nil {
		t.Fatal("同文件键不得覆盖")
	}
	if err := reopened.VerifyRegisteredPrivateCandidate(t.Context(), key, candidate.SHA256(), candidate.Size()-1); !errors.Is(err, ErrPrivateCandidateIntegrity) {
		t.Fatalf("错误大小应是已证不一致：%v", err)
	}
	if err := candidate.Discard(); err != nil {
		t.Fatal(err)
	}
	if err := reopened.VerifyRegisteredPrivateCandidate(t.Context(), key, candidate.SHA256(), candidate.Size()); !errors.Is(err, ErrPrivateCandidateIntegrity) {
		t.Fatalf("可信目录中对象缺失应是完整性失败：%v", err)
	}
}
