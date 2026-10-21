// 一键重启控件（问题 2）的守卫测试。
//
// 守的是什么：恢复的最后一步是重启服务，而此前界面只给出一条要用户自己复制的命令。
// 本文件钉住三件事：
//   ① **确认后才发请求**——重启会中断服务，误触代价真实（未确认不得发请求）；
//   ② `supported:false` 是**正常业务结果**（HTTP 200），界面必须显示可复制命令，
//      **不报错、不进入等待**——绝不假装成功，也不把"不支持"说成"失败"；
//   ③ `supported:true` 后自己探活，服务回来后调 `onRestarted` 让外层刷新。
//
// 探活的实现细节：首个探针延迟 1500ms、间隔 1500ms、总超时 60s，用 fetch("/api/v1/status")。
// 测试里把 fetch 换成可控桩（MSW 也拦得住，但这里要能逐次改返回值来表达
// "还没起来 → 起来了"这段过程，桩更直接）。
import { act, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { RestartControl } from "../src/components/backup/RestartControl";
import * as feedback from "../src/lib/feedback";
import * as endpoints from "../src/api/endpoints";
import { renderWithProviders } from "./harness";

/** 与 RestartControl 内的探活间隔一致（该常量未导出，故此处复写）。 */
const PROBE_INTERVAL_MS = 1500;

afterEach(() => {
  vi.restoreAllMocks();
});

/**
 * 把 confirmDanger 换成"直接确认"：本文件要测的是"确认后才发请求"，
 * 用真实 Mantine 弹窗点按钮会引入一层与被测行为无关的异步，且 Mantine 弹窗
 * 在 jsdom 下需要额外等待过渡。这里统一改成可控桩，并显式断言调用次数。
 */
function autoConfirm() {
  return vi.spyOn(feedback, "confirmDanger").mockImplementation((opts) => opts.onConfirm());
}

/**
 * 把探针 fetch 换成可编排的桩：每次调用取队列里的下一个结果，用尽后一律"活着"。
 *
 * **只统计探活路径**（`/api/v1/status`）：控件挂载时还会发起一次**只读**能力探测
 * （`getRestartCapability`），它同样走 fetch。若不按路径过滤，那条请求会被误当成"探针"
 * 而让"受理那一刻不该有探针"的断言假红——而两者是完全不同的事（一个是问"能不能重启"，
 * 一个是问"服务回来了吗"）。
 */
function stubProbe(results: boolean[]) {
  let index = 0;
  const probedUrls: string[] = [];
  const fetchStub = vi.fn(async (input: RequestInfo | URL) => {
    const url = String(input);
    if (url.includes("/api/v1/status")) {
      probedUrls.push(url);
      const alive = results[Math.min(index, results.length - 1)] ?? true;
      index += 1;
      return { ok: alive } as Response;
    }
    // 其余请求（能力探测等）不参与探活编排，一律给个成功空响应。
    return { ok: true, json: async () => ({}) } as Response;
  });
  vi.stubGlobal("fetch", fetchStub);
  return { fetchStub, probedUrls };
}

describe("一键重启控件", () => {
  it("点击按钮先弹确认框；未确认不发重启请求", async () => {
    // confirmDanger 只记录不确认，模拟"用户看到了确认框但还没按"。
    const confirmSpy = vi.spyOn(feedback, "confirmDanger").mockImplementation(() => {});
    const restartSpy = vi.spyOn(endpoints, "restartService");

    renderWithProviders(<RestartControl />, {
      route: "/migrations?tab=backups",
      authenticated: true,
    });

    const user = userEvent.setup();
    await user.click(screen.getByTestId("restart-service"));

    expect(confirmSpy).toHaveBeenCalledTimes(1);
    // 重启是中断服务级动作：确认框是它的唯一前置，未确认绝不能发出去。
    expect(restartSpy).not.toHaveBeenCalled();
    // 也不该提前进入等待态（那会让人以为已经在重启了）。
    expect(screen.queryByText("等待服务回来…")).toBeNull();
  });

  it("确认后调 restartService；supported:false 时显示降级标题与命令原文，且不进入等待", async () => {
    // 降级是正常业务结果（HTTP 200）：展示命令，不报错。
    // 若改成"报错"或"隐藏命令"，本用例必须转红——用户会因此走进死胡同。
    autoConfirm();
    const notifyError = vi.spyOn(feedback, "notifyError");
    const restartSpy = vi.spyOn(endpoints, "restartService").mockResolvedValue({
      supported: false,
      command: "sudo systemctl restart jianartifact",
      reason: "未检测到 systemd 单元 jianartifact.service",
      scope: "system",
    });

    renderWithProviders(<RestartControl />, {
      route: "/migrations?tab=backups",
      authenticated: true,
    });

    const user = userEvent.setup();
    await user.click(screen.getByTestId("restart-service"));

    await waitFor(() => {
      expect(restartSpy).toHaveBeenCalledTimes(1);
    });

    // 降级标题 + 原因 + **命令原文**（命令必须真的渲染在页面上，而不是只放在 title/属性里）。
    expect(await screen.findByText("无法自动重启")).toBeTruthy();
    expect(
      screen.getByText(
        "当前部署形态不支持自动重启（未检测到 systemd 单元 jianartifact.service），请手动执行：",
      ),
    ).toBeTruthy();
    expect(screen.getByText("sudo systemctl restart jianartifact")).toBeTruthy();
    // 命令可复制。
    expect(screen.getByRole("button", { name: "复制命令" })).toBeTruthy();

    // 不报错、不进入等待——"不支持"不等于"失败"。
    expect(notifyError).not.toHaveBeenCalled();
    expect(screen.queryByText("等待服务回来…")).toBeNull();
    expect(screen.queryByText("等待服务恢复超时，请手动刷新页面确认。")).toBeNull();
    // 也不该继续画那个会让人反复点的重启按钮（要点的已经变成"复制命令"）。
    expect(screen.queryByTestId("restart-service")).toBeNull();
  });

  it("supported:true 且探活成功：进入等待态，随后提示「服务已回来」并调 onRestarted", async () => {
    autoConfirm();
    const notifySuccess = vi.spyOn(feedback, "notifySuccess");
    vi.spyOn(endpoints, "restartService").mockResolvedValue({
      supported: true,
      command: "systemctl restart jianartifact",
      scope: "system",
      delayMs: 2000,
    });
    // 第一次探针还没起来，第二次起来。
    stubProbe([false, true]);
    const onRestarted = vi.fn();

    renderWithProviders(<RestartControl onRestarted={onRestarted} />, {
      route: "/migrations?tab=backups",
      authenticated: true,
    });

    const user = userEvent.setup();
    await user.click(screen.getByTestId("restart-service"));

    // 受理后立刻进入等待态（按钮文案变化 + 说明）。
    expect(await screen.findByText("等待服务回来…")).toBeTruthy();
    expect(screen.getByText(/重启期间页面与服务器断开属正常现象/)).toBeTruthy();
    expect(notifySuccess).toHaveBeenCalledWith("已受理重启，服务即将中断，请稍候刷新页面。");

    // 等探活成功（首个探针延迟 1500ms + 一次失败后的 1500ms 间隔，给足余量）。
    await waitFor(
      () => {
        expect(onRestarted).toHaveBeenCalledTimes(1);
      },
      { timeout: 10_000 },
    );
    expect(notifySuccess).toHaveBeenCalledWith("服务已回来，页面数据已刷新。");
    // 回来了就不再显示等待文案。
    await waitFor(() => {
      expect(screen.queryByText("等待服务回来…")).toBeNull();
    });
  }, 20_000);

  it("探活一直失败到超时：如实说明超时，不无限转圈", async () => {
    // 探活有上限是这条的重点：无限转圈与"卡住了"无从区分。
    // 用假定时器把 60s 推完，避免真等一分钟。
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      autoConfirm();
      vi.spyOn(endpoints, "restartService").mockResolvedValue({
        supported: true,
        command: "systemctl restart jianartifact",
      });
      stubProbe([false]);

      renderWithProviders(<RestartControl />, {
        route: "/migrations?tab=backups",
        authenticated: true,
      });

      const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
      await user.click(screen.getByTestId("restart-service"));
      expect(await screen.findByText("等待服务回来…")).toBeTruthy();

      // 推过超时上限（首探针延迟 + 若干间隔 > 60s，这里直接推 70s）。
      await act(async () => {
        await vi.advanceTimersByTimeAsync(70_000);
      });

      expect(screen.getByText("等待服务恢复超时，请手动刷新页面确认。")).toBeTruthy();
      expect(screen.queryByText("等待服务回来…")).toBeNull();
    } finally {
      vi.useRealTimers();
    }
  }, 20_000);

  it("探活首个请求不立即发出（重启有约 2 秒延迟，立刻探必然还是旧进程）", async () => {
    // 若首个探针不等待，会在旧进程被杀之前就探到"活着"，从而误报"服务已回来"。
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      autoConfirm();
      vi.spyOn(endpoints, "restartService").mockResolvedValue({
        supported: true,
        command: "systemctl restart jianartifact",
      });
      const { probedUrls } = stubProbe([true]);

      renderWithProviders(<RestartControl />, {
        route: "/migrations?tab=backups",
        authenticated: true,
      });

      const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
      await user.click(screen.getByTestId("restart-service"));
      expect(await screen.findByText("等待服务回来…")).toBeTruthy();

      // 受理那一刻探针不该已经发出去（按探活路径计数，见 stubProbe 的说明）。
      expect(probedUrls).toHaveLength(0);
      // 推到首个探针的时刻：此时才发出第一次探活。
      await act(async () => {
        await vi.advanceTimersByTimeAsync(PROBE_INTERVAL_MS);
      });
      expect(probedUrls).toHaveLength(1);
      expect(probedUrls[0]).toContain("/api/v1/status");
    } finally {
      vi.useRealTimers();
    }
  });

  it("挂载只读探测本机可用命令，且**不得**用会触发重启的 POST", async () => {
    // 实测缺陷（本机现场）：仓库自带单元含 NoNewPrivileges=true，`sudo systemctl restart`
    // 照做必失败，真正可用的是不带 sudo 的 `systemctl restart`（走 polkit）。
    // 界面此前硬编码 sudo 那条，于是在同一台机器上给出两条命令、其中一条是错的。
    // 现改为向后端只读探测要命令；这里守住"用的是只读 GET，不是会排定重启的 POST"。
    const capSpy = vi.spyOn(endpoints, "getRestartCapability").mockResolvedValue({
      supported: true,
      command: "systemctl restart jianartifact",
      scope: "system",
    });
    const postSpy = vi.spyOn(endpoints, "restartService");
    const onCommandResolved = vi.fn();

    renderWithProviders(<RestartControl onCommandResolved={onCommandResolved} />, {
      route: "/migrations?tab=backups",
      authenticated: true,
    });

    await waitFor(() => {
      expect(onCommandResolved).toHaveBeenCalledWith("systemctl restart jianartifact");
    });
    // 只读探测被调用，而会排定重启的 POST **一次都没发**——这是本用例的核心不变量：
    // 若改用 POST 探测，用户一打开弹窗服务就会被重启。
    expect(capSpy).toHaveBeenCalled();
    expect(postSpy).not.toHaveBeenCalled();
  });
});
