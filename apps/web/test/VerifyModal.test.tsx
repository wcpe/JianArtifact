// 校验 / 深度校验弹窗的守卫测试。
//
// 本文件的核心是**诚实性**：后端 verify 是同步接口（一次返回结论），没有中间进度可查。
// 因此界面不画百分比，改为
//   ① 开跑前列出真实会执行的检查项；
//   ② 跑完后就地展示结论（通过 / 未通过 + 原因 + 规模 + 耗时）；
//   ③ **浅校验必须显式写明"不核对对象内容"**——不然"校验通过"会被读成"这份包没问题"，
//      而它对"结构完好但内容被篡改"的包照样判通过。
// ③ 是本文件最重要的一条：去掉那句提示，用例 7 必须转红。
//
// 还有一条同样属"诚实性"的守卫：404（包已被淘汰）不是"校验失败"，不得就地留一个
// 误导性的红结论，而应把处置权交回外层（onStale）并关闭弹窗。
import { screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { VerifyModal } from "../src/components/backup/VerifyModal";
import { ApiError } from "../src/api/client";
import * as endpoints from "../src/api/endpoints";
import { renderWithProviders } from "./harness";

// 用例间恢复所有 spy，避免替换实现泄漏到后续用例。
afterEach(() => {
  vi.restoreAllMocks();
});

/** 一个永不落定的请求：用于稳定观察"运行中"这一帧。 */
function pending<T>(): Promise<T> {
  return new Promise<T>(() => {});
}

function renderModal(props: {
  request: { packageId: string; deep: boolean } | null;
  onClose?: () => void;
  onStale?: (err: unknown) => boolean;
  onSettled?: () => void;
}) {
  return renderWithProviders(
    <VerifyModal
      request={props.request}
      onClose={props.onClose ?? vi.fn()}
      onStale={props.onStale}
      onSettled={props.onSettled}
    />,
    { route: "/migrations?tab=backups", authenticated: true },
  );
}

describe("校验弹窗", () => {
  it("打开即列出检查项并显示运行中文案；完成后就地给出结论与规模、耗时", async () => {
    // 运行中挂一个不落定的请求，才能稳定断言"跑之前列了什么"。
    let release: (v: { ok: boolean; blobs: number; assets: number }) => void = () => {};
    vi.spyOn(endpoints, "verifyBackup").mockImplementation(
      () =>
        new Promise<{ ok: boolean; blobs: number; assets: number }>((resolve) => {
          release = resolve;
        }),
    );

    renderModal({ request: { packageId: "bk-20261014-120000-a1b2c3", deep: false } });

    // 检查项清单在**开跑之前**就要列出来（它是"将要做什么"，不是"做了什么"的假进度）。
    expect(await screen.findByText("将执行以下检查")).toBeTruthy();
    expect(screen.getByText("清单一致性：manifest.json 与包头记录的包标识一致")).toBeTruthy();
    expect(screen.getByText("数据库快照：db 的体积与 SHA256 摘要均与清单相符")).toBeTruthy();
    expect(
      screen.getByText("对象索引：blobs.index 摘要相符，条目数与总字节与清单一致"),
    ).toBeTruthy();
    expect(
      screen.getByText("归档完整性：manifest / db / blobs.index 三者齐备，无缺件多余件"),
    ).toBeTruthy();
    // 目标包标识与运行中文案。
    expect(screen.getByText("bk-20261014-120000-a1b2c3")).toBeTruthy();
    expect(screen.getByText("正在校验，请稍候…")).toBeTruthy();

    // 结论落地：就地显示通过 + 规模（数字必须真的渲染出来，而不是只显示标签）。
    release({ ok: true, blobs: 412, assets: 87 });
    expect(await screen.findByText("校验通过")).toBeTruthy();
    expect(screen.getByText("共 412 个对象、87 份制品")).toBeTruthy();
    // 耗时也要给一个具体量级（"秒"这个单位必须在）。
    expect(screen.getByText(/^耗时 \d+\.\d 秒$/)).toBeTruthy();
    // 跑完之后不再显示运行中文案。
    expect(screen.queryByText("正在校验，请稍候…")).toBeNull();
  });

  it("浅校验必须写明「不核对对象内容」；深校验替换为逐对象摘要项且不再出现该提示", async () => {
    // 浅校验通过 ≠ 内容未被篡改。不写这句，"校验通过"会被读成"这份包没问题"。
    vi.spyOn(endpoints, "verifyBackup").mockResolvedValue({ ok: true, blobs: 3, assets: 1 });

    const shallow = renderModal({
      request: { packageId: "bk-20261014-120000-a1b2c3", deep: false },
    });
    const notContent =
      "注意：浅校验不核对对象内容。它通过只说明包的结构与索引完整，不能证明内容未被篡改——需要后者请用深度校验。";
    expect(await screen.findByText(notContent)).toBeTruthy();
    // 浅校验不列"逐对象内容摘要"这一项（它确实不会执行）。
    expect(screen.queryByText("逐对象内容摘要：把每个 blob 重新算一遍摘要与清单比对")).toBeNull();
    // 通过后该提示仍在：它约束的是"通过"这个结论的含义，不随某一次结果消失。
    expect(await screen.findByText("校验通过")).toBeTruthy();
    expect(screen.getByText(notContent)).toBeTruthy();
    shallow.unmount();

    // 深校验：逐对象内容摘要这一项必须列出来，且那句"不核对内容"不再出现
    // （深校验确实核对了，留着这句反而变成误导性免责声明）。
    renderModal({ request: { packageId: "bk-20261014-120000-a1b2c3", deep: true } });

    expect(
      await screen.findByText("逐对象内容摘要：把每个 blob 重新算一遍摘要与清单比对"),
    ).toBeTruthy();
    expect(screen.queryByText(notContent)).toBeNull();
    // 深校验的运行中文案要如实说明"耗时与包体积同阶"，别让用户以为卡住了。
    expect(screen.getByText("深度校验备份包")).toBeTruthy();
  });

  it("深校验运行时给出「耗时同阶」的说明，而不是只转圈", async () => {
    vi.spyOn(endpoints, "verifyBackup").mockImplementation(() => pending());
    renderModal({ request: { packageId: "bk-20261014-120000-a1b2c3", deep: true } });

    expect(
      await screen.findByText("正在逐字节核对内容，耗时与包体积同阶，请耐心等待…"),
    ).toBeTruthy();
  });

  it("校验未通过（后端 ok=false 带 error）时就地显示失败结论与原因", async () => {
    // 失败结论必须留在弹窗里，不能随一个 toast 消失——用户需要照着原因去处理。
    vi.spyOn(endpoints, "verifyBackup").mockResolvedValue({
      ok: false,
      error: "blobs.index 摘要不符：期望 9f2c…，实际 1a4e…",
    });

    renderModal({ request: { packageId: "bk-20261014-120000-a1b2c3", deep: true } });

    expect(await screen.findByText("校验未通过")).toBeTruthy();
    expect(screen.getByText("blobs.index 摘要不符：期望 9f2c…，实际 1a4e…")).toBeTruthy();
    // 未通过时不该出现"共 N 个对象"的规模总结（那读起来像通过了）。
    expect(screen.queryByText(/共 \d+ 个对象、\d+ 份制品/)).toBeNull();
  });

  it("请求本身失败（HTTP 422）时同样就地显示原因，不吞掉", async () => {
    vi.spyOn(endpoints, "verifyBackup").mockRejectedValue(
      new ApiError("invalid_request", "deep 参数与包模式不兼容", 422),
    );

    renderModal({ request: { packageId: "bk-20261014-120000-a1b2c3", deep: true } });

    expect(await screen.findByText("校验未通过")).toBeTruthy();
    expect(screen.getByText("deep 参数与包模式不兼容")).toBeTruthy();
  });

  it("包已被淘汰（404）：不显示误导性的失败结论，交回外层处置并关闭", async () => {
    // 404 是"这一行过期了"，不是"这份包校验没过"。就地画一个红结论会让人去查一份
    // 根本不存在的包。
    vi.spyOn(endpoints, "verifyBackup").mockRejectedValue(
      new ApiError("not_found", "备份包不存在", 404),
    );
    const onStale = vi.fn().mockReturnValue(true);
    const onClose = vi.fn();

    renderModal({
      request: { packageId: "bk-20261014-120000-a1b2c3", deep: false },
      onStale,
      onClose,
    });

    await waitFor(() => {
      expect(onStale).toHaveBeenCalledTimes(1);
    });
    expect(onClose).toHaveBeenCalled();
    expect(screen.queryByText("校验未通过")).toBeNull();
  });

  it("结论落地后通知外层一次（用于重拉「最近校验」）", async () => {
    vi.spyOn(endpoints, "verifyBackup").mockResolvedValue({ ok: true, blobs: 1, assets: 1 });
    const onSettled = vi.fn();

    renderModal({ request: { packageId: "bk-20261014-120000-a1b2c3", deep: false }, onSettled });

    expect(await screen.findByText("校验通过")).toBeTruthy();
    await waitFor(() => {
      expect(onSettled).toHaveBeenCalledTimes(1);
    });
  });
});
