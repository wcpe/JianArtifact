// system 命名空间文案（FR-111，待各页迁移时填充）。
export default {
  // 页标题
  pageTitle: '系统',

  // tab 标签
  tabUpdate: '在线更新',
  tabRestart: '重启',
  tabShutdown: '关闭',

  // 在线更新 tab 兜底
  loadConfigFailed: '无法加载在线更新配置',

  // 在线更新卡
  updateCardTitle: '应用更新',
  updateCardDesc: '管理员手动触发的自更新，配置即时生效、无须重启。',
  channelStable: '正式版',
  channelPrerelease: '测试版',
  checkUpdate: '检查更新',
  enableUpdateSwitch: '启用在线更新（出站开关）',
  prereleaseAlertTitle: '测试版通道',
  prereleaseAlertBody: '滚动开发预览，由 main 最新构建，可能不稳定。仅用于尝鲜 / 灰度，生产环境建议用正式版。',
  currentVersion: '当前版本',
  latestVersionArrow: '→ 最新版本',
  updateAvailableBadge: '有可用更新',
  upToDateBadge: '已是最新',
  prereleaseBadge: '预发布',
  releaseNotes: '发布说明',
  updateDisabledAlertTitle: '在线更新未启用',
  updateDisabledAlertBody: '在线更新出站开关当前关闭。请启用并保存后，再检查 / 应用更新。',
  downloadingReplacing: '正在下载并替换新版本…',
  // 进度说明（含资产名插值）
  progressHintWithAsset: '资产 {{name}}（进度为体感估算，实际以服务端替换为准）',
  progressHint: '进度为体感估算，实际以服务端替换为准。',
  upgradeTriggeredAlertTitle: '已触发升级',
  upgradeTriggeredAlertBody: '服务正在重启…当前连接将断开，请稍候片刻后手动刷新页面。',
  applyNow: '立即更新并重启',
  rollbackNow: '回滚到上一版',
  noRollbackBackup: '暂无可回滚的备份版本（成功升级一次后才会生成回滚备份）。',
  saved: '已保存。在线更新配置即时生效。',

  // 高级设置
  advancedSettingsToggle: '高级设置（仓库源 / API 基址 / 重启模式 / 访问令牌）',
  repoLabel: '仓库源（owner/repo）',
  apiBaseUrlLabel: 'API 基址',
  restartModeLabel: '重启模式',
  restartModeSelf: 'self（自拉起新进程）',
  restartModeExit: 'exit（交外部进程管理器重启）',
  tokenLabel: '访问令牌（私有仓库可选）',
  tokenDescConfigured: '已配置令牌（不回显）。留空保留现有，填新值则替换。',
  tokenDescUnconfigured: '未配置。留空表示不设置，填值则设置。',
  tokenPlaceholderConfigured: '保留现有令牌',
  tokenPlaceholderUnconfigured: '可选',

  // 重启 tab
  restartCardTitle: '重启服务',
  restartCardDesc: '重启服务进程。重启期间服务将短暂不可用，当前连接会断开，重启完成后需手动刷新页面。',
  restartButton: '重启服务',

  // 关闭 tab
  shutdownCardTitle: '关闭服务',
  shutdownCardDesc: '关闭服务进程。关闭后服务将停止，无法从本控制台再次启动，需在服务器上经进程管理器（systemd / docker 等）重新拉起。',
  shutdownButton: '关闭服务',

  // 升级弹窗
  upgradeModalTitle: '确认升级到新版本',
  upgradeConfirmPrefix: '将升级到 ',
  upgradeConfirmSuffix: '。升级成功后服务会立即重启，当前连接将断开。确认继续？',
  confirmUpgrade: '确认升级',

  // 回滚弹窗
  rollbackModalTitle: '确认回滚到上一版本',
  rollbackConfirmBody: '将用备份还原到上一版本的二进制。回滚成功后服务会立即重启，当前连接将断开。确认继续？',
  confirmRollback: '确认回滚',

  // 重启弹窗
  restartModalTitle: '确认重启服务',
  restartConfirmBody: '将重启服务进程。重启期间服务短暂不可用，当前连接会断开，完成后需手动刷新页面。确认继续？',
  confirmRestart: '确认重启',

  // 关闭弹窗
  shutdownModalTitle: '确认关闭服务',
  shutdownWarningTitle: '警告',
  shutdownWarningBody: '关闭后服务将停止，无法从本控制台再次启动，需在服务器上经进程管理器（systemd / docker 等）重新拉起。',
  shutdownConfirmBody: '确认关闭服务？',
  confirmShutdown: '确认关闭',

  // 系统操作通知
  updateInProgress: '更新进行中，请稍后',
  restartingNotice: '正在重启…当前连接将断开，请稍候片刻后手动刷新页面',
  shuttingDownNotice: '正在关闭…服务将停止，需在服务器上重新启动',
} as const;
