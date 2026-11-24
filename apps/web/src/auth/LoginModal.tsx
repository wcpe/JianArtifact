//  登录模态框：取代整页 /login。任意位置经 useLoginModal.openLogin 弹出，
// 登录成功后关闭并停留当前页；取消时执行调用方传入的 onCancel（如受保护页回落仓库列表）。
import { Alert, Anchor, Button, Code, Group, Modal, PasswordInput, Stack, Text, TextInput } from "@mantine/core";
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
  const { login, persistSession, oidcError, clearOidcError } = useAuth();
  const [opened, setOpened] = useState(false);
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [oidcEnabled, setOidcEnabled] = useState(false);
  // onCancel 用函数状态持有（setState 传入函数会被当 updater，需再包一层）。
  const [onCancel, setOnCancel] = useState<(() => void) | null>(null);
  // 挑战流步骤：口令 →（按需）OTP / 恢复码 / 认领票 → 因子登记 → 恢复码展示。
  const [step, setStep] = useState<"credentials" | "mfa" | "recovery" | "claim" | "enroll" | "codes">("credentials");
  const [challengeToken, setChallengeToken] = useState("");
  const [enrollUri, setEnrollUri] = useState("");
  const [otp, setOtp] = useState("");
  const [recoveryCode, setRecoveryCode] = useState("");
  const [claimTicket, setClaimTicket] = useState("");
  const [recoveryCodes, setRecoveryCodes] = useState<string[]>([]);

  // 仅在打开登录框时查询一次 OIDC 是否启用（未配置时后端端点为 404）；
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

  // OIDC 回调失败时自动弹出登录框并展示对应提示。
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
      setStep("credentials");
      setChallengeToken("");
      setEnrollUri("");
      setOtp("");
      setRecoveryCode("");
      setClaimTicket("");
      setRecoveryCodes([]);
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

  const run = (work: () => Promise<void>) => {
    setSubmitting(true);
    setError(null);
    work()
      .catch((err: unknown) => {
        setError(err instanceof ApiError ? err.message : String(err));
      })
      .finally(() => setSubmitting(false));
  };

  const handleSubmit = form.onSubmit((values) => {
    run(async () => {
      const res = await login(values.username, values.password);
      if (res.next === "authenticated") {
        // 登录成功：仅关闭，停留当前页（取消强制跳转）。
        setOpened(false);
        return;
      }
      if (res.next === "mfa_required") {
        setChallengeToken(res.challengeToken ?? "");
        if (res.enrollmentUri) {
          // 恢复重绑挑战：直接展示新登记 URI 走确认。
          setEnrollUri(res.enrollmentUri);
          setStep("enroll");
        } else {
          setStep("mfa");
        }
        return;
      }
      if (res.next === "claim_required") {
        // 账号须持认领票进入因子登记引导。
        setStep("claim");
      }
    });
  });

  const handleVerifyMfa = () => {
    run(async () => {
      const res = await api.verifyLoginMfa(challengeToken, otp.trim());
      if (res.token) {
        persistSession(res.token, res.user);
        setOpened(false);
      }
    });
  };

  const handleRecover = () => {
    run(async () => {
      const en = await api.recoverLoginMfa(challengeToken, recoveryCode.trim());
      setChallengeToken(en.challengeToken);
      setEnrollUri(en.enrollmentUri);
      setStep("enroll");
    });
  };

  const handleClaim = () => {
    run(async () => {
      const en = await api.claimMfaEnroll(form.values.username, form.values.password, claimTicket.trim());
      setChallengeToken(en.challengeToken);
      setEnrollUri(en.enrollmentUri);
      setStep("enroll");
    });
  };

  const handleConfirmEnroll = () => {
    run(async () => {
      const codes = await api.confirmMfaEnroll(challengeToken, otp.trim(), claimTicket || undefined);
      setRecoveryCodes(codes.recoveryCodes);
      setStep("codes");
    });
  };

  const handleCodesDone = () => {
    // 因子已激活：回到口令步重新登录（新会话会走 mfa_required）。
    setStep("credentials");
    setOtp("");
    setChallengeToken("");
    setEnrollUri("");
    setRecoveryCodes([]);
    setError(null);
  };

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
          {step === "credentials" ? (
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
          ) : null}
          {step === "mfa" ? (
            <Stack>
              <Text size="sm">{t("auth.mfaCodeHint")}</Text>
              <TextInput
                label={t("auth.mfaCode")}
                withAsterisk
                data-autofocus
                value={otp}
                onChange={(e) => setOtp(e.currentTarget.value)}
              />
              <Anchor component="button" type="button" size="sm" onClick={() => setStep("recovery")}>
                {t("auth.useRecoveryCode")}
              </Anchor>
              <Group justify="flex-end">
                <Button variant="default" onClick={handleCancel}>
                  {t("common.cancel")}
                </Button>
                <Button onClick={handleVerifyMfa} loading={submitting} disabled={!otp.trim()}>
                  {t("auth.mfaVerify")}
                </Button>
              </Group>
            </Stack>
          ) : null}
          {step === "recovery" ? (
            <Stack>
              <Text size="sm">{t("auth.recoveryCodeHint")}</Text>
              <TextInput
                label={t("auth.recoveryCode")}
                withAsterisk
                data-autofocus
                value={recoveryCode}
                onChange={(e) => setRecoveryCode(e.currentTarget.value)}
              />
              <Group justify="flex-end">
                <Button variant="default" onClick={() => setStep("mfa")}>
                  {t("common.back")}
                </Button>
                <Button onClick={handleRecover} loading={submitting} disabled={!recoveryCode.trim()}>
                  {t("auth.recoverFactor")}
                </Button>
              </Group>
            </Stack>
          ) : null}
          {step === "claim" ? (
            <Stack>
              <Text size="sm">{t("auth.claimTicketHint")}</Text>
              <TextInput
                label={t("auth.claimTicket")}
                withAsterisk
                data-autofocus
                value={claimTicket}
                onChange={(e) => setClaimTicket(e.currentTarget.value)}
              />
              <Group justify="flex-end">
                <Button variant="default" onClick={handleCancel}>
                  {t("common.cancel")}
                </Button>
                <Button onClick={handleClaim} loading={submitting} disabled={!claimTicket.trim()}>
                  {t("auth.claimEnroll")}
                </Button>
              </Group>
            </Stack>
          ) : null}
          {step === "enroll" ? (
            <Stack>
              <Text size="sm">{t("auth.enrollUriHint")}</Text>
              <Code block>{enrollUri}</Code>
              <TextInput
                label={t("auth.mfaCode")}
                withAsterisk
                data-autofocus
                value={otp}
                onChange={(e) => setOtp(e.currentTarget.value)}
              />
              <Group justify="flex-end">
                <Button variant="default" onClick={handleCancel}>
                  {t("common.cancel")}
                </Button>
                <Button onClick={handleConfirmEnroll} loading={submitting} disabled={!otp.trim()}>
                  {t("auth.confirmEnroll")}
                </Button>
              </Group>
            </Stack>
          ) : null}
          {step === "codes" ? (
            <Stack>
              <Text size="sm">{t("auth.recoveryCodesHint")}</Text>
              <Code block>{recoveryCodes.join("\n")}</Code>
              <Group justify="flex-end">
                <Button onClick={handleCodesDone}>{t("auth.codesSaved")}</Button>
              </Group>
            </Stack>
          ) : null}
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
