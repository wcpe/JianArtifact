// Nexus 迁移管理页文案命名空间（FR-111）。
export default {
  // 页头
  title: 'Nexus 迁移',
  intro:
    '从源 Nexus OSS 迁移仓库与制品：在线 REST 或离线 blob store 预览 → 勾选 → 执行 → 查看报告。源 Nexus 凭据仅以引用名提供，明文不入库、不回显。',

  // 步骤条（Stepper.Step 的 label / description）
  step: {
    sourceLabel: '选源与预览',
    sourceDesc: '填源地址或离线路径',
    selectLabel: '勾选执行',
    selectDesc: '选仓库、选方式并搬运',
    reportLabel: '迁移报告',
    reportDesc: '查看结果',
  },

  // 源形态切换（SegmentedControl）
  mode: {
    online: '在线（REST API）',
    offline: '离线（blob store）',
  },

  // 迁移方式切换（SegmentedControl）
  method: {
    online: '在线拉取（HTTP 下载）',
    offline: '离线目录（blob store）',
  },

  // 源配置输入项
  source: {
    baseUrlLabel: '源 Nexus 地址',
    authRefLabel: '凭据引用（auth_ref，可选）',
    authRefDesc: '仅填引用名；真实凭据由后端 env 解析，明文不入库、不回显。匿名源可留空。',
    authRefPlaceholder: '例如 NEXUS_SRC',
    offlinePathLabel: '离线 blob store 路径',
    offlinePathDesc: '服务进程可访问的本地 Nexus 文件型 blob store 根目录。',
    migratePathLabel: '离线 blob store 路径（制品本体来源）',
    migratePathDesc: '搬运需从离线 blob store 读取制品本体，其下应含 content/ 子目录。',
    targetRepoAria: '{{name}} 目标仓库名',
    targetRepoPlaceholder: '目标名（默认 {{name}}）',
  },

  // 阶段中文标签（OnlinePullJob.phase）
  phase: {
    enumerating: '枚举资产中',
    downloading: '下载搬运中',
    paused: '已暂停',
    cancelled: '已取消',
    done: '已完成',
    failed: '失败',
  },

  // 预览结果区
  preview: {
    button: '预览仓库',
    next: '下一步：勾选执行',
    count: '可迁移仓库（{{count}}）',
    thRepo: '仓库',
    thFormat: '格式',
    thDetail: '类型 / 内容',
    // 离线预览行的内容文案（blob 数量）
    blobCount: '{{count}} 个 blob',
    // FR-124 离线预览异步化：枚举任务超时 / 失败提示
    timeout: '离线预览任务超时（blob store 可能过大），请稍后重试或缩小目录',
    failed: '离线预览枚举失败，请检查 blob store 路径与目录结构',
  },

  // 勾选与执行区
  select: {
    empty: '请先在上一步预览仓库。',
    method: '迁移方式',
    chosen: '勾选要搬运的仓库（已选 {{count}}）',
    onlineHint:
      '在线拉取仅对 maven2 hosted 仓库有效，经源 Nexus REST 枚举并逐个 HTTP 下载制品，无需本地 blob store 目录；非 maven / 非 hosted 的所选仓库会被跳过并列入报告。 每个仓库可选填目标仓库名，留空即与源同名。',
    prevStep: '上一步',
    runOnline: '执行在线拉取',
    onlineFootnote:
      '在线拉取建仓 + 经 HTTP 下载同步制品；仅 maven2 hosted 仓库被处理，其余进报告 的整仓跳过列表。',
    runProxy: '执行 proxy 搬运',
    runHosted: '执行 hosted 搬运',
    offlineFootnote:
      'proxy 搬运建仓 + 搬运缓存制品；hosted 搬运建仓 + 搬运完整制品。两者均按源仓库 类型在后端各取所需，非目标类型仓库会被跳过并列入报告。',
  },

  // 迁移报告区
  report: {
    empty: '尚无迁移报告。',
    title: '迁移报告',
    noRepos: '无仓库被搬运。',
    thRepo: '仓库',
    thFormat: '格式',
    thCreated: '新建仓库',
    thMigrated: '已迁制品',
    thSkipped: '跳过制品',
    createdYes: '是',
    createdExisting: '已存在',
    skippedRepos: '整仓跳过（非目标类型）：',
    backToSelect: '返回勾选',
  },

  // 成功提示（toast）
  toast: {
    offlineDone: '迁移已完成，请查看报告',
    offlineStarted: '离线目录搬运任务已发起，正在后台导入',
    onlineStarted: '在线拉取任务已发起，正在导入',
  },

  // 在线拉取任务进度面板
  job: {
    queueTitle: '在线拉取导入队列',
    resume: '继续',
    pause: '暂停',
    cancel: '取消',
    progressAria: '导入进度',
    progress: '进度 {{done}} / {{total}}（{{percent}}%）',
    migrated: '已迁 {{count}}',
    skipped: '已跳过 {{count}}',
    currentRepo: '当前仓库：',
    currentPath: '当前文件：',
    failedTitle: '任务失败',
    noRepos: '无仓库被搬运。',
    thSourceRepo: '源仓库',
    thTargetRepo: '目标仓库',
    thFormat: '格式',
    thCreated: '新建仓库',
    thMigrated: '已迁制品',
    thSkipped: '跳过制品',
    createdYes: '是',
    createdExisting: '已存在',
    skippedRepos: '整仓跳过（非 maven hosted）：',
  },
} as const;
