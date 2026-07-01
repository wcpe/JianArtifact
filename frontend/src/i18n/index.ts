// 前端国际化初始化（FR-111，ADR-0034）。
//
// 架构：i18next + react-i18next，按**命名空间分文件**（每页一个 ns 文件，见 locales/zh-CN/）。
// 本期只交付 zh-CN（默认且唯一语言），但 key 结构与按 ns 分文件的组织为多语言扩展预留——
// 加一种语言只需在 locales/<lng>/ 下补同名 ns 文件并注册。
// 在 main.tsx 与测试 setup 顶部 `import './i18n'` 完成初始化（全局单例，组件经 useTranslation 读取）。

import i18n from 'i18next';
import { initReactI18next } from 'react-i18next';

import common from './locales/zh-CN/common';
import nav from './locales/zh-CN/nav';
import errors from './locales/zh-CN/errors';
import auditActions from './locales/zh-CN/auditActions';
import login from './locales/zh-CN/login';
import dashboard from './locales/zh-CN/dashboard';
import repositories from './locales/zh-CN/repositories';
import repositoryDetail from './locales/zh-CN/repositoryDetail';
import artifactDetail from './locales/zh-CN/artifactDetail';
import search from './locales/zh-CN/search';
import settings from './locales/zh-CN/settings';
import protection from './locales/zh-CN/protection';
import system from './locales/zh-CN/system';
import systemLogs from './locales/zh-CN/systemLogs';
import users from './locales/zh-CN/users';
import groups from './locales/zh-CN/groups';
import tokens from './locales/zh-CN/tokens';
import upload from './locales/zh-CN/upload';
import migration from './locales/zh-CN/migration';
import monitor from './locales/zh-CN/monitor';
import analytics from './locales/zh-CN/analytics';
import protectionMonitor from './locales/zh-CN/protectionMonitor';
import audit from './locales/zh-CN/audit';
import licenses from './locales/zh-CN/licenses';
import acl from './locales/zh-CN/acl';
import mock from './locales/zh-CN/mock';
import taskCenter from './locales/zh-CN/taskCenter';

/** 默认且唯一首发语言。 */
export const DEFAULT_LNG = 'zh-CN';

/** 全部命名空间资源（按 ns 分文件，便于并行维护、防写冲突）。 */
const resources = {
  [DEFAULT_LNG]: {
    common,
    nav,
    errors,
    auditActions,
    login,
    dashboard,
    repositories,
    repositoryDetail,
    artifactDetail,
    search,
    settings,
    protection,
    system,
    systemLogs,
    users,
    groups,
    tokens,
    upload,
    migration,
    monitor,
    analytics,
    protectionMonitor,
    audit,
    licenses,
    acl,
    mock,
    taskCenter,
  },
} as const;

void i18n.use(initReactI18next).init({
  resources,
  lng: DEFAULT_LNG,
  fallbackLng: DEFAULT_LNG,
  defaultNS: 'common',
  ns: Object.keys(resources[DEFAULT_LNG]),
  // React 已对插值做转义，i18next 无需再转义。
  interpolation: { escapeValue: false },
  // 缺 key 时回落 key 本身而非 null，便于发现未迁移项。
  returnNull: false,
});

/**
 * 审计动作 key → 中文标签（FR-111）。
 *
 * 后端 action 含点（如 `artifact.upload`），而 i18next 默认按 `.` 解析嵌套 key，故把点归一为 `_`
 * 再查 `auditActions` 命名空间；未知动作回落原始串（不丢信息）。
 */
export function tAuditAction(action: string): string {
  const key = action.replace(/\./g, '_');
  return i18n.t(`auditActions:${key}`, { defaultValue: action });
}

export default i18n;
