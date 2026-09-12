package protocol

import (
	"bytes"
	"errors"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/domain"
)

func TestCargoIndexPath(t *testing.T) {
	tests := []struct {
		name string
		want string
	}{
		{name: "a", want: "1/a"},
		{name: "ab", want: "2/ab"},
		{name: "abc", want: "3/a/abc"},
		{name: "abcd", want: "ab/cd/abcd"},
	}
	for _, tt := range tests {
		if got := cargoIndexPath(tt.name); got != tt.want {
			t.Errorf("cargoIndexPath(%q) = %q，期望 %q", tt.name, got, tt.want)
		}
	}
}

func TestCargoMergeIndexKeepsFirstMemberAndYank(t *testing.T) {
	got, err := mergeCargoIndex([]byte(`{"name":"demo","vers":"1.0.0","cksum":"a","yanked":false}`+"\n"), []byte(`{"name":"demo","vers":"1.0.0","cksum":"b","yanked":true}`+"\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{`{"name":"demo","vers":"1.0.0","cksum":"a","yanked":false}`}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("合并索引 = %#v，期望 %#v", got, want)
	}
	changed, ok, err := setCargoYanked(got, "1.0.0", true)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || !containsYanked(changed[0]) {
		t.Fatal("yank 应更新版本记录")
	}
}

func TestOCIDigestValidation(t *testing.T) {
	for _, digest := range []string{"sha256:" + string(make([]byte, 64)), "sha1:abc", "sha256:abc", ""} {
		if _, ok := validOCIDigest(digest); ok {
			t.Errorf("摘要 %q 不应通过校验", digest)
		}
	}
	if hex, ok := validOCIDigest("sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"); !ok || hex == "" {
		t.Fatal("合法 sha256 摘要应通过校验")
	}
}

func TestOCIExpiredUploadSessionRemovesTempFile(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "oci-upload-")
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	session := &ociUploadSession{filePath: file.Name(), createdAt: time.Now().Add(-time.Minute)}
	handler := &OCIHandler{uploadTTL: time.Second}
	handler.sessions.Store("expired", session)
	handler.cleanupExpiredUploads(time.Now())
	if _, exists := handler.sessions.Load("expired"); exists {
		t.Fatal("过期 OCI 上传会话必须移除")
	}
	if _, err := os.Stat(file.Name()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("过期 OCI 上传临时文件必须删除，err=%v", err)
	}
}

func TestOCIChunkUploadRejectsDataBeyondReservedQuota(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "oci-upload-")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	session := &ociUploadSession{limit: 3}
	handler := &OCIHandler{}
	n, err := handler.copyUploadChunk(session, file, bytes.NewReader([]byte("oversized")))
	if !errors.Is(err, domain.ErrQuotaExceeded) || n != 3 || session.size != 3 {
		t.Fatalf("分块上传必须在预留上限处中断：n=%d size=%d err=%v", n, session.size, err)
	}
	info, statErr := file.Stat()
	if statErr != nil {
		t.Fatalf("读取临时文件：%v", statErr)
	}
	if info.Size() != 3 {
		t.Fatalf("临时文件不得写入超出额度的数据：size=%d", info.Size())
	}
}

func TestOCIUploadClosureSettlesReservationOnce(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "oci-upload-")
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	var calls int
	session := &ociUploadSession{filePath: file.Name(), size: 3, settle: func(success bool, size int64) {
		if success || size != 3 {
			t.Fatalf("超时关闭必须释放原预留：success=%t size=%d", success, size)
		}
		calls++
	}}
	handler := &OCIHandler{}
	handler.sessions.Store("session", session)
	handler.closeUpload("session", session)
	handler.closeUpload("session", session)
	if calls != 1 {
		t.Fatalf("上传会话无论关闭多少次都只能结算一次：%d", calls)
	}
}

func TestOCIUploadSuccessfulCloseSettlesReservationOnce(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "oci-upload-")
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	var calls int
	session := &ociUploadSession{filePath: file.Name(), size: 3, settle: func(success bool, size int64) {
		if !success || size != 3 {
			t.Fatalf("成功关闭必须按实际字节结算：success=%t size=%d", success, size)
		}
		calls++
	}}
	handler := &OCIHandler{}
	handler.sessions.Store("session", session)
	session.mu.Lock()
	cleanup := handler.closeUploadLocked("session", session, true)
	session.mu.Unlock()
	cleanup()
	handler.closeUpload("session", session)
	if calls != 1 {
		t.Fatalf("成功关闭后不得重复结算：%d", calls)
	}
}

func TestFormatUploadSpoolsRejectQuotaBeforeWritingOversizedTempFile(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv("TMP", tempDir)
	t.Setenv("TEMP", tempDir)
	for _, spool := range []struct {
		name string
		fn   func(io.Reader, int64) (*os.File, int64, error)
	}{
		{name: "pypi", fn: spoolPyPIFile},
		{name: "nuget", fn: spoolNuGetReader},
	} {
		t.Run(spool.name, func(t *testing.T) {
			_, _, err := spool.fn(bytes.NewReader([]byte("oversized")), 3)
			if !errors.Is(err, domain.ErrQuotaExceeded) {
				t.Fatalf("超过发布额度必须拒绝，实际：%v", err)
			}
			entries, readErr := os.ReadDir(tempDir)
			if readErr != nil {
				t.Fatalf("读取临时目录：%v", readErr)
			}
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), "jianartifact-") {
					t.Fatalf("拒绝的上传不得遗留临时文件：%s", entry.Name())
				}
			}
		})
	}
}
