// 迁移向导：选来源 → 配置 → 预览多选 → 显式 start（可视化，无 raw）。
import {
  Alert,
  Badge,
  Button,
  Card,
  Checkbox,
  Group,
  Loader,
  Progress,
  ScrollArea,
  Select,
  SimpleGrid,
  Stack,
  Stepper,
  Table,
  Text,
  TextInput,
  TagsInput,
  ThemeIcon,
  Title,
  UnstyledButton,
} from "@mantine/core";
import { useForm } from "@mantine/form";
import { PageHeader } from "@jianartifact/ui";
import {
  IconCloudDownload,
  IconDatabase,
  IconFolder,
  IconLink,
  IconPackage,
  IconPlayerPlay,
  IconRefresh,
  IconSearch,
} from "@tabler/icons-react";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { Link, useNavigate } from "react-router-dom";

import { ApiError } from "../api/client";
import {
  cancelOfflineDirIndex,
  discoverMigrations,
  getMigration,
  getOfflineDirIndex,
  listMigrations,
  listRemoteNexusRepositories,
  startMigration,
  startOfflineDirIndex,
  type OfflineDirIndexStatus,
} from "../api/endpoints";
import type {
  MigrationConflictPolicy,
  MigrationDiscoverResponse,
  MigrationSourceType,
  MigrationTask,
} from "../api/types";
import { MigrationRepoTable } from "../components/migration/MigrationRepoTable";
import { formatColor, planEstimatedAssets, sourceColor } from "../components/migration/status";
import { notifyError, notifySuccess } from "../lib/feedback";
import { density } from "../theme/density";

interface RemoteRepoItem {
  name: string;
  format: string;
  type: string;
}

const SESSION_KEY = "jianartifact.migration.wizard";
const DISCOVER_TIMEOUT_MS = 90_000;

interface WizardDraft {
  active: number;
  sourceType: MigrationSourceType;
  url: string;
  path: string;
  credentialRef: string;
  conflictPolicy: MigrationConflictPolicy;
  includeRepositories: string[];
  taskId?: number;
  selectedRepos?: string[];
}

function loadDraft(): WizardDraft | null {
  try {
    const raw = sessionStorage.getItem(SESSION_KEY);
    if (!raw) {
      return null;
    }
    return JSON.parse(raw) as WizardDraft;
  } catch {
    return null;
  }
}

function saveDraft(draft: WizardDraft): void {
  try {
    sessionStorage.setItem(SESSION_KEY, JSON.stringify(draft));
  } catch {
    /* ignore */
  }
}

function clearDraft(): void {
  try {
    sessionStorage.removeItem(SESSION_KEY);
  } catch {
    /* ignore */
  }
}

function isActiveStatus(status: string): boolean {
  return status === "planned" || status === "running";
}

const SOURCE_OPTIONS: {
  value: MigrationSourceType;
  icon: typeof IconLink;
  hintKey: "sourcePickOnline" | "sourcePickOfflineDir" | "sourcePickOfflineBundle";
}[] = [
  { value: "online_rest", icon: IconLink, hintKey: "sourcePickOnline" },
  { value: "offline_dir", icon: IconFolder, hintKey: "sourcePickOfflineDir" },
  { value: "offline_bundle", icon: IconPackage, hintKey: "sourcePickOfflineBundle" },
];

export function MigrationWizardPage() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const draft = useMemo(() => loadDraft(), []);

  const [active, setActive] = useState(draft?.active ?? 0);
  const [busy, setBusy] = useState(false);
  const [discoverPhase, setDiscoverPhase] = useState<"idle" | "checking" | "discovering" | "done">(
    "idle",
  );
  const [discoverElapsed, setDiscoverElapsed] = useState(0);
  const [discoverResult, setDiscoverResult] = useState<MigrationDiscoverResponse | null>(null);
  const [selectedRepos, setSelectedRepos] = useState<string[]>(draft?.selectedRepos ?? []);
  const [gateTasks, setGateTasks] = useState<MigrationTask[] | null>(null);
  const [gateLoading, setGateLoading] = useState(true);
  /** 离线目录：从在线 Nexus 拉到的仓库索引（不落库） */
  const [remoteRepos, setRemoteRepos] = useState<RemoteRepoItem[]>([]);
  const [remoteBusy, setRemoteBusy] = useState(false);
  const [indexUrl, setIndexUrl] = useState(draft?.url ?? "");
  /** 离线目录持久化索引状态 */
  const [offlineIdx, setOfflineIdx] = useState<OfflineDirIndexStatus | null>(null);
  const [offlineIdxBusy, setOfflineIdxBusy] = useState(false);
  const abortRef = useRef<AbortController | null>(null);
  const timerRef = useRef<number | null>(null);
  const offlineIdxPollRef = useRef<number | null>(null);

  const form = useForm({
    initialValues: {
      sourceType: (draft?.sourceType ?? "online_rest") as MigrationSourceType,
      url: draft?.url ?? "",
      path: draft?.path ?? "",
      credentialRef: draft?.credentialRef ?? "",
      conflictPolicy: (draft?.conflictPolicy ?? "skip") as MigrationConflictPolicy,
      includeRepositories: draft?.includeRepositories ?? ([] as string[]),
    },
    validate: {
      url: (v, values) =>
        values.sourceType === "online_rest" && !/^https?:\/\/.+/.test(v.trim())
          ? t("migrations.urlRequired")
          : null,
      path: (v, values) =>
        values.sourceType !== "online_rest" && !v.trim() ? t("migrations.pathRequired") : null,
    },
  });

  useEffect(() => {
    saveDraft({
      active,
      sourceType: form.values.sourceType,
      url: form.values.url,
      path: form.values.path,
      credentialRef: form.values.credentialRef,
      conflictPolicy: form.values.conflictPolicy,
      includeRepositories: form.values.includeRepositories,
      taskId: discoverResult?.taskId,
      selectedRepos,
    });
  }, [active, form.values, discoverResult?.taskId, selectedRepos]);

  const refreshGate = useCallback(() => {
    setGateLoading(true);
    listMigrations({ page_size: 100 })
      .then(async (list) => {
        const activeTasks = list.items.filter((x) => isActiveStatus(x.status));
        setGateTasks(activeTasks);

        if (draft?.taskId) {
          try {
            const task = await getMigration(draft.taskId);
            if (task.status === "planned" && task.plan) {
              setDiscoverResult({ taskId: task.id, plan: task.plan });
              setSelectedRepos(
                draft.selectedRepos?.length
                  ? draft.selectedRepos
                  : task.plan.repositories.map((r) => r.name),
              );
              setActive(2);
              setDiscoverPhase("done");
              return;
            }
            if (task.status === "running") {
              notifySuccess(t("migrations.resumeRunningRedirect"));
              navigate(`/migrations/${task.id}`, { replace: true });
              return;
            }
          } catch {
            /* ignore */
          }
        }

        const running = activeTasks.find((x) => x.status === "running");
        if (running) {
          notifyError(t("migrations.blockRunning", { id: running.id }));
          navigate(`/migrations/${running.id}`, { replace: true });
        }
      })
      .catch((e: Error) => notifyError(e.message || t("common.error")))
      .finally(() => setGateLoading(false));
  }, [draft?.taskId, draft?.selectedRepos, navigate, t]);

  useEffect(() => {
    refreshGate();
    return () => {
      abortRef.current?.abort();
      if (timerRef.current != null) {
        window.clearInterval(timerRef.current);
      }
    };
  }, [refreshGate]);

  const planRepoNames = useMemo(
    () => discoverResult?.plan.repositories.map((r) => r.name) ?? [],
    [discoverResult],
  );
  const plannedOthers = (gateTasks ?? []).filter(
    (x) => x.status === "planned" && x.id !== discoverResult?.taskId,
  );
  const firstPlanned = plannedOthers[0];
  const previewEst = planEstimatedAssets(discoverResult?.plan.repositories);

  const refreshOfflineIndex = useCallback((path: string) => {
    if (!path.trim()) {
      setOfflineIdx(null);
      return;
    }
    getOfflineDirIndex(path.trim())
      .then(setOfflineIdx)
      .catch(() => setOfflineIdx(null));
  }, []);

  // path / 离线源变化时拉索引状态；scanning 时 1s 轮询
  useEffect(() => {
    if (form.values.sourceType !== "offline_dir") {
      if (offlineIdxPollRef.current != null) {
        window.clearInterval(offlineIdxPollRef.current);
        offlineIdxPollRef.current = null;
      }
      return;
    }
    const path = form.values.path.trim();
    if (!path) {
      setOfflineIdx(null);
      return;
    }
    refreshOfflineIndex(path);
  }, [form.values.sourceType, form.values.path, refreshOfflineIndex]);

  useEffect(() => {
    if (offlineIdxPollRef.current != null) {
      window.clearInterval(offlineIdxPollRef.current);
      offlineIdxPollRef.current = null;
    }
    if (offlineIdx?.status !== "scanning") {
      return;
    }
    const path = form.values.path.trim();
    if (!path) {
      return;
    }
    offlineIdxPollRef.current = window.setInterval(() => {
      getOfflineDirIndex(path)
        .then((st) => {
          setOfflineIdx(st);
          if (st.status === "ready") {
            notifySuccess(t("migrations.offlineIndexReadyHint"));
          }
        })
        .catch(() => undefined);
    }, 1000);
    return () => {
      if (offlineIdxPollRef.current != null) {
        window.clearInterval(offlineIdxPollRef.current);
        offlineIdxPollRef.current = null;
      }
    };
  }, [offlineIdx?.status, form.values.path, t]);

  const offlineIdxScanning = offlineIdx?.status === "scanning";
  const offlineIdxActionsLocked = offlineIdxBusy || offlineIdxScanning || busy;

  const runOfflineIndexScan = (mode: "full" | "update" | "rebuild") => {
    const path = form.values.path.trim();
    if (!path) {
      notifyError(t("migrations.offlineIndexNeedPath"));
      return;
    }
    // 互斥：请求中 / 扫描中禁止再点任何建立·更新·重建
    if (offlineIdxBusy || offlineIdx?.status === "scanning") {
      notifyError(t("migrations.offlineIndexBusy"));
      return;
    }
    const st = offlineIdx?.status ?? "idle";
    // 状态门：idle/failed → 仅 full；ready → 仅 update|rebuild
    if (mode === "full" && st !== "idle" && st !== "failed") {
      return;
    }
    if (mode === "update" && st !== "ready") {
      return;
    }
    if (mode === "rebuild" && st !== "ready") {
      return;
    }
    setOfflineIdxBusy(true);
    // 乐观锁定 UI，避免连点
    setOfflineIdx((prev) =>
      prev
        ? { ...prev, status: "scanning", mode, message: t("migrations.offlineIndexStarted") }
        : {
            status: "scanning",
            mode,
            path,
            message: t("migrations.offlineIndexStarted"),
          },
    );
    startOfflineDirIndex({ path, mode })
      .then((next) => {
        setOfflineIdx(next);
        notifySuccess(t("migrations.offlineIndexStarted"));
      })
      .catch((e: Error) => {
        notifyError(e.message || t("common.error"));
        refreshOfflineIndex(path);
      })
      .finally(() => setOfflineIdxBusy(false));
  };

  const runCancelOfflineIndex = () => {
    const path = form.values.path.trim();
    if (!path) {
      return;
    }
    if (offlineIdxBusy) {
      return;
    }
    setOfflineIdxBusy(true);
    cancelOfflineDirIndex(path)
      .then(() => refreshOfflineIndex(path))
      .catch((e: Error) => notifyError(e.message || t("common.error")))
      .finally(() => setOfflineIdxBusy(false));
  };

  /** 索引是否可用于发现（ready 时允许空 include = 全部仓） */
  const offlineIndexReady =
    form.values.sourceType === "offline_dir" && offlineIdx?.status === "ready";
  const indexRepoList = offlineIdx?.repositories;
  const indexRepoNames = useMemo(() => (indexRepoList ?? []).map((r) => r.name), [indexRepoList]);
  const includeSelectedCount = form.values.includeRepositories.length;
  const offlineNeedPickRepos =
    form.values.sourceType === "offline_dir" && !offlineIndexReady && includeSelectedCount === 0;

  // 索引刚就绪（或进入页面已是 ready 且尚未勾选）时默认全选，便于一键发现
  const prevOfflineStatusRef = useRef<string | undefined>(undefined);
  const autoPickedIndexPathRef = useRef<string>("");
  useEffect(() => {
    if (form.values.sourceType !== "offline_dir") {
      return;
    }
    const st = offlineIdx?.status;
    const prev = prevOfflineStatusRef.current;
    prevOfflineStatusRef.current = st;
    if (st !== "ready") {
      return;
    }
    const names = (offlineIdx?.repositories ?? []).map((r) => r.name);
    if (names.length === 0) {
      return;
    }
    const pathKey = form.values.path.trim();
    const justBecameReady = prev !== "ready";
    const notYetAutoPicked = autoPickedIndexPathRef.current !== pathKey;
    if (justBecameReady || (notYetAutoPicked && form.values.includeRepositories.length === 0)) {
      form.setFieldValue("includeRepositories", names);
      autoPickedIndexPathRef.current = pathKey;
    }
    // form 引用稳定，仅在 status / 仓列表边沿触发（本项目 ESLint 未启用 react-hooks 规则集）
  }, [offlineIdx?.status, offlineIdx?.repositories, form.values.sourceType, form.values.path]);

  const toggleIncludeRepo = (name: string) => {
    const cur = form.values.includeRepositories;
    if (cur.includes(name)) {
      form.setFieldValue(
        "includeRepositories",
        cur.filter((n) => n !== name),
      );
    } else {
      form.setFieldValue("includeRepositories", [...cur, name]);
    }
  };

  const selectAllIndexRepos = () => {
    form.setFieldValue("includeRepositories", [...indexRepoNames]);
  };

  const selectNoneIncludeRepos = () => {
    form.setFieldValue("includeRepositories", []);
  };

  const fetchRemoteIndex = () => {
    const url = indexUrl.trim() || form.values.url.trim();
    if (!/^https?:\/\/.+/.test(url)) {
      notifyError(t("migrations.urlRequired"));
      return;
    }
    setRemoteBusy(true);
    const ac = new AbortController();
    const timeout = window.setTimeout(() => ac.abort(), 30_000);
    listRemoteNexusRepositories(
      {
        url,
        credentialRef: form.values.credentialRef.trim() || undefined,
      },
      { signal: ac.signal },
    )
      .then((res) => {
        setRemoteRepos(res.items);
        form.setFieldValue("url", url);
        notifySuccess(t("migrations.remoteIndexOk", { n: res.total }));
      })
      .catch((e: unknown) => {
        if (e instanceof ApiError && e.code === "aborted") {
          notifyError(t("migrations.remoteIndexTimeout"));
        } else {
          notifyError(e instanceof Error ? e.message : t("common.error"));
        }
      })
      .finally(() => {
        window.clearTimeout(timeout);
        setRemoteBusy(false);
      });
  };

  const runDiscover = form.onSubmit((values) => {
    const running = (gateTasks ?? []).find((x) => x.status === "running");
    if (running) {
      notifyError(t("migrations.blockRunning", { id: running.id }));
      navigate(`/migrations/${running.id}`);
      return;
    }
    if (firstPlanned && !discoverResult) {
      notifyError(t("migrations.blockPlanned", { id: firstPlanned.id }));
      return;
    }
    // 离线目录：无索引时禁止空 include（会全盘扫超时）；有索引时空 include = 索引内全部仓
    if (
      values.sourceType === "offline_dir" &&
      values.includeRepositories.length === 0 &&
      offlineIdx?.status !== "ready"
    ) {
      notifyError(t("migrations.offlineNeedInclude"));
      return;
    }

    abortRef.current?.abort();
    const ac = new AbortController();
    abortRef.current = ac;
    const timeout = window.setTimeout(() => ac.abort(), DISCOVER_TIMEOUT_MS);

    setBusy(true);
    setDiscoverPhase("discovering");
    setDiscoverElapsed(0);
    if (timerRef.current != null) {
      window.clearInterval(timerRef.current);
    }
    timerRef.current = window.setInterval(() => {
      setDiscoverElapsed((s) => s + 1);
    }, 1000);

    const sourceConfig: Record<string, unknown> =
      values.sourceType === "online_rest"
        ? { url: values.url.trim() }
        : { path: values.path.trim() };
    if (values.includeRepositories.length > 0) {
      sourceConfig.includeRepositories = values.includeRepositories;
    }

    discoverMigrations(
      {
        sourceType: values.sourceType,
        sourceConfig,
        credentialRef: values.credentialRef.trim() || undefined,
        conflictPolicy: values.conflictPolicy,
      },
      { signal: ac.signal },
    )
      .then((res) => {
        setDiscoverResult(res);
        setSelectedRepos(res.plan.repositories.map((r) => r.name));
        setActive(2);
        setDiscoverPhase("done");
        notifySuccess(t("migrations.discoverOk"));
        listMigrations({ page_size: 100 })
          .then((list) => setGateTasks(list.items.filter((x) => isActiveStatus(x.status))))
          .catch(() => undefined);
      })
      .catch((e: unknown) => {
        setDiscoverPhase("idle");
        if (e instanceof ApiError && e.code === "aborted") {
          notifyError(t("migrations.discoverTimeout"));
        } else {
          notifyError(e instanceof Error ? e.message : t("common.error"));
        }
      })
      .finally(() => {
        window.clearTimeout(timeout);
        if (timerRef.current != null) {
          window.clearInterval(timerRef.current);
          timerRef.current = null;
        }
        setBusy(false);
      });
  });

  const cancelDiscover = () => {
    abortRef.current?.abort();
    setBusy(false);
    setDiscoverPhase("idle");
    notifyError(t("migrations.discoverCancelled"));
  };

  const toggleRepo = (name: string) => {
    setSelectedRepos((prev) =>
      prev.includes(name) ? prev.filter((n) => n !== name) : [...prev, name],
    );
  };
  const selectAll = () => setSelectedRepos([...planRepoNames]);
  const selectNone = () => setSelectedRepos([]);

  const resumePlanned = (id: number) => {
    setBusy(true);
    getMigration(id)
      .then((task) => {
        if (!task.plan) {
          notifyError(t("migrations.needDiscover"));
          return;
        }
        setDiscoverResult({ taskId: task.id, plan: task.plan });
        setSelectedRepos(task.plan.repositories.map((r) => r.name));
        setActive(2);
        setDiscoverPhase("done");
        notifySuccess(t("migrations.restoredPlanned", { id: task.id }));
      })
      .catch((e: Error) => notifyError(e.message || t("common.error")))
      .finally(() => setBusy(false));
  };

  const runStart = () => {
    if (!discoverResult) {
      return;
    }
    if (selectedRepos.length === 0) {
      notifyError(t("migrations.needSelectRepos"));
      return;
    }
    const running = (gateTasks ?? []).find((x) => x.status === "running");
    if (running && running.id !== discoverResult.taskId) {
      notifyError(t("migrations.blockRunning", { id: running.id }));
      navigate(`/migrations/${running.id}`);
      return;
    }
    setBusy(true);
    startMigration(discoverResult.taskId, { includeRepositories: selectedRepos })
      .then(() => {
        clearDraft();
        notifySuccess(t("migrations.started"));
        navigate(`/migrations/${discoverResult.taskId}`);
      })
      .catch((e: Error) => notifyError(e.message || t("common.error")))
      .finally(() => setBusy(false));
  };

  const progressPct = Math.min(
    95,
    Math.round((discoverElapsed / (DISCOVER_TIMEOUT_MS / 1000)) * 100),
  );

  if (gateLoading) {
    return (
      <Stack align="center" py="xl" gap="sm">
        <Loader />
        <Text c="dimmed">{t("migrations.checkingActive")}</Text>
      </Stack>
    );
  }

  return (
    <Stack gap="md">
      <PageHeader
        title={t("migrations.wizardTitle")}
        description={t("migrations.wizardCardHint")}
        actions={
          <Button component={Link} to="/migrations" variant="default">
            {t("migrations.backList")}
          </Button>
        }
      />

      {firstPlanned && (
        <Alert color="orange" title={t("migrations.activeTaskTitle")}>
          <Text size="sm" mb="xs">
            {t("migrations.activeTaskHint", {
              count: plannedOthers.length,
              id: firstPlanned.id,
            })}
          </Text>
          <Group gap="xs">
            <Button size="xs" onClick={() => resumePlanned(firstPlanned.id)} loading={busy}>
              {t("migrations.continuePlanned")}
            </Button>
            <Button
              size="xs"
              variant="light"
              component={Link}
              to={`/migrations/${firstPlanned.id}`}
            >
              {t("migrations.gotoDetail")}
            </Button>
          </Group>
        </Alert>
      )}

      <Card withBorder padding={density.cardPadding} radius="md">
        <Stepper
          active={active}
          onStepClick={(step) => {
            if (busy && discoverPhase === "discovering") {
              notifyError(t("migrations.discoverInProgress"));
              return;
            }
            if (step === 2 && !discoverResult) {
              notifyError(t("migrations.needDiscover"));
              return;
            }
            setActive(step);
          }}
          allowNextStepsSelect={false}
        >
          <Stepper.Step
            label={t("migrations.stepSource")}
            description={t("migrations.stepSourceDesc")}
          >
            <Stack mt="md" gap="md">
              <Text size="sm" c="dimmed">
                {t("migrations.sourcePickTitle")}
              </Text>
              <SimpleGrid cols={{ base: 1, sm: 3 }} spacing="sm">
                {SOURCE_OPTIONS.map((opt) => {
                  const selected = form.values.sourceType === opt.value;
                  const Icon = opt.icon;
                  return (
                    <UnstyledButton
                      key={opt.value}
                      disabled={busy}
                      onClick={() => form.setFieldValue("sourceType", opt.value)}
                      style={{
                        borderRadius: "var(--mantine-radius-md)",
                        border: selected
                          ? "2px solid var(--mantine-color-blue-6)"
                          : "1px solid var(--mantine-color-default-border)",
                        background: selected
                          ? "var(--mantine-color-blue-light)"
                          : "var(--mantine-color-body)",
                        padding: "var(--mantine-spacing-md)",
                        textAlign: "left",
                      }}
                    >
                      <Stack gap="sm">
                        <ThemeIcon
                          size={40}
                          radius="md"
                          variant="light"
                          color={sourceColor(opt.value)}
                        >
                          <Icon size={20} />
                        </ThemeIcon>
                        <Text fw={600}>{t(`migrations.source_${opt.value}`)}</Text>
                        <Text size="xs" c="dimmed" lineClamp={3}>
                          {t(`migrations.${opt.hintKey}`)}
                        </Text>
                      </Stack>
                    </UnstyledButton>
                  );
                })}
              </SimpleGrid>
              <Group>
                <Button disabled={busy} onClick={() => setActive(1)}>
                  {t("common.confirm")}
                </Button>
              </Group>
            </Stack>
          </Stepper.Step>

          <Stepper.Step
            label={t("migrations.stepConfig")}
            description={t("migrations.stepConfigDesc")}
          >
            <Stack mt="md" gap="md">
              {form.values.sourceType === "online_rest" ? (
                <>
                  <TextInput
                    label={t("migrations.url")}
                    placeholder="https://maven.example.com"
                    disabled={busy}
                    {...form.getInputProps("url")}
                  />
                  <TextInput
                    label={t("migrations.credentialRef")}
                    description={t("migrations.credentialRefHint")}
                    placeholder="NEXUS_BASIC"
                    disabled={busy}
                    {...form.getInputProps("credentialRef")}
                  />
                </>
              ) : (
                <TextInput
                  label={t("migrations.path")}
                  description={
                    form.values.sourceType === "offline_dir"
                      ? t("migrations.pathHintOfflineDir")
                      : t("migrations.pathHint")
                  }
                  placeholder={
                    form.values.sourceType === "offline_dir"
                      ? "/opt/nexus/blobs/default"
                      : "/data/nexus-bundle"
                  }
                  disabled={busy}
                  {...form.getInputProps("path")}
                />
              )}

              {/* 离线目录流程：① 索引 → ② 选仓 → ③ 策略与发现 */}
              {form.values.sourceType === "offline_dir" && (
                <>
                  <Text size="xs" c="dimmed">
                    {t("migrations.offlineFlowStepIndex")} · {t("migrations.offlineFlowStepPick")} ·{" "}
                    {t("migrations.offlineFlowStepPolicy")}
                  </Text>

                  <Card withBorder padding={density.cardPadding} radius="md">
                    <Stack gap="sm">
                      <Group gap="xs" justify="space-between" wrap="wrap">
                        <Group gap="xs">
                          <ThemeIcon variant="light" color="violet" size="md">
                            <IconDatabase size={16} />
                          </ThemeIcon>
                          <div>
                            <Text fw={600} size="sm">
                              {t("migrations.offlineFlowStepIndex")} ·{" "}
                              {t("migrations.offlineIndexTitle")}
                            </Text>
                            <Text size="xs" c="dimmed">
                              {t("migrations.offlineIndexHint")}
                            </Text>
                          </div>
                        </Group>
                        <Badge
                          variant="light"
                          color={
                            offlineIdx?.status === "ready"
                              ? "green"
                              : offlineIdx?.status === "scanning"
                                ? "blue"
                                : offlineIdx?.status === "failed"
                                  ? "red"
                                  : "gray"
                          }
                        >
                          {offlineIdx?.status === "ready"
                            ? t("migrations.offlineIndexStatusReady")
                            : offlineIdx?.status === "scanning"
                              ? t("migrations.offlineIndexStatusScanning")
                              : offlineIdx?.status === "failed"
                                ? t("migrations.offlineIndexStatusFailed")
                                : t("migrations.offlineIndexStatusIdle")}
                        </Badge>
                      </Group>
                      {offlineIdx?.message && (
                        <Text size="sm" c="dimmed">
                          {offlineIdx.message}
                          {offlineIdx.status === "scanning" && offlineIdx.scannedProps
                            ? ` · props ${offlineIdx.scannedProps}`
                            : ""}
                          {offlineIdx.totalEntries ? ` · entries ${offlineIdx.totalEntries}` : ""}
                          {offlineIdx.repoCount ? ` · repos ${offlineIdx.repoCount}` : ""}
                        </Text>
                      )}
                      {offlineIdx?.status === "scanning" && (
                        <Stack gap={4}>
                          <Progress value={35} animated striped />
                          <Text size="xs" c="blue">
                            {t("migrations.offlineIndexScanningHint")}
                          </Text>
                        </Stack>
                      )}
                      {offlineIdx?.status === "ready" && (
                        <Alert color="green" title={t("migrations.offlineIndexStatusReady")}>
                          {t("migrations.offlineIndexReadyHint")}
                        </Alert>
                      )}
                      {(!offlineIdx || offlineIdx.status === "idle") && (
                        <Alert color="gray" title={t("migrations.offlineIndexStatusIdle")}>
                          {t("migrations.offlineIndexIdleHint")}
                        </Alert>
                      )}
                      {offlineIdx?.status === "failed" && offlineIdx.errorMessage && (
                        <Alert color="red">{offlineIdx.errorMessage}</Alert>
                      )}
                      {/* 按钮互斥：idle/failed 仅「建立」；ready 仅「更新+清空重建」；scanning 仅「取消」 */}
                      <Group gap="xs" align="flex-start">
                        {offlineIdxScanning ? (
                          <Button
                            size="xs"
                            color="red"
                            variant="light"
                            loading={offlineIdxBusy}
                            disabled={offlineIdxBusy}
                            onClick={runCancelOfflineIndex}
                          >
                            {t("migrations.offlineIndexCancel")}
                          </Button>
                        ) : offlineIdx?.status === "ready" ? (
                          <>
                            <Button
                              size="xs"
                              variant="light"
                              leftSection={<IconRefresh size={14} />}
                              loading={offlineIdxBusy}
                              disabled={offlineIdxActionsLocked}
                              title={t("migrations.offlineIndexScanUpdateHint")}
                              onClick={() => runOfflineIndexScan("update")}
                            >
                              {t("migrations.offlineIndexScanUpdate")}
                            </Button>
                            <Button
                              size="xs"
                              variant="light"
                              color="orange"
                              leftSection={<IconDatabase size={14} />}
                              loading={offlineIdxBusy}
                              disabled={offlineIdxActionsLocked}
                              title={t("migrations.offlineIndexScanRebuildHint")}
                              onClick={() => runOfflineIndexScan("rebuild")}
                            >
                              {t("migrations.offlineIndexScanRebuild")}
                            </Button>
                          </>
                        ) : (
                          <Button
                            size="xs"
                            leftSection={<IconDatabase size={14} />}
                            loading={offlineIdxBusy}
                            disabled={offlineIdxActionsLocked || !form.values.path.trim()}
                            title={t("migrations.offlineIndexScanFullHint")}
                            onClick={() => runOfflineIndexScan("full")}
                          >
                            {offlineIdx?.status === "failed"
                              ? t("migrations.offlineIndexScanFullRetry")
                              : t("migrations.offlineIndexScanFull")}
                          </Button>
                        )}
                      </Group>
                      {!offlineIdxScanning && (
                        <Text size="xs" c="dimmed">
                          {offlineIdx?.status === "ready"
                            ? `${t("migrations.offlineIndexScanUpdateHint")}；${t("migrations.offlineIndexScanRebuildHint")}`
                            : t("migrations.offlineIndexScanFullHint")}
                        </Text>
                      )}
                    </Stack>
                  </Card>

                  {/* ② 选仓：索引就绪用索引列表；否则用在线列表 / 手动 Tags */}
                  {offlineIndexReady && indexRepoNames.length > 0 ? (
                    <Card withBorder padding={density.cardPadding} radius="md">
                      <Stack gap="sm">
                        <Group gap="xs" justify="space-between" wrap="wrap">
                          <div>
                            <Text fw={600} size="sm">
                              {t("migrations.offlineFlowStepPick")} ·{" "}
                              {t("migrations.offlinePickFromIndexTitle")}
                            </Text>
                            <Text size="xs" c="dimmed">
                              {t("migrations.offlinePickFromIndexHint")}
                            </Text>
                          </div>
                          <Badge variant="light" color="violet">
                            {t("migrations.offlinePickSelected", {
                              n: includeSelectedCount,
                              total: indexRepoNames.length,
                            })}
                          </Badge>
                        </Group>
                        <Group gap="xs">
                          <Button
                            size="compact-xs"
                            variant="light"
                            disabled={busy}
                            onClick={selectAllIndexRepos}
                          >
                            {t("migrations.selectAll")}
                          </Button>
                          <Button
                            size="compact-xs"
                            variant="light"
                            disabled={busy}
                            onClick={selectNoneIncludeRepos}
                          >
                            {t("migrations.selectNone")}
                          </Button>
                        </Group>
                        <ScrollArea.Autosize mah={320} type="auto" offsetScrollbars>
                          <Table striped highlightOnHover stickyHeader>
                            <Table.Thead>
                              <Table.Tr>
                                <Table.Th w={40} />
                                <Table.Th>{t("migrations.repoName")}</Table.Th>
                                <Table.Th>{t("migrations.estimatedAssets")}</Table.Th>
                              </Table.Tr>
                            </Table.Thead>
                            <Table.Tbody>
                              {(indexRepoList ?? []).map((r) => {
                                const checked = form.values.includeRepositories.includes(r.name);
                                return (
                                  <Table.Tr
                                    key={r.name}
                                    style={{ cursor: "pointer" }}
                                    bg={checked ? "var(--mantine-color-violet-light)" : undefined}
                                    onClick={() => toggleIncludeRepo(r.name)}
                                  >
                                    <Table.Td onClick={(e) => e.stopPropagation()}>
                                      <Checkbox
                                        checked={checked}
                                        onChange={() => toggleIncludeRepo(r.name)}
                                        aria-label={r.name}
                                        disabled={busy}
                                      />
                                    </Table.Td>
                                    <Table.Td>
                                      <Text size="sm" fw={500}>
                                        {r.name}
                                      </Text>
                                    </Table.Td>
                                    <Table.Td>
                                      <Text size="sm" c="dimmed">
                                        {r.assets.toLocaleString()}
                                      </Text>
                                    </Table.Td>
                                  </Table.Tr>
                                );
                              })}
                            </Table.Tbody>
                          </Table>
                        </ScrollArea.Autosize>
                        <TagsInput
                          label={t("migrations.includeRepos")}
                          description={t("migrations.includeReposHintOfflineReady")}
                          placeholder={t("migrations.includeReposPlaceholder")}
                          clearable
                          disabled={busy}
                          {...form.getInputProps("includeRepositories")}
                        />
                      </Stack>
                    </Card>
                  ) : (
                    <>
                      <Card withBorder padding={density.cardPadding} radius="md">
                        <Stack gap="sm">
                          <Group gap="xs">
                            <ThemeIcon variant="light" color="cyan" size="md">
                              <IconCloudDownload size={16} />
                            </ThemeIcon>
                            <div>
                              <Text fw={600} size="sm">
                                {t("migrations.offlineFlowStepPick")} ·{" "}
                                {t("migrations.remoteIndexTitle")}
                              </Text>
                              <Text size="xs" c="dimmed">
                                {t("migrations.remoteIndexHint")}
                              </Text>
                            </div>
                          </Group>
                          <TextInput
                            label={t("migrations.remoteIndexUrl")}
                            placeholder="https://maven.wcpe.top"
                            description={t("migrations.remoteIndexUrlHint")}
                            disabled={busy || remoteBusy}
                            value={indexUrl}
                            onChange={(e) => setIndexUrl(e.currentTarget.value)}
                          />
                          <TextInput
                            label={t("migrations.credentialRef")}
                            description={t("migrations.credentialRefHint")}
                            placeholder="NEXUS_BASIC"
                            disabled={busy || remoteBusy}
                            {...form.getInputProps("credentialRef")}
                          />
                          <Group>
                            <Button
                              leftSection={<IconCloudDownload size={16} />}
                              loading={remoteBusy}
                              disabled={busy}
                              variant="light"
                              onClick={fetchRemoteIndex}
                            >
                              {t("migrations.fetchRemoteIndex")}
                            </Button>
                            {remoteRepos.length > 0 && (
                              <>
                                <Badge variant="light" color="cyan">
                                  {t("migrations.remoteIndexCount", {
                                    n: remoteRepos.length,
                                  })}
                                </Badge>
                                <Button
                                  size="compact-xs"
                                  variant="light"
                                  disabled={busy}
                                  onClick={() =>
                                    form.setFieldValue(
                                      "includeRepositories",
                                      remoteRepos.map((r) => r.name),
                                    )
                                  }
                                >
                                  {t("migrations.selectAll")}
                                </Button>
                                <Button
                                  size="compact-xs"
                                  variant="light"
                                  disabled={busy}
                                  onClick={selectNoneIncludeRepos}
                                >
                                  {t("migrations.selectNone")}
                                </Button>
                              </>
                            )}
                          </Group>
                          {remoteRepos.length > 0 && (
                            <ScrollArea.Autosize mah={280} type="auto" offsetScrollbars>
                              <Table striped highlightOnHover stickyHeader>
                                <Table.Thead>
                                  <Table.Tr>
                                    <Table.Th w={40} />
                                    <Table.Th>{t("migrations.repoName")}</Table.Th>
                                    <Table.Th>{t("migrations.repoFormat")}</Table.Th>
                                    <Table.Th>{t("migrations.repoType")}</Table.Th>
                                  </Table.Tr>
                                </Table.Thead>
                                <Table.Tbody>
                                  {remoteRepos.map((r) => {
                                    const checked = form.values.includeRepositories.includes(
                                      r.name,
                                    );
                                    return (
                                      <Table.Tr
                                        key={r.name}
                                        style={{ cursor: "pointer" }}
                                        bg={checked ? "var(--mantine-color-blue-light)" : undefined}
                                        onClick={() => toggleIncludeRepo(r.name)}
                                      >
                                        <Table.Td onClick={(e) => e.stopPropagation()}>
                                          <Checkbox
                                            checked={checked}
                                            onChange={() => toggleIncludeRepo(r.name)}
                                            aria-label={r.name}
                                            disabled={busy}
                                          />
                                        </Table.Td>
                                        <Table.Td>
                                          <Text size="sm" fw={500}>
                                            {r.name}
                                          </Text>
                                        </Table.Td>
                                        <Table.Td>
                                          <Badge
                                            size="sm"
                                            variant="light"
                                            color={formatColor(r.format)}
                                          >
                                            {r.format}
                                          </Badge>
                                        </Table.Td>
                                        <Table.Td>
                                          <Badge size="sm" variant="outline" color="gray">
                                            {r.type}
                                          </Badge>
                                        </Table.Td>
                                      </Table.Tr>
                                    );
                                  })}
                                </Table.Tbody>
                              </Table>
                            </ScrollArea.Autosize>
                          )}
                        </Stack>
                      </Card>
                      <TagsInput
                        label={t("migrations.includeRepos")}
                        description={t("migrations.includeReposHintOffline")}
                        placeholder={t("migrations.includeReposPlaceholder")}
                        clearable
                        disabled={busy}
                        {...form.getInputProps("includeRepositories")}
                      />
                      {offlineNeedPickRepos && (
                        <Alert color="orange" title={t("migrations.offlineNeedIncludeTitle")}>
                          {t("migrations.offlineNeedInclude")}
                        </Alert>
                      )}
                    </>
                  )}
                </>
              )}

              <Select
                label={
                  form.values.sourceType === "offline_dir"
                    ? `${t("migrations.offlineFlowStepPolicy")} · ${t("migrations.conflictPolicy")}`
                    : t("migrations.conflictPolicy")
                }
                data={[
                  { value: "skip", label: "skip — 已存在则跳过" },
                  { value: "overwrite", label: "overwrite — 覆盖" },
                  { value: "fail", label: "fail — 冲突即失败" },
                ]}
                disabled={busy}
                {...form.getInputProps("conflictPolicy")}
              />
              {form.values.sourceType !== "offline_dir" && (
                <TagsInput
                  label={t("migrations.includeRepos")}
                  description={t("migrations.includeReposHint")}
                  placeholder={t("migrations.includeReposPlaceholder")}
                  clearable
                  disabled={busy}
                  {...form.getInputProps("includeRepositories")}
                />
              )}

              {discoverPhase === "discovering" && (
                <Card withBorder padding="md" radius="md" bg="var(--mantine-color-blue-light)">
                  <Stack gap="xs">
                    <Group gap="xs">
                      <Loader size="sm" />
                      <Text fw={600}>{t("migrations.discoverInProgress")}</Text>
                    </Group>
                    <Text size="sm" c="dimmed">
                      {t("migrations.discoverProgressHint", { seconds: discoverElapsed })}
                    </Text>
                    <Progress value={progressPct} animated striped />
                    <Button
                      size="xs"
                      color="red"
                      variant="light"
                      w="fit-content"
                      onClick={cancelDiscover}
                    >
                      {t("migrations.cancelDiscover")}
                    </Button>
                  </Stack>
                </Card>
              )}

              <Group>
                <Button variant="default" disabled={busy} onClick={() => setActive(0)}>
                  {t("setup.back")}
                </Button>
                <Button
                  leftSection={<IconSearch size={16} />}
                  loading={busy}
                  disabled={(Boolean(firstPlanned) && !discoverResult) || offlineNeedPickRepos}
                  onClick={() => runDiscover()}
                >
                  {offlineIndexReady && includeSelectedCount === 0 && indexRepoNames.length > 0
                    ? t("migrations.offlineDiscoverAllFromIndex")
                    : offlineIndexReady && includeSelectedCount > 0
                      ? `${t("migrations.runDiscover")}（${includeSelectedCount}）`
                      : t("migrations.runDiscover")}
                </Button>
              </Group>
              {firstPlanned && !discoverResult && (
                <Text size="sm" c="orange">
                  {t("migrations.mustHandlePlannedFirst")}
                </Text>
              )}
            </Stack>
          </Stepper.Step>

          <Stepper.Step
            label={t("migrations.stepPreview")}
            description={t("migrations.stepPreviewDesc")}
          >
            {discoverResult ? (
              <Stack mt="md" gap="md">
                <Alert color="blue" title={t("migrations.plannedHint")}>
                  {t("migrations.previewSummary", { id: discoverResult.taskId })}
                </Alert>

                <SimpleGrid cols={{ base: 2, sm: 4 }} spacing="sm">
                  <MiniStat
                    label={t("migrations.planRepoCount", {
                      n: discoverResult.plan.repositories.length,
                    })}
                    value={String(discoverResult.plan.repositories.length)}
                  />
                  <MiniStat
                    label={t("migrations.estimatedAssets")}
                    value={previewEst > 0 ? previewEst.toLocaleString() : "—"}
                  />
                  <MiniStat label={t("migrations.selectRepos")} value={`${selectedRepos.length}`} />
                  <MiniStat
                    label={t("migrations.conflictPolicy")}
                    value={form.values.conflictPolicy}
                  />
                </SimpleGrid>

                {(discoverResult.plan.warnings?.length ?? 0) > 0 && (
                  <Alert color="yellow" title={t("migrations.warnings")}>
                    <Stack gap={4}>
                      {discoverResult.plan.warnings.map((w) => (
                        <Text key={w} size="sm">
                          · {w}
                        </Text>
                      ))}
                    </Stack>
                  </Alert>
                )}

                <Card withBorder padding={density.cardPadding} radius="md">
                  <Group justify="space-between" mb="sm">
                    <Title order={5}>{t("migrations.selectRepos")}</Title>
                    <Group gap="xs">
                      <Button size="compact-xs" variant="light" onClick={selectAll} disabled={busy}>
                        {t("migrations.selectAll")}
                      </Button>
                      <Button
                        size="compact-xs"
                        variant="light"
                        onClick={selectNone}
                        disabled={busy}
                      >
                        {t("migrations.selectNone")}
                      </Button>
                    </Group>
                  </Group>
                  <MigrationRepoTable
                    repositories={discoverResult.plan.repositories}
                    selected={selectedRepos}
                    onToggle={toggleRepo}
                    disabled={busy}
                    maxHeight={400}
                  />
                </Card>

                <Text size="sm" c="dimmed">
                  {t("migrations.explicitStartHint")}
                </Text>
                <Group>
                  <Button variant="default" disabled={busy} onClick={() => setActive(1)}>
                    {t("setup.back")}
                  </Button>
                  <Button
                    color="green"
                    leftSection={<IconPlayerPlay size={16} />}
                    loading={busy}
                    disabled={selectedRepos.length === 0}
                    onClick={runStart}
                  >
                    {t("migrations.start")}（{selectedRepos.length}）
                  </Button>
                  <Button
                    variant="subtle"
                    disabled={busy}
                    onClick={() => navigate(`/migrations/${discoverResult.taskId}`)}
                  >
                    {t("migrations.savePlanned")}
                  </Button>
                </Group>
              </Stack>
            ) : (
              <Text c="dimmed" mt="md">
                {t("migrations.needDiscover")}
              </Text>
            )}
          </Stepper.Step>
        </Stepper>
      </Card>
    </Stack>
  );
}

function MiniStat({ label, value }: { label: string; value: string }) {
  return (
    <Card withBorder padding="sm" radius="md">
      <Text size="xs" c="dimmed" lineClamp={1}>
        {label}
      </Text>
      <Text fw={700} size="lg">
        {value}
      </Text>
    </Card>
  );
}
