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
import { useTranslation } from 'react-i18next';
import * as api from '../api/endpoints';
import type { CreateTokenResponse, TokenView } from '../api/types';
import { errorMessage } from '../lib/format';
import { notifyError, notifySuccess } from '../lib/notify';
import { ErrorAlert } from '../components/ErrorAlert';

/** Token 管理页面。 */
export function TokensPage() {
  const { t } = useTranslation('tokens');
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
    if (!window.confirm(t('confirmRevoke', { name: token.name }))) return;
    try {
      await api.revokeToken(token.id);
      notifySuccess(t('tokenRevoked'));
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
        <Title order={2}>{t('title')}</Title>
        <Button leftSection={<IconPlus size={16} />} onClick={createModal.open}>
          {t('issue')}
        </Button>
      </Group>
      <Text c="dimmed" size="sm">
        {t('intro')}
      </Text>
      {error && <ErrorAlert message={error} />}

      {tokens.length === 0 ? (
        <Text c="dimmed">{t('empty')}</Text>
      ) : (
        <Table.ScrollContainer minWidth={620}>
          <Table striped highlightOnHover>
            <Table.Thead>
              <Table.Tr>
                <Table.Th>{t('colName')}</Table.Th>
                <Table.Th>{t('colCreatedAt')}</Table.Th>
                <Table.Th>{t('colLastUsed')}</Table.Th>
                <Table.Th>{t('colStatus')}</Table.Th>
                <Table.Th>{t('colActions')}</Table.Th>
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
                      {token.last_used_at ?? t('neverUsed')}
                    </Text>
                  </Table.Td>
                  <Table.Td>
                    <Badge color={token.revoked ? 'gray' : 'green'} variant="light">
                      {token.revoked ? t('statusRevoked') : t('statusActive')}
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
                      {t('revoke')}
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
  const { t } = useTranslation('tokens');
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
    <Modal opened={opened} onClose={onClose} title={t('createModalTitle')} centered>
      <Stack>
        <TextInput
          label={t('fieldName')}
          placeholder={t('namePlaceholder')}
          value={name}
          onChange={(e) => setName(e.currentTarget.value)}
          required
        />
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>
            {t('common:cancel')}
          </Button>
          <Button onClick={handleSubmit} loading={submitting} disabled={!name}>
            {t('issueSubmit')}
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
  const { t } = useTranslation('tokens');
  return (
    <Modal
      opened={token !== null}
      onClose={onClose}
      title={t('issuedModalTitle')}
      centered
      closeOnClickOutside={false}
    >
      {token && (
        <Stack>
          <Alert icon={<IconAlertTriangle size={16} />} color="yellow" variant="light">
            {t('issuedWarning')}
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
                  {copied ? t('copied') : t('copyToken')}
                </Button>
              )}
            </CopyButton>
            <Button variant="default" onClick={onClose}>
              {t('saved')}
            </Button>
          </Group>
        </Stack>
      )}
    </Modal>
  );
}
