// DevMock 版本锚点。
//
// `VERSION` 文件与这里必须一致：开发期两者都写 `<下一版>-dev`（发布时同步改为正式号），
// 用于云构建产物命名（曾产出 `jianartifact-0.8.0-dev-<日期>-<平台>`）与部署脚本注入
// `main.version`；由 store（`/api/v1/status`）、handlers（契约测试）与备份包元数据共同引用，
// 避免版本号散落成三份各自过期的字面量。
/** 当前开发版版本号。 */
export const MOCK_APP_VERSION = "0.9.0";

/**
 * 当前数据库 schema 版本与迁移标识。
 * 必须与 apps/server/internal/persistence/migrations 下最新一条迁移保持一致，
 * 否则备份包元数据与设置页显示的迁移版本会落后于真实实现。
 */
export const MOCK_SCHEMA_VERSION = 36;
export const MOCK_MIGRATION_VERSION = "0036_audit_http_context";
