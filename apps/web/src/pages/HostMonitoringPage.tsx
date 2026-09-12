// 主机监控页：开发与生产统一读取当前主机真实观测接口。

import { HostMonitoringLive } from "../components/observability/HostMonitoringLive";

export function HostMonitoringPage() {
  return (
    <>
      <HostMonitoringLive />
    </>
  );
}
