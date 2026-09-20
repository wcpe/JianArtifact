// 语言切换的端到端行为（页眉入口 → i18next → <html lang> → localStorage → 格式化输出）。
// 语言解析规则本身由 src/i18n/language.test.ts 覆盖，这里只验证「接线是否正确」。
import { cleanup, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it } from "vitest";

import { AppRoutes } from "../src/app/router";
import { actorText, formatFullTime, formatTime } from "../src/components/audit/labels";
import { currentLocaleTag } from "../src/i18n/current";
import { LANGUAGE_STORAGE_KEY } from "../src/i18n/language";
import i18n from "../src/i18n";
import { formatCount } from "../src/lib/format";
import { renderWithProviders } from "../test/harness";

afterEach(() => {
  // 语言是全局单例状态，用例后复位，避免影响后续文件。
  void i18n.changeLanguage("zh");
});

/** 找到页眉的语言切换按钮（桌面态显示目标语言的自称）。 */
function languageButton() {
  return screen.getByRole("button", { name: "切换语言" });
}

describe("页眉语言切换", () => {
  it("点击后在两种语言间切换，界面文案、<html lang> 与缓存三者同步", async () => {
    renderWithProviders(<AppRoutes />, { route: "/repositories" });

    // 基线：中文（test/setup.ts 把浏览器语言桩成 zh-CN）。
    expect(await screen.findByRole("button", { name: "刷新" })).toBeTruthy();
    expect(document.documentElement.lang).toBe("zh-CN");
    expect(localStorage.getItem(LANGUAGE_STORAGE_KEY)).toBeNull();

    await userEvent.click(languageButton());

    // 切到英文：页眉文案、<html lang>、偏好缓存同时变更。
    expect(await screen.findByRole("button", { name: "Refresh" })).toBeTruthy();
    expect(document.documentElement.lang).toBe("en");
    expect(localStorage.getItem(LANGUAGE_STORAGE_KEY)).toBe("en");

    // 切回中文。
    await userEvent.click(screen.getByRole("button", { name: "Switch language" }));
    expect(await screen.findByRole("button", { name: "刷新" })).toBeTruthy();
    expect(document.documentElement.lang).toBe("zh-CN");
    expect(localStorage.getItem(LANGUAGE_STORAGE_KEY)).toBe("zh");
  });

  it("显式选择在管理页生效，不被路由默认语言覆盖", async () => {
    // 管理页默认中文，用户显式选英文后应保持英文。
    renderWithProviders(<AppRoutes />, { route: "/settings", authenticated: true });

    await screen.findByRole("button", { name: "刷新" });
    await userEvent.click(languageButton());

    expect(await screen.findByRole("button", { name: "Refresh" })).toBeTruthy();
    await waitFor(() => expect(document.documentElement.lang).toBe("en"));
  });
});

describe("格式化输出跟随语言", () => {
  it("时间格式在切换后按当前语言重算，不沿用旧语言（模块级常量缺陷的回归守卫）", async () => {
    renderWithProviders(<AppRoutes />, { route: "/repositories" });
    await screen.findByRole("button", { name: "刷新" });

    expect(currentLocaleTag()).toBe("zh-CN");
    const zhTime = formatTime("2026-01-05T08:00:00Z");

    await userEvent.click(languageButton());
    await screen.findByRole("button", { name: "Refresh" });

    expect(currentLocaleTag()).toBe("en-US");
    // 审计时间此前由模块级 Intl.DateTimeFormat("zh-CN") 常量产出，切语言后不会重建；
    // 这条断言正是拦那个缺陷——两种语言下必须给出不同格式。
    const enTime = formatTime("2026-01-05T08:00:00Z");
    expect(enTime).not.toBe(zhTime);
    expect(enTime).not.toBe("—");
    // 计数型指标仍是千分位（两种 locale 的分组符都是逗号），确认入口切换后依然可用。
    expect(formatCount(12345)).toBe("12,345");
  });

  it("审计列表主时间列（formatFullTime）也跟随语言，而非手写拼接固定格式", async () => {
    renderWithProviders(<AppRoutes />, { route: "/repositories" });
    await screen.findByRole("button", { name: "刷新" });
    const zhFull = formatFullTime("2026-01-05T08:00:00Z");

    await userEvent.click(languageButton());
    await screen.findByRole("button", { name: "Refresh" });

    const enFull = formatFullTime("2026-01-05T08:00:00Z");
    // 宽屏审计表的时间列走 formatFullTime：它此前是手写 getFullYear/getMonth 拼接，
    // 完全绕过 locale，是本次收敛唯一的漏网路径。
    expect(enFull).not.toBe(zhFull);
    expect(enFull).not.toBe("—");
  });

  it("审计操作者的括号随语言：英文用半角，不出现中文全角括号", async () => {
    const actor = { displayName: "admin", authSource: "web" };

    // 断言括号形态而非具体文案：文案可随契约枚举演进，回归点是「英文界面不得出现
    // 中文全角括号」（此前括号是硬编码在模板里的，与语言无关）。
    await i18n.changeLanguage("zh");
    const zh = actorText(actor, i18n.t.bind(i18n));
    expect(zh).toContain("admin");
    expect(zh).toMatch(/（.+）/);

    await i18n.changeLanguage("en");
    const en = actorText(actor, i18n.t.bind(i18n));
    expect(en).toContain("admin");
    expect(en).not.toMatch(/[（）]/);
    expect(en).toMatch(/^admin \(.+\)$/);
  });

  it("偏好被记住：重新挂载后仍是用户选择的语言", async () => {
    localStorage.setItem(LANGUAGE_STORAGE_KEY, "en");
    renderWithProviders(<AppRoutes />, { route: "/repositories" });
    expect(await screen.findByRole("button", { name: "Refresh" })).toBeTruthy();
    expect(document.documentElement.lang).toBe("en");
    cleanup();
  });
});
