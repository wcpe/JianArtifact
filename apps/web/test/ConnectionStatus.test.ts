// 连接状态映射完整性守卫（FR-114 + FR-43）。
//
// `CONN_COLOR` / `CONN_LABEL_KEY` 是 `Record<ConnectionStatusValue, string>` 穷举映射，
// 契约加枚举会直接编译失败（编译期已兜底）；本用例补运行期两块编译期管不到的事：
// 1) 映射引用的 i18n 键在 zh 与 en 都必须存在（否则界面回显 "repositories.statusXxx" 裸键）；
// 2) 「阻止态」判定必须同时覆盖 AUTO_BLOCKED 与 HALF_OPEN——后端 `blockedCount` 与告警
//    都按这两态统计，前端若只数 AUTO_BLOCKED，半开仓库会在读数里凭空消失。
import { describe, expect, it } from "vitest";

import type { ConnectionStatusValue } from "../src/api/types";
import {
  CONN_COLOR,
  CONN_LABEL_KEY,
  isBlockedStatus,
  UPSTREAM_BLOCKED_CODE,
} from "../src/lib/connectionStatus";
import { en } from "../src/i18n/en";
import { zh } from "../src/i18n/zh";

/** 契约里 ConnectionStatus.status 的全部取值，逐个钉住以防漏映射。 */
const ALL_STATUSES: ConnectionStatusValue[] = [
  "READY",
  "AVAILABLE",
  "UNAVAILABLE",
  "AUTO_BLOCKED",
  "HALF_OPEN",
  "OFFLINE",
];

describe("连接状态映射（FR-114 / FR-43）", () => {
  it("每个契约枚举都有颜色与标签键（含新增的 HALF_OPEN）", () => {
    for (const status of ALL_STATUSES) {
      expect(CONN_COLOR[status], `${status} 缺颜色映射`).toBeTruthy();
      expect(CONN_LABEL_KEY[status], `${status} 缺标签键映射`).toBeTruthy();
    }
  });

  it("映射引用的 i18n 键在 zh 与 en 中都存在", () => {
    const missing: string[] = [];
    for (const status of ALL_STATUSES) {
      const field = CONN_LABEL_KEY[status]!.replace("repositories.", "");
      for (const [lang, res] of [
        ["zh", zh],
        ["en", en],
      ] as const) {
        const value = (res.repositories as Record<string, string>)[field];
        if (!value) missing.push(`${status} → ${lang}:${CONN_LABEL_KEY[status]}`);
      }
    }
    expect(missing).toEqual([]);
  });

  it("半开单独成态：颜色与标签都与自动阻止不同", () => {
    // 两者同属阻止态，但「窗口内封锁」与「正在试探上游」必须一眼可辨。
    expect(CONN_COLOR.HALF_OPEN).not.toBe(CONN_COLOR.AUTO_BLOCKED);
    expect(CONN_LABEL_KEY.HALF_OPEN).not.toBe(CONN_LABEL_KEY.AUTO_BLOCKED);
  });

  it("阻止态判定同时覆盖 AUTO_BLOCKED 与 HALF_OPEN", () => {
    expect(isBlockedStatus("AUTO_BLOCKED")).toBe(true);
    expect(isBlockedStatus("HALF_OPEN")).toBe(true);
    // 其余状态都不是阻止态——尤其 AVAILABLE 不能因「半开也是阻止态」而被顺带算进去。
    for (const status of ALL_STATUSES.filter((s) => s !== "AUTO_BLOCKED" && s !== "HALF_OPEN")) {
      expect(isBlockedStatus(status), `${status} 不应算阻止态`).toBe(false);
    }
  });

  it("告警 code 与后端一致（半开复用同一告警，不新增 code）", () => {
    // 后端 HALF_OPEN 仍产生 `upstream_auto_blocked` 告警（operations_observability_handlers.go），
    // 前端去重面板据此过滤，故该常量必须保持唯一。
    expect(UPSTREAM_BLOCKED_CODE).toBe("upstream_auto_blocked");
  });
});
