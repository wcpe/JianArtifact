// 防护配置节（FR-110，原 FR-80 防护配置页并入设置页）：作为「设置」页的一个锚点节渲染，
// 各防护维度启停 / 调参，保存即调 PATCH /api/v1/protection/config，校验通过即时生效、无须重启。
//
// 数据来自后端 GET /api/v1/protection/config（仅管理员）。把七个维度（限流 / IP 名单 /
// 异常封禁 / 慢速攻击 / CC 挑战 / WAF / 告警）拆为分区表单，编辑后整体回传。
// 校验失败（如窗口为 0）后端返回 400，节内展示其错误文案，不改变现有生效配置。
//
// 与设置页的代理 / 动态配置「全局保存」相互独立：本节有自己的保存按钮与 PATCH 链路
// （即时生效，区别于动态配置的「保存后重启生效」），不并入设置页全局保存。

import { useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';
import {
  Stack,
  Title,
  Text,
  Card,
  Group,
  Badge,
  Switch,
  NumberInput,
  Textarea,
  Button,
  Loader,
  Center,
  Divider,
  Table,
  TextInput,
  Select,
  ActionIcon,
} from '@mantine/core';
import { IconTrash, IconPlus } from '@tabler/icons-react';
import * as api from '../api/endpoints';
import type { ProtectionConfig, WafRuleConfig } from '../api/types';
import { errorMessage } from '../lib/format';
import { ErrorAlert } from '../components/ErrorAlert';
import { density } from '../theme/density';

/** 把字符串文本域按行解析为去空白、去空行的字符串数组。 */
function linesToList(text: string): string[] {
  return text
    .split('\n')
    .map((s) => s.trim())
    .filter((s) => s.length > 0);
}

/** 分区卡片：标题 + 启用开关 + 内容。 */
function Section({
  title,
  description,
  children,
}: {
  title: string;
  description?: string;
  children: React.ReactNode;
}) {
  return (
    <Card withBorder padding="lg" radius="md">
      <Title order={5}>{title}</Title>
      {description && (
        <Text size="sm" c="dimmed" mb="sm">
          {description}
        </Text>
      )}
      <Stack gap="sm" mt="sm">
        {children}
      </Stack>
    </Card>
  );
}

/** WAF 规则字段 / 匹配类型 / 动作的可选项（对齐后端受限枚举）。 */
const WAF_FIELDS = ['method', 'path', 'query', 'header'];
const WAF_MATCH_TYPES = ['literal', 'wildcard', 'regex'];
const WAF_ACTIONS = ['block', 'allow'];

/** 防护配置节（嵌入设置页，FR-110）。 */
export function ProtectionConfigSection() {
  const { t } = useTranslation('protection');
  const [config, setConfig] = useState<ProtectionConfig | null>(null);
  const [allowText, setAllowText] = useState('');
  const [denyText, setDenyText] = useState('');
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [saved, setSaved] = useState(false);

  useEffect(() => {
    api
      .getProtectionConfig()
      .then((cfg) => {
        setConfig(cfg);
        setAllowText(cfg.ip_list.allow.join('\n'));
        setDenyText(cfg.ip_list.deny.join('\n'));
      })
      .catch((err) => setError(errorMessage(err)))
      .finally(() => setLoading(false));
  }, []);

  // 局部更新某维度的某字段（保持其余不变，整体可回传）
  function patch<K extends keyof ProtectionConfig>(key: K, value: ProtectionConfig[K]) {
    setConfig((prev) => (prev ? { ...prev, [key]: value } : prev));
  }

  async function handleSave() {
    if (!config) return;
    setSaving(true);
    setError(null);
    setSaved(false);
    // IP 名单以文本域为准，提交前归并回配置
    const payload: ProtectionConfig = {
      ...config,
      ip_list: { allow: linesToList(allowText), deny: linesToList(denyText) },
    };
    try {
      const updated = await api.updateProtectionConfig(payload);
      setConfig(updated);
      setAllowText(updated.ip_list.allow.join('\n'));
      setDenyText(updated.ip_list.deny.join('\n'));
      setSaved(true);
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setSaving(false);
    }
  }

  // 外层节卡片（id=protection，供锚点导航定位）：标题 + 即时生效徽标 + 内容。
  // scrollMarginTop 取页眉高度：点击锚点滚到本节时停在固定页眉下方、不被遮住（增强 FR-92，与设置页各节一致）。
  return (
    <Card
      component="section"
      id="protection"
      withBorder
      padding={density.cardPadding}
      radius="md"
      style={{ scrollMarginTop: density.headerHeight }}
    >
      <Group gap="xs" mb="xs">
        <Title order={4}>{t('card.title')}</Title>
        <Badge size="sm" color="green" variant="light">
          {t('card.instantBadge')}
        </Badge>
      </Group>
      <Text size="sm" c="dimmed" mb="sm">
        {t('card.description')}
      </Text>

      {loading ? (
        <Center h={200}>
          <Loader />
        </Center>
      ) : !config ? (
        error && <ErrorAlert message={error} />
      ) : (
        <ProtectionForm
          config={config}
          allowText={allowText}
          denyText={denyText}
          saving={saving}
          error={error}
          saved={saved}
          onAllowTextChange={setAllowText}
          onDenyTextChange={setDenyText}
          onPatch={patch}
          onSave={handleSave}
        />
      )}
    </Card>
  );
}

/** 防护表单主体（配置已加载后渲染）。 */
function ProtectionForm({
  config,
  allowText,
  denyText,
  saving,
  error,
  saved,
  onAllowTextChange,
  onDenyTextChange,
  onPatch,
  onSave,
}: {
  config: ProtectionConfig;
  allowText: string;
  denyText: string;
  saving: boolean;
  error: string | null;
  saved: boolean;
  onAllowTextChange: (v: string) => void;
  onDenyTextChange: (v: string) => void;
  onPatch: <K extends keyof ProtectionConfig>(key: K, value: ProtectionConfig[K]) => void;
  onSave: () => void;
}) {
  const { t } = useTranslation('protection');
  const rl = config.rate_limit;
  const ban = config.ban;
  const slow = config.slowloris;
  const cc = config.cc_challenge;
  const waf = config.waf;
  const alerts = config.alerts;

  return (
    <Stack>
      {error && <ErrorAlert message={error} />}
      {saved && <Text c="green">{t('savedHint')}</Text>}

      {/* —— 速率限制 —— */}
      <Section
        title={t('rateLimit.title')}
        description={t('rateLimit.description')}
      >
        <Switch
          label={t('rateLimit.enable')}
          checked={rl.enabled}
          onChange={(e) => onPatch('rate_limit', { ...rl, enabled: e.currentTarget.checked })}
        />
        <Group grow>
          <NumberInput
            label={t('rateLimit.windowSecs')}
            min={1}
            value={rl.window_secs}
            onChange={(v) => onPatch('rate_limit', { ...rl, window_secs: Number(v) || 0 })}
          />
          <NumberInput
            label={t('rateLimit.ipMaxRequests')}
            min={0}
            value={rl.ip_max_requests}
            onChange={(v) => onPatch('rate_limit', { ...rl, ip_max_requests: Number(v) || 0 })}
          />
          <NumberInput
            label={t('rateLimit.identityMaxRequests')}
            min={0}
            value={rl.identity_max_requests}
            onChange={(v) =>
              onPatch('rate_limit', { ...rl, identity_max_requests: Number(v) || 0 })
            }
          />
        </Group>
        <Group grow>
          <NumberInput
            label={t('rateLimit.repoMaxRequests')}
            min={0}
            value={rl.repo_max_requests}
            onChange={(v) => onPatch('rate_limit', { ...rl, repo_max_requests: Number(v) || 0 })}
          />
          <NumberInput
            label={t('rateLimit.ipMaxConcurrent')}
            min={0}
            value={rl.ip_max_concurrent}
            onChange={(v) => onPatch('rate_limit', { ...rl, ip_max_concurrent: Number(v) || 0 })}
          />
          <NumberInput
            label={t('rateLimit.userMaxConcurrent')}
            min={0}
            value={rl.user_max_concurrent}
            onChange={(v) => onPatch('rate_limit', { ...rl, user_max_concurrent: Number(v) || 0 })}
          />
          <NumberInput
            label={t('rateLimit.repoMaxConcurrent')}
            min={0}
            value={rl.repo_max_concurrent}
            onChange={(v) => onPatch('rate_limit', { ...rl, repo_max_concurrent: Number(v) || 0 })}
          />
        </Group>
      </Section>

      {/* —— IP 黑 / 白名单 —— */}
      <Section
        title={t('ipList.title')}
        description={t('ipList.description')}
      >
        <Textarea
          label={t('ipList.allowLabel')}
          autosize
          minRows={2}
          value={allowText}
          onChange={(e) => onAllowTextChange(e.currentTarget.value)}
        />
        <Textarea
          label={t('ipList.denyLabel')}
          autosize
          minRows={2}
          value={denyText}
          onChange={(e) => onDenyTextChange(e.currentTarget.value)}
        />
      </Section>

      {/* —— 异常检测与自动封禁 —— */}
      <Section
        title={t('ban.title')}
        description={t('ban.description')}
      >
        <Switch
          label={t('ban.enable')}
          checked={ban.enabled}
          onChange={(e) => onPatch('ban', { ...ban, enabled: e.currentTarget.checked })}
        />
        <Group grow>
          <NumberInput
            label={t('ban.windowSecs')}
            min={1}
            value={ban.window_secs}
            onChange={(v) => onPatch('ban', { ...ban, window_secs: Number(v) || 0 })}
          />
          <NumberInput
            label={t('ban.threshold')}
            min={1}
            value={ban.threshold}
            onChange={(v) => onPatch('ban', { ...ban, threshold: Number(v) || 0 })}
          />
          <NumberInput
            label={t('ban.durationSecs')}
            min={1}
            value={ban.duration_secs}
            onChange={(v) => onPatch('ban', { ...ban, duration_secs: Number(v) || 0 })}
          />
        </Group>
      </Section>

      {/* —— 慢速攻击防护 —— */}
      <Section
        title={t('slowloris.title')}
        description={t('slowloris.description')}
      >
        <Switch
          label={t('slowloris.enable')}
          checked={slow.enabled}
          onChange={(e) => onPatch('slowloris', { ...slow, enabled: e.currentTarget.checked })}
        />
        <Group grow>
          <NumberInput
            label={t('slowloris.bodyReadTimeoutSecs')}
            min={1}
            value={slow.body_read_timeout_secs}
            onChange={(v) =>
              onPatch('slowloris', { ...slow, body_read_timeout_secs: Number(v) || 0 })
            }
          />
          <NumberInput
            label={t('slowloris.headerTimeoutSecs')}
            min={1}
            value={slow.header_timeout_secs}
            onChange={(v) => onPatch('slowloris', { ...slow, header_timeout_secs: Number(v) || 0 })}
          />
          <NumberInput
            label={t('slowloris.maxBodyBytes')}
            min={0}
            value={slow.max_body_bytes}
            onChange={(v) => onPatch('slowloris', { ...slow, max_body_bytes: Number(v) || 0 })}
          />
        </Group>
      </Section>

      {/* —— CC 挑战 —— */}
      <Section
        title={t('ccChallenge.title')}
        description={t('ccChallenge.description')}
      >
        <Switch
          label={t('ccChallenge.enable')}
          checked={cc.enabled}
          onChange={(e) => onPatch('cc_challenge', { ...cc, enabled: e.currentTarget.checked })}
        />
        <Switch
          label={t('ccChallenge.exemptAuthenticated')}
          checked={cc.exempt_authenticated}
          onChange={(e) =>
            onPatch('cc_challenge', { ...cc, exempt_authenticated: e.currentTarget.checked })
          }
        />
        <Group grow>
          <NumberInput
            label={t('ccChallenge.difficulty')}
            min={0}
            max={64}
            value={cc.difficulty}
            onChange={(v) => onPatch('cc_challenge', { ...cc, difficulty: Number(v) || 0 })}
          />
          <NumberInput
            label={t('ccChallenge.ttlSecs')}
            min={1}
            value={cc.ttl_secs}
            onChange={(v) => onPatch('cc_challenge', { ...cc, ttl_secs: Number(v) || 0 })}
          />
        </Group>
      </Section>

      {/* —— WAF 规则引擎 —— */}
      <Section
        title={t('waf.title')}
        description={t('waf.description')}
      >
        <Switch
          label={t('waf.enable')}
          checked={waf.enabled}
          onChange={(e) => onPatch('waf', { ...waf, enabled: e.currentTarget.checked })}
        />
        <Table>
          <Table.Thead>
            <Table.Tr>
              <Table.Th>{t('waf.colField')}</Table.Th>
              <Table.Th>{t('waf.colHeaderName')}</Table.Th>
              <Table.Th>{t('waf.colPattern')}</Table.Th>
              <Table.Th>{t('waf.colMatchType')}</Table.Th>
              <Table.Th>{t('waf.colAction')}</Table.Th>
              <Table.Th />
            </Table.Tr>
          </Table.Thead>
          <Table.Tbody>
            {waf.rules.map((rule, idx) => {
              const updateRule = (next: Partial<WafRuleConfig>) => {
                const rules = waf.rules.map((r, i) => (i === idx ? { ...r, ...next } : r));
                onPatch('waf', { ...waf, rules });
              };
              return (
                <Table.Tr key={idx}>
                  <Table.Td>
                    <Select
                      data={WAF_FIELDS}
                      value={rule.field}
                      onChange={(v) => updateRule({ field: v ?? 'path' })}
                      aria-label={t('waf.ariaField')}
                    />
                  </Table.Td>
                  <Table.Td>
                    <TextInput
                      value={rule.header_name ?? ''}
                      onChange={(e) => updateRule({ header_name: e.currentTarget.value })}
                      aria-label={t('waf.ariaHeaderName')}
                    />
                  </Table.Td>
                  <Table.Td>
                    <TextInput
                      value={rule.pattern}
                      onChange={(e) => updateRule({ pattern: e.currentTarget.value })}
                      aria-label={t('waf.ariaPattern')}
                    />
                  </Table.Td>
                  <Table.Td>
                    <Select
                      data={WAF_MATCH_TYPES}
                      value={rule.match_type}
                      onChange={(v) => updateRule({ match_type: v ?? 'literal' })}
                      aria-label={t('waf.ariaMatchType')}
                    />
                  </Table.Td>
                  <Table.Td>
                    <Select
                      data={WAF_ACTIONS}
                      value={rule.action}
                      onChange={(v) => updateRule({ action: v ?? 'block' })}
                      aria-label={t('waf.ariaAction')}
                    />
                  </Table.Td>
                  <Table.Td>
                    <ActionIcon
                      color="red"
                      variant="subtle"
                      aria-label={t('waf.ariaDeleteRule')}
                      onClick={() =>
                        onPatch('waf', { ...waf, rules: waf.rules.filter((_, i) => i !== idx) })
                      }
                    >
                      <IconTrash size={16} />
                    </ActionIcon>
                  </Table.Td>
                </Table.Tr>
              );
            })}
          </Table.Tbody>
        </Table>
        <Group>
          <Button
            variant="light"
            leftSection={<IconPlus size={16} />}
            onClick={() =>
              onPatch('waf', {
                ...waf,
                rules: [
                  ...waf.rules,
                  { field: 'path', pattern: '', match_type: 'literal', action: 'block' },
                ],
              })
            }
          >
            {t('waf.addRule')}
          </Button>
        </Group>
      </Section>

      {/* —— 监控告警 —— */}
      <Section
        title={t('alerts.title')}
        description={t('alerts.description')}
      >
        <Switch
          label={t('alerts.enable')}
          checked={alerts.enabled}
          onChange={(e) => onPatch('alerts', { ...alerts, enabled: e.currentTarget.checked })}
        />
        <Group grow>
          <NumberInput
            label={t('alerts.windowSecs')}
            min={1}
            value={alerts.window_secs}
            onChange={(v) => onPatch('alerts', { ...alerts, window_secs: Number(v) || 0 })}
          />
          <NumberInput
            label={t('alerts.rateLimitThreshold')}
            min={0}
            value={alerts.rate_limit_warn_threshold}
            onChange={(v) =>
              onPatch('alerts', { ...alerts, rate_limit_warn_threshold: Number(v) || 0 })
            }
          />
          <NumberInput
            label={t('alerts.banThreshold')}
            min={0}
            value={alerts.ban_warn_threshold}
            onChange={(v) => onPatch('alerts', { ...alerts, ban_warn_threshold: Number(v) || 0 })}
          />
        </Group>
        <Group grow>
          <NumberInput
            label={t('alerts.ccFailThreshold')}
            min={0}
            value={alerts.cc_challenge_fail_warn_threshold}
            onChange={(v) =>
              onPatch('alerts', { ...alerts, cc_challenge_fail_warn_threshold: Number(v) || 0 })
            }
          />
          <NumberInput
            label={t('alerts.wafBlockThreshold')}
            min={0}
            value={alerts.waf_block_warn_threshold}
            onChange={(v) =>
              onPatch('alerts', { ...alerts, waf_block_warn_threshold: Number(v) || 0 })
            }
          />
          <NumberInput
            label={t('alerts.slowlorisThreshold')}
            min={0}
            value={alerts.slowloris_warn_threshold}
            onChange={(v) =>
              onPatch('alerts', { ...alerts, slowloris_warn_threshold: Number(v) || 0 })
            }
          />
        </Group>
      </Section>

      <Divider />
      <Group>
        <Button onClick={onSave} loading={saving}>
          {t('saveButton')}
        </Button>
      </Group>
    </Stack>
  );
}
