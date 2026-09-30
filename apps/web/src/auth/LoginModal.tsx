// FR-67 登录模态框：取代整页 /login。任意位置经 useLoginModal().openLogin() 弹出，
// 登录成功后关闭并停留当前页；取消时执行调用方传入的 onCancel（如受保护页回落仓库列表）。
import { Alert, Button, Group, Modal, PasswordInput, Stack, Text, TextInput } from "@mantine/core";
import { useForm } from "@mantine/form";
import { IconAlertCircle, IconKey, IconLogin } from "@tabler/icons-react";
import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
} from "react";
import type { ReactNode } from "react";
import { useTranslation } from "react-i18next";

import { ApiError } from "../api/client";
import * as api from "../api/endpoints";
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
  const { login, oidcError, clearOidcError } = useAuth();
  const [opened, setOpened] = useState(false);
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [oidcEnabled, setOidcEnabled] = useState(false);
  // onCancel 用函数状态持有（setState 传入函数会被当 updater，需再包一层）。
  const [onCancel, setOnCancel] = useState<(() => void) | null>(null);

  // FR-34：仅在打开登录框时查询一次 OIDC 是否启用（未配置时后端端点为 404）；
  // 用 ref 做一次性守卫——若用 state 标记，effect 会因依赖变化重跑并把在途结果丢弃。
  const oidcFetchedRef = useRef(false);
  useEffect(() => {
    if (!opened || oidcFetchedRef.current) {
      return;
    }
    oidcFetchedRef.current = true;
    api
      .getStatus()
      .then((status) => setOidcEnabled(status.oidcEnabled))
      .catch(() => setOidcEnabled(false));
  }, [opened]);

  // FR-34：OIDC 回调失败时自动弹出登录框并展示对应提示。
  useEffect(() => {
    if (oidcError) {
      setError(t(oidcError));
      setOpened(true);
      clearOidcError();
    }
  }, [oidcError, clearOidcError, t]);

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
                <Button type="submit" loading={submitting} leftSection={<IconLogin size={16} />}>
                  {t("auth.login")}
                </Button>
              </Group>
            </Stack>
          </form>
          {oidcEnabled ? (
            // 该入口是顶层跳转（浏览器离开本页），不能用 fetch，故用链接按钮。
            <Button
              component="a"
              href="/api/v1/auth/oidc/start"
              variant="light"
              fullWidth
              leftSection={<IconKey size={16} />}
            >
              {t("auth.oidcLogin")}
            </Button>
          ) : null}
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
