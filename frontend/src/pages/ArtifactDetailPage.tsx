// 制品详情页（FR-22 / FR-66 / FR-68 / FR-69 / FR-93）：深链入口，加载制品后复用
// ArtifactDetailPanel 展示元数据、四校验和、后端 usage 与多格式依赖坐标 / HTML View / 下载。
// 经查询参数 ?repo=&path= 定位，避免与后端格式 catch-all 路由冲突。

import { useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Stack, Group, Loader, Center, Button } from '@mantine/core';
import { IconArrowLeft } from '@tabler/icons-react';
import { useNavigate, useSearchParams } from 'react-router-dom';
import * as api from '../api/endpoints';
import type { ArtifactDetailDto } from '../api/types';
import { errorMessage } from '../lib/format';
import { ErrorAlert } from '../components/ErrorAlert';
import { ArtifactDetailPanel } from '../components/ArtifactDetailPanel';

/** 制品详情页面。 */
export function ArtifactDetailPage() {
  const { t } = useTranslation('artifactDetail');
  const [params] = useSearchParams();
  const navigate = useNavigate();
  const repoId = params.get('repo') ?? '';
  const path = params.get('path') ?? '';
  const [detail, setDetail] = useState<ArtifactDetailDto | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (!repoId || !path) {
      setError(t('missingId'));
      setLoading(false);
      return;
    }
    api
      .getArtifactDetail(repoId, path)
      .then(setDetail)
      .catch((err) => setError(errorMessage(err)))
      .finally(() => setLoading(false));
  }, [repoId, path, t]);

  if (loading) {
    return (
      <Center h={200}>
        <Loader />
      </Center>
    );
  }

  return (
    <Stack>
      <Group>
        <Button
          variant="subtle"
          size="xs"
          leftSection={<IconArrowLeft size={16} />}
          onClick={() => navigate(-1)}
        >
          {t('common:back')}
        </Button>
      </Group>

      {error || !detail ? (
        <ErrorAlert message={error ?? t('notFound')} />
      ) : (
        <ArtifactDetailPanel detail={detail} />
      )}
    </Stack>
  );
}
