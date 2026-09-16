// DevMock 控制台：右下角入口 + 数据量 × 网速 3×3 矩阵。
//
// 仅开发态挂载（App.tsx 以 import.meta.env.DEV 为条件懒加载，不进生产包）。
// 切档后会清空页面数据缓存并派发全局刷新事件——否则看到的仍是旧档位拉回的数据。
//
// 三个易用性设计：
// 1. 矩阵自解释：行标「少量 ×1」、列表头「快 0ms」——数字来自 devmock 的档位常量，不在这里另抄一份；
// 2. 面板可拖拽：右下角默认位置会盖住内容，按住标题拖动即可挪开（收起态则是图标球本身），
//    位置记在 localStorage，双击标题归位；
// 3. 「重置」：把 mock 数据恢复初始种子 + 档位回默认（small/fast）——改坏数据后不用刷新页面找后门。
import {
  ActionIcon,
  Box,
  Button,
  Card,
  Group,
  Stack,
  Text,
  Tooltip,
  UnstyledButton,
} from "@mantine/core";
import { useMediaQuery } from "@mantine/hooks";
import { IconAdjustments, IconCheck, IconRefresh, IconRestore, IconX } from "@tabler/icons-react";
import { useEffect, useRef, useState } from "react";

import {
  MOCK_SPEED_DELAYS,
  MOCK_VOLUME_FACTORS,
  getMockConsole,
  setMockConsole,
  subscribeMockConsole,
  type MockConsoleConfig,
  type MockSpeed,
  type MockVolume,
} from "@jianartifact/devmock/console";
import { resetStore } from "@jianartifact/devmock/store";

import { REFRESH_EVENT, invalidateAsyncCache } from "../hooks/useAsync";

const VOLUMES: MockVolume[] = ["small", "medium", "large"];
const SPEEDS: MockSpeed[] = ["fast", "medium", "slow"];

const VOLUME_LABEL: Record<MockVolume, string> = {
  small: "少量",
  medium: "中量",
  large: "大量",
};
const SPEED_LABEL: Record<MockSpeed, string> = {
  fast: "快",
  medium: "中",
  slow: "慢",
};

const POS_KEY = "jianartifact.devmock.pos";
const DEFAULT_VOLUME: MockVolume = "small";
const DEFAULT_SPEED: MockSpeed = "fast";

/** 触发全站重拉：切档后必须清页面缓存 + 派发刷新事件，否则页面仍展示旧档位的数据。 */
function reloadAllPages() {
  invalidateAsyncCache();
  window.dispatchEvent(new CustomEvent(REFRESH_EVENT));
}

/** 延迟展示：0 → 0ms，400 → 400ms，1500 → 1.5s。 */
function delayLabel(ms: number) {
  return ms >= 1000 ? `${ms / 1000}s` : `${ms}ms`;
}

type Pos = { x: number; y: number };

function readPos(): Pos | null {
  try {
    if (typeof localStorage === "undefined") return null;
    const raw = localStorage.getItem(POS_KEY);
    if (!raw) return null;
    const parsed = JSON.parse(raw) as Partial<Pos>;
    return typeof parsed.x === "number" && typeof parsed.y === "number"
      ? { x: parsed.x, y: parsed.y }
      : null;
  } catch {
    return null;
  }
}

/** 夹在视口内，保证面板永远能被点到（拖出屏幕就再也抓不回来了）。 */
function clampPos(pos: Pos, width: number, height: number): Pos {
  const margin = 4;
  const maxX = Math.max(margin, window.innerWidth - width - margin);
  const maxY = Math.max(margin, window.innerHeight - height - margin);
  return {
    x: Math.min(Math.max(pos.x, margin), maxX),
    y: Math.min(Math.max(pos.y, margin), maxY),
  };
}

export function DevMockConsole() {
  const [config, setConfig] = useState<MockConsoleConfig>(() => getMockConsole());
  const [opened, setOpened] = useState(false);
  const [pos, setPos] = useState<Pos | null>(() => readPos());
  const [dragging, setDragging] = useState(false);
  // 窄屏整体收一号：默认位置在手机上会明显盖住右下角内容（拖走或收起即可）。
  const isNarrow = useMediaQuery("(max-width: 48em)") ?? false;

  const boxRef = useRef<HTMLDivElement>(null);
  const dragRef = useRef<{ dx: number; dy: number; startX: number; startY: number } | null>(null);
  const lastPosRef = useRef<Pos | null>(null);
  // 拖过之后紧跟的 click 是拖拽的副产物，不能当成"点击打开面板"。
  const movedRef = useRef(false);

  useEffect(() => subscribeMockConsole(setConfig), []);

  // 视口变化（旋屏 / 改窗口）后把面板拉回可视范围。
  useEffect(() => {
    const onResize = () => {
      const el = boxRef.current;
      if (!el) return;
      setPos((current) => (current ? clampPos(current, el.offsetWidth, el.offsetHeight) : current));
    };
    window.addEventListener("resize", onResize);
    return () => window.removeEventListener("resize", onResize);
  }, []);

  const startDrag = (event: React.PointerEvent) => {
    if (event.button !== 0) return;
    const el = boxRef.current;
    if (!el) return;
    const rect = el.getBoundingClientRect();
    dragRef.current = {
      dx: event.clientX - rect.left,
      dy: event.clientY - rect.top,
      startX: event.clientX,
      startY: event.clientY,
    };
    movedRef.current = false;
    setDragging(true);
    event.currentTarget.setPointerCapture(event.pointerId);
  };

  const onDrag = (event: React.PointerEvent) => {
    const drag = dragRef.current;
    const el = boxRef.current;
    if (!drag || !el) return;
    // 位移超过 4px 才算拖拽（否则轻微抖动会把后面的 click 吃掉、面板打不开）。
    if (Math.hypot(event.clientX - drag.startX, event.clientY - drag.startY) > 4) {
      movedRef.current = true;
    }
    const next = clampPos(
      { x: event.clientX - drag.dx, y: event.clientY - drag.dy },
      el.offsetWidth,
      el.offsetHeight,
    );
    lastPosRef.current = next;
    setPos(next);
  };

  const endDrag = () => {
    if (!dragRef.current) return;
    dragRef.current = null;
    setDragging(false);
    try {
      if (lastPosRef.current) localStorage.setItem(POS_KEY, JSON.stringify(lastPosRef.current));
    } catch {
      // 隐私模式下 localStorage 可能不可写，拖动本身仍应生效（只是不记忆）。
    }
  };

  const resetPosition = () => {
    if (movedRef.current) return;
    setPos(null);
    lastPosRef.current = null;
    try {
      localStorage.removeItem(POS_KEY);
    } catch {
      // 同上：清不掉也不影响本次归位。
    }
  };

  const apply = (volume: MockVolume, speed: MockSpeed) => {
    setMockConsole({ volume, speed });
    reloadAllPages();
  };

  /** 恢复初始种子数据 + 默认档位（等于"重开一个干净环境"，比手动改回各项快）。 */
  const resetAll = () => {
    setMockConsole({ volume: DEFAULT_VOLUME, speed: DEFAULT_SPEED });
    resetStore();
    reloadAllPages();
  };

  const activeDelay = MOCK_SPEED_DELAYS[config.speed];
  const activeFactor = MOCK_VOLUME_FACTORS[config.volume];
  // 入口图标不再带文字，档位与用途由这条文案承担（悬停可见 + 读屏可读）。
  const entryLabel = `打开 Mock 控制台（当前：${VOLUME_LABEL[config.volume]} ×${activeFactor} · ${
    SPEED_LABEL[config.speed]
  } ${delayLabel(activeDelay)}）`;

  return (
    <Box
      ref={boxRef}
      style={{
        position: "fixed",
        zIndex: 400,
        ...(pos
          ? { left: pos.x, top: pos.y }
          : { right: isNarrow ? 10 : 16, bottom: isNarrow ? 10 : 16 }),
      }}
    >
      {/* 窄屏 262：236 装不下「Mock 控制台 + 重载 + 收起」，标题会折成两行。 */}
      {opened ? (
        <Card withBorder shadow="md" radius="md" padding="sm" w={isNarrow ? 262 : 272}>
          <Group justify="space-between" mb="xs" wrap="nowrap">
            <Tooltip label="按住可拖动位置，双击归位" openDelay={400}>
              <Text
                size="sm"
                fw={600}
                onPointerDown={startDrag}
                onPointerMove={onDrag}
                onPointerUp={endDrag}
                onPointerCancel={endDrag}
                onDoubleClick={resetPosition}
                style={{
                  cursor: dragging ? "grabbing" : "grab",
                  userSelect: "none",
                  touchAction: "none",
                }}
              >
                Mock 控制台
              </Text>
            </Tooltip>
            <Group gap={4} wrap="nowrap">
              {/* 与全站按钮约定一致：图标 + 文字（纯图标看不出是重载还是关闭）。
                  文案取短的「重载 / 收起」，完整语义留在 aria-label 与 Tooltip 里。 */}
              <Tooltip label="按当前档位重拉本页数据">
                <Button
                  size="compact-xs"
                  variant="subtle"
                  aria-label="重载本页数据"
                  leftSection={<IconRefresh size={14} />}
                  onClick={reloadAllPages}
                >
                  重载
                </Button>
              </Tooltip>
              <Button
                size="compact-xs"
                variant="subtle"
                aria-label="收起控制台"
                leftSection={<IconX size={14} />}
                onClick={() => setOpened(false)}
              >
                收起
              </Button>
            </Group>
          </Group>

          {/* 列头：网速档位 + 各自延迟（两行，矩阵因此自解释，不必悬停猜数字）。 */}
          <Group gap={4} wrap="nowrap" mb={4} align="flex-start">
            <Text size="xs" c="dimmed" w={64} style={{ flexShrink: 0 }}>
              数据量
            </Text>
            {SPEEDS.map((speed) => (
              <Stack key={speed} gap={0} style={{ flex: 1 }}>
                <Text size="xs" c="dimmed" ta="center" lh={1.2}>
                  {SPEED_LABEL[speed]}
                </Text>
                <Text size="xs" c="dimmed" ta="center" lh={1.2} opacity={0.7}>
                  {delayLabel(MOCK_SPEED_DELAYS[speed])}
                </Text>
              </Stack>
            ))}
          </Group>

          {/* 3×3 矩阵：行 = 数据量（含倍数），列 = 网速（含延迟），点击即同时设定两维 */}
          {VOLUMES.map((volume) => (
            <Group key={volume} gap={4} wrap="nowrap" mb={4}>
              <Text size="xs" c="dimmed" w={64} style={{ flexShrink: 0 }}>
                {VOLUME_LABEL[volume]} ×{MOCK_VOLUME_FACTORS[volume]}
              </Text>
              {SPEEDS.map((speed) => {
                const active = config.volume === volume && config.speed === speed;
                const label = `数据量${VOLUME_LABEL[volume]}（×${
                  MOCK_VOLUME_FACTORS[volume]
                }），网速${SPEED_LABEL[speed]}（${delayLabel(MOCK_SPEED_DELAYS[speed])}）`;
                return (
                  <UnstyledButton
                    key={speed}
                    onClick={() => apply(volume, speed)}
                    aria-pressed={active}
                    aria-label={label}
                    title={label}
                    style={{
                      flex: 1,
                      height: 30,
                      display: "inline-flex",
                      alignItems: "center",
                      justifyContent: "center",
                      borderRadius: "var(--mantine-radius-sm)",
                      border: `1px solid ${
                        active
                          ? "var(--mantine-color-blue-6)"
                          : "var(--mantine-color-default-border)"
                      }`,
                      background: active ? "var(--mantine-color-blue-light)" : "transparent",
                      color: active ? "var(--mantine-color-blue-8)" : undefined,
                    }}
                  >
                    {active ? <IconCheck size={14} /> : null}
                  </UnstyledButton>
                );
              })}
            </Group>
          ))}

          <Group justify="space-between" align="center" gap="xs" mt="xs" wrap="nowrap">
            <Text size="xs" c="dimmed" truncate>
              当前：{VOLUME_LABEL[config.volume]} ×{activeFactor} · {SPEED_LABEL[config.speed]}{" "}
              {delayLabel(activeDelay)}
            </Text>
            <Tooltip label="恢复初始种子数据与默认档位（少量 / 快）">
              <Button
                size="compact-xs"
                variant="subtle"
                aria-label="重置 mock 数据与档位"
                leftSection={<IconRestore size={14} />}
                onClick={resetAll}
              >
                重置
              </Button>
            </Tooltip>
          </Group>
        </Card>
      ) : (
        // 入口只留图标（不占文字宽度，不挡右下角内容）：用途与当前档位收进 Tooltip / aria-label。
        // 按住可拖动、双击归位（与展开态标题同一套指针逻辑）。
        <Tooltip label={entryLabel} openDelay={300}>
          <ActionIcon
            size={isNarrow ? 40 : 46}
            radius="xl"
            variant="filled"
            aria-label={entryLabel}
            onPointerDown={startDrag}
            onPointerMove={onDrag}
            onPointerUp={endDrag}
            onPointerCancel={endDrag}
            onClick={() => {
              if (movedRef.current) {
                movedRef.current = false;
                return;
              }
              setOpened(true);
            }}
          >
            <IconAdjustments size={isNarrow ? 18 : 20} />
          </ActionIcon>
        </Tooltip>
      )}
    </Box>
  );
}
