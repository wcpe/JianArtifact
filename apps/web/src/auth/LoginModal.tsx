// FR-67 登录模态框：取代整页 /login。任意位置经 useLoginModal().openLogin() 弹出，
// 登录成功后关闭并停留当前页；取消时执行调用方传入的 onCancel（如受保护页回落仓库列表）。
import { Alert, Button, Group, Modal, PasswordInput, Stack, Text, TextInput } from "@mantine/core";
import { useForm } from "@mantine/form";
import { IconAlertCircle } from "@tabler/icons-react";
import { createContext, useCallback, useContext, useMemo, useState } from "react";
import type { ReactNode } from "react";
import { useTranslation } from "react-i18next";

import { ApiError } from "../api/client";
import { useAuth } from "./AuthContext";

export interface OpenLoginOptions {
  /** 用户主动关闭（未登录成功）时回调；登录成功关闭不触发。 */
  onCancel?: () => void;
}

interface LoginModalContextValue {
  openLogin: (opts?: OpenLoginOptions) => void;
}

const LoginModalContext = createContext<LoginModalContextValue | null>(null);

export function LoginModalProvider({ children }: { children: ReactNode }) {
  const { t } = useTranslation();
  const { login } = useAuth();
  const [opened, setOpened] = useState(false);
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);
  // onCancel 用函数状态持有（setState 传入函数会被当 updater，需再包一层）。
  const [onCancel, setOnCancel] = useState<(() => void) | null>(null);

  const form = useForm({
    initialValues: { username: "", password: "" },
    validate: {
      username: (v) => (v.trim().length > 0 ? null : t("auth.username")),
      password: (v) => (v.length >= 8 ? null : t("auth.passwordRule")),
    },
  });

  const openLogin = useCallback(
    (opts?: OpenLoginOptions) => {
      setOnCancel(() => opts?.onCancel ?? null);
      setError(null);
      form.reset();
      setOpened(true);
    },
    // form 实例引用稳定（Mantine useForm），不列入依赖以保持 openLogin 稳定。
    [],
  );

  // 用户主动关闭：触发调用方的取消回调（如回落 /repositories）。
  const handleCancel = () => {
    setOpened(false);
    onCancel?.();
  };

  const handleSubmit = form.onSubmit((values) => {
    setSubmitting(true);
    setError(null);
    login(values.username, values.password)
      .then(() => {
        // 登录成功：仅关闭，停留当前页（FR-67 取消强制跳转）。
        setOpened(false);
      })
      .catch((err: unknown) => {
        setError(err instanceof ApiError ? err.message : String(err));
      })
      .finally(() => {
        setSubmitting(false);
      });
  });

  const value = useMemo<LoginModalContextValue>(() => ({ openLogin }), [openLogin]);

  return (
    <LoginModalContext.Provider value={value}>
      {children}
      <Modal opened={opened} onClose={handleCancel} title={t("auth.loginTitle")} centered>
        <Stack>
          <Text c="dimmed" size="sm">
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
                data-autofocus
                {...form.getInputProps("username")}
              />
              <PasswordInput
                label={t("auth.password")}
                withAsterisk
                {...form.getInputProps("password")}
              />
              <Group justify="flex-end">
                <Button variant="default" onClick={handleCancel}>
                  {t("common.cancel")}
                </Button>
                <Button type="submit" loading={submitting}>
                  {t("auth.login")}
                </Button>
              </Group>
            </Stack>
          </form>
        </Stack>
      </Modal>
    </LoginModalContext.Provider>
  );
}

/** 读取登录模态框上下文；必须在 LoginModalProvider 内使用。 */
export function useLoginModal(): LoginModalContextValue {
  const ctx = useContext(LoginModalContext);
  if (!ctx) {
    throw new Error("useLoginModal 必须在 LoginModalProvider 内使用");
  }
  return ctx;
}
