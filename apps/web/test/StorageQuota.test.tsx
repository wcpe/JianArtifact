// FR-41 存储配额（前端）集成测试：
// 1) 仓库详情「配置」页签——用量/上限只读展示、状态区分（正常/接近上限/已超限）、
//    带单位的编辑（GB/MB/字节，0 或留空 = 不限）、保存换算、非法输入拦截、失败提示；
// 2) 页头只读配额徽章——超限/接近上限对所有登录用户可见，非管理员仍不能编辑；
// 3) 仓库列表——用量列同时给出「当前用量 / 上限」，状态落到 data-quota-state；
// 4) 治理字段的类型约束——配额两项（存储上限 / 制品数上限）只在 hosted 类型出现与提交
//    （group 不承载写入、proxy 的缓存在回源路径上，后端对非 hosted 的非 0 配额直接 400），
//    代理缓存保留天数只在 proxy 类型出现与提交；详情页配置页签与新建仓库弹窗各覆盖一遍。
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { describe, expect, it, vi } from "vitest";
import { Route, Routes } from "react-router-dom";

import { store } from "@jianartifact/devmock";
import { server } from "@jianartifact/devmock/node";
import { RepositoriesPage } from "../src/pages/RepositoriesPage";
import { RepositoryDetailPage } from "../src/pages/RepositoryDetailPage";
import { renderWithProviders } from "./harness";

/** 非管理员会话快照：能看列表与详情，但看不到配置页签（只读展示配额状态）。 */
const MEMBER_USER = {
  id: 2,
  username: "developer",
  role: "user" as const,
  status: "active" as const,
  createdAt: "2026-01-02T00:00:00Z",
};

function renderDetail(name: string, options: { user?: typeof MEMBER_USER } = {}) {
  return renderWithProviders(
    <Routes>
      <Route path="/repositories/:name" element={<RepositoryDetailPage />} />
    </Routes>,
    {
      route: `/repositories/${name}`,
      authenticated: true,
      user: options.user,
      token: options.user ? "mock.jwt.token:user" : undefined,
    },
  );
}

/** 渲染详情页、切到「配置」页签并等配额区块出现。 */
async function openQuotaSection(
  name = "maven-releases",
  options: { user?: typeof MEMBER_USER } = {},
) {
  const user = userEvent.setup();
  renderDetail(name, options);
  await user.click(await screen.findByRole("tab", { name: "配置" }));
  const section = await screen.findByTestId("repo-quota-section");
  return { user, section };
}

describe("FR-41 仓库配额：详情页配置区", () => {
  it("展示当前用量与上限，并按配额回显「数值 + 单位」", async () => {
    const { section } = await openQuotaSection();
    // 种子 maven-releases：artifactCount 1284 / quotaAssets 2000、totalSize 8 GiB / quotaBytes 10 GiB
    expect(within(section).getByText("存储用量：8.0 GB / 10.0 GB")).toBeTruthy();
    expect(within(section).getByText("制品用量：1,284 / 2,000")).toBeTruthy();
    // 8 GiB / 10 GiB = 80%，未到 90% 阈值 → 正常
    expect(within(section).getByTestId("repo-quota-state").textContent).toBe("正常");
    // 输入框按 GB 回显，管理员不必心算字节
    expect((screen.getByLabelText("存储上限") as HTMLInputElement).value).toBe("10");
    // Select 的选项 listbox 也带同一个 aria-labelledby，取第一个（输入框本身）。
    expect((screen.getAllByLabelText("容量单位")[0] as HTMLInputElement).value).toBe("GB");
    expect((screen.getByLabelText("制品数上限") as HTMLInputElement).value).toBe("2000");
    // 页头此时不该出现配额徽章（正常状态没有信息量）
    expect(screen.queryByTestId("repo-quota-badge")).toBeNull();
  });

  it("按 GB 填写上限后保存，提交换算后的字节数", async () => {
    let patchBody: { quotaBytes?: number; quotaAssets?: number } | null = null;
    server.use(
      http.patch("*/api/v1/repositories/:name", async ({ request }) => {
        patchBody = (await request.json()) as { quotaBytes?: number; quotaAssets?: number };
        return HttpResponse.json({
          id: 1,
          name: "maven-releases",
          format: "maven",
          type: "hosted",
          visibility: "private",
          createdAt: "2026-01-01T00:00:00Z",
          artifactCount: 1284,
          totalSize: 8589934592,
          quotaBytes: patchBody.quotaBytes,
          quotaAssets: patchBody.quotaAssets,
        });
      }),
    );
    const { user } = await openQuotaSection();

    const bytesInput = screen.getByLabelText("存储上限");
    await user.clear(bytesInput);
    await user.type(bytesInput, "2");
    // 友好提示：换算结果一眼可见（2 GB = 2147483648 字节）
    expect(await screen.findByText("= 2.0 GB（2,147,483,648 字节）")).toBeTruthy();

    await user.click(screen.getByRole("button", { name: "保存配置" }));
    expect(await screen.findByText("保存成功")).toBeTruthy();
    await waitFor(() => expect(patchBody?.quotaBytes).toBe(2147483648));
    // 未改动的制品数上限按原值提交（不是被顺手清空）
    expect(patchBody?.quotaAssets).toBe(2000);
  });

  it("上限留空 / 填 0 表示不限，并显式提交 0", async () => {
    let patchBody: { quotaBytes?: number; quotaAssets?: number } | null = null;
    server.use(
      http.patch("*/api/v1/repositories/:name", async ({ request }) => {
        patchBody = (await request.json()) as { quotaBytes?: number; quotaAssets?: number };
        return HttpResponse.json({
          id: 1,
          name: "maven-releases",
          format: "maven",
          type: "hosted",
          visibility: "private",
          createdAt: "2026-01-01T00:00:00Z",
          artifactCount: 1284,
          totalSize: 8589934592,
        });
      }),
    );
    const { user } = await openQuotaSection();

    await user.clear(screen.getByLabelText("存储上限"));
    await user.clear(screen.getByLabelText("制品数上限"));
    // 空输入 = 不限：提示文案明确写出「当前不限」，避免用户以为必须填数
    expect((await screen.findAllByText("当前不限（上限为 0）")).length).toBe(2);

    await user.click(screen.getByRole("button", { name: "保存配置" }));
    expect(await screen.findByText("保存成功")).toBeTruthy();
    // 必须显式提交 0：契约里「缺省」表示不修改，清空上限否则会静默失效
    await waitFor(() => expect(patchBody?.quotaBytes).toBe(0));
    expect(patchBody?.quotaAssets).toBe(0);
  });

  it("负数上限被前端拦截：给出可读提示且不发保存请求", async () => {
    const patch = vi.fn(() =>
      HttpResponse.json({
        id: 1,
        name: "maven-releases",
        format: "maven",
        type: "hosted",
        visibility: "private",
        createdAt: "2026-01-01T00:00:00Z",
      }),
    );
    server.use(http.patch("*/api/v1/repositories/:name", patch));
    const { user } = await openQuotaSection();

    const bytesInput = screen.getByLabelText("存储上限");
    await user.clear(bytesInput);
    await user.type(bytesInput, "-1");
    await user.click(screen.getByRole("button", { name: "保存配置" }));

    // 内联错误 + 通知（既有 notifyError 风格）都要出现，且不发请求
    expect(
      (await screen.findAllByText("请填写不小于 0 的数字（0 或留空表示不限）")).length,
    ).toBeGreaterThan(0);
    expect(patch).not.toHaveBeenCalled();
  });

  it("保存失败时按既有 notifyError 提示，并保留已填写的上限", async () => {
    server.use(
      http.patch("*/api/v1/repositories/:name", () =>
        HttpResponse.json(
          { error: { code: "standby_read_only", message: "备用节点为只读，当前请求已拒绝" } },
          { status: 503 },
        ),
      ),
    );
    const { user } = await openQuotaSection();

    const bytesInput = screen.getByLabelText("存储上限");
    await user.clear(bytesInput);
    await user.type(bytesInput, "3");
    await user.click(screen.getByRole("button", { name: "保存配置" }));

    expect(await screen.findByText("备用节点为只读，当前请求已拒绝")).toBeTruthy();
    expect((screen.getByLabelText("存储上限") as HTMLInputElement).value).toBe("3");
  });

  it("已超限仓库给出 429 拒绝写入的明确提示", async () => {
    // 种子 npm-proxy：artifactCount 5240 > quotaAssets 5000（只设了制品数上限）
    const { section } = await openQuotaSection("npm-proxy");

    expect(within(section).getByTestId("repo-quota-state").textContent).toBe("已超限");
    expect(within(section).getByText("制品用量：5,240 / 5,000")).toBeTruthy();
    // 未设字节上限的维度显示「不限」
    expect(within(section).getByText("存储用量：12.0 GB / 不限")).toBeTruthy();
    expect(within(section).getByTestId("repo-quota-over-notice").textContent).toBe(
      "已超出上限：继续写入会被拒绝（HTTP 429，quota_exceeded）。",
    );
  });

  it("接近上限（≥90%）给出橙色预警", async () => {
    // 8 GiB / 8.5 GiB ≈ 94.1% → 接近上限（仍可写，但需要提前预警）
    store.updateRepository("maven-releases", { quotaBytes: 9126805504 });
    const { section } = await openQuotaSection();

    expect(within(section).getByTestId("repo-quota-state").textContent).toBe("接近上限");
    expect(within(section).getByTestId("repo-quota-near-notice").textContent).toContain("90%");
    expect(within(section).getByText("存储用量：8.0 GB / 8.5 GB")).toBeTruthy();
  });

  it("proxy 仓库不渲染配额输入，只保留只读用量与「只对 hosted 生效」说明", async () => {
    const { section } = await openQuotaSection("npm-proxy");

    // 后端对非 hosted 仓库提交非 0 配额直接 400，故前端不提供输入（隐藏而非灰显）。
    expect(within(section).queryByLabelText("存储上限")).toBeNull();
    expect(within(section).queryByLabelText("容量单位")).toBeNull();
    expect(within(section).queryByLabelText("制品数上限")).toBeNull();
    // 只读用量与「为什么没有输入框」的说明仍在，用户不至于对着空白区块猜。
    expect(within(section).getByText("制品用量：5,240 / 5,000")).toBeTruthy();
    expect(within(section).getByTestId("repo-quota-state").textContent).toBe("已超限");
    expect(within(section).getByText(/只对 hosted 仓库生效/)).toBeTruthy();
  });

  it("group 仓库不渲染配额输入", async () => {
    // devmock 种子没有 group 仓库，按真实字段建一个（成员与自身同格式）。
    store.createRepository({
      name: "maven-group",
      format: "maven",
      type: "group",
      members: ["maven-releases"],
    });
    const { section } = await openQuotaSection("maven-group");

    expect(within(section).queryByLabelText("存储上限")).toBeNull();
    expect(within(section).queryByLabelText("制品数上限")).toBeNull();
    expect(within(section).getByText(/只对 hosted 仓库生效/)).toBeTruthy();
  });

  it("保存非 hosted 仓库配置时不提交配额字段（否则后端 400）", async () => {
    let patchBody: Record<string, unknown> | null = null;
    server.use(
      http.patch("*/api/v1/repositories/:name", async ({ request }) => {
        patchBody = (await request.json()) as Record<string, unknown>;
        return HttpResponse.json({
          id: 2,
          name: "npm-proxy",
          format: "npm",
          type: "proxy",
          visibility: "public",
          createdAt: "2026-01-02T00:00:00Z",
        });
      }),
    );
    const { user, section } = await openQuotaSection("npm-proxy");

    const retention = within(section).getByLabelText("代理缓存保留天数");
    await user.clear(retention);
    await user.type(retention, "14");
    await user.click(screen.getByRole("button", { name: "保存配置" }));
    expect(await screen.findByText("保存成功")).toBeTruthy();

    await waitFor(() => expect(patchBody?.cacheRetentionDays).toBe(14));
    // 关键：两个配额字段一个都不能出现，否则非 hosted 仓库的保存会被后端拒绝。
    expect(patchBody && "quotaBytes" in patchBody).toBe(false);
    expect(patchBody && "quotaAssets" in patchBody).toBe(false);
  });
});

describe("FR-41 仓库配额：只读可见性", () => {
  it("页头配额徽章对非管理员可见，但不提供配置页签（只读）", async () => {
    renderDetail("npm-proxy", { user: MEMBER_USER });

    const badge = await screen.findByTestId("repo-quota-badge");
    expect(badge.textContent).toBe("已超限");
    expect(screen.queryByRole("tab", { name: "配置" })).toBeNull();
    expect(screen.queryByLabelText("存储上限")).toBeNull();
  });

  it("正常状态的仓库不渲染页头配额徽章", async () => {
    renderDetail("maven-releases");
    // 等页头渲染完成（仓库属性徽章出现）后再断言徽章缺席
    expect(await screen.findByText("maven")).toBeTruthy();
    expect(screen.queryByTestId("repo-quota-badge")).toBeNull();
  });
});

describe("FR-41 仓库配额：列表展示", () => {
  it("用量列同时显示当前用量与上限，未设上限的维度保持原样", async () => {
    renderWithProviders(<RepositoriesPage />, { route: "/repositories", authenticated: true });

    // maven-releases：制品 1284 / 2000、体积 8 GiB / 10 GiB
    const artifacts = await screen.findByText("1,284 / 2,000");
    expect(screen.getByText("8.0 GB / 10.0 GB")).toBeTruthy();
    // npm-proxy：制品数超限（5240 > 5000），体积未设上限 → 只显示用量
    const overArtifacts = screen.getByText("5,240 / 5,000");
    expect(screen.getByText("12.0 GB")).toBeTruthy();

    // 状态落到 data-quota-state（列表不必依赖颜色也能读出语义）
    expect(artifacts.getAttribute("data-quota-state")).toBe("ok");
    expect(overArtifacts.getAttribute("data-quota-state")).toBe("over");
    expect(screen.getByText("8.0 GB / 10.0 GB").getAttribute("data-quota-state")).toBe("ok");
    expect(screen.getByText("12.0 GB").getAttribute("data-quota-state")).toBe("unlimited");
  });

  it("接近上限（≥90%）在列表中标注为 near", async () => {
    // devmock 种子 raw-hosted：1.4 GiB / 1.5 GiB ≈ 93% → 接近上限（橙色预警，仍可写）
    renderWithProviders(<RepositoriesPage />, { route: "/repositories", authenticated: true });

    const cell = await screen.findByText("1.4 GB / 1.5 GB");
    expect(cell.getAttribute("data-quota-state")).toBe("near");
  });

  it("阈值边界：≥90% 才算接近上限（89% 仍是正常）", async () => {
    // 1284 / 1440 ≈ 89.2% → 正常；1440 换成 1400（91.7%）即为接近上限。
    store.updateRepository("maven-releases", { quotaAssets: 1440 });
    renderWithProviders(<RepositoriesPage />, { route: "/repositories", authenticated: true });

    const cell = await screen.findByText("1,284 / 1,440");
    expect(cell.getAttribute("data-quota-state")).toBe("ok");
  });

  it("窄屏没有用量列时，超限仓库在名称下方补出状态文字", async () => {
    // jsdom 的 matchMedia 默认一律不命中，这里临时按「窄屏」渲染，跑完立刻还原。
    const original = window.matchMedia;
    window.matchMedia = ((query: string) =>
      ({
        matches: query.includes("max-width: 48em"),
        media: query,
        onchange: null,
        addListener: () => {},
        removeListener: () => {},
        addEventListener: () => {},
        removeEventListener: () => {},
        dispatchEvent: () => false,
      }) as MediaQueryList) as typeof window.matchMedia;
    try {
      renderWithProviders(<RepositoriesPage />, { route: "/repositories", authenticated: true });
      // 窄屏的用量摘要（assetCount 文案直接插值，不带千分位）
      expect(await screen.findByText(/5240 个制品/)).toBeTruthy();
      // 超限状态跟在用量后面（窄屏看不到用量列，否则用户不知道写不进去）
      expect(await screen.findByText(/已超限/)).toBeTruthy();
    } finally {
      window.matchMedia = original;
    }
  });
});

describe("FR-41 代理缓存保留天数", () => {
  it("proxy 仓库显示保留天数输入并回显种子值（maven-central：30 天）", async () => {
    const { section } = await openQuotaSection("maven-central");

    // 种子 maven-central 是 proxy 且 cacheRetentionDays=30
    const input = await within(section).findByLabelText("代理缓存保留天数");
    expect((input as HTMLInputElement).value).toBe("30");
    expect(within(section).getByText("= 保留 30 天")).toBeTruthy();
    // 说明里写清「按最后一次写入时间」与「0 = 关闭」
    expect(within(section).getByText(/最后一次写入时间/)).toBeTruthy();
    expect(within(section).getByText(/0 或留空表示关闭/)).toBeTruthy();
  });

  it("hosted / group 仓库不渲染保留天数输入（无「可重拉的代理缓存」语义）", async () => {
    const { section } = await openQuotaSection("maven-releases");
    expect(within(section).queryByLabelText("代理缓存保留天数")).toBeNull();
  });

  it("保存保留天数：填 45 提交 45；清空 = 关闭并提交 0", async () => {
    let patchBody: { cacheRetentionDays?: number } | null = null;
    server.use(
      http.patch("*/api/v1/repositories/:name", async ({ request }) => {
        patchBody = (await request.json()) as { cacheRetentionDays?: number };
        return HttpResponse.json({
          id: 2,
          name: "npm-proxy",
          format: "npm",
          type: "proxy",
          visibility: "public",
          createdAt: "2026-01-02T00:00:00Z",
          cacheRetentionDays: patchBody.cacheRetentionDays,
        });
      }),
    );
    const { user, section } = await openQuotaSection("npm-proxy");

    const input = within(section).getByLabelText("代理缓存保留天数");
    // 种子 npm-proxy 未设保留天数 → 渲染为关闭
    expect((input as HTMLInputElement).value).toBe("0");
    expect(within(section).getByText("已关闭（0 天 = 不自动清理）")).toBeTruthy();

    await user.clear(input);
    await user.type(input, "45");
    expect(within(section).getByText("= 保留 45 天")).toBeTruthy();
    await user.click(screen.getByRole("button", { name: "保存配置" }));
    expect(await screen.findByText("保存成功")).toBeTruthy();
    await waitFor(() => expect(patchBody?.cacheRetentionDays).toBe(45));

    // 清空 = 关闭：显式提交 0（缺省在契约里表示「不修改」）
    await user.clear(within(section).getByLabelText("代理缓存保留天数"));
    await user.click(screen.getByRole("button", { name: "保存配置" }));
    await waitFor(() => expect(patchBody?.cacheRetentionDays).toBe(0));
  });

  it("保留天数为负数时被前端拦截（提示关闭语义）且不发请求", async () => {
    const patch = vi.fn(() =>
      HttpResponse.json({
        id: 2,
        name: "npm-proxy",
        format: "npm",
        type: "proxy",
        visibility: "public",
        createdAt: "2026-01-02T00:00:00Z",
      }),
    );
    server.use(http.patch("*/api/v1/repositories/:name", patch));
    const { user, section } = await openQuotaSection("npm-proxy");

    const input = within(section).getByLabelText("代理缓存保留天数");
    await user.clear(input);
    await user.type(input, "-3");
    await user.click(screen.getByRole("button", { name: "保存配置" }));

    expect(
      (await screen.findAllByText("请填写不小于 0 的整数天（0 或留空表示关闭）")).length,
    ).toBeGreaterThan(0);
    expect(patch).not.toHaveBeenCalled();
  });
});

describe("FR-41 新建仓库弹窗的治理字段", () => {
  /** 打开「新建仓库」弹窗并返回其 dialog 节点。 */
  async function openCreateModal() {
    const user = userEvent.setup();
    renderWithProviders(<RepositoriesPage />, { route: "/repositories", authenticated: true });
    await screen.findByText("raw-hosted");
    await user.click(screen.getByRole("button", { name: "新建仓库" }));
    const dialog = await screen.findByRole("dialog");
    return { user, dialog };
  }

  it("类型切换决定可见字段：配额两项只在 hosted，保留天数只在 proxy", async () => {
    const { user, dialog } = await openCreateModal();

    // 默认类型是 hosted：配额两项可见，代理缓存保留天数不可见。
    expect(within(dialog).getByLabelText("存储上限")).toBeTruthy();
    expect(within(dialog).getByLabelText("制品数上限")).toBeTruthy();
    expect(within(dialog).queryByLabelText("代理缓存保留天数")).toBeNull();

    await user.click(within(dialog).getByRole("combobox", { name: /类型/ }));
    await user.click(await screen.findByRole("option", { name: "group" }));
    // group 不承载写入：配额区块整体隐藏（连标题一起），避免提交必然被 400 的字段。
    expect(within(dialog).queryByLabelText("存储上限")).toBeNull();
    expect(within(dialog).queryByLabelText("制品数上限")).toBeNull();
    expect(within(dialog).queryByText("存储配额")).toBeNull();

    await user.click(within(dialog).getByRole("combobox", { name: /类型/ }));
    await user.click(await screen.findByRole("option", { name: "proxy" }));
    // proxy 的缓存在回源路径上：配额仍隐藏，保留天数出现。
    expect(within(dialog).queryByLabelText("存储上限")).toBeNull();
    expect(within(dialog).queryByLabelText("制品数上限")).toBeNull();
    expect(await within(dialog).findByLabelText("代理缓存保留天数")).toBeTruthy();
  });

  it("hosted 类型提交配额两项（字节按 GB 换算），且不提交保留天数", async () => {
    let postBody: {
      quotaBytes?: number;
      quotaAssets?: number;
      cacheRetentionDays?: number;
    } | null = null;
    server.use(
      http.post("*/api/v1/repositories", async ({ request }) => {
        postBody = (await request.json()) as typeof postBody;
        return HttpResponse.json(
          {
            id: 99,
            name: "hosted-with-quota",
            format: "raw",
            type: "hosted",
            visibility: "private",
            createdAt: "2026-01-02T00:00:00Z",
            ...(postBody ?? {}),
          },
          { status: 201 },
        );
      }),
    );
    const { user, dialog } = await openCreateModal();

    await user.type(within(dialog).getByLabelText(/名称/), "hosted-with-quota");

    // 字节按 GB 填写（默认单位 GB），保存时换算成字节
    const quotaBytes = within(dialog).getByLabelText("存储上限");
    await user.clear(quotaBytes);
    await user.type(quotaBytes, "2");
    expect(within(dialog).getByText("= 2.0 GB（2,147,483,648 字节）")).toBeTruthy();

    const quotaAssets = within(dialog).getByLabelText("制品数上限");
    await user.clear(quotaAssets);
    await user.type(quotaAssets, "100");

    await user.click(within(dialog).getByRole("button", { name: "新建" }));
    await waitFor(() => expect(postBody?.quotaBytes).toBe(2147483648));
    expect(postBody?.quotaAssets).toBe(100);
    // hosted 没有「可重拉的代理缓存」语义，保留天数不提交（提交会被后端 400）。
    expect(postBody && "cacheRetentionDays" in postBody).toBe(false);
  });

  it("proxy 类型只提交保留天数，不提交配额字段（否则后端 400）", async () => {
    let postBody: {
      quotaBytes?: number;
      quotaAssets?: number;
      cacheRetentionDays?: number;
    } | null = null;
    server.use(
      http.post("*/api/v1/repositories", async ({ request }) => {
        postBody = (await request.json()) as typeof postBody;
        return HttpResponse.json(
          {
            id: 98,
            name: "proxy-no-quota",
            format: "raw",
            type: "proxy",
            visibility: "private",
            createdAt: "2026-01-02T00:00:00Z",
            ...(postBody ?? {}),
          },
          { status: 201 },
        );
      }),
    );
    const { user, dialog } = await openCreateModal();

    await user.type(within(dialog).getByLabelText(/名称/), "proxy-no-quota");
    await user.click(within(dialog).getByRole("combobox", { name: /类型/ }));
    await user.click(await screen.findByRole("option", { name: "proxy" }));
    await user.type(within(dialog).getByLabelText(/上游地址/), "https://repo.example.com/raw");

    const retention = within(dialog).getByLabelText("代理缓存保留天数");
    await user.clear(retention);
    await user.type(retention, "45");

    await user.click(within(dialog).getByRole("button", { name: "新建" }));
    await waitFor(() => expect(postBody?.cacheRetentionDays).toBe(45));
    expect(postBody && "quotaBytes" in postBody).toBe(false);
    expect(postBody && "quotaAssets" in postBody).toBe(false);
  });

  it("创建时配额非法（负数）被拦截：提示且不发创建请求", async () => {
    const post = vi.fn(() =>
      HttpResponse.json({ id: 99, name: "bad", format: "raw", type: "hosted" }, { status: 201 }),
    );
    server.use(http.post("*/api/v1/repositories", post));
    const { user, dialog } = await openCreateModal();

    await user.type(within(dialog).getByLabelText(/名称/), "bad-quota");
    const quotaBytes = within(dialog).getByLabelText("存储上限");
    await user.clear(quotaBytes);
    await user.type(quotaBytes, "-1");
    await user.click(within(dialog).getByRole("button", { name: "新建" }));

    expect(
      (await screen.findAllByText("请填写不小于 0 的数字（0 或留空表示不限）")).length,
    ).toBeGreaterThan(0);
    expect(post).not.toHaveBeenCalled();
  });
});
