// usePinnedRepos 单元测试：数据来源（服务端用户级 / 匿名回退全局）、排序与乐观更新回滚。
import { server } from "@jianartifact/devmock/node";
import { http, HttpResponse, delay } from "msw";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { ApiError } from "../src/api/client";
import { usePinnedRepos } from "../src/hooks/usePinnedRepos";
import { renderWithProviders } from "./harness";

/** 探针：暴露 pinned / 排序结果 / toggle（并把错误写到 DOM 便于断言）。 */
function Probe() {
  const { pinned, isPinned, toggle, sortPinnedFirst } = usePinnedRepos();
  const ordered = sortPinnedFirst([
    { name: "docker-hub" },
    { name: "maven-central" },
    { name: "npm-proxy" },
  ]).map((item) => item.name);
  return (
    <div>
      <div data-testid="pinned">{pinned.join(",")}</div>
      <div data-testid="ordered">{ordered.join(",")}</div>
      <div data-testid="is-pinned-npm">{String(isPinned("npm-proxy"))}</div>
      <button
        type="button"
        onClick={() => {
          void toggle("docker-hub").catch((err: unknown) => {
            const label = err instanceof ApiError ? `error:${err.status}` : `error:${String(err)}`;
            document.body.dataset.toggleError = label;
          });
        }}
      >
        toggle-docker
      </button>
      {pinned.map((name) => (
        <span key={name} data-testid={`pinned-${name}`} />
      ))}
    </div>
  );
}

describe("usePinnedRepos", () => {
  it("登录用户读取自己的置顶并置顶排序前置", async () => {
    renderWithProviders(<Probe />, { route: "/", authenticated: true });
    // 种子无用户级置顶（全局置顶不叠加到登录用户的个人集合）。
    await waitFor(() => expect(screen.getByTestId("is-pinned-npm").textContent).toBe("false"));

    const user = userEvent.setup();
    await user.click(screen.getByRole("button", { name: "toggle-docker" }));

    await waitFor(() => expect(screen.getByTestId("pinned").textContent).toBe("docker-hub"));
    // sortPinnedFirst 把置顶项提到最前（其余保持原相对顺序）。
    expect(screen.getByTestId("ordered").textContent).toBe("docker-hub,maven-central,npm-proxy");
  });

  it("匿名读取回退全局置顶（只读）", async () => {
    renderWithProviders(<Probe />, { route: "/", authenticated: false });
    // 种子全局置顶 = [maven-central(id5), npm-proxy(id2)]。
    await waitFor(() =>
      expect(screen.getByTestId("pinned").textContent).toBe("maven-central,npm-proxy"),
    );
    expect(screen.getByTestId("ordered").textContent).toBe("maven-central,npm-proxy,docker-hub");
  });

  it("写入失败时回滚乐观更新并抛出错误", async () => {
    server.use(
      http.put("*/api/v1/me/pinned-repositories", () =>
        HttpResponse.json(
          { error: { code: "internal_error", message: "内部错误" } },
          { status: 500 },
        ),
      ),
    );
    renderWithProviders(<Probe />, { route: "/", authenticated: true });
    await waitFor(() => expect(screen.getByTestId("pinned").textContent).toBe(""));

    const user = userEvent.setup();
    await user.click(screen.getByRole("button", { name: "toggle-docker" }));

    await waitFor(() => expect(document.body.dataset.toggleError).toBe("error:500"));
    // 回滚：乐观层被丢弃，回到服务端已知的空集合。
    await waitFor(() => expect(screen.getByTestId("pinned").textContent).toBe(""));
  });

  it("写入成功后重拉不复读旧缓存快照（不把刚保存的置顶顶回去）", async () => {
    let getCount = 0;
    server.use(
      // 放慢写入，让乐观层可见（真实网络下写入有几十到几百毫秒延迟，测试里若不放慢，
      // 乐观层会在同一次批处理里被清掉，观察不到）。
      http.put("*/api/v1/me/pinned-repositories", async () => {
        await delay(150);
        return HttpResponse.json({ repositoryIds: [1], names: ["docker-hub"] });
      }),
      http.get("*/api/v1/me/pinned-repositories", async () => {
        getCount++;
        // 首次（进入时的加载）返回写入前的旧快照并被缓存；写入后的重拉放慢并返回最新快照，
        // 留出观察窗口——期间界面不得被旧快照顶回空集合。
        if (getCount === 1) {
          return HttpResponse.json({ repositoryIds: [], names: [] });
        }
        await delay(150);
        return HttpResponse.json({ repositoryIds: [1], names: ["docker-hub"] });
      }),
    );
    renderWithProviders(<Probe />, { route: "/", authenticated: true });
    await waitFor(() => expect(screen.getByTestId("pinned").textContent).toBe(""));

    const user = userEvent.setup();
    await user.click(screen.getByRole("button", { name: "toggle-docker" }));

    // 乐观值在写入期间可见。
    await waitFor(() => expect(screen.getByTestId("pinned").textContent).toBe("docker-hub"));
    // 写入返回后、重拉返回前（窗口内）：不得被写入前的旧缓存快照顶回空集合。
    await new Promise((resolve) => setTimeout(resolve, 220));
    expect(screen.getByTestId("pinned").textContent).toBe("docker-hub");

    // 重拉返回后仍是最新快照。
    await waitFor(() => expect(getCount).toBeGreaterThan(1));
    expect(screen.getByTestId("pinned").textContent).toBe("docker-hub");
  });

  it("匿名写入被服务端 401 拒绝（需登录）", async () => {
    renderWithProviders(<Probe />, { route: "/", authenticated: false });
    await waitFor(() =>
      expect(screen.getByTestId("pinned").textContent).toBe("maven-central,npm-proxy"),
    );

    const user = userEvent.setup();
    await user.click(screen.getByRole("button", { name: "toggle-docker" }));

    await waitFor(() => expect(document.body.dataset.toggleError).toBe("error:401"));
    // 回滚到全局置顶，不被匿名写入污染。
    await waitFor(() =>
      expect(screen.getByTestId("pinned").textContent).toBe("maven-central,npm-proxy"),
    );
  });
});
