// 通知中心（FR-132）：页眉右上角图标+下拉，轮询 GET /api/v1/tasks 对比快照，
// 状态跃迁时推 @mantine/notifications 通知（通知文案用 taskCenter i18n）。
// 轮询间隔 5s；仅 Admin 可见；轮询错误静默忽略。

import { useEffect, useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { ActionIcon, Badge, Group, Menu, Text, ThemeIcon, Tooltip } from '@mantine/core';
import { IconBell, IconArrowsExchange, IconArrowUpCircle, IconShield } from '@tabler/icons-react';
import { useNavigate } from 'react-router-dom';
import * as api from '../api/endpoints';
import type { TaskRecord, TaskKind, TaskState } from '../api/types';
import { notifySuccess, notifyError } from '../lib/notify';

/** 通知中心轮询间隔（毫秒）。 */
const POLL_INTERVAL_MS = 5000;

/** 任务类型图标（用于下拉列表）。 */
function KindIcon({ kind }: { kind: TaskKind }) {
  switch (kind) {
    case 'migration':
      return <IconArrowsExchange size={14} />;
    case 'update':
      return <IconArrowUpCircle size={14} />;
    case 'vuln':
      return <IconShield size={14} />;
  }
}

/** 任务状态 → Badge 颜色。 */
function stateColor(state: TaskState): string {
  switch (state) {
    case 'running':
      return 'blue';
    case 'paused':
      return 'yellow';
    case 'succeeded':
      return 'green';
    case 'failed':
      return 'red';
    case 'cancelled':
      return 'gray';
  }
}

/** 任务类别 → 中文名（不调用 hook，纯函数）。 */
function kindLabel(kind: TaskKind): string {
  switch (kind) {
    case 'migration':
      return 'Nexus 迁移';
    case 'update':
      return '在线更新';
    case 'vuln':
      return '漏洞库刷新';
  }
}

/** 任务状态 → 中文描述。 */
function stateLabel(state: TaskState): string {
  switch (state) {
    case 'running':
      return '运行中';
    case 'paused':
      return '已暂停';
    case 'succeeded':
      return '已完成';
    case 'failed':
      return '失败';
    case 'cancelled':
      return '已取消';
  }
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

/**
 * 通知中心组件（FR-132）。
 * - 仅 Admin 可见（isAdmin=false 时渲染 null）。
 * - 轮询 GET /api/v1/tasks（5s），对比快照 Map<id, state> 判断状态跃迁并推通知。
 * - 图标按钮 + Mantine Menu 下拉展示最近 10 条任务概要；底部「查看全部」→ /tasks。
 */
export function NotificationCenter({ isAdmin }: { isAdmin: boolean }) {
  const { t } = useTranslation('taskCenter');
  const navigate = useNavigate();
  const [tasks, setTasks] = useState<TaskRecord[]>([]);
  // 用 ref 维持上次快照，避免 effect 依赖 tasks state 导致重复注册定时器
  const prevSnapshot = useRef<Map<string, TaskState>>(new Map());

  useEffect(() => {
    if (!isAdmin) return;

    let cancelled = false;

    /** 比较快照，状态跃迁时推通知。 */
    const processSnapshot = (current: TaskRecord[]) => {
      const prev = prevSnapshot.current;
      current.forEach((task) => {
        const prevState = prev.get(task.id);
        const label = task.label ?? kindLabel(task.kind);
        if (prevState === undefined) {
          // 新出现的任务：若为 running 则推「已开始」
          if (task.state === 'running') {
            notifySuccess(t('notifyStarted', { label }));
          }
        } else if (prevState !== task.state) {
          // 状态跃迁
          if (task.state === 'succeeded') {
            notifySuccess(t('notifySucceeded', { label }));
          } else if (task.state === 'failed') {
            notifyError(t('notifyFailed', { label }));
          } else if (task.state === 'cancelled') {
            notifySuccess(t('notifyCancelled', { label }));
          }
        }
      });
      // 更新快照
      const newMap = new Map<string, TaskState>();
      current.forEach((task) => newMap.set(task.id, task.state));
      prevSnapshot.current = newMap;
    };

    // 首次加载（建立基线快照，不推通知）
    api
      .listTasks()
      .then((result) => {
        if (cancelled) return;
        // 首次只建快照，不推通知（避免启动时把历史任务都推一遍）
        const initMap = new Map<string, TaskState>();
        result.forEach((task) => initMap.set(task.id, task.state));
        prevSnapshot.current = initMap;
        setTasks(result);
      })
      .catch(() => {
        // 静默忽略
      });

    // 轮询
    const timer = setInterval(() => {
      api
        .listTasks()
        .then((result) => {
          if (cancelled) return;
          processSnapshot(result);
          setTasks(result);
        })
        .catch(() => {
          // 静默忽略
        });
    }, POLL_INTERVAL_MS);

    return () => {
      cancelled = true;
      clearInterval(timer);
    };
  }, [isAdmin, t]);

  if (!isAdmin) return null;

  const recentTasks = tasks.slice(0, 10);

  return (
    <Menu shadow="md" width={300} position="bottom-end">
      <Menu.Target>
        <Tooltip label={t('notificationCenterAriaLabel')} position="bottom">
          <ActionIcon variant="subtle" size="lg" aria-label={t('notificationCenterAriaLabel')}>
            <IconBell size={18} />
          </ActionIcon>
        </Tooltip>
      </Menu.Target>
      <Menu.Dropdown>
        <Menu.Label>{t('title')}</Menu.Label>
        {recentTasks.length === 0 ? (
          <Menu.Item disabled>
            <Text size="sm" c="dimmed">
              {t('noRecentTasks')}
            </Text>
          </Menu.Item>
        ) : (
          recentTasks.map((task) => (
            <Menu.Item
              key={task.id}
              leftSection={
                <ThemeIcon variant="light" size="xs" color="blue">
                  <KindIcon kind={task.kind} />
                </ThemeIcon>
              }
              onClick={() => navigate(kindRoute(task.kind))}
            >
              <Group justify="space-between" wrap="nowrap" gap="xs">
                <Text size="sm" truncate="end" style={{ minWidth: 0, flex: 1 }}>
                  {task.label ?? kindLabel(task.kind)}
                </Text>
                <Badge
                  color={stateColor(task.state)}
                  variant="light"
                  size="xs"
                  style={{ flexShrink: 0 }}
                >
                  {stateLabel(task.state)}
                </Badge>
              </Group>
            </Menu.Item>
          ))
        )}
        <Menu.Divider />
        <Menu.Item onClick={() => navigate('/tasks')}>
          <Text size="sm" c="blue">
            {t('viewAll')}
          </Text>
        </Menu.Item>
      </Menu.Dropdown>
    </Menu>
  );
}
