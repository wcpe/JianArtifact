# ADR-0030：全局搜索建立 FTS5 trigram 索引

## 状态

已接受

## 背景

全局搜索（FR-30）把每个关键词翻译成 `path LIKE '%词%'`（`apps/server/internal/repository/asset_repo.go` 的 `buildSearchWhere`，L244-278）。前导通配使 `asset` 上的任何 B-Tree 索引都无法参与过滤，`SearchByFilter`（L307-320）在这条 WHERE 上先 `SELECT COUNT(*)`、再带 `ORDER BY ... LIMIT ? OFFSET ?` 取页，`SearchFacetsByFilter`（L330-339）还要按 `repository_id` 再聚合一次，于是每次搜索对百万级 `asset` 至少做两到三次全表扫描。

`apps/server/internal/domain/repository_service.go` 的 `SearchAssets`（L1021 起）另有可搜范围的计算成本：非管理员先 `ListPublic()`，再 `List(1000, 0)` 取全量仓库并逐仓库判 `HasPermission`（L1054-1071）；带 `repoScope` 时再 `List(1000, 0)` 一次（L1081）。`apps/server/internal/api/repository_handlers.go` 的 `SearchAssets`（L849）把结果一次性 `c.JSON` 返回，没有超时保护。

`asset` 表（`apps/server/internal/persistence/migrations/0002_asset.sql` L13-23；0005 追加 `sha1`、`md5`）以 `path` 记录制品路径，**没有结构化的版本号列**——版本只是路径文本的一段。因此"按版本号 `1.17.12` 搜索"在语义上等同于任意子串匹配，不存在"按版本列等值或前缀检索"的捷径。当时存在的索引（`idx_asset_repository`、`idx_asset_repo_path`、`idx_asset_path`、`idx_asset_repo_updated`，见 0002 L24、0006 L8-9、0045 L42）全部是 B-Tree，只能服务等值与前缀条件。（注：`idx_asset_repository` 后来被迁移 `0050` 用复合覆盖索引 `idx_asset_repo_size (repository_id, size)` 取代并删除——那是容量聚合与配额查询的优化，与本文的 FTS 决策无关，此处保留原貌以如实记录当时的判断依据。）

FR-149 的性能目标是百万级 `asset` 下常见关键词与版本号搜索 P95 < 500ms。

## 决策

1. **新增 FTS5 虚拟表对 `asset.path` 建全文索引，分词器用 `tokenize='trigram'`。**
2. **FTS 表是派生索引，`asset` 仍是唯一真源**：采用 external content 形态（`content='asset'`、`content_rowid='id'`），FTS 表以 `asset.id` 为 `rowid`，不复制路径文本。
3. **索引由应用层在与 `asset` 写入同一事务内显式维护**，不使用触发器；未索引的行不进候选集。
4. **升级后后台异步回填**：按 `rowid` 水位分批推进、中断后可续跑，不阻塞进程启动；回填完成前搜索继续走既有 `LIKE` 路径，完成后改为「FTS 候选集收敛 + `LIKE` 后置复核」——**不是**完全改用 `MATCH` 定案，理由见「理由」第二条。
5. **索引可丢弃重建**：前向迁移只建 FTS 表与回填状态，不回填数据；一致性异常时按水位续灌或全量 `rebuild`。
6. **FTS5 是 SQLite 内建能力**，随 `modernc.org/sqlite` 编入同一二进制，不引入外部服务、进程或新的运行依赖。

## 理由

- **trigram 是唯一同时满足"走索引"与"任意子串"的分词器**。实测 5 万行合成数据：`WHERE path LIKE '%1.17.12%'` 在 trigram 表上约 1ms，在 unicode61 表上约 29ms，后者是逐行扫描、耗时随行数线性增长。若改用 `MATCH` 配合 unicode61，`1.17.12` 会按分隔符被切成 `1` / `17` / `12`，命中 `1.17.120`、`1.17.1212` 等无关路径；此时若再用 `LIKE` 复核，因为候选集本身就是全表量级，等于退回逐行扫描、白付索引成本。
- **但生产查询仍然保留 `LIKE` 作为后置复核，这不与上一条矛盾**：trigram 折叠**整个 Unicode**，而 `LIKE` 只折叠 ASCII，因此 trigram 的命中集是 `LIKE` 的**超集**（实测 `Ärger-1.0.jar` 与 `ärger-2.0.jar` 在 `q=ärger` 下 FTS 命中 2 条、`LIKE` 只命中 1 条）。所以 FTS 只用来**收敛候选集**，最终由 `LIKE` 在小集合上定案——此时 `LIKE` 面对的是已收窄的行集而非全表，代价可忽略，而语义与改造前逐字等价。换言之：上一条否掉的是「unicode61 的噪声候选 + 全表 `LIKE`」，本处采用「trigram 的精确候选 + 小集 `LIKE`」，两者前提不同。
- **external content 让索引滞后只可能漏、不可能伪造内容**：查询以 `rowid` 回 `JOIN asset` 取真实 `path`，被删除的 `asset` 行会让对应索引行在 join 中自然消失，无需额外的孤儿清理；同时省去把百万条路径再存一份。**但仅限"只读 rowid"的查询形态**：一旦对 `asset_path_fts` 的列值直接过滤（例如 `SELECT ... WHERE path LIKE ...`），孤儿行会让 SQLite 报 `267 fts5: missing row N from content table`。因此生产查询固定为 `id IN (SELECT rowid FROM asset_path_fts WHERE asset_path_fts MATCH ?)`，一律回表取真值。
- **显式同步而非触发器**：`asset` 的写入点有限且集中在仓储层（`AssetRepo.UpsertWithTime`、`DeleteByPath`，以及 `asset_mutation_repo.go` 的 apply 与回滚），全部可穷举，并且已经在事务边界内。而 external content 表上的 `UPDATE` 语句不会重建索引（实测改 `path` 后新文本查不到），正确维护必须"先移除旧条目、再插入新条目"——把这套易错操作封在一个可测试的入口，比固化进只增不改的迁移里的触发器更可控。仓库删除会经 `repository` 的 `ON DELETE CASCADE`（0002 L15）级联清掉该仓库全部 `asset`，此时按仓库一次性清理也比逐行触发更直接。
- **移除条目必须"先确保它在索引里"**（实测踩坑，决定了实现形态）：当内容行存在、但索引里没有该 `rowid` 时（迁移刚完成、历史行尚未回填，这是常态），FTS5 移除条目会报 `267 database disk image is malformed`——专用命令 `'delete'` 与 `DELETE FROM` 都会踩，且报错会污染同一连接上该表的后续操作。因此移除前先对同一 `(rowid, path)` 插一次（对已入索引的行幂等无害），再执行删除，使"删除从未入索引的历史行"不会失败。
- **移除条目必须在 `asset` 行仍在时执行**：external content 表要靠内容行才能确定该 `rowid` 有哪些词要移除；内容行先消失虽不报错，但清不掉词、会留下残留条目。故删除资产的顺序固定为"先清索引、再删 `asset` 行"，换路径固定为"先插新行、再删旧行"。
- **回填不阻塞启动**：百万行一次性 `rebuild` 会长时间持有写事务；分批推进可以控制单事务时长，并把"未完成"表达为可查询的状态，而不是启动失败或搜索结果静默不完整。
- **不破坏 NFR-01**：FTS5 与 trigram tokenizer 都由 SQLite 本体提供（`modernc.org/sqlite v1.60.1` 实测可用），默认形态仍是单二进制零外部依赖；代价只是索引计入数据库文件体积。

## 后果

- **正面**：版本号与任意子串检索由索引给出候选集，百万级 `asset` 下不再每次全表扫描；搜索 API 的请求与响应结构不变，前端与协议不分叉；索引是纯派生数据，损坏可重建而不影响真源。
- **负面 / 约束**：
  - trigram 的可索引长度下限是 3 字符。实测短于 3 字符的关键词（`%1%`）与逐行扫描同量级（5 万行约 22ms），没有收益；命中率过高的关键词反而可能慢于扫描（`%artifact%` 命中全部 5 万行，trigram 约 141ms，扫描约 22ms）。查询侧仍需对命中规模设上限并加超时（FR-149 的 `context` 超时）。
  - 一致性由写入路径保证：任何绕过仓储层直接写 `asset` 的代码都会让索引滞后（表现为漏结果），需在评审与测试中守住；"新增写入路径必须同步索引"成为后续约束。
  - 数据库文件与备份体积随索引增长。
  - 索引只解决"候选集"；`ORDER BY` 与分页、`COUNT(*)`、按仓库聚合仍作用在命中集上，相关度分层排序与包级聚合不在本决策范围（FR-150）。
  - 回填完成前的窗口内搜索仍走旧路径，性能不改善但结果口径不变。

## 备选方案

- **只优化现有 `LIKE` 或追加普通索引**：前导通配无法命中 B-Tree，加索引不改变扫描量，落选。
- **新增结构化 `version` 列并按列检索**：需从各类格式路径中解析版本，覆盖不了裸路径与任意子串关键词，且把"路径即真源"裂成两套语义，落选。
- **unicode61 + `MATCH`**：分词会切碎版本号、噪声高；改用 `LIKE` 复核又退回扫描，落选。
- **contentless FTS 表**：不保存列值，无法回读 `path` 做展示与复核，且不支持常规删除，落选。
- **普通（contentful）FTS 表**：可读回文本，但重复存储百万条路径，且真源删除后索引行不会自动消失，需额外清理，落选。
- **触发器维护索引**：能覆盖全部写入路径（实测级联删除会触发 `asset` 上的删除触发器），但 external content 的正确维护必须成对写 `'delete'` 与插入，触发器把这套逻辑固化进只增不改的迁移，演进与测试成本高，落选。
- **引入外部搜索引擎（Elasticsearch 等）**：违背 NFR-01 零外部依赖默认形态，落选。
