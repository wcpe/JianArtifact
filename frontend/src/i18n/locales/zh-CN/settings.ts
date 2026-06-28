// settings 命名空间文案（FR-111，待各页迁移时填充）。
export default {
  // 页标题（含 loading 兜底分支）
  pageTitle: '设置',
  // 左侧分节导航容器无障碍标签
  navAriaLabel: '设置分节导航',
  // 左侧锚点导航各节标签（与右侧分节标题共用同一真源；防护配置文案归 protection ns，不在此处使用）
  nav: {
    proxy: '网络代理',
    limits: '限制与配额',
    observability: '可观测性',
    vuln: '漏洞库',
    auth: '安全 / 会话',
    protection: '防护配置',
  },
  // 各「重启生效」节共用的徽标
  restartHint: '保存后重启生效',
  // 网络代理节
  proxy: {
    title: '网络代理',
    desc: '统一出站代理（回源 / 迁移 / 漏洞库 / OIDC / 在线更新共用）。每代理可填用户名 + 密码；用户名回显、密码不回显（留空保留现有），URL 留空表示不配置该代理。保存后**即时生效、无须重启**。',
    httpTitle: 'HTTP 代理',
    httpsTitle: 'HTTPS 代理',
    socks5Title: 'SOCKS5 代理（all，兜底全 scheme）',
    noProxyLabel: '直连绕过（no_proxy）',
  },
  // 单代理三字段子组件（ProxyFields）
  proxyFields: {
    passwordConfigured: '密码已配置',
    passwordWillClear: '保存后清除密码',
    urlLabel: 'URL',
    usernameLabel: '用户名',
    usernamePlaceholder: '可选',
    passwordLabel: '密码',
    passwordPlaceholder: '留空保留现有密码',
    clearPassword: '清除密码',
  },
  // 限制与配额节
  limits: {
    title: '限制与配额',
    maxArtifactSizeLabel: '单个制品上传上限（字节）',
    maxArtifactSizeDesc: '留空表示不额外限制；超限上传返回 413。',
    maxArtifactSizePlaceholder: '不限制',
  },
  // 可观测性节
  observability: {
    title: '可观测性',
    auditRetentionDays: '审计日志保留天数',
    auditMaxRows: '审计日志行数上限',
    usageDetailEnabled: '记录逐条访问 / 下载明细（使用分析）',
    usageMaxDetailRows: '使用明细行数上限',
    metricsEnabled: '启用 Prometheus 指标端点（/metrics）',
    metricsAllowAnonymous: '允许匿名抓取 /metrics（须限内网 / 反代后）',
    timeseriesEnabled: '启用指标时序采集',
    timeseriesSampleInterval: '时序采样间隔（秒）',
    timeseriesRetentionDays: '时序保留天数',
  },
  // 漏洞库节
  vuln: {
    title: '漏洞库',
    enabled: '启用漏洞库离线镜像',
    sourceBaseUrl: '镜像数据源基址',
    refreshInterval: '刷新周期（秒）',
    downloadTimeout: '下载超时（秒）',
  },
  // 安全 / 会话节
  auth: {
    title: '安全 / 会话',
    desc: '仅会话 / 登录锁定可调标量；OIDC / LDAP 等密钥项不在此处、只能经配置文件 / 环境变量设置。',
    sessionTtl: '会话有效期（秒）',
    loginMaxFailures: '触发锁定的连续失败次数',
    loginLockoutSecs: '锁定时长（秒）',
  },
  // 底部全局保存条
  saveBar: {
    savedHint: '已保存。代理即时生效；限制配额 / 可观测性 / 漏洞库 / 安全会话重启后生效。',
  },
} as const;
