// Token 管理界面（FR-21）：自助签发 / 列表 / 吊销 API Token。
// 签发时一次性显示明文，提示用户立即保存（此后服务端只存哈希、不可再得）。

import { useEffect, useState } from 'react';
import {
  Table,
  Button,
  Group,
  Title,
  Stack,
  Badge,
  Modal,
  TextInput,
  Text,
  Loader,
  Center,
  Alert,
  CopyButton,
  Code,
} from '@mantine/core';
import { useDisclosure } from '@mantine/hooks';
import { IconPlus, IconCopy, IconCheck, IconAlertTriangle } from '@tabler/icons-react';
import * as api from '../api/endpoints';
import type { CreateTokenResponse, TokenView } from '../api/types';
import { errorMessage } from '../lib/format';
import { notifyError, notifySuccess } from '../lib/notify';
import { ErrorAlert } from '../components/ErrorAlert';

/** Token 管理页面。 */
export function TokensPage() {
  const [tokens, setTokens] = useState<TokenView[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [createOpened, createModal] = useDisclosure(false);
  // 新签发的明文 Token（仅本次可见）；展示在专门弹窗中
  const [issued, setIssued] = useState<CreateTokenResponse | null>(null);

  const reload = () => {
    setLoading(true);
    api
      .listTokens()
      .then(setTokens)
      .catch((err) => setError(errorMessage(err)))
      .finally(() => setLoading(false));
  };

  useEffect(reload, []);

  const handleRevoke = async (token: TokenView) => {
    if (!window.confirm(`确认吊销 Token「${token.name}」？吊销后立即失效。`)) return;
    try {
      await api.revokeToken(token.id);
      notifySuccess('Token 已吊销');
      reload();
    } catch (err) {
      notifyError(errorMessage(err));
    }
  };

  if (loading) {
    return (
      <Center h={200}>
        <Loader />
      </Center>
    );
  }

  return (
    <Stack>
      <Group justify="space-between">
        <Title order={2}>Token 管理</Title>
        <Button leftSection={<IconPlus size={16} />} onClick={createModal.open}>
          签发 Token
        </Button>
      </Group>
      <Text c="dimmed" size="sm">
        API Token 供 CLI 与包管理器客户端鉴权使用；明文仅在签发时显示一次，请妥善保存。
      </Text>
      {error && <ErrorAlert message={error} />}

      {tokens.length === 0 ? (
        <Text c="dimmed">暂无 Token。</Text>
      ) : (
        <Table.ScrollContainer minWidth={620}>
          <Table striped highlightOnHover>
            <Table.Thead>
              <Table.Tr>
                <Table.Th>名称</Table.Th>
                <Table.Th>创建时间</Table.Th>
                <Table.Th>最近使用</Table.Th>
                <Table.Th>状态</Table.Th>
                <Table.Th>操作</Table.Th>
              </Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {tokens.map((token) => (
                <Table.Tr key={token.id}>
                  <Table.Td>{token.name}</Table.Td>
                  <Table.Td>
                    <Text size="sm" c="dimmed">
                      {token.created_at}
                    </Text>
                  </Table.Td>
                  <Table.Td>
                    <Text size="sm" c="dimmed">
                      {token.last_used_at ?? '从未使用'}
                    </Text>
                  </Table.Td>
                  <Table.Td>
                    <Badge color={token.revoked ? 'gray' : 'green'} variant="light">
                      {token.revoked ? '已吊销' : '有效'}
                    </Badge>
                  </Table.Td>
                  <Table.Td>
                    <Button
                      size="xs"
                      variant="default"
                      color="red"
                      disabled={token.revoked}
                      onClick={() => handleRevoke(token)}
                    >
                      吊销
                    </Button>
                  </Table.Td>
                </Table.Tr>
              ))}
            </Table.Tbody>
          </Table>
        </Table.ScrollContainer>
      )}

      <CreateTokenModal
        opened={createOpened}
        onClose={createModal.close}
        onCreated={(resp) => {
          createModal.close();
          setIssued(resp);
          reload();
        }}
      />

      <IssuedTokenModal token={issued} onClose={() => setIssued(null)} />
    </Stack>
  );
}

/** 签发 Token 弹窗。 */
function CreateTokenModal({
  opened,
  onClose,
  onCreated,
}: {
  opened: boolean;
  onClose: () => void;
  onCreated: (resp: CreateTokenResponse) => void;
}) {
  const [name, setName] = useState('');
  const [submitting, setSubmitting] = useState(false);

  const handleSubmit = async () => {
    setSubmitting(true);
    try {
      const resp = await api.createToken(name);
      setName('');
      onCreated(resp);
    } catch (err) {
      notifyError(errorMessage(err));
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <Modal opened={opened} onClose={onClose} title="签发 API Token" centered>
      <Stack>
        <TextInput
          label="名称"
          placeholder="如 ci-pipeline"
          value={name}
          onChange={(e) => setName(e.currentTarget.value)}
          required
        />
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>
            取消
          </Button>
          <Button onClick={handleSubmit} loading={submitting} disabled={!name}>
            签发
          </Button>
        </Group>
      </Stack>
    </Modal>
  );
}

/** 展示新签发明文 Token 的弹窗（仅本次可见）。 */
function IssuedTokenModal({
  token,
  onClose,
}: {
  token: CreateTokenResponse | null;
  onClose: () => void;
}) {
  return (
    <Modal
      opened={token !== null}
      onClose={onClose}
      title="Token 已签发"
      centered
      closeOnClickOutside={false}
    >
      {token && (
        <Stack>
          <Alert icon={<IconAlertTriangle size={16} />} color="yellow" variant="light">
            请立即复制并妥善保存。该明文仅显示这一次，关闭后将无法再次查看。
          </Alert>
          <Text size="sm" fw={600}>
            {token.name}
          </Text>
          <Code block>{token.token}</Code>
          <Group justify="flex-end">
            <CopyButton value={token.token}>
              {({ copied, copy }) => (
                <Button
                  leftSection={copied ? <IconCheck size={16} /> : <IconCopy size={16} />}
                  color={copied ? 'green' : undefined}
                  onClick={copy}
                >
                  {copied ? '已复制' : '复制 Token'}
                </Button>
              )}
            </CopyButton>
            <Button variant="default" onClick={onClose}>
              我已保存
            </Button>
          </Group>
        </Stack>
      )}
    </Modal>
  );
}
