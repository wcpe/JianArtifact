// 搜索页的身份边界：私有结果不得跨越会话身份残留。
//
// 复现路径（审查发现的一处隐私泄漏）：
//   已登录 → 搜到私有仓库命中 → 会话失效（user 变为 null）→ 匿名请求失败或被取消。
//
// 旧实现的漏洞点有两处：
//   1. keepPreviousData 只看“手里有没有数据”，不看数据属于谁——缓存键带了用户 ID 也只隔离了
//      模块级 asyncCache，隔离不了 useAsync 组件内的 data state（clearAsyncCache() 清不到它）；
//   2. 结果归属只比对关键词（dataQuery === q），不比对身份，于是上一身份的结果被判为“当前口径”。
// 结果是：新发的匿名请求一旦失败或被取消，上一主体的私有仓库名、路径与校验和就永久留在界面上。
//
// 这里断言的是**用户可见性**：会话失效的那一刻，上一身份的内容就必须从 DOM 里消失，而不是
// “等匿名请求落定之后再说”。
import { MantineProvider } from "@mantine/core";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import "@testing-library/jest-dom/vitest";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { MemoryRouter } from "react-router-dom";
import { describe, expect, it } from "vitest";

import { server } from "@jianartifact/devmock/node";

import type { SearchPackage, SearchPackageFile, SearchResult } from "../api/endpoints";
import { AUTH_EXPIRED_EVENT, setToken } from "../api/client";
import { AuthProvider } from "../auth/AuthContext";
import "../i18n";
import { SearchPage } from "./SearchPage";

/** 私有仓库里的样本文件；sha1 取可辨识的前缀，便于断言“校验和有没有残留”。 */
const PRIVATE_SHA1 = "0123456789abcdef0123456789abcdef01234567";

function privateFile(name: string, path: string, fileKind = "jar"): SearchPackageFile {
  return {
    name,
    path,
    repository: "secret-repo",
    size: 2048,
    sha1: PRIVATE_SHA1,
    updatedAt: "2026-10-18 09:12:33",
    fileKind,
  };
}

/** 命中私有仓库的包节点（含一个校验和文件，覆盖「校验和」这一路泄漏面）。 */
const PRIVATE_PACKAGE: SearchPackage = {
  coordinate: "com.acme:secret-lib",
  kind: "maven",
  repositories: ["secret-repo"],
  versionCount: 1,
  fileCount: 2,
  bestPrecision: "exact",
  latestUpdatedAt: "2026-10-18 09:12:33",
  topVersion: "1.0.0",
  versions: [
    {
      version: "1.0.0",
      precision: "exact",
      fileCount: 2,
      size: 2048,
      latestUpdatedAt: "2026-10-18 09:12:33",
      files: [
        privateFile("secret-lib-1.0.0.jar", "com/acme/secret-lib/1.0.0/secret-lib-1.0.0.jar"),
        privateFile(
          "secret-lib-1.0.0.jar.sha1",
          "com/acme/secret-lib/1.0.0/secret-lib-1.0.0.jar.sha1",
          "checksum",
        ),
      ],
    },
  ],
};

const PRIVATE_RESULT: SearchResult = {
  items: [],
  total: 2,
  facets: [{ repository: "secret-repo", count: 2 }],
  packageTotal: 1,
  packages: [PRIVATE_PACKAGE],
  aggregation: { scanned: 2, truncated: false },
  appliedOrder: "relevance",
};

/** 以已登录会话渲染搜索页（AuthProvider 从 localStorage 恢复身份）。 */
function renderAuthedPage(q = "artifact") {
  setToken("mock.jwt.token");
  localStorage.setItem(
    "jianartifact.user",
    JSON.stringify({
      id: 1,
      username: "admin",
      role: "admin",
      status: "active",
      createdAt: "2026-01-01T00:00:00Z",
    }),
  );
  return render(
    <MantineProvider>
      {/* 搜索页要读会话身份（取数依赖、缓存键与结果归属都按它区分），必须包在 AuthProvider 里。 */}
      <AuthProvider>
        <MemoryRouter initialEntries={[`/search?q=${encodeURIComponent(q)}`]}>
          <SearchPage />
        </MemoryRouter>
      </AuthProvider>
    </MantineProvider>,
  );
}

/** 模拟会话失效：401 拦截器与 AuthContext 用的就是同一条全局事件。 */
function expireSession() {
  act(() => {
    window.dispatchEvent(new CustomEvent(AUTH_EXPIRED_EVENT));
  });
}

/** 断言上一身份的私有内容（包坐标 / 仓库名 / 校验和 / 详情面板）已全部离开界面。 */
function expectPrivateResultGone() {
  expect(screen.queryByTestId("search-package-tree")).toBeNull();
  expect(screen.queryByTestId("search-detail-panel")).toBeNull();
  expect(screen.queryByText(/secret-repo/)).toBeNull();
  expect(screen.queryByText(/secret-lib/)).toBeNull();
  expect(screen.queryByText(new RegExp(PRIVATE_SHA1.slice(0, 16)))).toBeNull();
}

/** 断言私有结果确实曾经在界面上（用例的前提，防止空跑）。 */
function expectPrivateResultVisible() {
  const tree = screen.getByTestId("search-package-tree");
  const row = screen
    .getAllByTestId("search-package-row")
    .find((element) => element.getAttribute("data-coordinate") === "com.acme:secret-lib");
  expect(row).toBeDefined();
  expect(within(tree).getByText("secret-lib-1.0.0.jar")).toBeInTheDocument();
  // 分面 chip 带出仓库名，详情面板带出路径与校验和。
  expect(screen.getByText(/secret-repo \d+/)).toBeInTheDocument();
  const panel = screen.getByTestId("search-detail-panel");
  expect(
    within(panel).getByText("com/acme/secret-lib/1.0.0/secret-lib-1.0.0.jar"),
  ).toBeInTheDocument();
  expect(within(panel).getByText(new RegExp(PRIVATE_SHA1.slice(0, 16)))).toBeInTheDocument();
}

describe("SearchPage 身份边界", () => {
  it("会话失效后匿名搜索失败：上一身份的私有结果不得残留", async () => {
    let anonymousCalls = 0;
    let releaseAnonymous: (() => void) | undefined;
    const anonymousGate = new Promise<void>((resolve) => {
      releaseAnonymous = resolve;
    });
    server.use(
      http.get("*/api/v1/search", async ({ request }) => {
        // 带上 Bearer 的即登录口径（有私有仓库可见性），否则是会话失效后的匿名口径。
        if (request.headers.get("authorization")) {
          return HttpResponse.json(PRIVATE_RESULT as never);
        }
        anonymousCalls += 1;
        // 挂在闸门后：先断言“旧内容已消失”，再放行让匿名请求失败。
        await anonymousGate;
        return HttpResponse.json(
          { error: { code: "internal_error", message: "搜索服务暂不可用" } },
          { status: 500 },
        );
      }),
    );

    renderAuthedPage();
    await screen.findByTestId("search-package-tree");
    expectPrivateResultVisible();

    expireSession();

    // 关键断言：**不等匿名请求落定**，上一身份的内容就必须已经消失。
    // 旧实现里这份 data 被 keepPreviousData 原样留在组件里，此时仍会照常渲染。
    expectPrivateResultGone();

    // 匿名请求已发出但仍挂在闸门后（响应未回）：这段窗口正是旧实现里“私有结果继续展示”的窗口。
    await waitFor(() => expect(anonymousCalls).toBe(1));
    expectPrivateResultGone();

    // 匿名请求失败后同样不得回填：只留错误态与重试入口。
    await act(async () => {
      releaseAnonymous?.();
    });
    expect(await screen.findByTestId("state-error")).toBeInTheDocument();
    expectPrivateResultGone();
  });

  it("会话失效后取消匿名搜索：上一身份的私有结果同样不得残留", async () => {
    let anonymousAborted = false;
    server.use(
      http.get("*/api/v1/search", async ({ request }) => {
        if (request.headers.get("authorization")) {
          return HttpResponse.json(PRIVATE_RESULT as never);
        }
        await new Promise<void>((resolve) => {
          request.signal.addEventListener(
            "abort",
            () => {
              anonymousAborted = true;
              resolve();
            },
            { once: true },
          );
          // 兜底：即便运行时不转发 signal，5s 后也自行落定，避免用例结束时留下挂起的 handler。
          setTimeout(resolve, 5_000);
        });
        return HttpResponse.json(PRIVATE_RESULT as never);
      }),
    );

    renderAuthedPage();
    await screen.findByTestId("search-package-tree");
    expectPrivateResultVisible();

    expireSession();

    // 匿名搜索在飞：进度条与取消按钮出现，此时界面上已经没有任何私有内容。
    await screen.findByTestId("search-progress");
    expectPrivateResultGone();

    await userEvent.click(screen.getByRole("button", { name: "取消搜索" }));

    // 取消必须是真中止，且静默回到空闲态——不是失败、也不是“继续显示上次结果”。
    await waitFor(() => expect(anonymousAborted).toBe(true));
    expect(await screen.findByTestId("search-cancelled")).toBeInTheDocument();
    expect(screen.queryByTestId("state-error")).toBeNull();
    expect(screen.queryByTestId("search-refresh-error")).toBeNull();
    expectPrivateResultGone();
  });
});
