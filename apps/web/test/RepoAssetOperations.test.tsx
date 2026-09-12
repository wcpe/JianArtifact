// FR-105：管理员在资产树中选择文件和目录，并通过右键菜单调用统一资产操作 API。
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { describe, expect, it } from "vitest";

import { server } from "@jianartifact/devmock/node";
import { RepoBrowser } from "../src/components/repo/RepoBrowser";
import { assetOperationTarget } from "../src/lib/assetOperations";
import { renderWithProviders } from "./harness";

const ADMIN = {
  id: 1,
  username: "admin",
  role: "admin" as const,
  status: "active" as const,
  createdAt: "2026-01-01T00:00:00Z",
};

function renderRawBrowser() {
  return renderWithProviders(
    <RepoBrowser repoName="raw-releases" forcedFormat="raw" forcedType="hosted" />,
    { authenticated: true, user: ADMIN },
  );
}

function useRawTree(onRootRead?: () => void) {
  server.use(
    http.get("*/api/v1/repositories/:name/tree", ({ request }) => {
      const prefix = new URL(request.url).searchParams.get("prefix") ?? "";
      if (!prefix) {
        onRootRead?.();
        return HttpResponse.json({
          directories: ["docs/"],
          files: [
            { path: "a.txt", size: 1, hash: "a", updatedAt: "2026-08-21T00:00:00Z" },
            { path: "b.txt", size: 1, hash: "b", updatedAt: "2026-08-21T00:00:00Z" },
            { path: "c.txt", size: 1, hash: "c", updatedAt: "2026-08-21T00:00:00Z" },
          ],
        });
      }
      return HttpResponse.json({
        directories: [],
        files: [{ path: "docs/readme.md", size: 1, hash: "r", updatedAt: "2026-08-21T00:00:00Z" }],
      });
    }),
  );
}

function useNpmTree() {
  server.use(
    http.get("*/api/v1/repositories/:name/tree", () =>
      HttpResponse.json({
        directories: [],
        files: [
          {
            path: "demo/-/demo-1.0.0.tgz",
            size: 1,
            hash: "npm",
            updatedAt: "2026-08-21T00:00:00Z",
          },
        ],
      }),
    ),
  );
}

function useSearchResults() {
  server.use(
    http.get("*/api/v1/search", () =>
      HttpResponse.json({
        items: [
          {
            repository: "raw-releases",
            path: "z.txt",
            size: 1,
            hash: "z",
            updatedAt: "2026-08-21T00:00:00Z",
          },
          {
            repository: "raw-releases",
            path: "x.txt",
            size: 1,
            hash: "x",
            updatedAt: "2026-08-21T00:00:00Z",
          },
          {
            repository: "raw-releases",
            path: "y.txt",
            size: 1,
            hash: "y",
            updatedAt: "2026-08-21T00:00:00Z",
          },
        ],
        total: 3,
      }),
    ),
  );
}

function useLazyTree(onChildRead: () => void) {
  server.use(
    http.get("*/api/v1/repositories/:name/tree", ({ request }) => {
      const prefix = new URL(request.url).searchParams.get("prefix") ?? "";
      if (prefix === "docs/") {
        onChildRead();
        return HttpResponse.json({
          directories: [],
          files: [
            { path: "docs/hidden.txt", size: 1, hash: "hidden", updatedAt: "2026-08-21T00:00:00Z" },
          ],
        });
      }
      return HttpResponse.json({
        directories: ["docs/"],
        files: [
          { path: "outside.txt", size: 1, hash: "outside", updatedAt: "2026-08-21T00:00:00Z" },
        ],
      });
    }),
  );
}

function treeItem(path: string) {
  const item = document.querySelector<HTMLElement>(`[role="treeitem"][data-path="${path}"]`);
  if (!item) throw new Error(`缺少树节点：${path}`);
  return item;
}

describe("FR-105 资产树操作", () => {
  it("Maven 文件不提升为父级逻辑删除目标", () => {
    for (const path of [
      "com/example/demo/1.0/demo-1.0.pom",
      "com/example/demo/1.0/demo-1.0.jar.sha256",
      "com/example/demo/maven-metadata.xml",
    ]) {
      expect(assetOperationTarget("maven", path, "file")).toEqual({ type: "asset_path", path });
    }
  });

  it("移除复选框，普通点击、Ctrl/Meta 和 Shift 选择文件与目录", async () => {
    const user = userEvent.setup();
    useRawTree();
    renderRawBrowser();

    await user.click(await screen.findByText("a.txt"));
    fireEvent.click(screen.getByText("c.txt"), { shiftKey: true });
    fireEvent.click(screen.getByText("docs"), { ctrlKey: true });
    expect(screen.queryByRole("checkbox")).toBeNull();
    expect(screen.queryByRole("button", { name: /删除所选/ })).toBeNull();
  });

  it("右键菜单提供独立移动操作并调用统一资产操作 API", async () => {
    const user = userEvent.setup();
    useRawTree();
    let body: Record<string, unknown> | null = null;
    server.use(
      http.post("*/api/v1/repositories/:name/assets/operations", async ({ request }) => {
        body = (await request.json()) as Record<string, unknown>;
        return HttpResponse.json({ operationId: "op-1", affected: 1 });
      }),
    );
    renderRawBrowser();

    const file = await screen.findByText("a.txt");
    fireEvent.contextMenu(file);
    await user.click(await screen.findByRole("menuitem", { name: "移动" }));
    const input = await screen.findByLabelText("目标目录");
    await user.type(input, "archive");
    await user.click(screen.getByRole("button", { name: "移动" }));

    await waitFor(() =>
      expect(body).toEqual({
        action: "move",
        targets: [{ type: "raw_path", path: "a.txt" }],
        destinationPath: "archive",
        overrideReason: "管理员资产操作",
      }),
    );
  });

  it("多选 Raw 文件后移动并调用统一资产操作 API", async () => {
    const user = userEvent.setup();
    useRawTree();
    let body: Record<string, unknown> | null = null;
    server.use(
      http.post("*/api/v1/repositories/:name/assets/operations", async ({ request }) => {
        body = (await request.json()) as Record<string, unknown>;
        return HttpResponse.json({ operationId: "op-move-many", affected: 2 });
      }),
    );
    renderRawBrowser();

    await user.click(await screen.findByText("a.txt"));
    fireEvent.click(screen.getByText("b.txt"), { ctrlKey: true });
    fireEvent.contextMenu(treeItem("a.txt"));
    await user.click(await screen.findByRole("menuitem", { name: "移动" }));
    await user.type(await screen.findByLabelText("目标目录"), "archive");
    await user.click(screen.getByRole("button", { name: "移动" }));

    await waitFor(() =>
      expect(body).toEqual({
        action: "move",
        targets: [
          { type: "raw_path", path: "a.txt" },
          { type: "raw_path", path: "b.txt" },
        ],
        destinationPath: "archive",
        overrideReason: "管理员资产操作",
      }),
    );
  });

  it("Shift 范围与 Ctrl 选择在右键已选节点时保留", async () => {
    const user = userEvent.setup();
    useRawTree();
    let body: Record<string, unknown> | null = null;
    server.use(
      http.post("*/api/v1/repositories/:name/assets/operations", async ({ request }) => {
        body = (await request.json()) as Record<string, unknown>;
        return HttpResponse.json({ operationId: "op-selection", affected: 4 });
      }),
    );
    renderRawBrowser();

    await user.click(await screen.findByText("a.txt"));
    fireEvent.click(screen.getByText("c.txt"), { shiftKey: true });
    fireEvent.click(screen.getByText("docs"), { ctrlKey: true });
    fireEvent.contextMenu(treeItem("b.txt"));
    await user.click(await screen.findByRole("menuitem", { name: "删除" }));
    await user.click(
      within(await screen.findByRole("dialog")).getByRole("button", { name: "删除" }),
    );

    await waitFor(() =>
      expect(body).toEqual({
        action: "delete",
        targets: [
          { type: "raw_path", path: "a.txt" },
          { type: "raw_path", path: "b.txt" },
          { type: "raw_path", path: "c.txt" },
          { type: "raw_path", path: "docs" },
        ],
        overrideReason: "管理员资产操作",
      }),
    );
  });

  it("搜索结果的 Shift 范围只使用当前可见顺序", async () => {
    const user = userEvent.setup();
    useRawTree();
    useSearchResults();
    let body: Record<string, unknown> | null = null;
    server.use(
      http.post("*/api/v1/repositories/:name/assets/operations", async ({ request }) => {
        body = (await request.json()) as Record<string, unknown>;
        return HttpResponse.json({ operationId: "op-search", affected: 3 });
      }),
    );
    renderRawBrowser();

    const input = await screen.findByPlaceholderText("搜索制品，支持 -排除词 ext:jar 等表达式");
    await user.type(input, "txt");
    fireEvent.keyDown(input, { key: "Enter" });
    await screen.findByText("x.txt");
    await user.click(screen.getByText("x.txt"));
    fireEvent.click(screen.getByText("z.txt"), { shiftKey: true });
    fireEvent.contextMenu(treeItem("y.txt"));
    await user.click(await screen.findByRole("menuitem", { name: "删除" }));
    await user.click(
      within(await screen.findByRole("dialog")).getByRole("button", { name: "删除" }),
    );

    await waitFor(() =>
      expect(body).toEqual({
        action: "delete",
        targets: [
          { type: "raw_path", path: "x.txt" },
          { type: "raw_path", path: "y.txt" },
          { type: "raw_path", path: "z.txt" },
        ],
        overrideReason: "管理员资产操作",
      }),
    );
  });

  it("懒加载目录展开后不向既有 Shift 选择补入未加载子项", async () => {
    const user = userEvent.setup();
    let childReads = 0;
    useLazyTree(() => {
      childReads += 1;
    });
    let body: Record<string, unknown> | null = null;
    server.use(
      http.post("*/api/v1/repositories/:name/assets/operations", async ({ request }) => {
        body = (await request.json()) as Record<string, unknown>;
        return HttpResponse.json({ operationId: "op-lazy", affected: 2 });
      }),
    );
    renderRawBrowser();

    await user.click(await screen.findByText("outside.txt"));
    fireEvent.click(screen.getByText("docs"), { shiftKey: true });
    await waitFor(() => expect(childReads).toBe(1));
    await screen.findByText("hidden.txt");
    fireEvent.contextMenu(treeItem("outside.txt"));
    await user.click(await screen.findByRole("menuitem", { name: "删除" }));
    await user.click(
      within(await screen.findByRole("dialog")).getByRole("button", { name: "删除" }),
    );

    await waitFor(() =>
      expect(body).toEqual({
        action: "delete",
        targets: [
          { type: "raw_path", path: "docs" },
          { type: "raw_path", path: "outside.txt" },
        ],
        overrideReason: "管理员资产操作",
      }),
    );
  });

  it("子目录加载失败时保留目录并显示明确错误", async () => {
    const user = userEvent.setup();
    server.use(
      http.get("*/api/v1/repositories/:name/tree", ({ request }) => {
        const prefix = new URL(request.url).searchParams.get("prefix") ?? "";
        if (prefix === "docs/") {
          return HttpResponse.json(
            { error: { code: "upstream_error", message: "目录加载失败" } },
            { status: 500 },
          );
        }
        return HttpResponse.json({ directories: ["docs/"], files: [] });
      }),
    );
    renderRawBrowser();

    await user.click(await screen.findByText("docs"));
    expect(await screen.findByText("目录加载失败")).toBeTruthy();
    expect(screen.getByText("docs")).toBeTruthy();
  });

  it("仓库内搜索失败时保留文件树并显示明确错误", async () => {
    const user = userEvent.setup();
    useRawTree();
    server.use(
      http.get("*/api/v1/search", () =>
        HttpResponse.json(
          { error: { code: "upstream_error", message: "搜索失败" } },
          { status: 500 },
        ),
      ),
    );
    renderRawBrowser();

    const search = await screen.findByPlaceholderText(/搜索制品/);
    await user.type(search, "readme");
    await user.keyboard("{Enter}");

    expect(await screen.findByText("搜索失败")).toBeTruthy();
    expect(screen.getByText("a.txt")).toBeTruthy();
  });

  it("Raw 单选重命名并调用统一资产操作 API", async () => {
    const user = userEvent.setup();
    useRawTree();
    let body: Record<string, unknown> | null = null;
    server.use(
      http.post("*/api/v1/repositories/:name/assets/operations", async ({ request }) => {
        body = (await request.json()) as Record<string, unknown>;
        return HttpResponse.json({ operationId: "op-rename", affected: 1 });
      }),
    );
    renderRawBrowser();

    fireEvent.contextMenu(await screen.findByText("a.txt"));
    await user.click(await screen.findByRole("menuitem", { name: "重命名" }));
    const input = await screen.findByLabelText("新路径");
    await user.clear(input);
    await user.type(input, "renamed.txt");
    await user.click(screen.getByRole("button", { name: "重命名" }));

    await waitFor(() =>
      expect(body).toEqual({
        action: "rename",
        targets: [{ type: "raw_path", path: "a.txt" }],
        newPath: "renamed.txt",
        overrideReason: "管理员资产操作",
      }),
    );
  });

  it("右键菜单删除单个目录目标并在成功后清空选择", async () => {
    const user = userEvent.setup();
    useRawTree();
    let body: Record<string, unknown> | null = null;
    server.use(
      http.post("*/api/v1/repositories/:name/assets/operations", async ({ request }) => {
        body = (await request.json()) as Record<string, unknown>;
        return HttpResponse.json({ operationId: "op-2", affected: 1 });
      }),
    );
    renderRawBrowser();

    fireEvent.contextMenu(await screen.findByText("docs"));
    await user.click(await screen.findByRole("menuitem", { name: "删除" }));
    const dialog = await screen.findByRole("dialog");
    await user.click(within(dialog).getByRole("button", { name: "删除" }));

    await waitFor(() =>
      expect(body).toEqual({
        action: "delete",
        targets: [{ type: "raw_path", path: "docs" }],
        overrideReason: "管理员资产操作",
      }),
    );
  });

  it.each(["proxy", "group"] as const)("Raw %s 不显示移动或重命名", async (type) => {
    useRawTree();
    const { unmount } = renderWithProviders(
      <RepoBrowser repoName={`raw-${type}`} forcedFormat="raw" forcedType={type} />,
      { authenticated: true, user: ADMIN },
    );

    fireEvent.contextMenu(await screen.findByText("a.txt"));

    expect(screen.getByRole("menuitem", { name: "删除" })).toBeTruthy();
    expect(screen.queryByRole("menuitem", { name: "移动" })).toBeNull();
    expect(screen.queryByRole("menuitem", { name: "重命名" })).toBeNull();
    unmount();
  });

  it("Maven/npm 不显示路径操作，npm 删除传 typed asset_path", async () => {
    const user = userEvent.setup();
    useRawTree();
    const { unmount } = renderWithProviders(
      <RepoBrowser repoName="maven-hosted" forcedFormat="maven" forcedType="hosted" />,
      { authenticated: true, user: ADMIN },
    );

    fireEvent.contextMenu(await screen.findByText("a.txt"));
    expect(screen.queryByRole("menuitem", { name: "移动" })).toBeNull();
    expect(screen.queryByRole("menuitem", { name: "重命名" })).toBeNull();
    unmount();

    let body: Record<string, unknown> | null = null;
    useNpmTree();
    server.use(
      http.post("*/api/v1/repositories/:name/assets/operations", async ({ request }) => {
        body = (await request.json()) as Record<string, unknown>;
        return HttpResponse.json({ operationId: "op-npm", affected: 1 });
      }),
    );
    renderWithProviders(
      <RepoBrowser repoName="npm-hosted" forcedFormat="npm" forcedType="hosted" />,
      { authenticated: true, user: ADMIN },
    );

    fireEvent.contextMenu(await screen.findByText("demo-1.0.0.tgz"));
    expect(screen.queryByRole("menuitem", { name: "移动" })).toBeNull();
    expect(screen.queryByRole("menuitem", { name: "重命名" })).toBeNull();
    await user.click(screen.getByRole("menuitem", { name: "删除" }));
    await user.click(
      within(await screen.findByRole("dialog")).getByRole("button", { name: "删除" }),
    );

    await waitFor(() =>
      expect(body).toEqual({
        action: "delete",
        targets: [{ type: "asset_path", path: "demo/-/demo-1.0.0.tgz" }],
        overrideReason: "管理员资产操作",
      }),
    );
  });

  it("成功操作刷新树并清空选择和详情", async () => {
    const user = userEvent.setup();
    let rootReads = 0;
    useRawTree(() => {
      rootReads += 1;
    });
    server.use(
      http.post("*/api/v1/repositories/:name/assets/operations", () =>
        HttpResponse.json({ operationId: "op-refresh", affected: 1 }),
      ),
    );
    renderRawBrowser();

    await user.click(await screen.findByText("a.txt"));
    expect(screen.getByRole("heading", { name: "文件详情" })).toBeTruthy();
    fireEvent.contextMenu(treeItem("a.txt"));
    await user.click(await screen.findByRole("menuitem", { name: "删除" }));
    await user.click(
      within(await screen.findByRole("dialog")).getByRole("button", { name: "删除" }),
    );

    await waitFor(() => expect(rootReads).toBeGreaterThanOrEqual(2));
    expect(treeItem("a.txt").getAttribute("style")).not.toContain("background");
    expect(screen.queryByRole("heading", { name: "文件详情" })).toBeNull();
  });

  it("成功刷新根树后会重新加载仍保持展开的目录", async () => {
    const user = userEvent.setup();
    let rootReads = 0;
    let childReads = 0;
    server.use(
      http.get("*/api/v1/repositories/:name/tree", ({ request }) => {
        const prefix = new URL(request.url).searchParams.get("prefix") ?? "";
        if (prefix === "docs/") {
          childReads += 1;
          return HttpResponse.json({
            directories: [],
            files: [
              {
                path: "docs/hidden.txt",
                size: 1,
                hash: "hidden",
                updatedAt: "2026-08-21T00:00:00Z",
              },
            ],
          });
        }
        rootReads += 1;
        return HttpResponse.json({
          directories: ["docs/"],
          files: [
            {
              path: "outside.txt",
              size: 1,
              hash: "outside",
              updatedAt: "2026-08-21T00:00:00Z",
            },
          ],
        });
      }),
      http.post("*/api/v1/repositories/:name/assets/operations", () =>
        HttpResponse.json({ operationId: "op-refresh-expanded", affected: 1 }),
      ),
    );
    renderRawBrowser();

    await user.click(await screen.findByText("docs"));
    expect(await screen.findByText("hidden.txt")).toBeTruthy();
    fireEvent.contextMenu(treeItem("outside.txt"));
    await user.click(await screen.findByRole("menuitem", { name: "删除" }));
    await user.click(
      within(await screen.findByRole("dialog")).getByRole("button", { name: "删除" }),
    );

    await waitFor(() => expect(rootReads).toBeGreaterThanOrEqual(2));
    await waitFor(() => expect(childReads).toBeGreaterThanOrEqual(2));
    expect(screen.getByText("hidden.txt")).toBeTruthy();
  });

  it("失败操作保留树、选择和详情", async () => {
    const user = userEvent.setup();
    let rootReads = 0;
    useRawTree(() => {
      rootReads += 1;
    });
    server.use(
      http.post("*/api/v1/repositories/:name/assets/operations", () =>
        HttpResponse.json({ error: { code: "conflict", message: "操作失败" } }, { status: 409 }),
      ),
    );
    renderRawBrowser();

    await user.click(await screen.findByText("a.txt"));
    expect(treeItem("a.txt").getAttribute("style")).toContain("background");
    fireEvent.contextMenu(treeItem("a.txt"));
    await user.click(await screen.findByRole("menuitem", { name: "删除" }));
    await user.click(
      within(await screen.findByRole("dialog")).getByRole("button", { name: "删除" }),
    );

    await waitFor(() => expect(screen.getByText("操作失败")).toBeTruthy());
    expect(rootReads).toBe(1);
    expect(treeItem("a.txt").getAttribute("style")).toContain("background");
    expect(screen.getByRole("heading", { name: "文件详情" })).toBeTruthy();
  });

  it("Maven 网页上传失败时保留已填写的 GAV", async () => {
    server.use(
      http.post("*/api/v1/repositories/:name/maven-upload", () =>
        HttpResponse.json(
          { error: { code: "standby_read_only", message: "备用节点为只读，当前请求已拒绝" } },
          { status: 503 },
        ),
      ),
    );
    const user = userEvent.setup();
    renderWithProviders(
      <RepoBrowser
        repoName="maven-releases"
        allowUpload
        forcedFormat="maven"
        forcedType="hosted"
      />,
      { authenticated: true, user: ADMIN },
    );

    await user.click(await screen.findByRole("button", { name: "上传制品" }));
    const groupId = screen.getByLabelText("GroupId") as HTMLInputElement;
    const artifactId = screen.getByLabelText("ArtifactId") as HTMLInputElement;
    const version = screen.getByLabelText("Version") as HTMLInputElement;
    await user.type(groupId, "com.example");
    await user.type(artifactId, "demo");
    await user.type(version, "1.0.0");
    const fileInput = document.querySelector<HTMLInputElement>('input[type="file"]')!;
    await user.upload(
      fileInput,
      new File(["jar"], "demo.jar", { type: "application/java-archive" }),
    );

    expect(await screen.findByText("备用节点为只读，当前请求已拒绝")).toBeTruthy();
    expect(groupId.value).toBe("com.example");
    expect(artifactId.value).toBe("demo");
    expect(version.value).toBe("1.0.0");
  });

  it("Maven 网页上传成功后展示反馈并清空版本", async () => {
    const user = userEvent.setup();
    renderWithProviders(
      <RepoBrowser
        repoName="maven-releases"
        allowUpload
        forcedFormat="maven"
        forcedType="hosted"
      />,
      { authenticated: true, user: ADMIN },
    );

    await user.click(await screen.findByRole("button", { name: "上传制品" }));
    const version = screen.getByLabelText("Version") as HTMLInputElement;
    await user.type(screen.getByLabelText("GroupId"), "com.example");
    await user.type(screen.getByLabelText("ArtifactId"), "demo");
    await user.type(version, "1.0.1");
    const fileInput = document.querySelector<HTMLInputElement>('input[type="file"]')!;
    await user.upload(
      fileInput,
      new File(["jar"], "demo.jar", { type: "application/java-archive" }),
    );

    expect(await screen.findByText("上传成功")).toBeTruthy();
    await waitFor(() => expect(version.value).toBe(""));
  });
});
