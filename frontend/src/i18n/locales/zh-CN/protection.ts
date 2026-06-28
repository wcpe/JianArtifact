// protection 命名空间文案（FR-111，待各页迁移时填充）。
export default {
  // —— 外层节卡头 / 底部 ——
  card: {
    title: '防护配置',
    instantBadge: '保存后即时生效',
    description:
      '各七层防护维度的启停与调参，保存后即时生效、无须重启；阈值 / 名单 / 规则为本机内部配置，不外发。',
  },
  savedHint: '已保存，配置已即时生效。',
  saveButton: '保存并即时生效',

  // —— 速率限制 ——
  rateLimit: {
    title: '速率限制',
    description: '按 IP / 身份 / 仓库维度固定窗计数，超阈值返回 429；并发上限 0 表示不限。',
    enable: '启用速率限制',
    windowSecs: '时间窗（秒）',
    ipMaxRequests: '单 IP 每窗上限',
    identityMaxRequests: '单身份每窗上限',
    repoMaxRequests: '单仓库每窗上限（0=不启用）',
    ipMaxConcurrent: '单 IP 并发上限（0=不限）',
    userMaxConcurrent: '单用户并发上限（0=不限）',
    repoMaxConcurrent: '单仓库并发上限（0=不限）',
  },

  // —— IP 黑 / 白名单 ——
  ipList: {
    title: 'IP 黑 / 白名单',
    description: '每行一个 IP 或 CIDR 网段；白名单豁免一切防护、黑名单直接拒。',
    allowLabel: '白名单（每行一个 IP / CIDR）',
    denyLabel: '黑名单（每行一个 IP / CIDR）',
  },

  // —— 异常检测与自动封禁 ——
  ban: {
    title: '异常检测与自动封禁',
    description: '窗内单 IP 异常信号达阈值即封禁一段时间，到期自动解封。',
    enable: '启用异常封禁',
    windowSecs: '时间窗（秒）',
    threshold: '封禁阈值',
    durationSecs: '封禁时长（秒）',
  },

  // —— 慢速攻击防护 ——
  slowloris: {
    title: '慢速攻击防护',
    description: '对慢速 drip 请求体设超时、对所有请求体设通用大小上限（0=不启用）。',
    enable: '启用慢速攻击防护',
    bodyReadTimeoutSecs: '块间空闲超时（秒）',
    headerTimeoutSecs: '首块等待超时（秒）',
    maxBodyBytes: '通用体上限（字节，0=不启用）',
  },

  // —— CC 挑战 ——
  ccChallenge: {
    title: 'CC 挑战（PoW）',
    description: '对匿名可疑流量要求工作量证明；难度越高刷流成本越高。默认豁免已认证客户端。',
    enable: '启用 CC 挑战',
    exemptAuthenticated: '豁免已认证请求',
    difficulty: '难度（前导零位，≤64）',
    ttlSecs: '令牌有效期（秒）',
  },

  // —— WAF 规则引擎 ——
  waf: {
    title: 'WAF 规则引擎',
    description:
      '按 method / path / query / header 有序匹配，首个命中生效（block 拒 / allow 放行）。',
    enable: '启用 WAF',
    colField: '字段',
    colHeaderName: '头名（仅 header）',
    colPattern: '模式',
    colMatchType: '匹配类型',
    colAction: '动作',
    ariaField: '规则字段',
    ariaHeaderName: '头名',
    ariaPattern: '模式',
    ariaMatchType: '匹配类型',
    ariaAction: '动作',
    ariaDeleteRule: '删除规则',
    addRule: '新增规则',
  },

  // —— 监控告警 ——
  alerts: {
    title: '监控告警',
    description: '窗内各防护维度计数达阈值即告警并落库；告警是本机内部数据、不外发。',
    enable: '启用阈值告警',
    windowSecs: '评估窗（秒）',
    rateLimitThreshold: '限流被拒阈值',
    banThreshold: '自动封禁阈值',
    ccFailThreshold: 'CC 失败阈值',
    wafBlockThreshold: 'WAF 阻断阈值',
    slowlorisThreshold: '慢速超时阈值',
  },
} as const;
