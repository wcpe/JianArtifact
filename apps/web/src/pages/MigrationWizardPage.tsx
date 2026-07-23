// 迁移向导：选来源 → 配置 → discover 预览 → 确认 start（显式，不自动跑）。
import {
  Alert,
  Button,
  Group,
  Select,
  Stack,
  Stepper,
  Table,
  Text,
  TextInput,
} from "@mantine/core";
import { useForm } from "@mantine/form";
import { PageHeader } from "@jianartifact/ui";
import { useState } from "react";
import { useTranslation } from "react-i18next";
import { useNavigate } from "react-router-dom";

import { discoverMigrations, startMigration } from "../api/endpoints";
import type {
  MigrationConflictPolicy,
  MigrationDiscoverResponse,
  MigrationSourceType,
} from "../api/types";
import { notifyError, notifySuccess } from "../lib/feedback";

export function MigrationWizardPage() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const [active, setActive] = useState(0);
  const [busy, setBusy] = useState(false);
  const [discoverResult, setDiscoverResult] = useState<MigrationDiscoverResponse | null>(null);

  const form = useForm({
    initialValues: {
      sourceType: "offline_bundle" as MigrationSourceType,
      url: "",
      path: "",
      credentialRef: "",
      conflictPolicy: "skip" as MigrationConflictPolicy,
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

  const runDiscover = form.onSubmit((values) => {
    setBusy(true);
    const sourceConfig: Record<string, unknown> =
      values.sourceType === "online_rest"
        ? { url: values.url.trim() }
        : { path: values.path.trim() };
    discoverMigrations({
      sourceType: values.sourceType,
      sourceConfig,
      credentialRef: values.credentialRef.trim() || undefined,
      conflictPolicy: values.conflictPolicy,
    })
      .then((res) => {
        setDiscoverResult(res);
        setActive(2);
        notifySuccess(t("migrations.discoverOk"));
      })
      .catch((e: Error) => notifyError(e.message || t("common.error")))
      .finally(() => setBusy(false));
  });

  const runStart = () => {
    if (!discoverResult) {
      return;
    }
    setBusy(true);
    startMigration(discoverResult.taskId)
      .then(() => {
        notifySuccess(t("migrations.started"));
        navigate(`/migrations/${discoverResult.taskId}`);
      })
      .catch((e: Error) => notifyError(e.message || t("common.error")))
      .finally(() => setBusy(false));
  };

  return (
    <>
      <PageHeader title={t("migrations.wizardTitle")} description={t("migrations.wizardDesc")} />
      <Stepper active={active} onStepClick={setActive} allowNextStepsSelect={false}>
        <Stepper.Step label={t("migrations.stepSource")} description={t("migrations.stepSourceDesc")}>
          <Select
            label={t("migrations.sourceType")}
            data={[
              { value: "online_rest", label: t("migrations.source_online_rest") },
              { value: "offline_dir", label: t("migrations.source_offline_dir") },
              { value: "offline_bundle", label: t("migrations.source_offline_bundle") },
            ]}
            {...form.getInputProps("sourceType")}
          />
          <Group mt="md">
            <Button onClick={() => setActive(1)}>{t("common.confirm")}</Button>
          </Group>
        </Stepper.Step>

        <Stepper.Step label={t("migrations.stepConfig")} description={t("migrations.stepConfigDesc")}>
          <Stack>
            {form.values.sourceType === "online_rest" ? (
              <>
                <TextInput
                  label={t("migrations.url")}
                  placeholder="http://nexus:8081"
                  {...form.getInputProps("url")}
                />
                <TextInput
                  label={t("migrations.credentialRef")}
                  description={t("migrations.credentialRefHint")}
                  placeholder="NEXUS_BASIC"
                  {...form.getInputProps("credentialRef")}
                />
              </>
            ) : (
              <TextInput
                label={t("migrations.path")}
                description={t("migrations.pathHint")}
                placeholder="/data/nexus-bundle"
                {...form.getInputProps("path")}
              />
            )}
            <Select
              label={t("migrations.conflictPolicy")}
              data={[
                { value: "skip", label: "skip" },
                { value: "overwrite", label: "overwrite" },
                { value: "fail", label: "fail" },
              ]}
              {...form.getInputProps("conflictPolicy")}
            />
            <Group>
              <Button variant="default" onClick={() => setActive(0)}>
                {t("setup.back")}
              </Button>
              <Button loading={busy} onClick={() => runDiscover()}>
                {t("migrations.runDiscover")}
              </Button>
            </Group>
          </Stack>
        </Stepper.Step>

        <Stepper.Step label={t("migrations.stepPreview")} description={t("migrations.stepPreviewDesc")}>
          {discoverResult ? (
            <Stack>
              <Alert color="blue" title={t("migrations.plannedHint")}>
                taskId = {discoverResult.taskId} · {t("migrations.status_planned")}
              </Alert>
              {discoverResult.plan.warnings.length > 0 && (
                <Alert color="yellow" title={t("migrations.warnings")}>
                  <ul>
                    {discoverResult.plan.warnings.map((w) => (
                      <li key={w}>{w}</li>
                    ))}
                  </ul>
                </Alert>
              )}
              <Table striped>
                <Table.Thead>
                  <Table.Tr>
                    <Table.Th>{t("migrations.repoName")}</Table.Th>
                    <Table.Th>{t("migrations.repoFormat")}</Table.Th>
                    <Table.Th>{t("migrations.repoType")}</Table.Th>
                    <Table.Th>{t("migrations.estimatedAssets")}</Table.Th>
                  </Table.Tr>
                </Table.Thead>
                <Table.Tbody>
                  {discoverResult.plan.repositories.map((r) => (
                    <Table.Tr key={r.name}>
                      <Table.Td>{r.name}</Table.Td>
                      <Table.Td>{r.format}</Table.Td>
                      <Table.Td>{r.type ?? "hosted"}</Table.Td>
                      <Table.Td>{r.estimatedAssets ?? "—"}</Table.Td>
                    </Table.Tr>
                  ))}
                </Table.Tbody>
              </Table>
              <Text size="sm" c="dimmed">
                {t("migrations.explicitStartHint")}
              </Text>
              <Group>
                <Button variant="default" onClick={() => setActive(1)}>
                  {t("setup.back")}
                </Button>
                <Button color="green" loading={busy} onClick={runStart}>
                  {t("migrations.start")}
                </Button>
                <Button
                  variant="subtle"
                  onClick={() => navigate(`/migrations/${discoverResult.taskId}`)}
                >
                  {t("migrations.savePlanned")}
                </Button>
              </Group>
            </Stack>
          ) : (
            <Text c="dimmed">{t("migrations.needDiscover")}</Text>
          )}
        </Stepper.Step>
      </Stepper>
    </>
  );
}
