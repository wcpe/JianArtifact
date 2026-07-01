// 任务中心页（FR-132，仅 Admin）：展示统一任务注册表的活跃+近期任务队列。
// 依赖 FR-131 后端：GET /api/v1/tasks（TaskRecord[]）。
// 轮询间隔 3s；卸载时立即 clearInterval；无任务显空状态；点击行跳转续看。

import { useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';
import {
  Stack,
  Title,
  Text,
  Badge,
  Card,
  Group,
  Loader,
  Center,
  Alert,
  ThemeIcon,
} from '@mantine/core';
import {
  IconArrowsExchange,
  IconArrowUpCircle,
  IconShield,
  IconListCheck,
  IconAlertCircle,
} from '@tabler/icons-react';
import { useNavigate } from 'react-router-dom';
import * as api from '../api/endpoints';
import type { TaskRecord, TaskKind, TaskState } from '../api/types';
import { errorMessage } from '../lib/format';

/** 轮询间隔（毫秒）。 */
const POLL_INTERVAL_MS = 3000;

/** 任务类型 → 中文名与图标。 */
function kindMeta(kind: TaskKind): { label: string; icon: React.ReactNode } {
  switch (kind) {
    case 'migration':
      return { label: 'Nexus 迁移', icon: <IconArrowsExchange size={16} /> };
    case 'update':
      return { label: '在线更新', icon: <IconArrowUpCircle size={16} /> };
    case 'vuln':
      return { label: '漏洞库刷新', icon: <IconShield size={16} /> };
  }
}

/** 任务状态 → Badge 文案与颜色。 */
function stateMeta(state: TaskState): { label: string; color: string } {
  switch (state) {
    case 'running':
      return { label: '运行中', color: 'blue' };
    case 'paused':
      return { label: '已暂停', color: 'yellow' };
    case 'succeeded':
      return { label: '已完成', color: 'green' };
    case 'failed':
      return { label: '失败', color: 'red' };
    case 'cancelled':
      return { label: '已取消', color: 'gray' };
  }
}

/** 任务是否为活跃态（running / paused）。 */
function isActive(state: TaskState): boolean {
  return state === 'running' || state === 'paused';
}

/** 格式化 Unix 秒为本地时间串。 */
function fmtUnixSecs(secs: number): string {
  return new Date(secs * 1000).toLocaleString('zh-CN', {
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
  });
}

/** 点击任务行跳转对应续看页路由（按 kind）。 */
function kindRoute(kind: TaskKind): string {
  switch (kind) {
    case 'migration':
      return '/migration';
    case 'update':
      return '/system';
    case 'vuln':
      return '/settings';
  }
}

/** 单条任务行组件。 */
function TaskRow({ task, onClick }: { task: TaskRecord; onClick: () => void }) {
  const { icon } = kindMeta(task.kind);
  const { label: stateLabel, color: stateColor } = stateMeta(task.state);
  const displayLabel = task.label ?? kindMeta(task.kind).label;

  return (
    <Card
      withBorder
      padding="sm"
      style={{ cursor: 'pointer' }}
      onClick={onClick}
      role="row"
      tabIndex={0}
      onKeyDown={(e) => {
        if (e.key === 'Enter' || e.key === ' ') {
          e.preventDefault();
          onClick();
        }
      }}
    >
      <Group justify="space-between" wrap="nowrap">
        <Group gap="sm" wrap="nowrap" style={{ minWidth: 0 }}>
          <ThemeIcon variant="light" size="sm" color="blue">
            {icon}
          </ThemeIcon>
          <Text fw={500} size="sm" truncate="end">
            {displayLabel}
          </Text>
        </Group>
        <Group gap="xs" wrap="nowrap" style={{ flexShrink: 0 }}>
          <Badge color={stateColor} variant="light" size="sm">
            {stateLabel}
          </Badge>
        </Group>
      </Group>
      <Group gap="md" mt={4}>
        <Text size="xs" c="dimmed">
          开始：{fmtUnixSecs(task.started_at)}
        </Text>
        {task.finished_at !== undefined && (
          <Text size="xs" c="dimmed">
            完成：{fmtUnixSecs(task.finished_at)}
          </Text>
        )}
        {task.error !== undefined && (
          <Text size="xs" c="red" truncate="end" style={{ maxWidth: 300 }}>
            {task.error}
          </Text>
        )}
      </Group>
    </Card>
  );
}

/**
 * 任务中心页（FR-132，仅 Admin）：展示统一任务注册表的活跃+近期任务，
 * 每行点击跳转对应续看页；轮询 3s 自动刷新；离开页面仍可找回已完成历史。
 */
export function TaskCenterPage() {
  const { t } = useTranslation('taskCenter');
  const navigate = useNavigate();
  const [tasks, setTasks] = useState<TaskRecord[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;

    // 首次加载
    api
      .listTasks()
      .then((result) => {
        if (!cancelled) {
          setTasks(result);
          setLoading(false);
        }
      })
      .catch((err: unknown) => {
        if (!cancelled) {
          setError(errorMessage(err));
          setLoading(false);
        }
      });

    // 轮询刷新（3s 间隔）
    const timer = setInterval(() => {
      api
        .listTasks()
        .then((result) => {
          if (!cancelled) setTasks(result);
        })
        .catch(() => {
          // 轮询失败静默忽略，不覆盖现有列表
        });
    }, POLL_INTERVAL_MS);

    return () => {
      cancelled = true;
      clearInterval(timer);
    };
  }, []);

  // 按活跃 / 近期历史分组
  const activeTasks = tasks.filter((t) => isActive(t.state));
  const recentTasks = tasks.filter((t) => !isActive(t.state));

  if (loading) {
    return (
      <Center h={200}>
        <Loader />
      </Center>
    );
  }

  if (error !== null) {
    return (
      <Alert icon={<IconAlertCircle size={16} />} color="red" mt="md">
        {error}
      </Alert>
    );
  }

  return (
    <Stack gap="lg">
      <Group gap="sm">
        <IconListCheck size={24} />
        <Title order={2}>{t('title')}</Title>
      </Group>

      {tasks.length === 0 ? (
        <Text c="dimmed" ta="center" py="xl">
          {t('empty')}
        </Text>
      ) : (
        <Stack gap="md">
          {activeTasks.length > 0 && (
            <Stack gap="xs">
              <Text fw={600} size="sm" c="dimmed">
                {t('sectionActive')}
              </Text>
              {activeTasks.map((task) => (
                <TaskRow key={task.id} task={task} onClick={() => navigate(kindRoute(task.kind))} />
              ))}
            </Stack>
          )}
          {recentTasks.length > 0 && (
            <Stack gap="xs">
              <Text fw={600} size="sm" c="dimmed">
                {t('sectionRecent')}
              </Text>
              {recentTasks.map((task) => (
                <TaskRow key={task.id} task={task} onClick={() => navigate(kindRoute(task.kind))} />
              ))}
            </Stack>
          )}
        </Stack>
      )}
    </Stack>
  );
}
