// 登录页：常规凭据登录，成功后进入仪表盘。
// 若实例尚未初始化（userCount=0），重定向到专属初始化页 /setup。
import { Alert, Button, Card, Center, PasswordInput, Stack, Text, TextInput, Title } from "@mantine/core";
import { useForm } from "@mantine/form";
import { IconAlertCircle } from "@tabler/icons-react";
import { useState } from "react";
import { useTranslation } from "react-i18next";
import { Navigate, useNavigate } from "react-router-dom";

import { ApiError } from "../api/client";
import { getStatus } from "../api/endpoints";
import { useAuth } from "../auth/AuthContext";
import { useAsync } from "../hooks/useAsync";
import { LoadingState } from "@jianartifact/ui";

interface FormValues {
  username: string;
  password: string;
}

export function LoginPage() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const { login } = useAuth();
  const status = useAsync(getStatus, []);
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const form = useForm<FormValues>({
    initialValues: { username: "", password: "" },
    validate: {
      username: (v) => (v.trim().length > 0 ? null : t("auth.username")),
      password: (v) => (v.length >= 8 ? null : t("auth.passwordRule")),
    },
  });

  if (status.loading) {
    return <LoadingState message={t("common.loading")} />;
  }

  // 空库实例交由专属初始化页引导创建首个管理员。
  if (status.data && status.data.userCount === 0) {
    return <Navigate to="/setup" replace />;
  }

  const handleSubmit = form.onSubmit((values) => {
    setSubmitting(true);
    setError(null);
    login(values.username, values.password)
      .then(() => {
        navigate("/dashboard", { replace: true });
      })
      .catch((err: unknown) => {
        setError(err instanceof ApiError ? err.message : String(err));
      })
      .finally(() => {
        setSubmitting(false);
      });
  });

  return (
    <Center h="100vh" p="md">
      <Card withBorder shadow="md" radius="md" p="xl" w={380}>
        <Stack>
          <Title order={2} ta="center">
            {t("auth.loginTitle")}
          </Title>
          <Text c="dimmed" size="sm" ta="center">
            {t("auth.loginSubtitle")}
          </Text>
          {error ? (
            <Alert icon={<IconAlertCircle size={16} />} color="red" variant="light">
              {error}
            </Alert>
          ) : null}
          <form onSubmit={handleSubmit}>
            <Stack>
              <TextInput
                label={t("auth.username")}
                withAsterisk
                {...form.getInputProps("username")}
              />
              <PasswordInput
                label={t("auth.password")}
                withAsterisk
                {...form.getInputProps("password")}
              />
              <Button type="submit" loading={submitting} fullWidth>
                {t("auth.login")}
              </Button>
            </Stack>
          </form>
        </Stack>
      </Card>
    </Center>
  );
}
