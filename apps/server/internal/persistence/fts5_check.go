package persistence

import (
	"fmt"

	"github.com/jmoiron/sqlx"
)

// CheckFTS5Support 探测当前 SQLite 构建是否具备搜索索引所需的两项能力：
// fts5 模块本身，以及 trigram 分词器（`tokenize='trigram'`）。
//
// 为什么要探测：asset_path_fts 是 FTS5 虚拟表（迁移 0049），而 fts5 与 trigram 都是**构建期**
// 编入 SQLite 的能力。modernc.org/sqlite v1.60.1 实测两者可用，但换成 CGO 构建、换驱动或换
// 版本时能力可能不同。若不探测，缺能力的部署会在升级时挂在一句泛化的 `SQL logic error` 上，
// 排查成本极高；探测后给出可读的中文根因，直接指向「换构建 / 换驱动」。
//
// 探测方式：在一个独立的**内存库**上建同名形态的探测虚拟表再 drop。刻意不复用主库：
// ①不往业务库里塞探测对象，进程在探测中途被杀也不会留下残留表；②不与主库争用连接池
// （主库池见 OpenWithMaxConns），探测与业务查询互不影响。
//
// 探测库固定单连接（SetMaxOpenConns(1)）：`file::memory:` 是**每条连接各一份**的独立空库，
// 池里若出现第二条连接，它看不到第一条连接建的探测表，DROP TABLE 会以 "no such table" 失败，
// 把「构建缺能力」误报成「自检失败」这种自伤。上限 1 让整个探测始终落在同一条连接上。
//
// 调用时机：Migrate() **之前**、开始对外服务之前（wiring.go 的 openServices）。必须早于
// 迁移，因为迁移 0049 的第一条语句就是建 fts5 虚拟表——排在后面就又会先抛泛化 SQL 错误。
func CheckFTS5Support() error {
	probe, err := sqlx.Connect("sqlite", "file::memory:")
	if err != nil {
		return fmt.Errorf("搜索索引自检失败：无法打开临时内存库（%v）", err)
	}
	defer func() { _ = probe.Close() }()
	probe.SetMaxOpenConns(1)

	// fts5 模块本身：先建最简虚拟表，确认编译期带上了 fts5。
	if _, err := probe.Exec(`CREATE VIRTUAL TABLE fts_probe USING fts5(path)`); err != nil {
		return fmt.Errorf("搜索索引不可用：当前 SQLite 构建不支持 fts5（搜索索引 asset_path_fts 无法建立，需换用带 fts5 的构建）：%w", err)
	}
	if _, err := probe.Exec(`DROP TABLE fts_probe`); err != nil {
		return fmt.Errorf("搜索索引自检失败：清理探测表 fts_probe：%w", err)
	}

	// trigram 分词器：搜索依赖它做「任意子串」匹配（trigram 是唯一同时满足「走索引」与
	// 「任意子串」的分词器，见 ADR-0030）。只建表不够，还要实际写入并 MATCH 一次，
	// 确认分词器真能工作而不是仅被接受。
	if _, err := probe.Exec(`CREATE VIRTUAL TABLE fts_probe USING fts5(path, tokenize='trigram')`); err != nil {
		return fmt.Errorf("搜索索引不可用：当前 SQLite 构建不支持 fts5 的 trigram 分词器（搜索索引 asset_path_fts 无法建立，需换用带 trigram 分词器的构建）：%w", err)
	}
	if _, err := probe.Exec(`INSERT INTO fts_probe(path) VALUES ('probe/trigram/1.0.0.jar')`); err != nil {
		return fmt.Errorf("搜索索引不可用：trigram 分词器无法写入（搜索索引 asset_path_fts 无法维护）：%w", err)
	}
	var hits int
	if err := probe.Get(&hits, `SELECT COUNT(*) FROM fts_probe WHERE fts_probe MATCH ?`, `"1.0.0"`); err != nil {
		return fmt.Errorf("搜索索引不可用：trigram 分词器无法检索（搜索索引 asset_path_fts 无法查询）：%w", err)
	}
	if hits != 1 {
		// 能建能写却查不出，说明分词/索引行为与预期不符（例如被替换成不支持三元组的实现）。
		return fmt.Errorf("搜索索引不可用：trigram 分词器检索行为异常（写入 1 行、按子串应命中 1 行，实际命中 %d 行），请确认 SQLite 构建的 trigram 实现", hits)
	}
	if _, err := probe.Exec(`DROP TABLE fts_probe`); err != nil {
		return fmt.Errorf("搜索索引自检失败：清理探测表 fts_probe：%w", err)
	}
	return nil
}
