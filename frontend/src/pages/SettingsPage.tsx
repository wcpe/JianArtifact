// 设置页（FR-87，仅管理员）：只读展示网络代理（FR-84）+ 在线更新（FR-85）配置，
// 并提供「检查更新 / 应用更新」入口。
//
// 数据来自后端 GET /api/v1/settings（已脱敏：代理 URL 去凭据、更新 token 仅以 has_token 暴露）。
// 配置真源为 config.toml / 环境变量，运行时不可改（守 ADR-0020），故本页对配置只读、不提供编辑；
// 唯一写动作是检查更新（GET /update/check）与应用更新（POST /update/apply，二次确认后触发）。

import { useEffect, useState } from 'react';
import {
  Stack,
  Title,
  Text,
  Card,
  Group,
  Badge,
  Button,
  Loader,
  Center,
  Divider,
  Table,
  Code,
  Modal,
  Alert,
} from '@mantine/core';
import { useDisclosure } from '@mantine/hooks';
import { IconRefresh, IconArrowUp, IconInfoCircle } from '@tabler/icons-react';
import * as api from '../api/endpoints';
import type { SettingsView, UpdateCheck } from '../api/types';
import { errorMessage } from '../lib/format';
import { ErrorAlert } from '../components/ErrorAlert';

/** 只读字段行：标签 + 值（值缺省时展示「未配置」）。 */
function FieldRow({ label, value }: { label: string; value: React.ReactNode }) {
  return (
    <Table.Tr>
      <Table.Td style={{ width: 180, whiteSpace: 'nowrap' }}>
        <Text c="dimmed" size="sm">
          {label}
        </Text>
      </Table.Td>
      <Table.Td>{value}</Table.Td>
    </Table.Tr>
  );
}

/** 展示一个可空字符串：有值用等宽 Code，无值展示灰色「未配置」。 */
function OptionalValue({ value }: { value: string | null }) {
  if (!value) {
    return (
      <Text c="dimmed" size="sm">
        未配置
      </Text>
    );
  }
  return <Code>{value}</Code>;
}

/** 设置页。 */
export function SettingsPage() {
  const [settings, setSettings] = useState<SettingsView | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  // 更新检查 / 应用相关状态
  const [check, setCheck] = useState<UpdateCheck | null>(null);
  const [checking, setChecking] = useState(false);
  const [checkError, setCheckError] = useState<string | null>(null);
  const [applying, setApplying] = useState(false);
  const [applyError, setApplyError] = useState<string | null>(null);
  const [restarting, setRestarting] = useState(false);
  const [confirmOpened, confirmModal] = useDisclosure(false);

  useEffect(() => {
    api
      .getSettings()
      .then(setSettings)
      .catch((err) => setError(errorMessage(err)))
      .finally(() => setLoading(false));
  }, []);

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

  const proxy = settings.network_proxy;
  const update = settings.update;

  async function handleCheck() {
    setChecking(true);
    setCheckError(null);
    setCheck(null);
    try {
      const result = await api.checkUpdate();
      setCheck(result);
    } catch (err) {
      setCheckError(errorMessage(err));
    } finally {
      setChecking(false);
    }
  }

  async function handleApply() {
    setApplying(true);
    setApplyError(null);
    try {
      await api.applyUpdate();
      // apply 成功即服务将停机重启，当前连接会断；进入「正在重启」提示态、引导手动刷新
      confirmModal.close();
      setRestarting(true);
    } catch (err) {
      setApplyError(errorMessage(err));
      confirmModal.close();
    } finally {
      setApplying(false);
    }
  }

  return (
    <Stack>
      <Title order={2}>设置</Title>
      <Text c="dimmed">
        网络代理与在线更新配置；配置真源为 config.toml / 环境变量，运行时只读不可改。
      </Text>
      {error && <ErrorAlert message={error} />}

      {/* —— 网络代理 —— */}
      <Card withBorder padding="lg" radius="md">
        <Title order={4}>网络代理</Title>
        <Text size="sm" c="dimmed" mb="sm">
          统一出站代理配置（已脱敏，不展示任何凭据）。配置真源为 config.toml /
          环境变量，运行时不可在此修改。
        </Text>
        <Table>
          <Table.Tbody>
            <FieldRow label="HTTP 代理" value={<OptionalValue value={proxy.http} />} />
            <FieldRow label="HTTPS 代理" value={<OptionalValue value={proxy.https} />} />
            <FieldRow
              label="直连绕过（no_proxy）"
              value={<OptionalValue value={proxy.no_proxy} />}
            />
          </Table.Tbody>
        </Table>
      </Card>

      {/* —— 在线更新 —— */}
      <Card withBorder padding="lg" radius="md">
        <Title order={4}>在线更新</Title>
        <Text size="sm" c="dimmed" mb="sm">
          管理员手动触发的自更新。是否启用、仓库源等真源为 config.toml / 环境变量，运行时只读。
        </Text>
        <Table>
          <Table.Tbody>
            <FieldRow
              label="状态"
              value={
                update.enabled ? (
                  <Badge color="green">已启用</Badge>
                ) : (
                  <Badge color="gray">未启用</Badge>
                )
              }
            />
            <FieldRow label="仓库源" value={<Code>{update.repo}</Code>} />
            <FieldRow label="API 基址" value={<Code>{update.api_base_url}</Code>} />
            <FieldRow label="重启模式" value={<Code>{update.restart_mode}</Code>} />
            <FieldRow
              label="访问令牌"
              value={
                update.has_token ? (
                  <Badge color="blue">已配置</Badge>
                ) : (
                  <Text c="dimmed" size="sm">
                    未配置
                  </Text>
                )
              }
            />
            <FieldRow label="当前版本" value={<Code>{settings.current_version}</Code>} />
          </Table.Tbody>
        </Table>

        <Divider my="md" />

        {!update.enabled && (
          <Alert
            icon={<IconInfoCircle size={16} />}
            color="gray"
            variant="light"
            title="在线更新未启用"
          >
            在线更新出站开关默认关闭。如需使用，请在 config.toml 的 [update]
            段或环境变量中开启后重启服务。
          </Alert>
        )}

        {restarting ? (
          <Alert
            icon={<IconInfoCircle size={16} />}
            color="blue"
            variant="light"
            title="已触发升级"
          >
            服务正在重启…当前连接将断开，请稍候片刻后手动刷新页面。
          </Alert>
        ) : (
          <Stack gap="sm">
            <Group>
              <Button
                leftSection={<IconRefresh size={16} />}
                onClick={handleCheck}
                loading={checking}
                disabled={!update.enabled}
              >
                检查更新
              </Button>
              {check?.update_available && (
                <Button
                  color="orange"
                  leftSection={<IconArrowUp size={16} />}
                  onClick={confirmModal.open}
                >
                  升级到 v{check.latest_version}
                </Button>
              )}
            </Group>

            {checkError && <ErrorAlert message={checkError} />}
            {applyError && <ErrorAlert message={applyError} />}

            {check && (
              <Card withBorder padding="md" radius="sm" bg="var(--mantine-color-gray-0)">
                <Group gap="xs">
                  <Text size="sm">当前版本</Text>
                  <Code>{check.current_version}</Code>
                  <Text size="sm">最新版本</Text>
                  <Code>{check.latest_version}</Code>
                  {check.update_available ? (
                    <Badge color="orange">有可用更新</Badge>
                  ) : (
                    <Badge color="green">已是最新</Badge>
                  )}
                </Group>
                {check.notes && (
                  <Text size="sm" c="dimmed" mt="sm" style={{ whiteSpace: 'pre-wrap' }}>
                    {check.notes}
                  </Text>
                )}
              </Card>
            )}
          </Stack>
        )}
      </Card>

      {/* —— 升级二次确认弹窗 —— */}
      <Modal opened={confirmOpened} onClose={confirmModal.close} title="确认升级到新版本" centered>
        <Stack>
          <Text>
            将升级到 <Code>v{check?.latest_version}</Code>
            。升级成功后服务会立即重启，当前连接将断开。确认继续？
          </Text>
          <Group justify="flex-end">
            <Button variant="default" onClick={confirmModal.close} disabled={applying}>
              取消
            </Button>
            <Button color="orange" onClick={handleApply} loading={applying}>
              确认升级
            </Button>
          </Group>
        </Stack>
      </Modal>
    </Stack>
  );
}
