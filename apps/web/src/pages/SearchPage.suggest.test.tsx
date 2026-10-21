// FR-153 前端交互回归（规格 docs/specs/search-index-and-performance.md §3.9）：
// - 防抖 ≤ 300ms：连续键入只发最后一次请求；去空白后不足 2 字符不发请求；
// - AbortController + 请求序号：旧响应晚回不得覆盖新候选；
// - 键盘 ↑/↓ 移动高亮、Enter 确认、Esc 收起、点击外部收起；
// - 无候选不弹空面板（不渲染「无结果」占位行）；
// - 确认语义：包名候选 → 写入输入框并直接搜索；仓库名候选 → 只改写表达式、不搜索；
// - 无障碍：role=combobox/listbox/option、aria-expanded、aria-activedescendant 随高亮移动；
// - 仅键入不刷新结果区（PRD 明确不做输入即搜）。
//
// 两个端点都换成 vi.fn（部分 mock，保留真实类型与其余导出）：suggest 的乱序与防抖需要
// 精确控制「哪个 promise 何时 resolve」，走 MSW 无法做到；搜索端点也一并 mock，
// 这样「是否发起了搜索」就是一次调用计数，而不是对网络时序的推断。
import { MantineProvider } from "@mantine/core";
import { act, fireEvent, render, screen } from "@testing-library/react";
import "@testing-library/jest-dom/vitest";
import { MemoryRouter } from "react-router-dom";
import { afterEach, beforeEach, describe, expect, it, vi, type Mock } from "vitest";

import type { SearchResult, SearchSuggestResult } from "../api/endpoints";
import { searchAssets, suggestSearch } from "../api/endpoints";
import "../i18n";
import { SUGGEST_DEBOUNCE_MS, SUGGEST_MIN_CHARS } from "../components/search/SearchSuggestInput";
import { SearchPage } from "./SearchPage";

vi.mock("../api/endpoints", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../api/endpoints")>();
  return { ...actual, searchAssets: vi.fn(), suggestSearch: vi.fn() };
});

const suggestMock = suggestSearch as unknown as Mock;
const searchMock = searchAssets as unknown as Mock;

/** 一次已发出但尚未落地的 suggest 请求；resolve/reject 由用例显式控制。 */
interface PendingSuggest {
  q: string;
  limit?: number;
  signal?: AbortSignal;
  resolve: (value: SearchSuggestResult) => void;
  reject: (reason?: unknown) => void;
}

let pending: PendingSuggest[] = [];

const EMPTY_SEARCH: SearchResult = { items: [], total: 0, facets: [] };

beforeEach(() => {
  // 假定时器：防抖窗口必须能被精确推进，而不是靠真实等待碰运气。
  vi.useFakeTimers();
  pending = [];
  suggestMock.mockReset();
  suggestMock.mockImplementation(
    (params: { q: string; limit?: number; signal?: AbortSignal }) =>
      new Promise<SearchSuggestResult>((resolve, reject) => {
        pending.push({ ...params, resolve, reject });
      }),
  );
  searchMock.mockReset();
  searchMock.mockResolvedValue(EMPTY_SEARCH);
});

afterEach(() => {
  vi.useRealTimers();
  localStorage.clear();
});

/** 推进防抖窗口并冲一遍微任务/渲染：让 setTimeout 到期、promise 链落地、React 提交。 */
async function flush(ms = SUGGEST_DEBOUNCE_MS): Promise<void> {
  await act(async () => {
    await vi.advanceTimersByTimeAsync(ms);
  });
  await act(async () => {
    await vi.advanceTimersByTimeAsync(0);
  });
}

/**
 * 渲染搜索页。初始 q 取 `seed` 而不是 `spring`：用例里键入的字符串必须与初始值不同，
 * 否则输入框值没变化、effect 不会重跑，测的就不是「键入触发建议」了。
 */
function renderPage(q = "seed") {
  return render(
    <MantineProvider>
      <MemoryRouter initialEntries={[`/search?q=${encodeURIComponent(q)}`]}>
        <SearchPage />
      </MemoryRouter>
    </MantineProvider>,
  );
}

const searchInput = () => screen.getByPlaceholderText(/搜索制品/) as HTMLInputElement;
const listbox = () => screen.queryByTestId("search-suggest-listbox");
const options = () => screen.queryAllByTestId("search-suggest-option");
/** 候选值（走 data-value，避免把「包名 / 仓库」类型标签一起读进来）。 */
const optionValues = () => options().map((o) => o.getAttribute("data-value") ?? "");

/** 键入（只改文本，不触发任何提交）。 */
function type(value: string): void {
  fireEvent.change(searchInput(), { target: { value } });
}

/** 按给定期望的完整候选集填充一次 suggest 响应。 */
function resolveSuggest(items: SearchSuggestResult["items"], index = pending.length - 1): void {
  pending[index]!.resolve({ items });
}

describe("SearchPage 搜索建议下拉（FR-153）", () => {
  it("首屏 URL 的 q 不触发建议请求（只响应真实键入）", async () => {
    renderPage("spring-core");
    await flush(SUGGEST_DEBOUNCE_MS * 2);
    // 进页面时输入框已被 URL 的 q 填满，但用户没动过输入框：不发请求、不弹面板。
    expect(pending).toHaveLength(0);
    expect(listbox()).toBeNull();
  });

  it("连续键入只发最后一次请求（防抖 ≤ 300ms）", async () => {
    renderPage();
    await flush(0);

    type("sp");
    await act(async () => {
      await vi.advanceTimersByTimeAsync(SUGGEST_DEBOUNCE_MS - 50);
    });
    // 第一段防抖还没到期就继续输入：前一次不应发出请求。
    type("spri");
    await act(async () => {
      await vi.advanceTimersByTimeAsync(SUGGEST_DEBOUNCE_MS - 50);
    });
    type("springb");
    expect(pending).toHaveLength(0);

    await flush();
    expect(pending).toHaveLength(1);
    expect(pending[0]!.q).toBe("springb");
    // 防抖窗口不得超过 300ms（规格硬上限）。
    expect(SUGGEST_DEBOUNCE_MS).toBeLessThanOrEqual(300);
  });

  it("去空白后不足 2 字符不发请求", async () => {
    renderPage();
    await flush(0);

    type(" s ");
    await flush();
    expect(pending).toHaveLength(0);
    expect(SUGGEST_MIN_CHARS).toBe(2);

    // 清空输入：同样不发请求，且面板收起。
    type("");
    await flush();
    expect(pending).toHaveLength(0);
    expect(listbox()).toBeNull();
  });

  it("旧响应晚回不覆盖新候选（乱序保护）", async () => {
    renderPage();
    await flush(0);

    type("spring");
    await flush();
    type("springboot");
    await flush();
    expect(pending).toHaveLength(2);

    // 新请求先落地。
    resolveSuggest([{ type: "package", value: "spring-boot" }], 1);
    await flush(0);
    expect(optionValues()).toEqual(["spring-boot"]);

    // 旧请求随后才回来：不得覆盖新候选。
    resolveSuggest([{ type: "package", value: "spring-old" }], 0);
    await flush(0);
    expect(optionValues()).toEqual(["spring-boot"]);
  });

  it("↑/↓ 在候选间移动并高亮，Esc 收起", async () => {
    renderPage();
    await flush(0);
    type("spring");
    await flush();
    resolveSuggest([
      { type: "repository", value: "repo-a" },
      { type: "package", value: "pkg-a" },
      { type: "package", value: "pkg-b" },
    ]);
    await flush(0);

    const input = searchInput();
    expect(input).toHaveAttribute("aria-expanded", "true");
    expect(listbox()).not.toBeNull();

    // 初始无高亮：没有 activedescendant。
    expect(input).not.toHaveAttribute("aria-activedescendant");

    fireEvent.keyDown(input, { key: "ArrowDown" });
    expect(options()[0]).toHaveAttribute("aria-selected", "true");
    expect(input.getAttribute("aria-activedescendant")).toBe(options()[0]!.id);

    fireEvent.keyDown(input, { key: "ArrowDown" });
    expect(options()[1]).toHaveAttribute("aria-selected", "true");
    expect(input.getAttribute("aria-activedescendant")).toBe(options()[1]!.id);

    // ↑ 从第一项往回绕到最后一项。
    fireEvent.keyDown(input, { key: "ArrowUp" });
    fireEvent.keyDown(input, { key: "ArrowUp" });
    expect(options()[2]).toHaveAttribute("aria-selected", "true");

    fireEvent.keyDown(input, { key: "Escape" });
    expect(listbox()).toBeNull();
    expect(input).toHaveAttribute("aria-expanded", "false");
  });

  it("无候选时不渲染面板（不出现空占位）", async () => {
    renderPage();
    await flush(0);
    type("spring");
    await flush();
    resolveSuggest([]);
    await flush(0);

    expect(listbox()).toBeNull();
    expect(searchInput()).toHaveAttribute("aria-expanded", "false");
  });

  it("包名候选确认：写入输入框并直接发起搜索", async () => {
    renderPage();
    await flush(0);
    expect(searchMock).toHaveBeenCalledTimes(1); // 首屏那一次

    type("spring");
    await flush();
    resolveSuggest([{ type: "package", value: "spring-core" }]);
    await flush(0);

    fireEvent.mouseDown(options()[0]!);
    await flush(0);

    expect(searchInput().value).toBe("spring-core");
    // 直接发起搜索：searchAssets 再次被调用，且关键词就是候选值。
    expect(searchMock).toHaveBeenCalledTimes(2);
    const lastCall = searchMock.mock.calls.at(-1)![0] as { q: string };
    expect(lastCall.q).toBe("spring-core");
    expect(listbox()).toBeNull();
  });

  it("仓库名候选确认：只改写表达式、不发起搜索", async () => {
    renderPage();
    await flush(0);
    expect(searchMock).toHaveBeenCalledTimes(1);

    type("maven");
    await flush();
    resolveSuggest([{ type: "repository", value: "maven-releases" }]);
    await flush(0);

    fireEvent.mouseDown(options()[0]!);
    await flush(0);

    // 复用 buildSearchExpression 写回：当前输入的关键词保留，追加 repo: 限定。
    expect(searchInput().value).toBe("maven repo:maven-releases");
    // 不立即搜索：结果区仍停在原来的口径上。
    expect(searchMock).toHaveBeenCalledTimes(1);
    expect(listbox()).toBeNull();
  });

  it("Enter 确认高亮项；无高亮时 Enter 仍是提交搜索", async () => {
    renderPage();
    await flush(0);

    type("maven");
    await flush();
    resolveSuggest([{ type: "repository", value: "maven-releases" }]);
    await flush(0);

    const input = searchInput();
    fireEvent.keyDown(input, { key: "ArrowDown" });
    fireEvent.keyDown(input, { key: "Enter" });
    await flush(0);
    expect(input.value).toBe("maven repo:maven-releases");
    expect(searchMock).toHaveBeenCalledTimes(1);

    // 面板已收起后回车：按输入框当前内容提交，发起一次新搜索。
    fireEvent.keyDown(input, { key: "Enter" });
    await flush(0);
    expect(searchMock).toHaveBeenCalledTimes(2);
  });

  it("仅键入不刷新结果区（不做输入即搜）", async () => {
    renderPage();
    await flush(0);
    expect(searchMock).toHaveBeenCalledTimes(1);

    type("springcloud");
    await flush();
    resolveSuggest([{ type: "package", value: "spring-cloud" }]);
    await flush(0);
    expect(options()).toHaveLength(1);

    // 键入 + 拿到候选，但结果区一次都没重新请求。
    expect(searchMock).toHaveBeenCalledTimes(1);
  });

  it("点击外部收起面板", async () => {
    renderPage();
    await flush(0);
    type("spring");
    await flush();
    resolveSuggest([{ type: "package", value: "spring-core" }]);
    await flush(0);
    expect(listbox()).not.toBeNull();

    fireEvent.mouseDown(document.body);
    expect(listbox()).toBeNull();
  });
});
