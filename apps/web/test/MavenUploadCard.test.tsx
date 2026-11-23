// Maven 网页上传集成测试——maven hosted 登录态渲染上传卡并完成上传；raw 仓不渲染该卡。
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";
import { Route, Routes } from "react-router-dom";

import { RepositoryDetailPage } from "../src/pages/RepositoryDetailPage";
import { renderWithProviders } from "./harness";

function renderDetail(name: string) {
  return renderWithProviders(
    <Routes>
      <Route path="/repositories/:name" element={<RepositoryDetailPage />} />
    </Routes>,
    { route: `/repositories/${name}`, authenticated: true },
  );
}

/** 上传卡默认收在 Collapse 里（display:none）；jsdom 30.1.3 起隐藏容器内的
 * 元素不可聚焦，user.type 键入会静默丢失——先点「上传制品」展开，再等字段
 * 可聚焦，与真实用户操作顺序一致。 */
async function expandUploadCard(user: ReturnType<typeof userEvent.setup>) {
  await user.click(screen.getByRole("button", { name: "上传制品" }));
}

async function waitFocusableUploadField(label: string) {
  const input = await screen.findByLabelText(label);
  await waitFor(() => {
    input.focus();
    expect(document.activeElement).toBe(input);
  });
  return input;
}

describe("Maven 网页上传", () => {
  it("maven hosted 仓库渲染 GAV 表单，填表选文件后上传成功并清空 Version", async () => {
    const user = userEvent.setup();
    const { container } = renderDetail("maven-releases");
    // 上传卡与 GAV 字段就位
    await expandUploadCard(user);
    expect(await screen.findByText("上传制品（Maven hosted）")).toBeTruthy();
    await user.type(await waitFocusableUploadField("GroupId"), "com.example");
    await user.type(await waitFocusableUploadField("ArtifactId"), "demo");
    await user.type(await waitFocusableUploadField("Version"), "1.2.3");
    // FileButton 渲染隐藏 file input，选择文件即触发上传
    const fileInput = container.querySelector('input[type="file"]');
    expect(fileInput).toBeTruthy();
    await user.upload(
      fileInput as HTMLInputElement,
      new File(["jar-bytes"], "demo-1.2.3.jar", { type: "application/java-archive" }),
    );
    // 成功信号：仅清空 Version、保留 GroupId/ArtifactId 便于连传多版本
    // （通知 toast 依赖应用层挂载的 <Notifications/>，测试夹具未挂载，不作断言）
    await waitFor(() =>
      expect((screen.getByLabelText("Version") as HTMLInputElement).value).toBe(""),
    );
    expect((screen.getByLabelText("GroupId") as HTMLInputElement).value).toBe("com.example");
  });

  it("SNAPSHOT 版本给出错误提示且不触发上传", async () => {
    const user = userEvent.setup();
    const { container } = renderDetail("maven-releases");
    await expandUploadCard(user);
    await screen.findByText("上传制品（Maven hosted）");
    await user.type(await waitFocusableUploadField("GroupId"), "com.example");
    await user.type(await waitFocusableUploadField("ArtifactId"), "demo");
    await user.type(await waitFocusableUploadField("Version"), "1.0.0-SNAPSHOT");
    await user.upload(
      container.querySelector('input[type="file"]') as HTMLInputElement,
      new File(["x"], "demo.jar"),
    );
    // 行内错误 + 通知均提示仅限 release
    expect(
      (await screen.findAllByText("网页上传仅限 release 版本，SNAPSHOT 请使用 mvn deploy 发布"))
        .length,
    ).toBeGreaterThan(0);
    expect(screen.queryByText("上传成功")).toBeNull();
  });

  it("raw hosted 仓库不渲染 Maven 上传卡", async () => {
    renderDetail("raw-hosted");
    // raw 仓走单文件上传卡，不出现 Maven GAV 表单
    expect(await screen.findByText("上传文件（Raw hosted）")).toBeTruthy();
    expect(screen.queryByText("上传制品（Maven hosted）")).toBeNull();
  });
});
