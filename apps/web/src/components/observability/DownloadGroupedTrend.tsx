// 下载分组趋势卡（替代旧「下载累计趋势」独立图，与请求主图不再重复）：
// - 主体：契约端点 downloads/trend 的分组时序多系列折线，叠加请求总量对照线（右轴）；
//   请求侧无 IP 维度（protocol_metric_minute 无 IP 列），对照线只给总量——不编造请求 IP 分组；
// - 桶轴：按 effectiveBucket 从 from 到 to 建连续桶，稀疏 points 缺失桶计 0；
// - 分组收敛：按 totals 降序取前 7 组画线，其余合并「其他」，避免上百条 IP 糊死图；
// - 图例：复用 TrendChart 的 hiddenSeries/aria-pressed 交互（原生 button + 视觉类）；
// - 底部双饼（IP 占比 / 客户端族占比）：取端点 A 两次 groupBy 的 totals 全窗口降序；
//   扇区与图例行可点击 → 跳审计（IP 饼带 clientIp，族饼带关键字 q）；
// - 折线数据点可点击 → 跳审计（该桶 [from, to) + range=custom）；
// - 空态 / 409（下载计量未接线）/ 403 优雅降级，不阻断仪表盘其余部分。
import {
  CartesianGrid,
  Cell,
  Line,
  LineChart,
  Pie,
  PieChart,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from "recharts";
import { Alert, Box, Button, Group, Skeleton, Stack, Text } from "@mantine/core";
import { useCallback, useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { useNavigate } from "react-router-dom";

import { getDownloadTrend } from "../../api/endpoints";
import type { DownloadGroupedTrendResponse, ObservabilityBucket } from "../../api/types";
import { useAsync } from "../../hooks/useAsync";
import { buildAuditLink } from "../../lib/auditLink";
import { formatStamp } from "../../lib/format";

interface DownloadGroupedTrendProps {
  /** 统计窗口起点（ISO date-time，随仪表盘时间范围联动）。 */
  from: string;
  /** 统计窗口终点（ISO date-time）。 */
  to: string;
  /** 请求总量对照序列（仪表盘 requestTrend.request，label 已本地化）。 */
  requestSeries: Array<{ label: string; value: number }>;
}

/** 时序最多画 7 个分组 + 1 条「其他」：IP 维度组数可达上百，全画会糊死图。 */
const TOP_GROUPS = 7;
/** 饼图同样只展示前 7 组，其余占比合并为「其他」扇区。 */
const PIE_TOP = 7;
/** 桶轴护栏：极端窗口下桶数封顶，避免前端笛卡尔爆炸（正常档位远达不到）。 */
const MAX_BUCKETS = 5000;

/** 分组系列色板（亮暗通用的一组 Mantine 系 hex；SVG attribute 不支持 CSS var）。 */
const SERIES_COLORS = [
  "#1c7ed6",
  "#40c057",
  "#fa5252",
  "#f59f00",
  "#7950f2",
  "#12b886",
  "#e64980",
  "#868e96",
];
/** 「其他」与请求对照线的固定色（灰阶 + 深红虚线，视觉上次要）。 */
const OTHER_COLOR = "#adb5bd";
const REQUEST_COLOR = "#c92a2a";

/** 桶粒度 → 毫秒步长。 */
function bucketStepMs(bucket: ObservabilityBucket): number {
  if (bucket === "minute") return 60_000;
  if (bucket === "hour") return 3_600_000;
  return 86_400_000;
}

interface Bucket {
  /** 服务端聚合后的对齐桶起点，用于与 points[].from 精确匹配。 */
  keyMs: number;
  /** 跳转审计时使用的当前桶与请求窗口的交集。 */
  from: string;
  to: string;
}

/**
 * 按服务端 effectiveBucket 生成连续桶轴。
 * 服务端 minute 桶只纳入 bucket_start 落在 [from,to) 的行；hour/day 桶先按该粒度聚合，
 * 因此首桶可能早于 from。桶轴按相同规则对齐，避免精确时间戳与整点/分钟标签错位。
 */
export function buildBucketAxis(
  fromIso: string,
  toIso: string,
  bucket: ObservabilityBucket,
): Bucket[] {
  const step = bucketStepMs(bucket);
  const minuteMs = 60_000;
  const requestedStart = Date.parse(fromIso);
  const end = Date.parse(toIso);
  if (!Number.isFinite(requestedStart) || !Number.isFinite(end) || end <= requestedStart) return [];

  // 仓储只查询起点落在 [from,to) 的整分钟源桶；前端先按同一规则确定首尾源分钟，
  // 再映射到 minute/hour/day 分组键（整点/午夜），以匹配服务端 RFC3339Nano 桶标签。
  const firstSourceMinute = Math.ceil(requestedStart / minuteMs) * minuteMs;
  const endSourceMinuteExclusive = Math.ceil(end / minuteMs) * minuteMs;
  if (firstSourceMinute >= endSourceMinuteExclusive) return [];
  const firstBucket = Math.floor(firstSourceMinute / step) * step;
  const lastSourceMinute = endSourceMinuteExclusive - minuteMs;
  const lastBucket = Math.floor(lastSourceMinute / step) * step;

  const buckets: Bucket[] = [];
  for (
    let cursor = firstBucket;
    cursor <= lastBucket && buckets.length < MAX_BUCKETS;
    cursor += step
  ) {
    const rangeFrom = Math.max(cursor, requestedStart);
    const rangeTo = Math.min(cursor + step, end);
    buckets.push({
      keyMs: cursor,
      from: new Date(rangeFrom).toISOString(),
      to: new Date(rangeTo).toISOString(),
    });
  }
  return buckets;
}

/** 窗口桶粒度的回退步长（响应缺失时按窗口跨度粗估，正常响应必带 effectiveBucket）。 */
function fallbackBucket(fromIso: string, toIso: string): ObservabilityBucket {
  const span = Date.parse(toIso) - Date.parse(fromIso);
  if (!Number.isFinite(span) || span <= 6 * 3_600_000) return "minute";
  if (span <= 7 * 24 * 3_600_000) return "hour";
  return "day";
}
export function DownloadGroupedTrend({ from, to, requestSeries }: DownloadGroupedTrendProps) {
  const { t } = useTranslation();
  const navigate = useNavigate();
  /** 当前分组维度：family（UA 归类，默认）/ ip（来源 IP 明文，仅管理员）。 */
  const [groupBy, setGroupBy] = useState<"family" | "ip">("family");
  /** 已隐藏的系列名（图例点击切换；键为组名 / 「其他」 / 请求总量色值）。 */
  const [hidden, setHidden] = useState<Record<string, boolean>>({});
  /** 饼图图例行显隐（键为 标题:组名，扇区 fillOpacity 联动压暗）。 */
  const [pieHidden, setPieHidden] = useState<Record<string, boolean>>({});

  // 两个维度各拉一次：主图用当前 groupBy 的那份，双饼各用一份 totals（同窗口同口径）。
  const familyState = useAsync<DownloadGroupedTrendResponse>(
    () => getDownloadTrend({ from, to, groupBy: "family" }),
    [from, to],
    { cacheKey: `dashboard:download-trend:family:${from}:${to}` },
  );
  const ipState = useAsync<DownloadGroupedTrendResponse>(
    () => getDownloadTrend({ from, to, groupBy: "ip" }),
    [from, to],
    { cacheKey: `dashboard:download-trend:ip:${from}:${to}` },
  );
  const active = groupBy === "family" ? familyState : ipState;

  /** 切换图例显隐（与 TrendChart 的 toggleSeries 同构，只翻转目标键）。 */
  const toggleSeries = useCallback((name: string) => {
    setHidden((prev) => ({ ...prev, [name]: !prev[name] }));
  }, []);
  const togglePie = useCallback((key: string) => {
    setPieHidden((prev) => ({ ...prev, [key]: !prev[key] }));
  }, []);

  const data = active.data;
  const otherLabel = t("dashboard.downloadOther");

  // 主图行数据：桶轴铺满 + 分组回填（缺失桶 = 0）+ 请求对照按 label 对齐。
  const { rows, groups, hasOther } = useMemo(() => {
    if (!data) {
      return { rows: [] as Record<string, unknown>[], groups: [] as string[], hasOther: false };
    }
    const bucket = data.effectiveBucket ?? fallbackBucket(data.from, data.to);
    const buckets = buildBucketAxis(data.from, data.to, bucket);
    // 服务端 RFC3339Nano 会省略 .000，前端 ISO 字符串会保留毫秒；统一按 epoch 毫秒对桶键。
    const countMap = new Map<string, number>();
    for (const point of data.points) {
      const bucketKey = Date.parse(point.from);
      if (Number.isFinite(bucketKey)) countMap.set(`${point.group} ${bucketKey}`, point.count);
    }
    // 非前 7 组的点先按对齐桶归并，供「其他」在该桶求和（保持总量守恒）。
    const otherByBucket = new Map<number, number>();
    const top = data.totals.slice(0, TOP_GROUPS).map((item) => item.group);
    const rest = data.totals.length > TOP_GROUPS;
    if (rest) {
      for (const point of data.points) {
        const bucketKey = Date.parse(point.from);
        if (!top.includes(point.group) && Number.isFinite(bucketKey)) {
          otherByBucket.set(bucketKey, (otherByBucket.get(bucketKey) ?? 0) + point.count);
        }
      }
    }
    const reqMap = new Map(requestSeries.map((point) => [point.label, point.value]));
    const filled = buckets.map((b) => {
      const label = formatStamp(new Date(b.keyMs).toISOString());
      const row: Record<string, unknown> = { label, __from: b.from, __to: b.to };
      for (const group of top) {
        // 稀疏点缺失桶 = 0（服务端不做笛卡尔补零，前端在此对齐桶轴补齐）。
        row[group] = countMap.get(`${group} ${b.keyMs}`) ?? 0;
      }
      if (rest) row[otherLabel] = otherByBucket.get(b.keyMs) ?? 0;
      row.__req = reqMap.get(label) ?? null;
      return row;
    });
    return { rows: filled, groups: top, hasOther: rest };
  }, [data, requestSeries, otherLabel]);

  /** 时序图上实际画的系列名（含「其他」）。 */
  const seriesNames = hasOther ? [...groups, otherLabel] : groups;
  /** 可见（未被图例隐藏）的下载组数——全隐藏时补一句占位提示。 */
  const visibleCount = seriesNames.filter((name) => !hidden[name]).length;

  const settled = !active.loading && !active.error;
  const empty = settled && (rows.length === 0 || (data?.totals.length ?? 0) === 0);

  /** 数据点点击 → 跳审计：带该桶 [from, to) 与 range=custom。 */
  const openAuditForBucket = (payload: unknown) => {
    const record = payload as { __from?: string; __to?: string } | null;
    if (!record?.__from || !record.__to) return;
    navigate(buildAuditLink({ range: "custom", from: record.__from, to: record.__to }));
  };
  /** 饼扇区 / 饼图例行点击 → 跳审计：IP 饼带 clientIp，族饼带关键字 q（审计侧模糊匹配）。 */
  const openAuditForGroup = (pie: "ip" | "family", group: string) => {
    if (!group || group === otherLabel) return;
    const link =
      pie === "ip"
        ? buildAuditLink({ range: "custom", from: data?.from, to: data?.to, clientIp: group })
        : buildAuditLink({ range: "custom", from: data?.from, to: data?.to, q: group });
    navigate(link);
  };

  // —— 降级渲染：加载中 → 骨架；错误 → 403 / 409 / 其它提示；空 → 无样本。 ——
  if (active.loading && !data) {
    return (
      <Stack gap="xs" data-testid="download-grouped-trend">
        <Text fw={600}>{t("dashboard.downloadGroupedTitle")}</Text>
        <Skeleton height={200} radius="md" />
      </Stack>
    );
  }
  if (active.error && !data) {
    const status = active.error.status;
    // 403 = 无权限；409 = 下载计量未接线（或聚合范围过大）；其余按通用不可用处理。
    const degraded = status === 403 || status === 409;
    return (
      <Stack gap="xs" data-testid="download-grouped-trend">
        <Text fw={600}>{t("dashboard.downloadGroupedTitle")}</Text>
        <Alert
          color={degraded ? "yellow" : "red"}
          title={
            status === 403 ? t("dashboard.downloadForbidden") : t("dashboard.downloadUnavailable")
          }
        >
          <Text size="sm">{active.error.message}</Text>
        </Alert>
      </Stack>
    );
  }
  if (empty) {
    return (
      <Stack gap="xs" data-testid="download-grouped-trend">
        <Text fw={600}>{t("dashboard.downloadGroupedTitle")}</Text>
        <Box
          h={200}
          style={{
            display: "flex",
            alignItems: "center",
            justifyContent: "center",
            border: "1px dashed var(--mantine-color-gray-3)",
            borderRadius: "var(--mantine-radius-md)",
          }}
        >
          <Text size="sm" c="dimmed">
            {t("dashboard.downloadNoSamples")}
          </Text>
        </Box>
      </Stack>
    );
  }

  /** 全窗口 totals → 饼图扇区：前 7 组保留，其余合并「其他」（占比不丢，颜色一致）。 */
  const pieTotals = (
    totals: Array<{ group: string; count: number }> | undefined,
  ): Array<{ group: string; count: number }> => {
    const list = totals ?? [];
    const top = list.slice(0, PIE_TOP);
    const rest = list.slice(PIE_TOP);
    if (rest.length === 0) return top;
    return [...top, { group: otherLabel, count: rest.reduce((sum, item) => sum + item.count, 0) }];
  };
  const ipTotals = pieTotals(ipState.data?.totals);
  const familyTotals = pieTotals(familyState.data?.totals);

  return (
    <Stack gap="sm" data-testid="download-grouped-trend">
      {/* 标题 + 分组维度切换 + 图例（图例即显隐开关，aria-pressed 表达当前显示态） */}
      <Group justify="space-between" align="center" wrap="wrap">
        <Group gap="sm" align="center" wrap="nowrap">
          <Text fw={600}>{t("dashboard.downloadGroupedTitle")}</Text>
          <Group gap={4} wrap="nowrap">
            <Button
              size="compact-xs"
              variant={groupBy === "family" ? "filled" : "light"}
              aria-pressed={groupBy === "family"}
              onClick={() => setGroupBy("family")}
            >
              {t("dashboard.downloadGroupByFamily")}
            </Button>
            <Button
              size="compact-xs"
              variant={groupBy === "ip" ? "filled" : "light"}
              aria-pressed={groupBy === "ip"}
              onClick={() => setGroupBy("ip")}
            >
              {t("dashboard.downloadGroupByIp")}
            </Button>
          </Group>
        </Group>
        <Group gap={6} wrap="wrap">
          {seriesNames.map((name, index) => (
            <GroupLegend
              key={name}
              color={name === otherLabel ? OTHER_COLOR : (SERIES_COLORS[index] ?? OTHER_COLOR)}
              label={name}
              hidden={Boolean(hidden[name])}
              onToggle={() => toggleSeries(name)}
            />
          ))}
          <GroupLegend
            color={REQUEST_COLOR}
            label={t("dashboard.downloadRequestTotal")}
            hidden={Boolean(hidden[REQUEST_COLOR])}
            onToggle={() => toggleSeries(REQUEST_COLOR)}
          />
        </Group>
      </Group>

      {/* 分组时序 + 请求总量对照（右轴，避免请求量级压平下载系列）。 */}
      <Box
        role="img"
        aria-label={t("dashboard.downloadGroupedTitle")}
        style={{ width: "100%", height: 220 }}
      >
        <ResponsiveContainer width="100%" height="100%">
          <LineChart data={rows} margin={{ top: 8, right: 8, bottom: 0, left: 0 }}>
            <CartesianGrid strokeDasharray="3 3" vertical={false} stroke="#e9ecef" />
            <XAxis
              dataKey="label"
              tickLine={false}
              axisLine={false}
              tickMargin={8}
              minTickGap={32}
              tick={{ fontSize: 11, fill: "#909296" }}
            />
            <YAxis
              width={56}
              tickLine={false}
              axisLine={false}
              allowDecimals={false}
              tick={{ fontSize: 11, fill: "#909296" }}
            />
            <YAxis
              yAxisId="right"
              orientation="right"
              width={48}
              tickLine={false}
              axisLine={false}
              allowDecimals={false}
              tick={{ fontSize: 11, fill: "#909296" }}
            />
            <Tooltip
              cursor={{ stroke: "#1c7ed6", strokeWidth: 1, strokeDasharray: "4 4" }}
              isAnimationActive={false}
            />
            {/* 数据点点击 → 跳审计（该桶窗口）；Line 的 onClick payload 即行数据。 */}
            {seriesNames.map((name, index) => (
              <Line
                key={name}
                type="monotone"
                dataKey={name}
                name={name}
                hide={Boolean(hidden[name])}
                stroke={name === otherLabel ? OTHER_COLOR : (SERIES_COLORS[index] ?? OTHER_COLOR)}
                strokeWidth={2}
                dot={false}
                activeDot={{ r: 4, strokeWidth: 2, stroke: "#ffffff" }}
                isAnimationActive={false}
                onClick={(payload: unknown) => openAuditForBucket(payload)}
              />
            ))}
            <Line
              yAxisId="right"
              type="monotone"
              dataKey="__req"
              name={t("dashboard.downloadRequestTotal")}
              hide={Boolean(hidden[REQUEST_COLOR])}
              stroke={REQUEST_COLOR}
              strokeWidth={1.6}
              strokeDasharray="4 3"
              dot={false}
              activeDot={{ r: 3, strokeWidth: 2, stroke: "#ffffff" }}
              isAnimationActive={false}
              onClick={(payload: unknown) => openAuditForBucket(payload)}
            />
          </LineChart>
        </ResponsiveContainer>
      </Box>
      {/* 全系列隐藏时仍保留占位（不破坏布局，也不假装有数据）。 */}
      {visibleCount === 0 ? (
        <Text size="xs" c="dimmed">
          {t("dashboard.downloadAllHidden")}
        </Text>
      ) : null}

      {/* 底部双饼：IP 占比 / 客户端族占比（端点 A 两次 groupBy 的 totals）。 */}
      <Group grow align="flex-start" wrap="wrap">
        <PieBlock
          title={t("dashboard.downloadPieIp")}
          data={ipTotals}
          otherLabel={otherLabel}
          hidden={pieHidden}
          onToggle={togglePie}
          onPick={(group) => openAuditForGroup("ip", group)}
        />
        <PieBlock
          title={t("dashboard.downloadPieFamily")}
          data={familyTotals}
          otherLabel={otherLabel}
          hidden={pieHidden}
          onToggle={togglePie}
          onPick={(group) => openAuditForGroup("family", group)}
        />
      </Group>
    </Stack>
  );
}

/** 占比饼块：标题 + 饼 + 可点击图例行（aria-label 暴露组值，供测试与读屏定位）。 */
function PieBlock({
  title,
  data,
  otherLabel,
  hidden,
  onToggle,
  onPick,
}: {
  title: string;
  data: Array<{ group: string; count: number }>;
  otherLabel: string;
  hidden: Record<string, boolean>;
  onToggle: (key: string) => void;
  onPick: (group: string) => void;
}) {
  const { t } = useTranslation();
  if (data.length === 0) {
    return (
      <Stack gap={4}>
        <Text size="xs" fw={600} c="dimmed">
          {title}
        </Text>
        <Text size="xs" c="dimmed">
          {t("dashboard.downloadNoSamples")}
        </Text>
      </Stack>
    );
  }
  const total = data.reduce((sum, item) => sum + item.count, 0) || 1;
  return (
    <Stack gap={4}>
      <Text size="xs" fw={600} c="dimmed">
        {title}
      </Text>
      <Box style={{ height: 150 }} role="img" aria-label={title}>
        <ResponsiveContainer width="100%" height="100%">
          <PieChart>
            <Pie
              data={data.map((item) => ({ name: item.group, value: item.count }))}
              dataKey="value"
              nameKey="name"
              cx="50%"
              cy="50%"
              outerRadius={60}
              isAnimationActive={false}
              // 扇区点击 → 跳审计（「其他」是聚合桶，无单一可查值，不跳）。
              onClick={(entry: unknown) => {
                const record = entry as { name?: string } | null;
                if (record?.name) onPick(record.name);
              }}
            >
              {data.map((item) => (
                <Cell
                  key={item.group}
                  fill={
                    item.group === otherLabel
                      ? OTHER_COLOR
                      : SERIES_COLORS[data.indexOf(item) % SERIES_COLORS.length]
                  }
                  // 隐藏态压暗：与图例 aria-pressed 联动，避免"图例灭了饼还在"。
                  fillOpacity={hidden[`${title}:${item.group}`] ? 0.25 : 1}
                />
              ))}
            </Pie>
            <Tooltip isAnimationActive={false} />
          </PieChart>
        </ResponsiveContainer>
      </Box>
      <Group gap={4} wrap="wrap">
        {data.map((item, index) => {
          const key = `${title}:${item.group}`;
          const isHidden = Boolean(hidden[key]);
          const pct = ((item.count / total) * 100).toFixed(1);
          return (
            <Box
              key={item.group}
              component="button"
              type="button"
              // 点击：跳审计 + 翻转显隐（饼图例同时承担两个交互，键盘原生可达）。
              onClick={() => {
                onPick(item.group);
                onToggle(key);
              }}
              aria-label={`${title} ${item.group}`}
              aria-pressed={!isHidden}
              className={isHidden ? "trend-legend trend-legend-hidden" : "trend-legend"}
              style={{
                display: "inline-flex",
                alignItems: "center",
                gap: 4,
                padding: "1px 5px",
                border: "none",
                borderRadius: "var(--mantine-radius-sm)",
                background: "transparent",
                cursor: "pointer",
                opacity: isHidden ? 0.45 : 1,
              }}
            >
              <Box
                w={8}
                h={8}
                style={{
                  background:
                    item.group === otherLabel
                      ? OTHER_COLOR
                      : SERIES_COLORS[index % SERIES_COLORS.length],
                  borderRadius: "50%",
                  flexShrink: 0,
                }}
              />
              <Text size="xs" c={isHidden ? "dimmed" : undefined}>
                {item.group} · {pct}%
              </Text>
            </Box>
          );
        })}
      </Group>
    </Stack>
  );
}
/**
 * 图例项：与 TrendChart 的 ChartLegend 同构——原生 button 保证键盘可达，
 * aria-pressed 表达「该系列当前显示」，隐藏态加 trend-legend-hidden 视觉类。
 */
function GroupLegend({
  color,
  label,
  hidden,
  onToggle,
}: {
  color: string;
  label: string;
  hidden: boolean;
  onToggle: () => void;
}) {
  return (
    <Box
      component="button"
      type="button"
      onClick={onToggle}
      aria-pressed={!hidden}
      aria-label={label}
      className={hidden ? "trend-legend trend-legend-hidden" : "trend-legend"}
      style={{
        display: "inline-flex",
        alignItems: "center",
        gap: 4,
        padding: "2px 6px",
        border: "none",
        borderRadius: "var(--mantine-radius-sm)",
        background: "transparent",
        cursor: "pointer",
        opacity: hidden ? 0.45 : 1,
      }}
    >
      <Box w={8} h={8} style={{ background: color, borderRadius: "50%", flexShrink: 0 }} />
      <Text
        size="xs"
        c={hidden ? "dimmed" : undefined}
        style={{ textDecorationLine: hidden ? "line-through" : undefined }}
      >
        {label}
      </Text>
    </Box>
  );
}
