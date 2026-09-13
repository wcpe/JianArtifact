// 私网来源勾选项（既存任务）与 i18n 覆盖：
// - PrivateSourceToggle 勾选后必须调用 PATCH source-config 且成功后提示；
// - zh / en 两种语言的 migrations.allowPrivateSource* key 必须存在且同构。
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
// 注册 jest-dom matchers（toBeChecked 等），setup.ts 未全局引入。
import "@testing-library/jest-dom/vitest";
import { MantineProvider } from "@mantine/core";
import { describe, expect, it, vi, beforeEach } from "vitest";

import { en } from "../i18n/en";
import { zh } from "../i18n/zh";
// 触发 i18next 初始化（react-i18next 需要 initReactI18next 实例）。
import "../i18n";
import { PrivateSourceToggle } from "./MigrationDetailPage";

const updateSourceConfig = vi.hoisted(() => vi.fn());
const notifySuccess = vi.hoisted(() => vi.fn());
const notifyError = vi.hoisted(() => vi.fn());

vi.mock("../api/endpoints", () => ({
  updateMigrationSourceConfig: updateSourceConfig,
}));

vi.mock("../lib/feedback", () => ({
  confirmAction: vi.fn(),
  confirmDanger: vi.fn(),
  notifySuccess,
  notifyError,
}));

function renderToggle(initial: boolean) {
  return render(
    <MantineProvider>
      <PrivateSourceToggle taskId={3} initial={initial} />
    </MantineProvider>,
  );
}

describe("PrivateSourceToggle", () => {
  beforeEach(() => {
    updateSourceConfig.mockReset();
    notifySuccess.mockReset();
    notifyError.mockReset();
  });

  it("勾选后调用 PATCH source-config 携带 allowPrivateSource:true 并提示成功", async () => {
    updateSourceConfig.mockResolvedValue({});
    renderToggle(false);

    const checkbox = screen.getByRole("checkbox", { name: zh.migrations.allowPrivateSource });
    expect(checkbox).not.toBeChecked();

    await userEvent.click(checkbox);

    await waitFor(() => {
      expect(updateSourceConfig).toHaveBeenCalledWith(3, { allowPrivateSource: true });
    });
    await waitFor(() => {
      expect(notifySuccess).toHaveBeenCalled();
    });
    expect(notifyError).not.toHaveBeenCalled();
    expect(screen.getByRole("checkbox", { name: zh.migrations.allowPrivateSource })).toBeChecked();
  });

  it("失败时提示错误且不更新勾选态", async () => {
    updateSourceConfig.mockRejectedValue(new Error("409 conflict"));
    renderToggle(false);

    await userEvent.click(screen.getByRole("checkbox", { name: zh.migrations.allowPrivateSource }));

    await waitFor(() => {
      expect(notifyError).toHaveBeenCalled();
    });
    expect(notifySuccess).not.toHaveBeenCalled();
    expect(
      screen.getByRole("checkbox", { name: zh.migrations.allowPrivateSource }),
    ).not.toBeChecked();
  });

  it("初始已勾选时按既存 sourceConfig 状态渲染", () => {
    renderToggle(true);
    expect(screen.getByRole("checkbox", { name: zh.migrations.allowPrivateSource })).toBeChecked();
    expect(updateSourceConfig).not.toHaveBeenCalled();
  });
});

describe("i18n 私网勾选文案（zh/en）", () => {
  it("两种语言都存在 allowPrivateSource 与 allowPrivateSourceHint", () => {
    for (const resource of [zh, en]) {
      expect(typeof resource.migrations.allowPrivateSource).toBe("string");
      expect(resource.migrations.allowPrivateSource.length).toBeGreaterThan(0);
      expect(typeof resource.migrations.allowPrivateSourceHint).toBe("string");
      expect(resource.migrations.allowPrivateSourceHint.length).toBeGreaterThan(0);
    }
  });

  it("en 与 zh 的 migrations 命名空间键集合一致（避免英文缺键回落中文）", () => {
    expect(Object.keys(en.migrations).sort()).toEqual(Object.keys(zh.migrations).sort());
  });
});
