// FR-73：Maven 网页上传集成测试——maven hosted 登录态渲染上传卡并完成上传；raw 仓不渲染该卡。
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

describe("Maven 网页上传", () => {
  it("maven hosted 仓库渲染 GAV 表单，填表选文件后上传成功并清空 Version", async () => {
    const user = userEvent.setup();
    const { container } = renderDetail("maven-releases");
    // 上传卡与 GAV 字段就位
    expect(await screen.findByText("上传制品（Maven hosted）")).toBeTruthy();
    await user.type(screen.getByLabelText("GroupId"), "com.example");
    await user.type(screen.getByLabelText("ArtifactId"), "demo");
    await user.type(screen.getByLabelText("Version"), "1.2.3");
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
    await screen.findByText("上传制品（Maven hosted）");
    await user.type(screen.getByLabelText("GroupId"), "com.example");
    await user.type(screen.getByLabelText("ArtifactId"), "demo");
    await user.type(screen.getByLabelText("Version"), "1.0.0-SNAPSHOT");
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
