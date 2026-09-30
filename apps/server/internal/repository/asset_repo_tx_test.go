package repository_test

import "testing"

// TestCountAndSizeByRepoTxMatchesNonTx 提交点复检用的聚合查询必须与既有聚合口径一致：
// 两者复用同一条 SQL，事务内视图与非事务视图在无写入时必须返回同一结果。
func TestCountAndSizeByRepoTxMatchesNonTx(t *testing.T) {
	assets, repos := newTestRepos(t)
	repoID, err := repos.Create("quota-tx", "raw", "hosted", "public", "{}")
	if err != nil {
		t.Fatalf("创建仓库：%v", err)
	}
	for _, item := range []struct {
		path string
		size int64
	}{{"a.bin", 10}, {"b.bin", 25}} {
		if err := assets.Upsert(repoID, item.path, "hash-"+item.path, item.size, "application/octet-stream", "", ""); err != nil {
			t.Fatalf("写入资产：%v", err)
		}
	}
	count, size, err := assets.CountAndSizeByRepo(repoID)
	if err != nil {
		t.Fatalf("非事务聚合：%v", err)
	}
	if count != 2 || size != 35 {
		t.Fatalf("非事务聚合应为 2 件 / 35 字节，得 %d / %d", count, size)
	}

	tx, err := assets.DB().Beginx()
	if err != nil {
		t.Fatalf("开启事务：%v", err)
	}
	defer func() { _ = tx.Rollback() }()
	txCount, txSize, err := assets.CountAndSizeByRepoTx(tx, repoID)
	if err != nil {
		t.Fatalf("事务内聚合：%v", err)
	}
	if txCount != count || txSize != size {
		t.Fatalf("事务内聚合应与既有口径一致，得 %d / %d，期望 %d / %d", txCount, txSize, count, size)
	}
	// 事务内写入未提交前的增量可见，正是提交点"当前占用 + 本次净增量"的判定依据。
	if _, err := tx.Exec(`INSERT INTO asset (repository_id, path, blob_hash, size, content_type, sha1, md5) VALUES (?, ?, ?, ?, ?, '', '')`,
		repoID, "c.bin", "hash-c.bin", int64(5), "application/octet-stream"); err != nil {
		t.Fatalf("事务内写入资产：%v", err)
	}
	txCount, txSize, err = assets.CountAndSizeByRepoTx(tx, repoID)
	if err != nil {
		t.Fatalf("事务内聚合：%v", err)
	}
	if txCount != 3 || txSize != 40 {
		t.Fatalf("事务内视图应含未提交写入（3 件 / 40 字节），得 %d / %d", txCount, txSize)
	}
}
