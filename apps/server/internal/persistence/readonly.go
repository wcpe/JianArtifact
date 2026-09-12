package persistence

import (
	"database/sql"
	"fmt"
	"regexp"
)

// tableNamePattern 约束 RowCounts 的标识符拼接，杜绝注入。
var tableNamePattern = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// AssetBlobs 打开 dbPath 并返回 asset 表引用的 blob 集合（哈希 → 字节数）。
//
// 这是备份包 blob 集合的唯一权威来源：以快照库的 asset 表为准，而不是扫描
// blobs 目录。理由有二：
//  1. 目录里可能残留未被任何资产引用的 blob（回收前的中间态），扫目录会把它们
//     打进包，造成包体虚大；
//  2. asset 表是"哪些内容还能被下载"的权威定义，导入端按同一集合校验即可闭环。
//
// 刻意不含 blob_quarantine：隔离区 blob 已从活动路径移走，属于在途操作的回滚态，
// 不在搬迁的数据边界内。若存在未完成的资产操作，打包阶段会因读不到 blob 而失败，
// 这是预期行为（宁可在源头报错，也不产出静默缺内容的包）。
func AssetBlobs(dbPath string) (map[string]int64, error) {
	conn, err := sql.Open("sqlite", snapshotDSN(dbPath))
	if err != nil {
		return nil, fmt.Errorf("打开快照 %s：%w", dbPath, err)
	}
	defer func() { _ = conn.Close() }()
	conn.SetMaxOpenConns(1)

	rows, err := conn.Query(`SELECT blob_hash, MAX(size) FROM asset GROUP BY blob_hash`)
	if err != nil {
		return nil, fmt.Errorf("读取 asset blob 集合：%w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[string]int64)
	for rows.Next() {
		var hash string
		var size int64
		if err := rows.Scan(&hash, &size); err != nil {
			return nil, fmt.Errorf("扫描 asset 行：%w", err)
		}
		out[hash] = size
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历 asset 行：%w", err)
	}
	return out, nil
}

// RowCounts 返回各表的行数，用于包内规模自检与导入端核对。
// tables 由调用方以固定白名单给出；标识符仍经正则校验后才拼接 SQL。
func RowCounts(dbPath string, tables []string) (map[string]int, error) {
	conn, err := sql.Open("sqlite", snapshotDSN(dbPath))
	if err != nil {
		return nil, fmt.Errorf("打开快照 %s：%w", dbPath, err)
	}
	defer func() { _ = conn.Close() }()
	conn.SetMaxOpenConns(1)

	out := make(map[string]int, len(tables))
	for _, table := range tables {
		if !tableNamePattern.MatchString(table) {
			return nil, fmt.Errorf("非法表名：%q", table)
		}
		var n int
		if err := conn.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
			return nil, fmt.Errorf("统计表 %s：%w", table, err)
		}
		out[table] = n
	}
	return out, nil
}
