// FR-73: Maven 网页上传卡（GAV 表单 + packaging + 文件选择）。
// 服务端自动生成 pom.xml、各文件 .md5/.sha1 与 maven-metadata.xml；仅限 release 版本。
import {
  Button,
  Card,
  FileButton,
  Group,
  Select,
  Stack,
  Text,
  TextInput,
  Title,
} from "@mantine/core";
import { IconUpload } from "@tabler/icons-react";
import { useState } from "react";
import { useTranslation } from "react-i18next";

import { uploadMavenArtifact } from "../../api/endpoints";
import { notifyError, notifySuccess } from "../../lib/feedback";
import { density } from "../../theme/density";

interface Props {
  repoName: string;
  /** 上传成功后触发（刷新文件树）。 */
  onUploaded: () => void;
}

export function MavenUploadCard({ repoName, onUploaded }: Props) {
  const { t } = useTranslation();
  const [groupId, setGroupId] = useState("");
  const [artifactId, setArtifactId] = useState("");
  const [version, setVersion] = useState("");
  const [packaging, setPackaging] = useState("jar");
  const [uploading, setUploading] = useState(false);

  const isSnapshot = version.toUpperCase().includes("-SNAPSHOT");

  const handleUpload = (file: File | null) => {
    if (!file || uploading) {
      return;
    }
    if (!groupId.trim() || !artifactId.trim() || !version.trim()) {
      notifyError(t("repoDetail.mavenUploadNeedFields"));
      return;
    }
    if (isSnapshot) {
      notifyError(t("repoDetail.mavenSnapshotNotSupported"));
      return;
    }
    setUploading(true);
    uploadMavenArtifact(repoName, {
      groupId: groupId.trim(),
      artifactId: artifactId.trim(),
      version: version.trim(),
      packaging,
      file,
    })
      .then(() => {
        notifySuccess(t("repoDetail.uploadOk"));
        // GAV 常连传多版本：保留 group/artifact，仅清 version
        setVersion("");
        onUploaded();
      })
      .catch((e: Error) => notifyError(e.message || t("common.error")))
      .finally(() => setUploading(false));
  };

  return (
    <Card withBorder padding={density.cardPadding} radius="md">
      <Stack gap="sm">
        <Title order={5}>{t("repoDetail.mavenUploadTitle")}</Title>
        <Text size="xs" c="dimmed">
          {t("repoDetail.mavenUploadHint")}
        </Text>
        <Group grow align="flex-start">
          <TextInput
            label="GroupId"
            placeholder="com.example"
            value={groupId}
            onChange={(e) => setGroupId(e.currentTarget.value)}
            disabled={uploading}
          />
          <TextInput
            label="ArtifactId"
            placeholder="my-lib"
            value={artifactId}
            onChange={(e) => setArtifactId(e.currentTarget.value)}
            disabled={uploading}
          />
          <TextInput
            label="Version"
            placeholder="1.0.0"
            value={version}
            onChange={(e) => setVersion(e.currentTarget.value)}
            error={isSnapshot ? t("repoDetail.mavenSnapshotNotSupported") : undefined}
            disabled={uploading}
          />
          <Select
            label="Packaging"
            data={["jar", "war", "pom"]}
            value={packaging}
            onChange={(v) => setPackaging(v ?? "jar")}
            allowDeselect={false}
            disabled={uploading}
          />
        </Group>
        <Group>
          <FileButton onChange={handleUpload} disabled={uploading}>
            {(props) => (
              <Button {...props} leftSection={<IconUpload size={16} />} loading={uploading}>
                {t("repoDetail.uploadPick")}
              </Button>
            )}
          </FileButton>
        </Group>
      </Stack>
    </Card>
  );
}
