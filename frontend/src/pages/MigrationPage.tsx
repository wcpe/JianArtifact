// Nexus 迁移管理页面（FR-81，对接 ADR-0006 已有迁移端点，仅 Admin）。
//
// 多步流程（Mantine Stepper）：
//   ① 选迁移形态（在线 REST / 离线 blob store）+ 填源 → 预览可迁移仓库列表（不搬运）。
//   ② 勾选要搬运的仓库 + 填离线 blob store 路径（制品本体来源）→ 执行 proxy / hosted 搬运。
//   ③ 展示迁移报告（每仓库已迁 / 跳过数、整仓跳过列表）。
//
// 凭据脱敏（红线）：源 Nexus 凭据仅以「引用名 auth_ref」形式输入，用口令型输入框承载、
// 真值在后端 env 解析，前端绝不回显明文、不持久化。

import { useState } from 'react';
import {
  Stack,
  Title,
  Text,
  Stepper,
  SegmentedControl,
  TextInput,
  PasswordInput,
  Button,
  Group,
  Table,
  Checkbox,
  Badge,
  Card,
  Loader,
  Center,
} from '@mantine/core';
import * as api from '../api/endpoints';
import type { MigrationReport, NexusRepoSummary, OfflineRepoSummary } from '../api/types';
import { errorMessage } from '../lib/format';
import { ErrorAlert } from '../components/ErrorAlert';
import { notifySuccess } from '../lib/notify';

/** 迁移形态：在线 REST 入口 / 离线 blob store 入口。 */
type SourceMode = 'online' | 'offline';

/** 搬运目标类型：proxy 仓库 / hosted 仓库。 */
type MigrateKind = 'proxy' | 'hosted';

/** 预览到的仓库名集合（在线与离线归一为「仓库名 + 类型/计数」用于展示与勾选）。 */
interface PreviewRow {
  /** 仓库名（在线取 name，离线取 repo_name）。 */
  name: string;
  /** 在线：格式；离线：'-'。 */
  format: string;
  /** 在线：hosted/proxy；离线：blob 数量文案。 */
  detail: string;
}

/** 把在线预览结果归一为展示行。 */
function fromOnline(repos: NexusRepoSummary[]): PreviewRow[] {
  return repos.map((r) => ({ name: r.name, format: r.format, detail: r.type }));
}

/** 把离线预览结果归一为展示行。 */
function fromOffline(repos: OfflineRepoSummary[]): PreviewRow[] {
  return repos.map((r) => ({
    name: r.repo_name,
    format: '-',
    detail: `${r.blob_count} 个 blob`,
  }));
}

/** Nexus 迁移管理页面。 */
export function MigrationPage() {
  const [active, setActive] = useState(0);

  // —— 源配置（步骤 ①）——
  const [mode, setMode] = useState<SourceMode>('online');
  const [baseUrl, setBaseUrl] = useState('');
  const [authRef, setAuthRef] = useState('');
  const [offlinePath, setOfflinePath] = useState('');

  // —— 预览结果（步骤 ①→②）——
  const [rows, setRows] = useState<PreviewRow[]>([]);
  const [previewing, setPreviewing] = useState(false);
  const [previewError, setPreviewError] = useState<string | null>(null);

  // —— 勾选与搬运（步骤 ②）——
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [migratePath, setMigratePath] = useState('');
  const [migrating, setMigrating] = useState(false);
  const [migrateError, setMigrateError] = useState<string | null>(null);

  // —— 报告（步骤 ③）——
  const [report, setReport] = useState<MigrationReport | null>(null);

  /** auth_ref 为空白时按未提供处理（匿名源）。 */
  const authRefValue = authRef.trim() === '' ? undefined : authRef.trim();

  /** 执行预览：据形态调用在线 / 离线预览端点，归一展示行。 */
  const handlePreview = async () => {
    setPreviewError(null);
    setPreviewing(true);
    try {
      if (mode === 'online') {
        const repos = await api.previewNexusRepositories({
          base_url: baseUrl.trim(),
          auth_ref: authRefValue,
        });
        setRows(fromOnline(repos));
      } else {
        const repos = await api.previewNexusOffline({ path: offlinePath.trim() });
        setRows(fromOffline(repos));
      }
      setSelected(new Set());
    } catch (err) {
      setPreviewError(errorMessage(err));
    } finally {
      setPreviewing(false);
    }
  };

  /** 切换单个仓库勾选。 */
  const toggleSelect = (name: string) => {
    setSelected((prev) => {
      const next = new Set(prev);
      if (next.has(name)) {
        next.delete(name);
      } else {
        next.add(name);
      }
      return next;
    });
  };

  /** 执行搬运：按目标类型调用 proxy / hosted 搬运端点，得到报告并进入步骤 ③。 */
  const handleMigrate = async (kind: MigrateKind) => {
    setMigrateError(null);
    setMigrating(true);
    try {
      const req = {
        base_url: baseUrl.trim(),
        auth_ref: authRefValue,
        offline_path: migratePath.trim(),
      };
      const result =
        kind === 'proxy' ? await api.migrateNexusProxy(req) : await api.migrateNexusHosted(req);
      setReport(result);
      notifySuccess('迁移已完成，请查看报告');
      setActive(2);
    } catch (err) {
      setMigrateError(errorMessage(err));
    } finally {
      setMigrating(false);
    }
  };

  // 在线形态搬运需源地址；两形态搬运均需离线路径提供制品本体来源
  const canMigrate =
    selected.size > 0 && migratePath.trim() !== '' && baseUrl.trim() !== '' && !migrating;
  const canPreview =
    (mode === 'online' ? baseUrl.trim() !== '' : offlinePath.trim() !== '') && !previewing;

  return (
    <Stack>
      <Title order={2}>Nexus 迁移</Title>
      <Text c="dimmed">
        从源 Nexus OSS 迁移仓库与制品：在线 REST 或离线 blob store 预览 → 勾选 → 执行 → 查看报告。源
        Nexus 凭据仅以引用名提供，明文不入库、不回显。
      </Text>

      <Stepper active={active} onStepClick={setActive}>
        <Stepper.Step label="选源与预览" description="填源地址或离线路径">
          <Stack mt="md">
            <SegmentedControl
              value={mode}
              onChange={(v) => setMode(v as SourceMode)}
              data={[
                { label: '在线（REST API）', value: 'online' },
                { label: '离线（blob store）', value: 'offline' },
              ]}
            />

            {mode === 'online' ? (
              <>
                <TextInput
                  label="源 Nexus 地址"
                  placeholder="https://nexus.example"
                  value={baseUrl}
                  onChange={(e) => setBaseUrl(e.currentTarget.value)}
                  required
                />
                <PasswordInput
                  label="凭据引用（auth_ref，可选）"
                  description="仅填引用名；真实凭据由后端 env 解析，明文不入库、不回显。匿名源可留空。"
                  placeholder="例如 NEXUS_SRC"
                  value={authRef}
                  onChange={(e) => setAuthRef(e.currentTarget.value)}
                />
              </>
            ) : (
              <TextInput
                label="离线 blob store 路径"
                description="服务进程可访问的本地 Nexus 文件型 blob store 根目录。"
                placeholder="/data/nexus/blobs/default"
                value={offlinePath}
                onChange={(e) => setOfflinePath(e.currentTarget.value)}
                required
              />
            )}

            {previewError && <ErrorAlert message={previewError} />}

            <Group>
              <Button onClick={handlePreview} disabled={!canPreview} loading={previewing}>
                预览仓库
              </Button>
              {rows.length > 0 && (
                <Button variant="default" onClick={() => setActive(1)}>
                  下一步：勾选执行
                </Button>
              )}
            </Group>

            {previewing ? (
              <Center h={120}>
                <Loader />
              </Center>
            ) : (
              rows.length > 0 && (
                <Card withBorder padding="md" radius="md">
                  <Text fw={600} mb="sm">
                    可迁移仓库（{rows.length}）
                  </Text>
                  <Table.ScrollContainer minWidth={420}>
                    <Table striped highlightOnHover>
                      <Table.Thead>
                        <Table.Tr>
                          <Table.Th>仓库</Table.Th>
                          <Table.Th>格式</Table.Th>
                          <Table.Th>类型 / 内容</Table.Th>
                        </Table.Tr>
                      </Table.Thead>
                      <Table.Tbody>
                        {rows.map((row) => (
                          <Table.Tr key={row.name}>
                            <Table.Td>{row.name}</Table.Td>
                            <Table.Td>{row.format}</Table.Td>
                            <Table.Td>{row.detail}</Table.Td>
                          </Table.Tr>
                        ))}
                      </Table.Tbody>
                    </Table>
                  </Table.ScrollContainer>
                </Card>
              )
            )}
          </Stack>
        </Stepper.Step>

        <Stepper.Step label="勾选执行" description="选仓库并搬运">
          <Stack mt="md">
            {rows.length === 0 ? (
              <Text c="dimmed">请先在上一步预览仓库。</Text>
            ) : (
              <>
                <Card withBorder padding="md" radius="md">
                  <Text fw={600} mb="sm">
                    勾选要搬运的仓库（已选 {selected.size}）
                  </Text>
                  <Stack gap="xs">
                    {rows.map((row) => (
                      <Checkbox
                        key={row.name}
                        label={`${row.name}（${row.format} / ${row.detail}）`}
                        checked={selected.has(row.name)}
                        onChange={() => toggleSelect(row.name)}
                      />
                    ))}
                  </Stack>
                </Card>

                <TextInput
                  label="离线 blob store 路径（制品本体来源）"
                  description="搬运需从离线 blob store 读取制品本体，其下应含 content/ 子目录。"
                  placeholder="/data/nexus/blobs/default"
                  value={migratePath}
                  onChange={(e) => setMigratePath(e.currentTarget.value)}
                  required
                />

                {migrateError && <ErrorAlert message={migrateError} />}

                <Group>
                  <Button onClick={() => setActive(0)} variant="default">
                    上一步
                  </Button>
                  <Button
                    onClick={() => handleMigrate('proxy')}
                    disabled={!canMigrate}
                    loading={migrating}
                  >
                    执行 proxy 搬运
                  </Button>
                  <Button
                    onClick={() => handleMigrate('hosted')}
                    disabled={!canMigrate}
                    loading={migrating}
                    color="grape"
                  >
                    执行 hosted 搬运
                  </Button>
                </Group>
                <Text size="xs" c="dimmed">
                  proxy 搬运建仓 + 搬运缓存制品；hosted 搬运建仓 + 搬运完整制品。两者均按源仓库
                  类型在后端各取所需，非目标类型仓库会被跳过并列入报告。
                </Text>
              </>
            )}
          </Stack>
        </Stepper.Step>

        <Stepper.Step label="迁移报告" description="查看结果">
          <Stack mt="md">
            {!report ? (
              <Text c="dimmed">尚无迁移报告。</Text>
            ) : (
              <Card withBorder padding="md" radius="md">
                <Text fw={600} mb="sm">
                  迁移报告
                </Text>
                {report.repos.length === 0 ? (
                  <Text c="dimmed" size="sm">
                    无仓库被搬运。
                  </Text>
                ) : (
                  <Table.ScrollContainer minWidth={520}>
                    <Table striped>
                      <Table.Thead>
                        <Table.Tr>
                          <Table.Th>仓库</Table.Th>
                          <Table.Th>格式</Table.Th>
                          <Table.Th>新建仓库</Table.Th>
                          <Table.Th ta="right">已迁制品</Table.Th>
                          <Table.Th ta="right">跳过制品</Table.Th>
                        </Table.Tr>
                      </Table.Thead>
                      <Table.Tbody>
                        {report.repos.map((r) => (
                          <Table.Tr key={r.repo_name}>
                            <Table.Td>{r.repo_name}</Table.Td>
                            <Table.Td>{r.format}</Table.Td>
                            <Table.Td>
                              <Badge color={r.created ? 'green' : 'gray'} variant="light">
                                {r.created ? '是' : '已存在'}
                              </Badge>
                            </Table.Td>
                            <Table.Td ta="right">{r.migrated_artifacts}</Table.Td>
                            <Table.Td ta="right">{r.skipped_artifacts}</Table.Td>
                          </Table.Tr>
                        ))}
                      </Table.Tbody>
                    </Table>
                  </Table.ScrollContainer>
                )}

                {report.skipped_repos.length > 0 && (
                  <Group mt="sm" gap="xs">
                    <Text size="sm" c="dimmed">
                      整仓跳过（非目标类型）：
                    </Text>
                    {report.skipped_repos.map((name) => (
                      <Badge key={name} color="orange" variant="light">
                        {name}
                      </Badge>
                    ))}
                  </Group>
                )}
              </Card>
            )}
            <Group>
              <Button variant="default" onClick={() => setActive(1)}>
                返回勾选
              </Button>
            </Group>
          </Stack>
        </Stepper.Step>
      </Stepper>
    </Stack>
  );
}
