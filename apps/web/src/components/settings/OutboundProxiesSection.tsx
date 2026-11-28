import {
  ActionIcon,
  Alert,
  Badge,
  Button,
  Group,
  Modal,
  SegmentedControl,
  Stack,
  Switch,
  Table,
  Text,
  Textarea,
  TextInput,
  Tooltip,
} from "@mantine/core";
import {
  IconCheck,
  IconEdit,
  IconNetwork,
  IconPlugConnected,
  IconPlus,
  IconTrash,
  IconX,
} from "@tabler/icons-react";
import { useState } from "react";
import { useTranslation } from "react-i18next";

import {
  createOutboundProxy,
  deleteOutboundProxy,
  listOutboundProxies,
  testOutboundProxy,
  updateOutboundProxy,
} from "../../api/endpoints";
import type { OutboundProxy, OutboundProxyKind, TestOutboundProxyResult } from "../../api/types";
import { OpsSection } from "../ops/OpsKit";
import { useAsync } from "../../hooks/useAsync";
import { confirmDanger, notifyError, notifySuccess } from "../../lib/feedback";

const KIND_COLOR: Record<OutboundProxyKind, string> = {
  http: "blue",
  socks5: "teal",
  mihomo: "violet",
};

interface ProxyFormState {
  name: string;
  kind: OutboundProxyKind;
  description: string;
  config: string;
  enabled: boolean;
}

const EMPTY_FORM: ProxyFormState = {
  name: "",
  kind: "socks5",
  description: "",
  config: "",
  enabled: true,
};

/**
 * 全局出站代理池管理分区（嵌入在设置页底部）：
 * 支持 HTTP/HTTPS、SOCKS5/SOCKS5H 与托管 Mihomo 节点协议（YAML 节点或分享链接），
 * 并在界面显式提示宿主机 mihomo 二进制可用状态及支持一键连通性探测。
 */
export function OutboundProxiesSection() {
  const { t } = useTranslation();
  const state = useAsync(listOutboundProxies, [], { cacheKey: "settings:outbound-proxies" });
  const data = state.data;

  const [modalOpen, setModalOpen] = useState(false);
  const [editing, setEditing] = useState<OutboundProxy | null>(null);
  const [form, setForm] = useState<ProxyFormState>(EMPTY_FORM);
  const [saving, setSaving] = useState(false);

  const [testTargetProxy, setTestTargetProxy] = useState<OutboundProxy | null>(null);
  const [testUrl, setTestUrl] = useState("https://libraries.minecraft.net/");
  const [testing, setTesting] = useState(false);
  const [testResult, setTestResult] = useState<TestOutboundProxyResult | null>(null);

  const openCreate = () => {
    setEditing(null);
    setForm(EMPTY_FORM);
    setModalOpen(true);
  };

  const openEdit = (proxy: OutboundProxy) => {
    setEditing(proxy);
    setForm({
      name: proxy.name,
      kind: proxy.kind,
      description: proxy.description,
      config: "",
      enabled: proxy.enabled,
    });
    setModalOpen(true);
  };

  const handleSave = () => {
    const trimmedName = form.name.trim();
    if (!trimmedName) {
      notifyError(new Error(t("outboundProxies.nameRequired")));
      return;
    }
    const trimmedConfig = form.config.trim();
    if (!editing && !trimmedConfig) {
      notifyError(new Error(t("outboundProxies.configRequired")));
      return;
    }
    if (editing && editing.kind !== form.kind && !trimmedConfig) {
      notifyError(new Error(t("outboundProxies.configRequiredOnKindChange")));
      return;
    }

    setSaving(true);
    const req = editing
      ? updateOutboundProxy(editing.id, {
          name: trimmedName,
          kind: form.kind,
          description: form.description.trim(),
          enabled: form.enabled,
          ...(trimmedConfig ? { config: trimmedConfig } : {}),
        })
      : createOutboundProxy({
          name: trimmedName,
          kind: form.kind,
          description: form.description.trim(),
          config: trimmedConfig,
          enabled: form.enabled,
        });

    req
      .then(() => {
        notifySuccess(editing ? t("outboundProxies.updated") : t("outboundProxies.created"));
        setModalOpen(false);
        state.reload();
      })
      .catch(notifyError)
      .finally(() => setSaving(false));
  };

  const handleDelete = (proxy: OutboundProxy) => {
    confirmDanger({
      title: t("outboundProxies.deleteTitle"),
      message: t("outboundProxies.deleteConfirm", { name: proxy.name }),
      confirmLabel: t("common.delete"),
      cancelLabel: t("common.cancel"),
      onConfirm: () => {
        deleteOutboundProxy(proxy.id)
          .then(() => {
            notifySuccess(t("outboundProxies.deleted"));
            state.reload();
          })
          .catch(notifyError);
      },
    });
  };

  const openTest = (proxy: OutboundProxy) => {
    setTestTargetProxy(proxy);
    setTestResult(null);
  };

  const handleRunTest = () => {
    if (!testTargetProxy) return;
    setTesting(true);
    setTestResult(null);
    testOutboundProxy(testTargetProxy.id, { targetUrl: testUrl.trim() || undefined })
      .then((res) => setTestResult(res))
      .catch(notifyError)
      .finally(() => setTesting(false));
  };

  const configPlaceholder =
    form.kind === "http"
      ? "http://127.0.0.1:7890"
      : form.kind === "socks5"
        ? "socks5h://127.0.0.1:7891"
        : "vless://uuid@hk.example.com:443?security=reality&sni=www.microsoft.com&pbk=...#HK\n# 或直接粘贴 Clash / Mihomo YAML 节点配置";

  return (
    <OpsSection
      title={
        <span style={{ display: "inline-flex", alignItems: "center", gap: 6 }}>
          <IconNetwork size={16} />
          <span>{t("outboundProxies.title")}</span>
        </span>
      }
      actions={
        <Button size="xs" leftSection={<IconPlus size={14} />} onClick={openCreate}>
          {t("outboundProxies.add")}
        </Button>
      }
      testId="outbound-proxies-section"
    >
      <Stack gap="sm">
        <Text size="xs" c="dimmed">
          {t("outboundProxies.description")}
        </Text>
        {/* 本机 Mihomo 运行时可用性提示 */}
        {data ? (
          <Group gap="xs" wrap="wrap">
            <Badge
              size="sm"
              variant="light"
              color={data.mihomoAvailable ? "teal" : "yellow"}
              leftSection={data.mihomoAvailable ? <IconCheck size={12} /> : <IconX size={12} />}
            >
              {data.mihomoAvailable
                ? t("outboundProxies.mihomoAvailable", {
                    version: data.mihomoVersion || "mihomo",
                  })
                : t("outboundProxies.mihomoUnavailable")}
            </Badge>
            {!data.mihomoAvailable ? (
              <Text size="xs" c="dimmed">
                {t("outboundProxies.mihomoInstallHint")}
              </Text>
            ) : null}
          </Group>
        ) : null}

        {state.error ? (
          <Alert color="red" variant="light">
            {t("outboundProxies.loadFailed")}
          </Alert>
        ) : data && data.items.length === 0 ? (
          <Text size="sm" c="dimmed">
            {t("outboundProxies.empty")}
          </Text>
        ) : data ? (
          <Table striped highlightOnHover verticalSpacing="xs">
            <Table.Thead>
              <Table.Tr>
                <Table.Th>{t("outboundProxies.colName")}</Table.Th>
                <Table.Th>{t("outboundProxies.colKind")}</Table.Th>
                <Table.Th>{t("outboundProxies.colPreview")}</Table.Th>
                <Table.Th>{t("outboundProxies.colStatus")}</Table.Th>
                <Table.Th>{t("outboundProxies.colRepos")}</Table.Th>
                <Table.Th ta="right">{t("outboundProxies.colActions")}</Table.Th>
              </Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {data.items.map((item) => (
                <Table.Tr key={item.id}>
                  <Table.Td>
                    <Stack gap={2}>
                      <Text size="sm" fw={500}>
                        {item.name}
                      </Text>
                      {item.description ? (
                        <Text size="xs" c="dimmed">
                          {item.description}
                        </Text>
                      ) : null}
                    </Stack>
                  </Table.Td>
                  <Table.Td>
                    <Badge size="xs" variant="light" color={KIND_COLOR[item.kind] ?? "gray"}>
                      {item.kind.toUpperCase()}
                    </Badge>
                  </Table.Td>
                  <Table.Td>
                    <Text size="xs" ff="monospace">
                      {item.endpointPreview}
                    </Text>
                  </Table.Td>
                  <Table.Td>
                    <Badge size="xs" variant="dot" color={item.enabled ? "teal" : "gray"}>
                      {item.enabled ? t("outboundProxies.enabled") : t("outboundProxies.disabled")}
                    </Badge>
                  </Table.Td>
                  <Table.Td>
                    {item.boundRepositories.length > 0 ? (
                      <Group gap={4} wrap="wrap">
                        {item.boundRepositories.map((r) => (
                          <Badge key={r} size="xs" variant="outline" color="gray">
                            {r}
                          </Badge>
                        ))}
                      </Group>
                    ) : (
                      <Text size="xs" c="dimmed">
                        —
                      </Text>
                    )}
                  </Table.Td>
                  <Table.Td>
                    <Group gap={4} justify="flex-end" wrap="nowrap">
                      <Tooltip label={t("outboundProxies.test")}>
                        <ActionIcon
                          variant="subtle"
                          color="blue"
                          aria-label={t("outboundProxies.test")}
                          onClick={() => openTest(item)}
                        >
                          <IconPlugConnected size={16} />
                        </ActionIcon>
                      </Tooltip>
                      <Tooltip label={t("common.edit")}>
                        <ActionIcon
                          variant="subtle"
                          aria-label={t("common.edit")}
                          onClick={() => openEdit(item)}
                        >
                          <IconEdit size={16} />
                        </ActionIcon>
                      </Tooltip>
                      <Tooltip label={t("common.delete")}>
                        <ActionIcon
                          variant="subtle"
                          color="red"
                          aria-label={t("common.delete")}
                          onClick={() => handleDelete(item)}
                        >
                          <IconTrash size={16} />
                        </ActionIcon>
                      </Tooltip>
                    </Group>
                  </Table.Td>
                </Table.Tr>
              ))}
            </Table.Tbody>
          </Table>
        ) : null}
      </Stack>

      {/* 新增 / 编辑出站代理弹窗 */}
      <Modal
        opened={modalOpen}
        onClose={() => setModalOpen(false)}
        title={editing ? t("outboundProxies.editTitle") : t("outboundProxies.createTitle")}
        size="lg"
      >
        <Stack gap="sm">
          <TextInput
            label={t("outboundProxies.fieldName")}
            placeholder="hk-mihomo"
            required
            value={form.name}
            onChange={(e) => setForm({ ...form, name: e.currentTarget.value })}
          />
          <Stack gap={4}>
            <Text size="sm" fw={500}>
              {t("outboundProxies.fieldKind")}
            </Text>
            <SegmentedControl
              value={form.kind}
              onChange={(v) => setForm({ ...form, kind: v as OutboundProxyKind })}
              data={[
                { label: "HTTP / HTTPS", value: "http" },
                { label: "SOCKS5", value: "socks5" },
                { label: "Mihomo (VLESS/VMess/Trojan/SS/Hy2/TUIC)", value: "mihomo" },
              ]}
            />
          </Stack>
          <Textarea
            label={t("outboundProxies.fieldConfig")}
            description={
              editing
                ? t("outboundProxies.fieldConfigEditHint", {
                    preview: editing.endpointPreview,
                  })
                : form.kind === "mihomo"
                  ? t("outboundProxies.fieldConfigMihomoHint")
                  : t("outboundProxies.fieldConfigStandardHint")
            }
            placeholder={configPlaceholder}
            autosize
            minRows={form.kind === "mihomo" ? 4 : 2}
            maxRows={10}
            styles={{ input: { fontFamily: "monospace", fontSize: 12 } }}
            required={!editing || editing.kind !== form.kind}
            value={form.config}
            onChange={(e) => setForm({ ...form, config: e.currentTarget.value })}
          />
          <TextInput
            label={t("outboundProxies.fieldDescription")}
            placeholder={t("outboundProxies.fieldDescriptionPlaceholder")}
            value={form.description}
            onChange={(e) => setForm({ ...form, description: e.currentTarget.value })}
          />
          <Switch
            label={t("outboundProxies.fieldEnabled")}
            checked={form.enabled}
            onChange={(e) => setForm({ ...form, enabled: e.currentTarget.checked })}
          />
          <Group justify="flex-end" mt="xs">
            <Button variant="default" onClick={() => setModalOpen(false)}>
              {t("common.cancel")}
            </Button>
            <Button loading={saving} onClick={handleSave}>
              {t("common.save")}
            </Button>
          </Group>
        </Stack>
      </Modal>

      {/* 连通性测试弹窗 */}
      <Modal
        opened={Boolean(testTargetProxy)}
        onClose={() => setTestTargetProxy(null)}
        title={t("outboundProxies.testTitle", { name: testTargetProxy?.name ?? "" })}
      >
        <Stack gap="sm">
          <TextInput
            label={t("outboundProxies.testTargetUrl")}
            value={testUrl}
            onChange={(e) => setTestUrl(e.currentTarget.value)}
            placeholder="https://libraries.minecraft.net/"
          />
          {testResult ? (
            <Alert color={testResult.ok ? "teal" : "red"} variant="light">
              <Stack gap={2}>
                <Text size="sm" fw={500}>
                  {testResult.ok
                    ? t("outboundProxies.testSuccess", { latency: testResult.latencyMs })
                    : t("outboundProxies.testFailed")}
                </Text>
                <Text size="xs">{testResult.message}</Text>
              </Stack>
            </Alert>
          ) : null}
          <Group justify="flex-end">
            <Button variant="default" onClick={() => setTestTargetProxy(null)}>
              {t("common.close")}
            </Button>
            <Button
              loading={testing}
              leftSection={<IconPlugConnected size={14} />}
              onClick={handleRunTest}
            >
              {t("outboundProxies.runTest")}
            </Button>
          </Group>
        </Stack>
      </Modal>
    </OpsSection>
  );
}
