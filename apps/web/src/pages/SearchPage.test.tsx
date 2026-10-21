// SearchPage 主从联动回归：
// - 默认展开首个包、选中其首版本、右栏显示该版本首个文件；
// - 点另一个版本 → 右栏就地切换，且不产生新的 /search 请求（排序与选中都是前端行为）；
// - 四态里的空结果与失败态只替换结果区，搜索框仍在；
// - 窄屏（820px）下点版本行进入详情、返回按钮回到列表（展开态保留）。
//
// 数据用 MSW 覆写 `GET /api/v1/search`：devmock 的搜索 handler 只产出文件级结果，
// 包级聚合（的 group_by=package）在本用例里直接给到前端契约形状。
import { MantineProvider } from "@mantine/core";
import { render, screen, waitFor, within } from "@testing-library/react";
import "@testing-library/jest-dom/vitest";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { MemoryRouter } from "react-router-dom";
import { afterEach, describe, expect, it } from "vitest";

import { server } from "@jianartifact/devmock/node";

import type { SearchPackage, SearchResult } from "../api/endpoints";
import "../i18n";
import { SearchPage } from "./SearchPage";

function file(name: string, path: string, fileKind: string, size = 1024) {
  return {
    name,
    path,
    repository: "maven-releases",
    size,
    sha1: `sha1-${name}`,
    updatedAt: "2026-10-18 09:12:33",
    fileKind,
  };
}

const PACKAGES: SearchPackage[] = [
  {
    coordinate: "com.example:foo",
    kind: "maven",
    repositories: ["maven-releases"],
    versionCount: 2,
    fileCount: 4,
    bestPrecision: "exact",
    latestUpdatedAt: "2026-10-18 09:12:33",
    topVersion: "1.17.12",
    versions: [
      {
        version: "1.17.12",
        precision: "exact",
        fileCount: 3,
        size: 2048,
        latestUpdatedAt: "2026-10-18 09:12:33",
        files: [
          file(
            "foo-1.17.12.jar",
            "com/example/foo/1.17.12/foo-1.17.12.jar",
            "jar",
            1523712,
          ),
          file("foo-1.17.12.pom", "com/example/foo/1.17.12/foo-1.17.12.pom", "pom"),
          file("foo-1.17.12.jar.sha1", "com/example/foo/1.17.12/foo-1.17.12.jar.sha1", "checksum"),
        ],
      },
      {
        version: "1.17.120",
        precision: "substring",
        fileCount: 1,
        size: 512,
        latestUpdatedAt: "2026-10-17 08:00:00",
        files: [
          file("foo-1.17.120.jar", "com/example/foo/1.17.120/foo-1.17.120.jar", "jar", 512),
        ],
      },
    ],
  },
  {
    coordinate: "com.example:bar",
    kind: "maven",
    repositories: ["maven-releases"],
    versionCount: 1,
    fileCount: 1,
    bestPrecision: "none",
    latestUpdatedAt: "2026-10-16 08:00:00",
    topVersion: "2.0.0",
    versions: [
      {
        version: "2.0.0",
        precision: "none",
        fileCount: 1,
        size: 256,
        latestUpdatedAt: "2026-10-16 08:00:00",
        files: [file("bar-2.0.0.jar", "com/example/bar/2.0.0/bar-2.0.0.jar", "jar", 256)],
      },
    ],
  },
];

function searchResponse(packages: SearchPackage[]): SearchResult {
  return {
    items: [],
    total: packages.reduce((sum, pkg) => sum + pkg.fileCount, 0),
    facets: [{ repository: "maven-releases", count: 5 }],
    packageTotal: packages.length,
    packages,
    aggregation: { scanned: 5, truncated: false },
    appliedOrder: "relevance",
  };
}

/** 覆写搜索端点，并返回被调用次数（用于断言"切版本不重发请求"）。 */
function mockSearch(body: SearchResult | (() => Response)) {
  const calls: string[] = [];
  server.use(
    http.get("*/api/v1/search", ({ request }) => {
      calls.push(request.url);
      return typeof body === "function"
        ? body()
        : HttpResponse.json(body as unknown as Record<string, unknown>);
    }),
  );
  return calls;
}

function renderPage(q = "1.17.12") {
  return render(
    <MantineProvider>
      <MemoryRouter initialEntries={[`/search?q=${encodeURIComponent(q)}`]}>
        <SearchPage />
      </MemoryRouter>
    </MantineProvider>,
  );
}

/**
 * 按数据属性定位树行：命中词高亮会把坐标 / 版本号 / 文件名切成多个元素，
 * 纯文本查询要么找不到、要么命中详情面板里的同值文本。这里统一走稳定属性。
 */
function rowByAttribute(testId: string, attribute: string, value: string): HTMLElement {
  const row = screen
    .getAllByTestId(testId)
    .find((element) => element.getAttribute(attribute) === value);
  if (!row) {
    throw new Error(`未找到 ${testId}[${attribute}="${value}"]`);
  }
  return row;
}

const packageRow = (coordinate: string) =>
  rowByAttribute("search-package-row", "data-coordinate", coordinate);
const versionRow = (version: string) => rowByAttribute("search-version-row", "data-version", version);
const fileRow = (path: string) => rowByAttribute("search-file-row", "data-path", path);
const queryFileRow = (path: string) =>
  screen
    .queryAllByTestId("search-file-row")
    .find((element) => element.getAttribute("data-path") === path);

/** 树容器：断言"树里没有 / 树里可见"时用它限定范围，避免命中详情面板。 */
const tree = () => screen.getByTestId("search-package-tree");
const detailPanel = () => screen.getByTestId("search-detail-panel");

/** 只在需要窄屏语义的用例里放开 820px 断点，其余保持桌面（matchMedia 默认不匹配）。 */
function stubNarrowViewport() {
  const original = window.matchMedia;
  window.matchMedia = ((query: string) => ({
    matches: query.includes("51.25em"),
    media: query,
    onchange: null,
    addListener: () => {},
    removeListener: () => {},
    addEventListener: () => {},
    removeEventListener: () => {},
    dispatchEvent: () => false,
  })) as typeof window.matchMedia;
  return () => {
    window.matchMedia = original;
  };
}

afterEach(() => {
  localStorage.clear();
});

describe("SearchPage 主从双栏", () => {
  it("首屏展开首个包、选中首版本，右栏显示该版本首个文件", async () => {
    mockSearch(searchResponse(PACKAGES));
    renderPage();

    // 左栏：两个包都在；首个包默认展开，其两个版本可见。
    await screen.findByTestId("search-package-tree");
    expect(packageRow("com.example:foo")).toBeInTheDocument();
    expect(packageRow("com.example:bar")).toBeInTheDocument();
    expect(versionRow("1.17.12")).toBeInTheDocument();
    expect(versionRow("1.17.120")).toBeInTheDocument();

    // 右栏：标题是首个版本的首个文件，且带同版本文件列表（含校验和）。
    const panel = await screen.findByTestId("search-detail-panel");
    expect(within(panel).getByRole("heading", { name: "foo-1.17.12.jar" })).toBeInTheDocument();
    expect(within(panel).getByText("com.example:foo:1.17.12")).toBeInTheDocument();
    expect(within(panel).getByText("foo-1.17.12.jar.sha1")).toBeInTheDocument();
  });

  it("点击另一个版本就地切换右栏，且不产生新的搜索请求", async () => {
    const calls = mockSearch(searchResponse(PACKAGES));
    renderPage();

    await screen.findByTestId("search-detail-panel");
    expect(calls).toHaveLength(1);

    await userEvent.click(versionRow("1.17.120"));

    await waitFor(() => {
      expect(within(detailPanel()).getByRole("heading", { name: "foo-1.17.120.jar" })).toBeInTheDocument();
    });
    // 切版本是纯前端行为：不重发请求、不折叠其它包。
    expect(calls).toHaveLength(1);
    expect(packageRow("com.example:bar")).toBeInTheDocument();
  });

  it("校验和文件在树内默认折成一行，展开后可见", async () => {
    mockSearch(searchResponse(PACKAGES));
    renderPage();

    const checksumPath = "com/example/foo/1.17.12/foo-1.17.12.jar.sha1";
    await screen.findByTestId("search-package-tree");
    // 默认折叠：树内没有校验和文件行（详情面板的同版本文件列表不算树）。
    expect(queryFileRow(checksumPath)).toBeUndefined();

    await userEvent.click(screen.getByText("校验和 (1)"));
    await waitFor(() => {
      expect(fileRow(checksumPath)).toBeInTheDocument();
    });
  });

  it("无结果时渲染空态，搜索框仍可用", async () => {
    mockSearch(searchResponse([]));
    renderPage();

    expect(await screen.findByTestId("search-empty")).toBeInTheDocument();
    expect(screen.getByText("未找到匹配的制品")).toBeInTheDocument();
    expect(screen.getByPlaceholderText(/搜索制品/)).toBeInTheDocument();
  });

  it("请求失败时渲染错误态并可重试", async () => {
    let fail = true;
    const calls = mockSearch(() => {
      if (fail) {
        fail = false;
        return HttpResponse.json(
          { error: { code: "internal", message: "搜索服务不可用" } },
          { status: 500 },
        );
      }
      return HttpResponse.json(searchResponse(PACKAGES) as unknown as Record<string, unknown>);
    });
    renderPage();

    expect(await screen.findByTestId("state-error")).toBeInTheDocument();
    expect(screen.getByText("搜索服务不可用")).toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: /重试/ }));
    await waitFor(() => {
      expect(packageRow("com.example:foo")).toBeInTheDocument();
    });
    expect(calls.length).toBeGreaterThanOrEqual(2);
  });

  it("窄屏下点版本行进详情，返回按钮回到列表并保留展开态", async () => {
    const restore = stubNarrowViewport();
    try {
      mockSearch(searchResponse(PACKAGES));
      renderPage();

      expect(await screen.findByTestId("search-package-tree")).toBeVisible();

      await userEvent.click(versionRow("1.17.120"));

      // 进入详情：左栏隐藏、右栏出现，顶部有返回按钮。
      await waitFor(() => {
        expect(detailPanel()).toBeVisible();
      });
      expect(detailPanel()).toHaveTextContent("foo-1.17.120.jar");
      expect(tree()).not.toBeVisible();

      await userEvent.click(screen.getByRole("button", { name: "返回搜索结果" }));

      // 回到列表：树重新可见，且之前展开的包/版本仍在（展开态未被重置）。
      await waitFor(() => {
        expect(tree()).toBeVisible();
      });
      expect(versionRow("1.17.120")).toBeInTheDocument();
      expect(versionRow("1.17.12")).toBeInTheDocument();
    } finally {
      restore();
    }
  });
});
