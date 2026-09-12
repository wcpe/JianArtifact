// DevMock 版本锚点。
//
// `VERSION` 文件是**发布边界**（保持上一个已发布版本，发布前不动）；
// DevMock 表达的是「当前正在开发、尚未发布的版本」，因此在这里单独维护，
// 由 store（`/api/v1/status`）、handlers（契约测试）与备份包元数据共同引用，
// 避免版本号散落成三份各自过期的字面量。
/** 当前开发版版本号（v0.8.0 开发线）。 */
export const MOCK_APP_VERSION = "0.8.0-dev";

/**
 * 当前数据库 schema 版本与迁移标识。
 * 必须与 apps/server/internal/persistence/migrations 下最新一条迁移保持一致，
 * 否则备份包元数据与设置页显示的迁移版本会落后于真实实现。
 */
export const MOCK_SCHEMA_VERSION = 36;
export const MOCK_MIGRATION_VERSION = "0036_audit_http_context";
