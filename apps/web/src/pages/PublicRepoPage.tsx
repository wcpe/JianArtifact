// 公开仓库浏览：/p/:name，无需登录；仅 public 仓可读（后端 requireRepoRead）。
import { Alert, Button, Container, Group, Stack, Text, Title } from "@mantine/core";
import { useTranslation } from "react-i18next";
import { Link, useParams } from "react-router-dom";

import { ApiError } from "../api/client";
import { getRepositoryUsage, listAllRepositoryAssets } from "../api/endpoints";
import { RepoBrowser } from "../components/repo/RepoBrowser";
import { useAsync } from "../hooks/useAsync";
import { density } from "../theme/density";

export function PublicRepoPage() {
  const { t } = useTranslation();
  const { name = "" } = useParams();

  // 探测可读性：usage 与 assets 均走匿名可读接口
  const probe = useAsync(async () => {
    const usage = await getRepositoryUsage(name);
    // 顺带探测 assets（权限与 usage 一致）
    await listAllRepositoryAssets(name, { maxItems: 1 });
    return usage;
  }, [name]);

  if (probe.loading) {
    return (
      <Container size="lg" py="xl">
        <Text c="dimmed">{t("common.loading")}</Text>
      </Container>
    );
  }

  if (probe.error) {
    const err = probe.error;
    const isAuth = err instanceof ApiError && (err.status === 401 || err.status === 403);
    const is404 = err instanceof ApiError && err.status === 404;
    return (
      <Container size="sm" py="xl">
        <Stack gap="md">
          <Title order={3}>{t("repoDetail.publicTitle")}</Title>
          <Alert color={isAuth ? "orange" : "red"} title={t("repoDetail.publicDeniedTitle")}>
            {is404
              ? t("repoDetail.publicNotFound")
              : isAuth
                ? t("repoDetail.publicPrivateHint")
                : err.message || t("common.error")}
          </Alert>
          <Group>
            <Button component={Link} to="/login" variant="light">
              {t("auth.login")}
            </Button>
          </Group>
        </Stack>
      </Container>
    );
  }

  return (
    <Container size="lg" py="md" style={{ maxWidth: density.contentMaxWidth }}>
      <Stack gap="md">
        <Group justify="space-between" align="flex-start" wrap="wrap">
          <div>
            <Title order={3}>
              {t("repoDetail.publicTitle")} · {name}
            </Title>
            <Text size="sm" c="dimmed">
              {t("repoDetail.publicHint")}
            </Text>
          </div>
          <Button component={Link} to="/login" variant="default" size="sm">
            {t("auth.login")}
          </Button>
        </Group>
        <RepoBrowser
          repoName={name}
          allowUpload={false}
          publicMode
          forcedFormat={probe.data?.format}
          forcedType={probe.data?.type}
        />
      </Stack>
    </Container>
  );
}
