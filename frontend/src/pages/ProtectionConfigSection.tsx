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
        <Title order={4}>防护配置</Title>
        <Badge size="sm" color="green" variant="light">
          保存后即时生效
        </Badge>
      </Group>
      <Text size="sm" c="dimmed" mb="sm">
        各七层防护维度的启停与调参，保存后即时生效、无须重启；阈值 / 名单 /
        规则为本机内部配置，不外发。
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
  const rl = config.rate_limit;
  const ban = config.ban;
  const slow = config.slowloris;
  const cc = config.cc_challenge;
  const waf = config.waf;
  const alerts = config.alerts;

  return (
    <Stack>
      {error && <ErrorAlert message={error} />}
      {saved && <Text c="green">已保存，配置已即时生效。</Text>}

      {/* —— 速率限制 —— */}
      <Section
        title="速率限制"
        description="按 IP / 身份 / 仓库维度固定窗计数，超阈值返回 429；并发上限 0 表示不限。"
      >
        <Switch
          label="启用速率限制"
          checked={rl.enabled}
          onChange={(e) => onPatch('rate_limit', { ...rl, enabled: e.currentTarget.checked })}
        />
        <Group grow>
          <NumberInput
            label="时间窗（秒）"
            min={1}
            value={rl.window_secs}
            onChange={(v) => onPatch('rate_limit', { ...rl, window_secs: Number(v) || 0 })}
          />
          <NumberInput
            label="单 IP 每窗上限"
            min={0}
            value={rl.ip_max_requests}
            onChange={(v) => onPatch('rate_limit', { ...rl, ip_max_requests: Number(v) || 0 })}
          />
          <NumberInput
            label="单身份每窗上限"
            min={0}
            value={rl.identity_max_requests}
            onChange={(v) =>
              onPatch('rate_limit', { ...rl, identity_max_requests: Number(v) || 0 })
            }
          />
        </Group>
        <Group grow>
          <NumberInput
            label="单仓库每窗上限（0=不启用）"
            min={0}
            value={rl.repo_max_requests}
            onChange={(v) => onPatch('rate_limit', { ...rl, repo_max_requests: Number(v) || 0 })}
          />
          <NumberInput
            label="单 IP 并发上限（0=不限）"
            min={0}
            value={rl.ip_max_concurrent}
            onChange={(v) => onPatch('rate_limit', { ...rl, ip_max_concurrent: Number(v) || 0 })}
          />
          <NumberInput
            label="单用户并发上限（0=不限）"
            min={0}
            value={rl.user_max_concurrent}
            onChange={(v) => onPatch('rate_limit', { ...rl, user_max_concurrent: Number(v) || 0 })}
          />
          <NumberInput
            label="单仓库并发上限（0=不限）"
            min={0}
            value={rl.repo_max_concurrent}
            onChange={(v) => onPatch('rate_limit', { ...rl, repo_max_concurrent: Number(v) || 0 })}
          />
        </Group>
      </Section>

      {/* —— IP 黑 / 白名单 —— */}
      <Section
        title="IP 黑 / 白名单"
        description="每行一个 IP 或 CIDR 网段；白名单豁免一切防护、黑名单直接拒。"
      >
        <Textarea
          label="白名单（每行一个 IP / CIDR）"
          autosize
          minRows={2}
          value={allowText}
          onChange={(e) => onAllowTextChange(e.currentTarget.value)}
        />
        <Textarea
          label="黑名单（每行一个 IP / CIDR）"
          autosize
          minRows={2}
          value={denyText}
          onChange={(e) => onDenyTextChange(e.currentTarget.value)}
        />
      </Section>

      {/* —— 异常检测与自动封禁 —— */}
      <Section
        title="异常检测与自动封禁"
        description="窗内单 IP 异常信号达阈值即封禁一段时间，到期自动解封。"
      >
        <Switch
          label="启用异常封禁"
          checked={ban.enabled}
          onChange={(e) => onPatch('ban', { ...ban, enabled: e.currentTarget.checked })}
        />
        <Group grow>
          <NumberInput
            label="时间窗（秒）"
            min={1}
            value={ban.window_secs}
            onChange={(v) => onPatch('ban', { ...ban, window_secs: Number(v) || 0 })}
          />
          <NumberInput
            label="封禁阈值"
            min={1}
            value={ban.threshold}
            onChange={(v) => onPatch('ban', { ...ban, threshold: Number(v) || 0 })}
          />
          <NumberInput
            label="封禁时长（秒）"
            min={1}
            value={ban.duration_secs}
            onChange={(v) => onPatch('ban', { ...ban, duration_secs: Number(v) || 0 })}
          />
        </Group>
      </Section>

      {/* —— 慢速攻击防护 —— */}
      <Section
        title="慢速攻击防护"
        description="对慢速 drip 请求体设超时、对所有请求体设通用大小上限（0=不启用）。"
      >
        <Switch
          label="启用慢速攻击防护"
          checked={slow.enabled}
          onChange={(e) => onPatch('slowloris', { ...slow, enabled: e.currentTarget.checked })}
        />
        <Group grow>
          <NumberInput
            label="块间空闲超时（秒）"
            min={1}
            value={slow.body_read_timeout_secs}
            onChange={(v) =>
              onPatch('slowloris', { ...slow, body_read_timeout_secs: Number(v) || 0 })
            }
          />
          <NumberInput
            label="首块等待超时（秒）"
            min={1}
            value={slow.header_timeout_secs}
            onChange={(v) => onPatch('slowloris', { ...slow, header_timeout_secs: Number(v) || 0 })}
          />
          <NumberInput
            label="通用体上限（字节，0=不启用）"
            min={0}
            value={slow.max_body_bytes}
            onChange={(v) => onPatch('slowloris', { ...slow, max_body_bytes: Number(v) || 0 })}
          />
        </Group>
      </Section>

      {/* —— CC 挑战 —— */}
      <Section
        title="CC 挑战（PoW）"
        description="对匿名可疑流量要求工作量证明；难度越高刷流成本越高。默认豁免已认证客户端。"
      >
        <Switch
          label="启用 CC 挑战"
          checked={cc.enabled}
          onChange={(e) => onPatch('cc_challenge', { ...cc, enabled: e.currentTarget.checked })}
        />
        <Switch
          label="豁免已认证请求"
          checked={cc.exempt_authenticated}
          onChange={(e) =>
            onPatch('cc_challenge', { ...cc, exempt_authenticated: e.currentTarget.checked })
          }
        />
        <Group grow>
          <NumberInput
            label="难度（前导零位，≤64）"
            min={0}
            max={64}
            value={cc.difficulty}
            onChange={(v) => onPatch('cc_challenge', { ...cc, difficulty: Number(v) || 0 })}
          />
          <NumberInput
            label="令牌有效期（秒）"
            min={1}
            value={cc.ttl_secs}
            onChange={(v) => onPatch('cc_challenge', { ...cc, ttl_secs: Number(v) || 0 })}
          />
        </Group>
      </Section>

      {/* —— WAF 规则引擎 —— */}
      <Section
        title="WAF 规则引擎"
        description="按 method / path / query / header 有序匹配，首个命中生效（block 拒 / allow 放行）。"
      >
        <Switch
          label="启用 WAF"
          checked={waf.enabled}
          onChange={(e) => onPatch('waf', { ...waf, enabled: e.currentTarget.checked })}
        />
        <Table>
          <Table.Thead>
            <Table.Tr>
              <Table.Th>字段</Table.Th>
              <Table.Th>头名（仅 header）</Table.Th>
              <Table.Th>模式</Table.Th>
              <Table.Th>匹配类型</Table.Th>
              <Table.Th>动作</Table.Th>
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
                      aria-label="规则字段"
                    />
                  </Table.Td>
                  <Table.Td>
                    <TextInput
                      value={rule.header_name ?? ''}
                      onChange={(e) => updateRule({ header_name: e.currentTarget.value })}
                      aria-label="头名"
                    />
                  </Table.Td>
                  <Table.Td>
                    <TextInput
                      value={rule.pattern}
                      onChange={(e) => updateRule({ pattern: e.currentTarget.value })}
                      aria-label="模式"
                    />
                  </Table.Td>
                  <Table.Td>
                    <Select
                      data={WAF_MATCH_TYPES}
                      value={rule.match_type}
                      onChange={(v) => updateRule({ match_type: v ?? 'literal' })}
                      aria-label="匹配类型"
                    />
                  </Table.Td>
                  <Table.Td>
                    <Select
                      data={WAF_ACTIONS}
                      value={rule.action}
                      onChange={(v) => updateRule({ action: v ?? 'block' })}
                      aria-label="动作"
                    />
                  </Table.Td>
                  <Table.Td>
                    <ActionIcon
                      color="red"
                      variant="subtle"
                      aria-label="删除规则"
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
            新增规则
          </Button>
        </Group>
      </Section>

      {/* —— 监控告警 —— */}
      <Section
        title="监控告警"
        description="窗内各防护维度计数达阈值即告警并落库；告警是本机内部数据、不外发。"
      >
        <Switch
          label="启用阈值告警"
          checked={alerts.enabled}
          onChange={(e) => onPatch('alerts', { ...alerts, enabled: e.currentTarget.checked })}
        />
        <Group grow>
          <NumberInput
            label="评估窗（秒）"
            min={1}
            value={alerts.window_secs}
            onChange={(v) => onPatch('alerts', { ...alerts, window_secs: Number(v) || 0 })}
          />
          <NumberInput
            label="限流被拒阈值"
            min={0}
            value={alerts.rate_limit_warn_threshold}
            onChange={(v) =>
              onPatch('alerts', { ...alerts, rate_limit_warn_threshold: Number(v) || 0 })
            }
          />
          <NumberInput
            label="自动封禁阈值"
            min={0}
            value={alerts.ban_warn_threshold}
            onChange={(v) => onPatch('alerts', { ...alerts, ban_warn_threshold: Number(v) || 0 })}
          />
        </Group>
        <Group grow>
          <NumberInput
            label="CC 失败阈值"
            min={0}
            value={alerts.cc_challenge_fail_warn_threshold}
            onChange={(v) =>
              onPatch('alerts', { ...alerts, cc_challenge_fail_warn_threshold: Number(v) || 0 })
            }
          />
          <NumberInput
            label="WAF 阻断阈值"
            min={0}
            value={alerts.waf_block_warn_threshold}
            onChange={(v) =>
              onPatch('alerts', { ...alerts, waf_block_warn_threshold: Number(v) || 0 })
            }
          />
          <NumberInput
            label="慢速超时阈值"
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
          保存并即时生效
        </Button>
      </Group>
    </Stack>
  );
}
