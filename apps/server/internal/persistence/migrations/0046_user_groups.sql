-- 0046：用户组与更细粒度权限动作的数据基座
-- 引入版本：未开窗（0.11.0 之后的下一窗口）
-- 影响：新增 user_group / user_group_member 两表；重建 acl 表以支持组主体与六档动作
-- 数据处理：acl 既有行按「用户主体」原样复制（subject_type='user'、subject_group_id 为 NULL），
--          (repository_id, subject_id, action) 三元组与 id 逐条不变；预计耗时：随 acl 行数增长
--          （重建需全表复制，大库为毫秒～百毫秒级，升级期间占用写锁）
-- 回滚：不支持 down migration；需要恢复升级前备份
-- 相关：FR-36、docs/specs/0.12.0-user-groups-and-fine-grained-actions.md
--
-- 背景：FR-36 要求两件事——授权主体从「单个用户」扩展到「用户组」，动作从
-- read / write / admin 三档扩展到 read / write / publish / delete / acl_manage / admin 六档。
-- 0001 建的 acl 表把主体写死为 subject_id NOT NULL REFERENCES user(id)，且 action 的 CHECK
-- 只认三值；SQLite 的 ALTER TABLE 既不能放宽 / 新增 CHECK 约束，也不能追加带 REFERENCES 的列
-- （ADD COLUMN 要求默认值为常量、且不允许 REFERENCES），因此按本目录既有先例
-- 0020_repository_formats.sql 的范式重建 acl：建新表 → 复制数据 → 删旧表 → 改名 → 重建索引。
-- 该先例同样用 PRAGMA foreign_keys=OFF 包住重建段——DROP TABLE 会让指向 acl 的外键在重建
-- 瞬间悬空，且 acl 在本库中没有被其它表外键引用，重建后名字与列不变，引用方无感知。
--
-- 主体口径：subject_type 标记本行是 user 还是 group 主体，subject_id 与 subject_group_id
-- 各归各主体、另一列恒为 NULL。之所以不把 subject_id 复用成「多态主体 ID」：①复用会让
-- 「用户 7」与「组 7」在同一列撞车，每个查询都必须带上类型条件，漏一处即跨类型误授权；
-- ②分开后两列各带 REFERENCES ... ON DELETE CASCADE，由 SQLite 直接兑现「删用户清用户条目、
-- 删组清组条目」，复用一列则无法建外键、清理全靠应用代码兜底。表内 CHECK 强制二者恰有其一。
--
-- 唯一性口径（关键）：原 UNIQUE(repository_id, subject_id, action) 在引入可空列后不再有效——
-- SQLite 唯一索引把 NULL 视作互不相同，多行 NULL 之间永不冲突，改成
-- UNIQUE(repository_id, subject_id, subject_group_id, action) 会让同一用户在同一仓库被插入
-- 无数条同样的 read。故改用**两条 partial unique index**，各自只覆盖一种主体、且该分区内键列
-- 恒非空（由上面的 CHECK 保证）：user 分区用 subject_id，group 分区用 subject_group_id。
--
-- 幂等性说明：迁移器把每个文件放在单个事务里执行（见 persistence/migrate.go），
-- 建表与 schema_migrations 记录同事务提交，不存在「表已建但版本未记」的半成品状态，
-- 因此这里按 0043 / 0044 / 0045 的既有写法用裸 CREATE TABLE / CREATE INDEX，不加 IF NOT EXISTS。

-- 用户组：授权主体之一。组名全局唯一，引用它的 ACL 条目存 id（改名不影响授权）。
CREATE TABLE user_group (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    name        TEXT NOT NULL UNIQUE,
    description TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL DEFAULT (datetime('now'))
);

-- 组成员关系：组 × 用户的多对多关联表。两侧外键均 CASCADE——删组清成员、删用户清归属。
CREATE TABLE user_group_member (
    group_id   INTEGER NOT NULL REFERENCES user_group (id) ON DELETE CASCADE,
    user_id    INTEGER NOT NULL REFERENCES user (id) ON DELETE CASCADE,
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    PRIMARY KEY (group_id, user_id)
);
-- 复合主键最左列是 group_id，无法服务「某用户属于哪些组」的反查；该反查正是鉴权热路径
-- （一次判定要先展开该用户的全部组），故单独在 user_id 上建索引。
CREATE INDEX idx_user_group_member_user ON user_group_member (user_id);

-- acl 重建：SQLite 不允许 ALTER TABLE 改 CHECK / 加 REFERENCES 列，故整表重建。
PRAGMA foreign_keys=OFF;

CREATE TABLE acl_new (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    repository_id    INTEGER NOT NULL REFERENCES repository (id) ON DELETE CASCADE,
    subject_id       INTEGER REFERENCES user (id) ON DELETE CASCADE,
    subject_type     TEXT NOT NULL DEFAULT 'user' CHECK (subject_type IN ('user', 'group')),
    subject_group_id INTEGER REFERENCES user_group (id) ON DELETE CASCADE,
    action           TEXT NOT NULL CHECK (action IN ('read', 'write', 'publish', 'delete', 'acl_manage', 'admin')),
    -- 两种主体恰有其一：既防止两列都填的歧义行，也保证下面两条 partial unique index
    -- 在各自分区内的键列恒非空（NULL 在唯一索引里互不冲突，非空是约束生效的前提）。
    CHECK (
        (subject_type = 'user'  AND subject_id IS NOT NULL AND subject_group_id IS NULL) OR
        (subject_type = 'group' AND subject_group_id IS NOT NULL AND subject_id IS NULL)
    )
);

-- 既有行全部是用户主体：subject_type 取默认值 'user' 的等价字面量、subject_group_id 为 NULL。
-- id 原样带入，AUTOINCREMENT 序列因此不回退。
INSERT INTO acl_new (id, repository_id, subject_id, subject_type, subject_group_id, action)
SELECT id, repository_id, subject_id, 'user', NULL, action
FROM acl;

DROP TABLE acl;
ALTER TABLE acl_new RENAME TO acl;

PRAGMA foreign_keys=ON;

-- 索引：repository_id 上的是「删仓库 / 列某仓全部条目」的既有索引，原样重建。
CREATE INDEX idx_acl_repository ON acl (repository_id);
-- subject_id 上的索引沿用旧名与语义（只服务用户主体：组主体行该列恒为 NULL）。
CREATE INDEX idx_acl_subject ON acl (subject_id);
-- 组主体反查：某组被授予了哪些仓库，以及删组时 CASCADE 的驱动索引。
CREATE INDEX idx_acl_group_subject ON acl (subject_group_id);

-- 唯一性：按主体类型分区的两条 partial unique index，避免 NULL 使约束失效（见文件头说明）。
CREATE UNIQUE INDEX idx_acl_user_grant ON acl (repository_id, subject_id, action)
    WHERE subject_type = 'user';
CREATE UNIQUE INDEX idx_acl_group_grant ON acl (repository_id, subject_group_id, action)
    WHERE subject_type = 'group';
