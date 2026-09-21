// 审计动作标签完整性守卫（防裸键回归）。
//
// 背景：仪表盘的「需要处理 / 最近活跃」面板曾直接渲染 `event.action` 原始串
// （如 asset.delete / management.write_rejected），且映射表缺 0.9.0 新增动作——
// 两处叠加导致界面出现裸 action 键。本测试锁两件事：
// 1) 映射表引用的 i18n 键在 zh 与 en 的 auditAction 段都必须存在
//    （否则 t(key) 会原样回显 "auditAction.xxx"，是裸键的另一副面孔）；
// 2) 已知审计动作（含 0.9.0 新增）必须有映射。
import { describe, expect, it } from "vitest";

import { ACTION_LABEL_KEYS, actionLabel } from "../src/components/audit/labels";
import { en } from "../src/i18n/en";
import { zh } from "../src/i18n/zh";

describe("审计动作标签完整性", () => {
  it("映射表引用的 i18n 键在 zh 与 en 中都存在", () => {
    const missing: string[] = [];
    for (const [action, key] of Object.entries(ACTION_LABEL_KEYS)) {
      const field = key.replace("auditAction.", "");
      for (const [lang, res] of [
        ["zh", zh],
        ["en", en],
      ] as const) {
        const value = (res.auditAction as Record<string, string>)[field];
        if (!value) missing.push(`${action} → ${lang}:${key}`);
      }
    }
    expect(missing, `以下映射缺失 i18n 键：\n${missing.join("\n")}`).toEqual([]);
  });

  it("已知审计动作均有映射（含 0.9.0 新增项）", () => {
    const known = [
      "asset.put",
      "asset.delete",
      "asset.move",
      "asset.rename",
      "repo.create",
      "repo.update",
      "repo.delete",
      "repo.recheck",
      "repo.online",
      "management.write_rejected",
      "acl.set",
      "audit.attention_acknowledge",
      "migration.start",
      "migration.complete",
      "user.password",
      "token.revoke",
      "settings.update",
    ];
    const unmapped = known.filter((action) => !ACTION_LABEL_KEYS[action]);
    expect(unmapped, `以下动作缺映射（会显示裸键）：${unmapped.join(", ")}`).toEqual([]);
  });

  it("未知动作原样返回（不误译、不报错）", () => {
    expect(actionLabel("unknown.action", (key) => key)).toBe("unknown.action");
    expect(actionLabel(undefined, (key) => key)).toBe("—");
  });
});
