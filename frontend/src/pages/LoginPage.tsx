// 登录页（FR-18）：用户名 + 口令登录，成功后回跳来源页或仪表盘。

import { useState, type FormEvent } from 'react';
import {
  Button,
  Card,
  Center,
  PasswordInput,
  Stack,
  Text,
  TextInput,
  Title,
  Alert,
} from '@mantine/core';
import { IconAlertCircle } from '@tabler/icons-react';
import { Navigate, useLocation, useNavigate } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { useAuth } from '../auth/useAuth';
import { errorMessage } from '../lib/format';

/** 登录页面组件。 */
export function LoginPage() {
  const { t } = useTranslation('login');
  const { user, signIn } = useAuth();
  const navigate = useNavigate();
  const location = useLocation();
  const [username, setUsername] = useState('');
  const [password, setPassword] = useState('');
  const [error, setError] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);

  // 已登录则直接进入应用
  if (user) {
    return <Navigate to="/" replace />;
  }

  const from = (location.state as { from?: string } | null)?.from ?? '/';

  const handleSubmit = async (e: FormEvent) => {
    e.preventDefault();
    setError(null);
    setSubmitting(true);
    try {
      await signIn(username, password);
      navigate(from, { replace: true });
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <Center h="100vh">
      <Card shadow="md" padding="xl" radius="md" withBorder w={380}>
        <Stack>
          <Title order={2} ta="center">
            {t('title')}
          </Title>
          <Text c="dimmed" size="sm" ta="center">
            {t('subtitle')}
          </Text>
          {error && (
            <Alert icon={<IconAlertCircle size={16} />} color="red" variant="light">
              {error}
            </Alert>
          )}
          <form onSubmit={handleSubmit}>
            <Stack>
              <TextInput
                label={t('username')}
                placeholder={t('usernamePlaceholder')}
                value={username}
                onChange={(e) => setUsername(e.currentTarget.value)}
                required
                autoFocus
              />
              <PasswordInput
                label={t('password')}
                placeholder={t('passwordPlaceholder')}
                value={password}
                onChange={(e) => setPassword(e.currentTarget.value)}
                required
              />
              <Button type="submit" loading={submitting} fullWidth>
                {t('submit')}
              </Button>
            </Stack>
          </form>
        </Stack>
      </Card>
    </Center>
  );
}
