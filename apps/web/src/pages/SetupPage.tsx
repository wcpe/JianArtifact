// 首次初始化页：实例空库（userCount=0）时的专属引导，分三步 欢迎 → 创建管理员 → 完成。
// 已初始化实例访问此页会被重定向回登录页。成功创建后进入控制台。
import {
  Alert,
  Box,
  Button,
  Card,
  Center,
  Group,
  PasswordInput,
  Progress,
  Stack,
  Stepper,
  Text,
  TextInput,
  ThemeIcon,
  Title,
} from "@mantine/core";
import { useForm } from "@mantine/form";
import { IconArrowRight, IconCircleCheck, IconInfoCircle } from "@tabler/icons-react";
import { useState } from "react";
import { useTranslation } from "react-i18next";
import { Navigate, useNavigate } from "react-router-dom";

import { ApiError } from "../api/client";
import { getStatus } from "../api/endpoints";
import { useAuth } from "../auth/AuthContext";
import { BrandLogo } from "../components/BrandLogo";
import { useAsync } from "../hooks/useAsync";
import { LoadingState } from "@jianartifact/ui";

interface FormValues {
  username: string;
  password: string;
  confirmPassword: string;
}

/** 口令强度评分（0–100）：长度达标为主，数字 / 大小写混合 / 符号各加成。 */
function passwordStrength(pw: string): number {
  let score = 0;
  if (pw.length >= 8) score += 40;
  if (/[0-9]/.test(pw)) score += 20;
  if (/[a-z]/.test(pw) && /[A-Z]/.test(pw)) score += 20;
  if (/[^A-Za-z0-9]/.test(pw)) score += 20;
  return Math.min(score, 100);
}

export function SetupPage() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const { bootstrap } = useAuth();
  const status = useAsync(getStatus, []);
  const [active, setActive] = useState(0);
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const form = useForm<FormValues>({
    initialValues: { username: "", password: "", confirmPassword: "" },
    validate: {
      username: (v) => (v.trim().length > 0 ? null : t("auth.username")),
      password: (v) => (v.length >= 8 ? null : t("auth.passwordRule")),
      confirmPassword: (v, values) => (v === values.password ? null : t("setup.passwordMismatch")),
    },
  });

  if (status.loading) {
    return <LoadingState message={t("common.loading")} />;
  }

  // 已初始化实例不应停留在初始化页：回登录页。
  if (status.data && status.data.userCount > 0) {
    return <Navigate to="/login" replace />;
  }

  const strength = passwordStrength(form.values.password);
  const strengthColor = strength < 50 ? "red" : strength < 80 ? "yellow" : "green";
  const strengthLabel =
    strength < 50
      ? t("setup.strengthWeak")
      : strength < 80
        ? t("setup.strengthMedium")
        : t("setup.strengthStrong");

  const handleSubmit = form.onSubmit((values) => {
    setSubmitting(true);
    setError(null);
    bootstrap(values.username, values.password)
      .then(() => {
        setActive(2);
      })
      .catch((err: unknown) => {
        setError(err instanceof ApiError ? err.message : String(err));
      })
      .finally(() => {
        setSubmitting(false);
      });
  });

  return (
    <Center mih="100vh" p="md">
      <Card withBorder shadow="md" radius="md" p="xl" w={560} maw="100%">
        <Stack gap="lg">
          <Group gap="sm">
            <BrandLogo size={32} />
            <Title order={3}>{t("setup.title")}</Title>
          </Group>

          <Stepper active={active} size="sm" allowNextStepsSelect={false}>
            {/* 第一步：欢迎与说明。 */}
            <Stepper.Step label={t("setup.stepWelcome")}>
              <Stack gap="md" mt="md">
                <Title order={4}>{t("setup.welcomeTitle")}</Title>
                <Text c="dimmed" size="sm">
                  {t("setup.welcomeDesc")}
                </Text>
                <Alert icon={<IconInfoCircle size={16} />} color="blue" variant="light">
                  {t("setup.welcomeNote")}
                </Alert>
                <Group justify="flex-end">
                  <Button rightSection={<IconArrowRight size={16} />} onClick={() => setActive(1)}>
                    {t("setup.start")}
                  </Button>
                </Group>
              </Stack>
            </Stepper.Step>

            {/* 第二步：创建管理员。 */}
            <Stepper.Step label={t("setup.stepAdmin")}>
              <form onSubmit={handleSubmit}>
                <Stack gap="sm" mt="md">
                  <Text c="dimmed" size="sm">
                    {t("setup.adminDesc")}
                  </Text>
                  {error ? (
                    <Alert color="red" variant="light">
                      {error}
                    </Alert>
                  ) : null}
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
                  {form.values.password ? (
                    <Box>
                      <Group justify="space-between" mb={4}>
                        <Text size="xs" c="dimmed">
                          {t("setup.strength")}
                        </Text>
                        <Text size="xs" c={strengthColor}>
                          {strengthLabel}
                        </Text>
                      </Group>
                      <Progress value={strength} color={strengthColor} size="sm" />
                    </Box>
                  ) : null}
                  <PasswordInput
                    label={t("setup.confirmPassword")}
                    withAsterisk
                    {...form.getInputProps("confirmPassword")}
                  />
                  <Group justify="space-between" mt="sm">
                    <Button variant="default" onClick={() => setActive(0)}>
                      {t("setup.back")}
                    </Button>
                    <Button type="submit" loading={submitting}>
                      {t("setup.submit")}
                    </Button>
                  </Group>
                </Stack>
              </form>
            </Stepper.Step>

            {/* 第三步：完成。 */}
            <Stepper.Step label={t("setup.stepDone")}>
              <Stack gap="md" mt="md" align="center">
                <ThemeIcon color="green" variant="light" size={64} radius="xl">
                  <IconCircleCheck size={40} />
                </ThemeIcon>
                <Title order={4}>{t("setup.doneTitle")}</Title>
                <Text c="dimmed" size="sm" ta="center">
                  {t("setup.doneDesc")}
                </Text>
                <Button onClick={() => navigate("/dashboard", { replace: true })}>
                  {t("setup.enterConsole")}
                </Button>
              </Stack>
            </Stepper.Step>
          </Stepper>
        </Stack>
      </Card>
    </Center>
  );
}
