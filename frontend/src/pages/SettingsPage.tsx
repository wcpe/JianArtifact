// 设置页（FR-87 只读 + FR-88 可编辑热替换 + FR-103 锚点单页重做 + FR-106 动态配置，仅管理员）：
// 左侧 sticky 锚点子导航（网络代理 / 限制与配额 / 可观测性 / 漏洞库 / 安全·会话）——
// 点击平滑滚动到对应节、滚动时按可视区高亮当前节；右侧单页分节（不强制等高、短节不留空白）。
// 底部**只有一个** sticky 全局保存按钮：一次性提交 PATCH /settings（即时生效）+ PATCH /settings/dynamic（重启生效），
// 去掉系统配置节早先自带的「保存系统配置」按钮。各节内用小字标注「即时生效」/「保存后重启生效」。
//
// 在线更新已迁至「系统」页（FR-109，SystemPage），本页不再含应用更新卡片。
//
// 数据来自后端 GET /api/v1/settings（已脱敏：代理 URL 去凭据）与
// GET /api/v1/settings/dynamic（非密钥项）。保存走 PATCH /settings（只发 network_proxy，部分更新）
// 与 PATCH /settings/dynamic（代理凭据只入内存槽、不写回 TOML / 不回显，重启回落文件 / env 配置）。

import { useEffect, useState } from 'react';
import {
  Box,
  Flex,
  Stack,
  Title,
  Text,
  Card,
  Group,
  Badge,
  Button,
  Loader,
  Center,
  TextInput,
  Switch,
  PasswordInput,
  NavLink,
  NumberInput,
} from '@mantine/core';
import { IconDeviceFloppy } from '@tabler/icons-react';
import * as api from '../api/endpoints';
import type { SettingsView, ProxyEntryPatch, DynamicConfig } from '../api/types';
import { errorMessage } from '../lib/format';
import { ErrorAlert } from '../components/ErrorAlert';
import { density } from '../theme/density';
import { ProtectionConfigSection } from './ProtectionConfigSection';

/** 锚点节定义（单一真源：左侧导航与右侧分节共用，避免标签 / id 复制散落）。 */
const SECTIONS = [
  { id: 'proxy', label: '网络代理' },
  { id: 'limits', label: '限制与配额' },
  { id: 'observability', label: '可观测性' },
  { id: 'vuln', label: '漏洞库' },
  { id: 'auth', label: '安全 / 会话' },
  // FR-110：防护配置由独立页并入设置页，作为一个锚点节；自带 PATCH /protection/config 保存（即时生效）、不并入全局保存。
  { id: 'protection', label: '防护配置' },
] as const;

/**
 * 各锚点节卡片的滚动外边距（增强 FR-92）：点击导航走 `scrollIntoView({block:'start'})` 时，
 * 目标节顶部会贴到视口顶端、被 alt 外壳的固定页眉遮住；以页眉高度作 `scroll-margin-top`，
 * 让目标节停在页眉下方而非藏到其后（与 sticky 导航的 `top` 偏移成对，单一真源 density.headerHeight）。
 */
const SECTION_SCROLL_STYLE = { scrollMarginTop: density.headerHeight } as const;

/** 单代理三字段（URL / 用户名 / 密码）一组（FR-100）。密码框始终空、不回显；已配置时标徽标 + 提供清除密码。 */
interface ProxyFieldsProps {
  title: string;
  urlPlaceholder: string;
  url: string;
  onUrlChange: (v: string) => void;
  username: string;
  onUsernameChange: (v: string) => void;
  password: string;
  onPasswordChange: (v: string) => void;
  hasPassword: boolean;
  passwordCleared: boolean;
  onClearPassword: () => void;
}

function ProxyFields(props: ProxyFieldsProps) {
  const {
    title,
    urlPlaceholder,
    url,
    onUrlChange,
    username,
    onUsernameChange,
    password,
    onPasswordChange,
    hasPassword,
    passwordCleared,
    onClearPassword,
  } = props;
  return (
    <Stack gap="xs">
      <Group gap="xs">
        <Text size="sm" fw={600}>
          {title}
        </Text>
        {/* 已配置密码标识（绝不回显密码本体）；点过清除则提示本次保存将清空 */}
        {hasPassword && !passwordCleared && (
          <Badge size="sm" color="blue" variant="light">
            密码已配置
          </Badge>
        )}
        {passwordCleared && (
          <Badge size="sm" color="orange" variant="light">
            保存后清除密码
          </Badge>
        )}
      </Group>
      <TextInput
        label="URL"
        placeholder={urlPlaceholder}
        value={url}
        onChange={(e) => onUrlChange(e.currentTarget.value)}
      />
      <TextInput
        label="用户名"
        placeholder="可选"
        value={username}
        onChange={(e) => onUsernameChange(e.currentTarget.value)}
      />
      <PasswordInput
        label="密码"
        placeholder="留空保留现有密码"
        value={password}
        onChange={(e) => onPasswordChange(e.currentTarget.value)}
      />
      {/* 仅在已配置密码时提供「清除密码」动作（发 password: ""）；填了新密码或已点清除则不再显示 */}
      {hasPassword && !passwordCleared && !password && (
        <Group>
          <Button size="xs" variant="subtle" color="red" onClick={onClearPassword}>
            清除密码
          </Button>
        </Group>
      )}
    </Stack>
  );
}

/** 设置页。 */
export function SettingsPage() {
  const [settings, setSettings] = useState<SettingsView | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  // —— 可编辑表单态（FR-88 + FR-100）——
  // 每代理（http / https / all）拆三字段：URL（脱敏 host，无凭据）、用户名（回显）、密码（不回显）。
  // 密码框始终空：留空保存=省略 password 字段（保留现有）；填值=设置；另有「清除密码」动作发空串清空。
  const [httpUrl, setHttpUrl] = useState('');
  const [httpUser, setHttpUser] = useState('');
  const [httpPass, setHttpPass] = useState('');
  const [httpHasPass, setHttpHasPass] = useState(false);
  const [httpsUrl, setHttpsUrl] = useState('');
  const [httpsUser, setHttpsUser] = useState('');
  const [httpsPass, setHttpsPass] = useState('');
  const [httpsHasPass, setHttpsHasPass] = useState(false);
  const [allUrl, setAllUrl] = useState('');
  const [allUser, setAllUser] = useState('');
  const [allPass, setAllPass] = useState('');
  const [allHasPass, setAllHasPass] = useState(false);
  // 三个「清除密码」动作的标记：点了清除即在本次 PATCH 发 password: "" 清空对应代理密码。
  const [httpClearPass, setHttpClearPass] = useState(false);
  const [httpsClearPass, setHttpsClearPass] = useState(false);
  const [allClearPass, setAllClearPass] = useState(false);
  const [noProxy, setNoProxy] = useState('');
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState<string | null>(null);
  const [saved, setSaved] = useState(false);

  // —— 系统配置（动态配置面板，FR-106）——
  // limits / observability / vuln / auth 非密钥项；保存落库、**重启生效**（无热替换槽）。
  // FR-103 起并入对应锚点节、随**全局保存**一并 PATCH（不再有独立保存按钮）。
  const [dynamic, setDynamic] = useState<DynamicConfig | null>(null);
  const [dynamicError, setDynamicError] = useState<string | null>(null);

  // 当前高亮的锚点节（FR-103）：由 IntersectionObserver 据可视区更新；点击导航即时设置以即时反馈。
  const [activeSection, setActiveSection] = useState<string>(SECTIONS[0].id);

  // 用一份设置填充表单态。
  function fillForm(s: SettingsView) {
    setSettings(s);
    // 网络代理三槽：URL / 用户名回显预填，密码框始终空、仅以 has_password 标识是否已配置（FR-100）。
    setHttpUrl(s.network_proxy.http.url ?? '');
    setHttpUser(s.network_proxy.http.username ?? '');
    setHttpPass('');
    setHttpHasPass(s.network_proxy.http.has_password);
    setHttpClearPass(false);
    setHttpsUrl(s.network_proxy.https.url ?? '');
    setHttpsUser(s.network_proxy.https.username ?? '');
    setHttpsPass('');
    setHttpsHasPass(s.network_proxy.https.has_password);
    setHttpsClearPass(false);
    setAllUrl(s.network_proxy.all.url ?? '');
    setAllUser(s.network_proxy.all.username ?? '');
    setAllPass('');
    setAllHasPass(s.network_proxy.all.has_password);
    setAllClearPass(false);
    setNoProxy(s.network_proxy.no_proxy ?? '');
  }

  useEffect(() => {
    api
      .getSettings()
      .then(fillForm)
      .catch((err) => setError(errorMessage(err)))
      .finally(() => setLoading(false));
  }, []);

  // 动态配置独立加载：失败不阻塞代理 / 更新主表单，仅在对应节内提示；加载失败时全局保存只发 settings PATCH。
  useEffect(() => {
    api
      .getDynamicConfig()
      .then(setDynamic)
      .catch((err) => setDynamicError(errorMessage(err)));
  }, []);

  // 锚点高亮（FR-103）：观察各节，取最靠上的可视节作为当前高亮。jsdom 无真实布局、IO 为空桩（见 test setup）。
  useEffect(() => {
    if (loading) return;
    const elements = SECTIONS.map((s) => document.getElementById(s.id)).filter(
      (el): el is HTMLElement => el !== null,
    );
    if (elements.length === 0) return;
    const observer = new IntersectionObserver(
      (entries) => {
        const visible = entries
          .filter((e) => e.isIntersecting)
          .sort((a, b) => a.boundingClientRect.top - b.boundingClientRect.top);
        if (visible.length > 0) {
          setActiveSection(visible[0].target.id);
        }
      },
      { rootMargin: '0px 0px -60% 0px', threshold: 0 },
    );
    elements.forEach((el) => observer.observe(el));
    return () => observer.disconnect();
  }, [loading]);

  /** 点击锚点导航：平滑滚动到对应节并即时高亮（即时反馈，可视区高亮随后由 IO 校正）。 */
  function scrollToSection(id: string) {
    setActiveSection(id);
    document.getElementById(id)?.scrollIntoView({ behavior: 'smooth', block: 'start' });
  }

  // 不可变更新动态配置某节的某字段（保持薄、复用于所有数值 / 开关项）。
  function patchDynamic<K extends keyof DynamicConfig>(section: K, value: DynamicConfig[K]): void {
    setDynamic((prev) => (prev ? { ...prev, [section]: value } : prev));
    setSaved(false);
  }

  if (loading) {
    return (
      <Center h={200}>
        <Loader />
      </Center>
    );
  }

  if (!settings) {
    return (
      <Stack>
        <Title order={2}>设置</Title>
        {error && <ErrorAlert message={error} />}
      </Stack>
    );
  }

  // 组装单代理 PATCH 项（FR-100）：url / username 照填；
  // 密码三态——填了新密码 → 带 password（设置）；点了「清除密码」→ 带 password: ""（清空）；
  // 否则省略 password 字段（保留现有）。
  function buildProxyPatch(
    url: string,
    username: string,
    password: string,
    clearPass: boolean,
  ): ProxyEntryPatch {
    const entry: ProxyEntryPatch = { url: url.trim(), username: username.trim() };
    if (password) {
      entry.password = password;
    } else if (clearPass) {
      entry.password = '';
    }
    return entry;
  }

  // 全局保存（FR-103）：一次性提交两处写入——
  // ① PATCH /settings（网络代理，运行时即时生效，沿用 FR-88/89/100，部分 PATCH 只发 network_proxy）；
  // ② 若动态配置已加载则 PATCH /settings/dynamic（limits/observability/vuln/auth，重启生效，FR-106）。
  // 顺序提交：先 settings 再 dynamic，任一失败聚合到 saveError、成功显「已保存」。
  async function handleSaveAll() {
    setSaving(true);
    setSaveError(null);
    setSaved(false);
    try {
      const updated = await api.updateSettings({
        network_proxy: {
          http: buildProxyPatch(httpUrl, httpUser, httpPass, httpClearPass),
          https: buildProxyPatch(httpsUrl, httpsUser, httpsPass, httpsClearPass),
          all: buildProxyPatch(allUrl, allUser, allPass, allClearPass),
          no_proxy: noProxy.trim(),
        },
      });
      fillForm(updated);
      // 动态配置已加载才提交（未加载则跳过，不报错）
      if (dynamic) {
        const updatedDynamic = await api.updateDynamicConfig(dynamic);
        setDynamic(updatedDynamic);
      }
      setSaved(true);
    } catch (err) {
      setSaveError(errorMessage(err));
    } finally {
      setSaving(false);
    }
  }

  return (
    <Stack gap={density.gridSpacing}>
      <Title order={2}>设置</Title>
      {error && <ErrorAlert message={error} />}

      {/* FR-103：左侧 sticky 锚点子导航 + 右侧单页分节。
          导航固定置顶（随右侧内容滚动常驻可见）；右侧各节纵向排列、不强制等高（短节不留空白）。
          滚动祖先是文档（AppShell.Main 不自带 overflow）；FR-92 alt 外壳的页眉 `position: fixed`
          覆盖视口顶部，故 sticky `top` 取页眉高度（density.headerHeight），让导航贴在页眉下方常驻、
          不被固定页眉遮住上方的 tab（修 sticky 滚动后失效）。 */}
      <Flex gap="md" align="flex-start">
        {/* —— 左侧 sticky 锚点导航 —— */}
        <Box
          component="nav"
          aria-label="设置分节导航"
          visibleFrom="sm"
          style={{ position: 'sticky', top: density.headerHeight, width: 180, flexShrink: 0 }}
        >
          {SECTIONS.map((s) => (
            <NavLink
              key={s.id}
              component="button"
              type="button"
              label={s.label}
              active={activeSection === s.id}
              onClick={() => scrollToSection(s.id)}
            />
          ))}
        </Box>

        {/* —— 右侧单页分节 —— */}
        <Stack gap={density.gridSpacing} style={{ flex: 1, minWidth: 0 }}>
          {/* —— 网络代理节 —— */}
          <Card
            component="section"
            id="proxy"
            withBorder
            padding={density.cardPadding}
            radius="md"
            style={SECTION_SCROLL_STYLE}
          >
            <Title order={4}>网络代理</Title>
            <Text size="sm" c="dimmed" mb="sm">
              统一出站代理（回源 / 迁移 / 漏洞库 / OIDC / 在线更新共用）。每代理可填用户名 + 密码；
              用户名回显、密码不回显（留空保留现有），URL
              留空表示不配置该代理。保存后**即时生效、无须重启**。
            </Text>
            <Stack gap="md">
              {/* HTTP / HTTPS 各自 scheme 专属代理；SOCKS5 填 all（兜底全 scheme，FR-100） */}
              <ProxyFields
                title="HTTP 代理"
                urlPlaceholder="http://host:3128"
                url={httpUrl}
                onUrlChange={setHttpUrl}
                username={httpUser}
                onUsernameChange={setHttpUser}
                password={httpPass}
                onPasswordChange={setHttpPass}
                hasPassword={httpHasPass}
                passwordCleared={httpClearPass}
                onClearPassword={() => setHttpClearPass(true)}
              />
              <ProxyFields
                title="HTTPS 代理"
                urlPlaceholder="http://host:3128"
                url={httpsUrl}
                onUrlChange={setHttpsUrl}
                username={httpsUser}
                onUsernameChange={setHttpsUser}
                password={httpsPass}
                onPasswordChange={setHttpsPass}
                hasPassword={httpsHasPass}
                passwordCleared={httpsClearPass}
                onClearPassword={() => setHttpsClearPass(true)}
              />
              <ProxyFields
                title="SOCKS5 代理（all，兜底全 scheme）"
                urlPlaceholder="socks5://host:1080"
                url={allUrl}
                onUrlChange={setAllUrl}
                username={allUser}
                onUsernameChange={setAllUser}
                password={allPass}
                onPasswordChange={setAllPass}
                hasPassword={allHasPass}
                passwordCleared={allClearPass}
                onClearPassword={() => setAllClearPass(true)}
              />
              <TextInput
                label="直连绕过（no_proxy）"
                placeholder="localhost,127.0.0.1,.internal"
                value={noProxy}
                onChange={(e) => setNoProxy(e.currentTarget.value)}
              />
            </Stack>
          </Card>

          {/* —— 系统配置各节（动态配置，FR-106）：limits / observability / vuln / auth 非密钥项 ——
              这些节无热替换槽，保存后**重启生效**；随全局保存一并 PATCH /settings/dynamic（无独立保存按钮）。 */}
          {dynamicError && <ErrorAlert message={dynamicError} />}

          {/* —— 限制与配额节 —— */}
          <Card
            component="section"
            id="limits"
            withBorder
            padding={density.cardPadding}
            radius="md"
            style={SECTION_SCROLL_STYLE}
          >
            <Group gap="xs" mb="xs">
              <Title order={4}>限制与配额</Title>
              <Badge size="sm" color="yellow" variant="light">
                保存后重启生效
              </Badge>
            </Group>
            {!dynamic ? (
              <Center h={80}>
                <Loader size="sm" />
              </Center>
            ) : (
              <NumberInput
                label="单个制品上传上限（字节）"
                description="留空表示不额外限制；超限上传返回 413。"
                placeholder="不限制"
                min={0}
                value={dynamic.limits.max_artifact_size ?? ''}
                onChange={(v) =>
                  patchDynamic('limits', {
                    max_artifact_size: v === '' || v === null ? null : Number(v),
                  })
                }
              />
            )}
          </Card>

          {/* —— 可观测性节 —— */}
          <Card
            component="section"
            id="observability"
            withBorder
            padding={density.cardPadding}
            radius="md"
            style={SECTION_SCROLL_STYLE}
          >
            <Group gap="xs" mb="xs">
              <Title order={4}>可观测性</Title>
              <Badge size="sm" color="yellow" variant="light">
                保存后重启生效
              </Badge>
            </Group>
            {!dynamic ? (
              <Center h={120}>
                <Loader size="sm" />
              </Center>
            ) : (
              <Stack gap="sm">
                <Group grow>
                  <NumberInput
                    label="审计日志保留天数"
                    min={0}
                    value={dynamic.audit.retention_days}
                    onChange={(v) =>
                      patchDynamic('audit', { ...dynamic.audit, retention_days: Number(v) || 0 })
                    }
                  />
                  <NumberInput
                    label="审计日志行数上限"
                    min={0}
                    value={dynamic.audit.max_rows}
                    onChange={(v) =>
                      patchDynamic('audit', { ...dynamic.audit, max_rows: Number(v) || 0 })
                    }
                  />
                </Group>
                <Switch
                  label="记录逐条访问 / 下载明细（使用分析）"
                  checked={dynamic.usage.detail_enabled}
                  onChange={(e) =>
                    patchDynamic('usage', {
                      ...dynamic.usage,
                      detail_enabled: e.currentTarget.checked,
                    })
                  }
                />
                <NumberInput
                  label="使用明细行数上限"
                  min={0}
                  value={dynamic.usage.max_detail_rows}
                  onChange={(v) =>
                    patchDynamic('usage', { ...dynamic.usage, max_detail_rows: Number(v) || 0 })
                  }
                />
                <Switch
                  label="启用 Prometheus 指标端点（/metrics）"
                  checked={dynamic.metrics.enabled}
                  onChange={(e) =>
                    patchDynamic('metrics', {
                      ...dynamic.metrics,
                      enabled: e.currentTarget.checked,
                    })
                  }
                />
                <Switch
                  label="允许匿名抓取 /metrics（须限内网 / 反代后）"
                  checked={dynamic.metrics.allow_anonymous}
                  onChange={(e) =>
                    patchDynamic('metrics', {
                      ...dynamic.metrics,
                      allow_anonymous: e.currentTarget.checked,
                    })
                  }
                />
                <Switch
                  label="启用指标时序采集"
                  checked={dynamic.metrics_timeseries.enabled}
                  onChange={(e) =>
                    patchDynamic('metrics_timeseries', {
                      ...dynamic.metrics_timeseries,
                      enabled: e.currentTarget.checked,
                    })
                  }
                />
                <Group grow>
                  <NumberInput
                    label="时序采样间隔（秒）"
                    min={1}
                    value={dynamic.metrics_timeseries.sample_interval_secs}
                    onChange={(v) =>
                      patchDynamic('metrics_timeseries', {
                        ...dynamic.metrics_timeseries,
                        sample_interval_secs: Number(v) || 0,
                      })
                    }
                  />
                  <NumberInput
                    label="时序保留天数"
                    min={0}
                    value={dynamic.metrics_timeseries.retention_days}
                    onChange={(v) =>
                      patchDynamic('metrics_timeseries', {
                        ...dynamic.metrics_timeseries,
                        retention_days: Number(v) || 0,
                      })
                    }
                  />
                </Group>
              </Stack>
            )}
          </Card>

          {/* —— 漏洞库节 —— */}
          <Card
            component="section"
            id="vuln"
            withBorder
            padding={density.cardPadding}
            radius="md"
            style={SECTION_SCROLL_STYLE}
          >
            <Group gap="xs" mb="xs">
              <Title order={4}>漏洞库</Title>
              <Badge size="sm" color="yellow" variant="light">
                保存后重启生效
              </Badge>
            </Group>
            {!dynamic ? (
              <Center h={120}>
                <Loader size="sm" />
              </Center>
            ) : (
              <Stack gap="sm">
                <Switch
                  label="启用漏洞库离线镜像"
                  checked={dynamic.vuln.enabled}
                  onChange={(e) =>
                    patchDynamic('vuln', { ...dynamic.vuln, enabled: e.currentTarget.checked })
                  }
                />
                <TextInput
                  label="镜像数据源基址"
                  placeholder="https://osv-vulnerabilities.storage.googleapis.com"
                  value={dynamic.vuln.source_base_url}
                  onChange={(e) =>
                    patchDynamic('vuln', {
                      ...dynamic.vuln,
                      source_base_url: e.currentTarget.value,
                    })
                  }
                />
                <Group grow>
                  <NumberInput
                    label="刷新周期（秒）"
                    min={1}
                    value={dynamic.vuln.refresh_interval_secs}
                    onChange={(v) =>
                      patchDynamic('vuln', {
                        ...dynamic.vuln,
                        refresh_interval_secs: Number(v) || 0,
                      })
                    }
                  />
                  <NumberInput
                    label="下载超时（秒）"
                    min={1}
                    value={dynamic.vuln.download_timeout_secs}
                    onChange={(v) =>
                      patchDynamic('vuln', {
                        ...dynamic.vuln,
                        download_timeout_secs: Number(v) || 0,
                      })
                    }
                  />
                </Group>
              </Stack>
            )}
          </Card>

          {/* —— 安全 / 会话节 —— */}
          <Card
            component="section"
            id="auth"
            withBorder
            padding={density.cardPadding}
            radius="md"
            style={SECTION_SCROLL_STYLE}
          >
            <Group gap="xs" mb="xs">
              <Title order={4}>安全 / 会话</Title>
              <Badge size="sm" color="yellow" variant="light">
                保存后重启生效
              </Badge>
            </Group>
            <Text size="xs" c="dimmed" mb="xs">
              仅会话 / 登录锁定可调标量；OIDC / LDAP 等密钥项不在此处、只能经配置文件 /
              环境变量设置。
            </Text>
            {!dynamic ? (
              <Center h={80}>
                <Loader size="sm" />
              </Center>
            ) : (
              <Group grow>
                <NumberInput
                  label="会话有效期（秒）"
                  min={1}
                  value={dynamic.auth.session_ttl_secs}
                  onChange={(v) =>
                    patchDynamic('auth', { ...dynamic.auth, session_ttl_secs: Number(v) || 0 })
                  }
                />
                <NumberInput
                  label="触发锁定的连续失败次数"
                  min={0}
                  value={dynamic.auth.login_max_failures}
                  onChange={(v) =>
                    patchDynamic('auth', { ...dynamic.auth, login_max_failures: Number(v) || 0 })
                  }
                />
                <NumberInput
                  label="锁定时长（秒）"
                  min={1}
                  value={dynamic.auth.login_lockout_secs}
                  onChange={(v) =>
                    patchDynamic('auth', { ...dynamic.auth, login_lockout_secs: Number(v) || 0 })
                  }
                />
              </Group>
            )}
          </Card>

          {/* —— 防护配置节（FR-110）：原独立页 /protection 并入此处 ——
              自带 GET/PATCH /protection/config 与独立保存按钮（即时生效），
              不并入设置页底部「全局保存」（代理 + 动态配置）。 */}
          <ProtectionConfigSection />
        </Stack>
      </Flex>

      {/* —— 单个全局保存（FR-103）——
          固定为 sticky 底部动作条：始终贴在滚动视口底部、不随内容 / 窗口缩放 / 滚动漂移；
          负的左右 / 下外边距抵消 AppShell.Main 的内边距，使其横向铺满、紧贴底缘；
          顶部描边 + 背景 + 内边距与内容区分隔，避免遮挡正文。
          一次提交两处写入：PATCH /settings（即时生效）+ PATCH /settings/dynamic（重启生效）。 */}
      <Box
        data-testid="settings-save-bar"
        style={{
          position: 'sticky',
          bottom: 0,
          zIndex: 1,
          marginInline: `calc(-1 * var(--mantine-spacing-sm))`,
          marginBottom: `calc(-1 * var(--mantine-spacing-sm))`,
          padding: `var(--mantine-spacing-sm)`,
          backgroundColor: 'var(--mantine-color-body)',
          borderTop: '1px solid var(--mantine-color-default-border)',
        }}
      >
        <Group>
          <Button
            leftSection={<IconDeviceFloppy size={16} />}
            onClick={handleSaveAll}
            loading={saving}
          >
            保存
          </Button>
          {saved && (
            <Text c="green" size="sm">
              已保存。代理即时生效；限制配额 / 可观测性 / 漏洞库 / 安全会话重启后生效。
            </Text>
          )}
        </Group>
        {saveError && (
          <Box mt="sm">
            <ErrorAlert message={saveError} />
          </Box>
        )}
      </Box>
    </Stack>
  );
}
