package httpserver_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/api"
)

// putChunk 以 application/octet-stream 上传单个分片（request 辅助只设 JSON 头，故单独实现）。
func (e *backupEnv) putChunk(t *testing.T, uploadID string, index int, token string, data []byte) (int, []byte) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut,
		fmt.Sprintf("/api/v1/backups/uploads/%s/chunks/%d", uploadID, index), bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/octet-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

// TestBackupUploadAllUnauthorized 覆盖五个端点的鉴权：未带令牌均 401。
func TestBackupUploadAllUnauthorized(t *testing.T) {
	env := newBackupEnv(t)
	cases := []struct {
		method string
		target string
	}{
		{http.MethodPost, "/api/v1/backups/uploads"},
		{http.MethodPut, "/api/v1/backups/uploads/up-x/chunks/0"},
		{http.MethodGet, "/api/v1/backups/uploads/up-x"},
		{http.MethodPost, "/api/v1/backups/uploads/up-x/complete"},
		{http.MethodPost, "/api/v1/backups/uploads/up-x/abort"},
	}
	for _, c := range cases {
		if code, _ := env.request(t, c.method, c.target, "", nil); code != http.StatusUnauthorized {
			t.Fatalf("%s %s 应 401，实际 %d", c.method, c.target, code)
		}
	}
}

// TestBackupUploadInitReturnsSession 覆盖发起会话：201 + chunkSize>0 + uploadedChunks 为空。
func TestBackupUploadInitReturnsSession(t *testing.T) {
	env := newBackupEnv(t)
	token := env.adminToken(t)
	code, body := env.request(t, http.MethodPost, "/api/v1/backups/uploads", token,
		map[string]any{"fileName": "pkg.tar.gz", "totalBytes": int64(20)})
	if code != http.StatusCreated {
		t.Fatalf("发起会话应 201，实际 %d body=%s", code, body)
	}
	var s api.BackupUploadSession
	if err := json.Unmarshal(body, &s); err != nil {
		t.Fatalf("解析会话：%v", err)
	}
	if s.UploadId == "" {
		t.Fatal("会话缺少 uploadId")
	}
	if s.ChunkSize <= 0 {
		t.Fatalf("chunkSize 应 >0，实得 %d", s.ChunkSize)
	}
	if s.FileName != "pkg.tar.gz" {
		t.Fatalf("fileName=%q，期望 pkg.tar.gz", s.FileName)
	}
	if len(s.UploadedChunks) != 0 {
		t.Fatalf("新会话 uploadedChunks 应为空，实得 %v", s.UploadedChunks)
	}
	if s.Status != "initialized" {
		t.Fatalf("status=%q，期望 initialized", s.Status)
	}
}

// TestBackupUploadTwoChunksAndResume 覆盖逐片上传 + GET 反映进度 + 续传语义（以磁盘为准）。
func TestBackupUploadTwoChunksAndResume(t *testing.T) {
	env := newBackupEnv(t)
	token := env.adminToken(t)

	// 先用一个合法的小会话探明服务端分片大小（chunkSize 由服务端固定决定）。
	initCode, initBody := env.request(t, http.MethodPost, "/api/v1/backups/uploads", token,
		map[string]any{"fileName": "pkg.tar.gz", "totalBytes": int64(1024)})
	if initCode != http.StatusCreated {
		t.Fatalf("探明分片大小：status=%d body=%s", initCode, initBody)
	}
	var probe api.BackupUploadSession
	if err := json.Unmarshal(initBody, &probe); err != nil {
		t.Fatalf("解析会话：%v", err)
	}
	chunkSize := probe.ChunkSize
	if chunkSize <= 0 {
		t.Fatalf("chunkSize 应 >0，实得 %d", chunkSize)
	}
	// 构造一段跨两片的数据：首片 chunkSize，次片 1024 字节。
	data := bytes.Repeat([]byte("J"), int(chunkSize)+1024)
	totalBytes := int64(len(data))

	// 发起一次真实会话（totalBytes 对应两片）。
	code, body := env.request(t, http.MethodPost, "/api/v1/backups/uploads", token,
		map[string]any{"fileName": "pkg.tar.gz", "totalBytes": totalBytes})
	if code != http.StatusCreated {
		t.Fatalf("发起会话：status=%d body=%s", code, body)
	}
	var sess api.BackupUploadSession
	if err := json.Unmarshal(body, &sess); err != nil {
		t.Fatalf("解析会话：%v", err)
	}
	uploadID := sess.UploadId

	// 切两片并上传。
	c0 := data[:chunkSize]
	c1 := data[chunkSize:]
	if code, b := env.putChunk(t, uploadID, 0, token, c0); code != http.StatusOK {
		t.Fatalf("上传片0：status=%d body=%s", code, b)
	}
	if code, b := env.putChunk(t, uploadID, 1, token, c1); code != http.StatusOK {
		t.Fatalf("上传片1：status=%d body=%s", code, b)
	}

	// GET 应反映 [0,1]。
	code, body = env.request(t, http.MethodGet, "/api/v1/backups/uploads/"+uploadID, token, nil)
	if code != http.StatusOK {
		t.Fatalf("查询会话：status=%d body=%s", code, body)
	}
	if err := json.Unmarshal(body, &sess); err != nil {
		t.Fatalf("解析会话：%v", err)
	}
	if len(sess.UploadedChunks) != 2 || sess.UploadedChunks[0] != 0 || sess.UploadedChunks[1] != 1 {
		t.Fatalf("GET 应反映 [0,1]，实得 %v", sess.UploadedChunks)
	}

	// 续传语义：直接删掉磁盘上的第 1 片，GET 必须只反映真实存在的片（[0]）。
	chunk1Path := filepath.Join(env.dir, "backup-uploads", uploadID, "chunks", "1")
	if err := os.Remove(chunk1Path); err != nil && !os.IsNotExist(err) {
		t.Fatalf("删除磁盘片1：%v", err)
	}
	code, body = env.request(t, http.MethodGet, "/api/v1/backups/uploads/"+uploadID, token, nil)
	if code != http.StatusOK {
		t.Fatalf("查询会话：status=%d body=%s", code, body)
	}
	if err := json.Unmarshal(body, &sess); err != nil {
		t.Fatalf("解析会话：%v", err)
	}
	if len(sess.UploadedChunks) != 1 || sess.UploadedChunks[0] != 0 {
		t.Fatalf("删盘后 GET 应只反映 [0]，实得 %v", sess.UploadedChunks)
	}

	// 补回片1，恢复完整。
	if code, b := env.putChunk(t, uploadID, 1, token, c1); code != http.StatusOK {
		t.Fatalf("补片1：status=%d body=%s", code, b)
	}
}

// TestBackupUploadCompleteStagesPendingRestart 覆盖成功链路：
// 生成一份真实备份包 → 按分片上传其字节 → 拼装 → 触发本地导入 → 记录走到 pending_restart。
// 目标已自举管理员（非空），故 complete 须传 overwrite=true。
func TestBackupUploadCompleteStagesPendingRestart(t *testing.T) {
	env := newBackupEnv(t)
	token := env.adminToken(t)
	env.seedAsset(t, "payload-for-upload-complete")

	// 生成一份真实备份包作为上传源（分片上传的最终目标必须是合法归档）。
	bc, bb := env.request(t, http.MethodPost, "/api/v1/backups", token, map[string]any{"mode": "hot"})
	if bc != http.StatusCreated {
		t.Fatalf("生成备份：status=%d body=%s", bc, bb)
	}
	var pkg api.BackupPackage
	if err := json.Unmarshal(bb, &pkg); err != nil {
		t.Fatalf("解析生成响应：%v", err)
	}
	env.waitDone(t, token, pkg.PackageId)
	raw, err := os.ReadFile(env.svc.PackagePath(pkg.PackageId))
	if err != nil {
		t.Fatalf("读取源包：%v", err)
	}
	sum := sha256.Sum256(raw)
	totalBytes := int64(len(raw))

	// 发起上传会话（带声明的整体 sha256）。
	code, body := env.request(t, http.MethodPost, "/api/v1/backups/uploads", token,
		map[string]any{"fileName": "pkg.tar.gz", "totalBytes": totalBytes, "sha256": hex.EncodeToString(sum[:])})
	if code != http.StatusCreated {
		t.Fatalf("发起会话：status=%d body=%s", code, body)
	}
	var sess api.BackupUploadSession
	if err := json.Unmarshal(body, &sess); err != nil {
		t.Fatalf("解析会话：%v", err)
	}
	uploadID := sess.UploadId
	chunkSize := sess.ChunkSize
	if chunkSize <= 0 {
		t.Fatalf("chunkSize 应 >0，实得 %d", chunkSize)
	}

	// 按 chunkSize 切分真实包并逐片上传。
	var chunks [][]byte
	for i := 0; i < len(raw); i += int(chunkSize) {
		end := i + int(chunkSize)
		if end > len(raw) {
			end = len(raw)
		}
		chunks = append(chunks, raw[i:end])
	}
	for i, c := range chunks {
		if code, b := env.putChunk(t, uploadID, i, token, c); code != http.StatusOK {
			t.Fatalf("上传片%d：status=%d body=%s", i, code, b)
		}
	}

	// complete（带 overwrite，因目标非空）。
	code, body = env.request(t, http.MethodPost, "/api/v1/backups/uploads/"+uploadID+"/complete", token,
		map[string]any{"overwrite": true, "sha256": hex.EncodeToString(sum[:])})
	if code != http.StatusAccepted {
		t.Fatalf("complete 应 202，实际 %d body=%s", code, body)
	}
	var imp api.BackupImport
	if err := json.Unmarshal(body, &imp); err != nil {
		t.Fatalf("解析导入响应：%v", err)
	}
	if imp.ImportId == "" {
		t.Fatal("complete 响应缺少 importId")
	}
	if imp.Status == "failed" {
		t.Fatalf("导入已失败：errorCode=%v", deref(t, imp.ErrorCode))
	}

	// 轮询导入记录直到 pending_restart（目标非空 + overwrite 应走到该终态）。
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		code, body = env.request(t, http.MethodGet, "/api/v1/backups/imports/"+imp.ImportId, token, nil)
		if code != http.StatusOK {
			t.Fatalf("查询导入：status=%d body=%s", code, body)
		}
		if err := json.Unmarshal(body, &imp); err != nil {
			t.Fatalf("解析导入：%v", err)
		}
		switch imp.Status {
		case "pending_restart":
			return
		case "failed":
			t.Fatalf("导入失败：errorCode=%v error=%v", deref(t, imp.ErrorCode), deref(t, imp.Error))
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("导入未在超时内到 pending_restart，终态 status=%q", imp.Status)
}

// TestBackupUploadAbortThenGone 覆盖取消：204；之后 GET 反映状态已中止（记录留作审计）。
func TestBackupUploadAbortThenGone(t *testing.T) {
	env := newBackupEnv(t)
	token := env.adminToken(t)
	code, body := env.request(t, http.MethodPost, "/api/v1/backups/uploads", token,
		map[string]any{"fileName": "pkg.tar.gz", "totalBytes": int64(20)})
	if code != http.StatusCreated {
		t.Fatalf("发起会话：status=%d body=%s", code, body)
	}
	var sess api.BackupUploadSession
	if err := json.Unmarshal(body, &sess); err != nil {
		t.Fatalf("解析会话：%v", err)
	}
	uploadID := sess.UploadId

	if code, b := env.request(t, http.MethodPost, "/api/v1/backups/uploads/"+uploadID+"/abort", token, nil); code != http.StatusNoContent {
		t.Fatalf("abort 应 204，实际 %d body=%s", code, b)
	}
	code, body = env.request(t, http.MethodGet, "/api/v1/backups/uploads/"+uploadID, token, nil)
	if code != http.StatusOK {
		t.Fatalf("abort 后 GET 应 200（状态 aborted），实际 %d body=%s", code, body)
	}
	if err := json.Unmarshal(body, &sess); err != nil {
		t.Fatalf("解析会话：%v", err)
	}
	if sess.Status != "aborted" {
		t.Fatalf("abort 后状态应 aborted，实得 %q", sess.Status)
	}
	// 磁盘会话目录应被清掉。
	if _, statErr := os.Stat(filepath.Join(env.dir, "backup-uploads", uploadID)); !os.IsNotExist(statErr) {
		t.Fatal("abort 后磁盘会话目录应被清理")
	}
}

// TestBackupUploadChunkIndexOutOfRangeReturns400 覆盖片号越界：合法整数但超出 [0,expected) → 400。
func TestBackupUploadChunkIndexOutOfRangeReturns400(t *testing.T) {
	env := newBackupEnv(t)
	token := env.adminToken(t)
	code, body := env.request(t, http.MethodPost, "/api/v1/backups/uploads", token,
		map[string]any{"fileName": "pkg.tar.gz", "totalBytes": int64(20)})
	if code != http.StatusCreated {
		t.Fatalf("发起会话：status=%d body=%s", code, body)
	}
	var sess api.BackupUploadSession
	if err := json.Unmarshal(body, &sess); err != nil {
		t.Fatalf("解析会话：%v", err)
	}
	// totalBytes=20，chunkSize=8MiB → 期望 1 片（index 0）；传 index=99 越界。
	if code, b := env.putChunk(t, sess.UploadId, 99, token, []byte("x")); code != http.StatusBadRequest {
		t.Fatalf("越界片号应 400，实际 %d body=%s", code, b)
	}
}

// TestBackupUploadCompleteMissingChunkReturns400 覆盖缺失片直接 complete：按 ErrValidation 映射 → 400
// （缺失片属「客户端上传不完整」的输入校验错，与片号越界/超额同族，故统一 400 而非 409）。
func TestBackupUploadCompleteMissingChunkReturns400(t *testing.T) {
	env := newBackupEnv(t)
	token := env.adminToken(t)
	code, body := env.request(t, http.MethodPost, "/api/v1/backups/uploads", token,
		map[string]any{"fileName": "pkg.tar.gz", "totalBytes": int64(16)})
	if code != http.StatusCreated {
		t.Fatalf("发起会话：status=%d body=%s", code, body)
	}
	var sess api.BackupUploadSession
	if err := json.Unmarshal(body, &sess); err != nil {
		t.Fatalf("解析会话：%v", err)
	}
	// 故意不传任何分片，直接 complete。
	code, body = env.request(t, http.MethodPost, "/api/v1/backups/uploads/"+sess.UploadId+"/complete", token, nil)
	if code != http.StatusBadRequest {
		t.Fatalf("缺失片 complete 应 400，实际 %d body=%s", code, body)
	}
}
