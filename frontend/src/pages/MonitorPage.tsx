// 统一监控页（FR-99，UX 重构 epic）：顶部 tab 切换四区，整合可观测性视图，仅 Admin。
//
// 四个 tab：
// - 主机监控（新，消费 FR-98 GET /api/v1/monitor/host）：HostMonitorPanel；
// - 使用分析（整合 FR-58）：复用既有 AnalyticsPage 组件；
// - 审计（整合 FR-77）：复用既有 AuditPage 组件；
// - 防护（整合 FR-78）：复用既有 ProtectionMonitorPage 组件。
//
// 复用而非重写：三页均为无 props、自带数据加载的自包含组件，直接作为 tab 面板内容挂载，
// 数据层零改动，其既有测试不回归。各 tab 面板按需挂载（keepMounted={false}），切到才拉数据。
// 路由已由 RequireAdmin 守卫（仅 Admin 可达）。图表手搓 SVG/CSS，零新增依赖。数据本机内部、不外发。

import { useState } from 'react';
import { Stack, Title, Tabs } from '@mantine/core';
import { IconServer, IconChartBar, IconHistory, IconShield } from '@tabler/icons-react';
import { HostMonitorPanel } from '../components/HostMonitorPanel';
import { AnalyticsPage } from './AnalyticsPage';
import { AuditPage } from './AuditPage';
import { ProtectionMonitorPage } from './ProtectionMonitorPage';

/** tab 取值（用作 URL 无关的内部状态键）。 */
type MonitorTab = 'host' | 'analytics' | 'audit' | 'protection';

/** 统一监控页。 */
export function MonitorPage() {
  const [tab, setTab] = useState<MonitorTab>('host');

  return (
    <Stack>
      <Title order={2}>监控</Title>
      <Tabs value={tab} onChange={(v) => setTab((v as MonitorTab) ?? 'host')} keepMounted={false}>
        <Tabs.List>
          <Tabs.Tab value="host" leftSection={<IconServer size={16} />}>
            主机监控
          </Tabs.Tab>
          <Tabs.Tab value="analytics" leftSection={<IconChartBar size={16} />}>
            使用分析
          </Tabs.Tab>
          <Tabs.Tab value="audit" leftSection={<IconHistory size={16} />}>
            审计
          </Tabs.Tab>
          <Tabs.Tab value="protection" leftSection={<IconShield size={16} />}>
            防护
          </Tabs.Tab>
        </Tabs.List>

        <Tabs.Panel value="host" pt="md">
          <HostMonitorPanel />
        </Tabs.Panel>
        <Tabs.Panel value="analytics" pt="md">
          <AnalyticsPage />
        </Tabs.Panel>
        <Tabs.Panel value="audit" pt="md">
          <AuditPage />
        </Tabs.Panel>
        <Tabs.Panel value="protection" pt="md">
          <ProtectionMonitorPage />
        </Tabs.Panel>
      </Tabs>
    </Stack>
  );
}
