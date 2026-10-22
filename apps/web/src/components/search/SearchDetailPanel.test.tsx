// 详情面板的同版本文件列表：
// - 每行是「选中按钮 + 下载链接」两个并列元素，**不能**把 `<a href>` 嵌进 `<button>`——
//   HTML 内容模型禁止可交互后代，浏览器还会把可聚焦后代从 Tab 序列里剔除，
//   键盘用户根本到不了下载链接；
// - 下载链接是真正的 `<a>` 且带 href，选中按钮才是 `<button>`；
// - 点击行内下载不会顺带切换选中项（两者互不干扰）。
import { MantineProvider } from "@mantine/core";
import { render, screen, within } from "@testing-library/react";
import "@testing-library/jest-dom/vitest";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import type { SearchPackage, SearchPackageFile, SearchPackageVersion } from "../../api/endpoints";
import "../../i18n";
import { SearchDetailPanel } from "./SearchDetailPanel";

function file(name: string, path: string): SearchPackageFile {
  return {
    name,
    path,
    repository: "maven-releases",
    size: 1024,
    sha1: `sha1-${name}`,
    updatedAt: "2026-10-18 09:12:33",
    fileKind: "jar",
  };
}

const JAR = file("foo-1.17.12.jar", "com/example/foo/1.17.12/foo-1.17.12.jar");
const POM = file("foo-1.17.12.pom", "com/example/foo/1.17.12/foo-1.17.12.pom");
const SIBLINGS = [JAR, POM];

const PKG: SearchPackage = {
  coordinate: "com.example:foo",
  kind: "maven",
  repositories: ["maven-releases"],
  versionCount: 1,
  fileCount: 2,
  bestPrecision: "exact",
  latestUpdatedAt: "2026-10-18 09:12:33",
  topVersion: "1.17.12",
  versions: [],
};

const VERSION: SearchPackageVersion = {
  version: "1.17.12",
  precision: "exact",
  fileCount: 2,
  size: 2048,
  latestUpdatedAt: "2026-10-18 09:12:33",
  files: SIBLINGS,
};

function renderPanel(onSelectFile = vi.fn()) {
  render(
    <MantineProvider>
      <SearchDetailPanel
        pkg={PKG}
        version={VERSION}
        file={JAR}
        siblings={SIBLINGS}
        onSelectFile={onSelectFile}
        onOpenRepository={vi.fn()}
      />
    </MantineProvider>,
  );
  return { onSelectFile };
}

/** 同版本文件列表的每一行（容器元素，内含选中按钮与下载链接）。 */
function siblingRows(): HTMLElement[] {
  return screen.getAllByTestId("search-sibling-file").map((button) => button.parentElement!);
}

describe("SearchDetailPanel 同版本文件列表", () => {
  it("下载链接不嵌在按钮里，且是带 href 的真实链接", () => {
    renderPanel();

    for (const row of siblingRows()) {
      // 行里不允许出现「链接被按钮包住」的结构。
      expect(row.querySelector("button a")).toBeNull();
      expect(row.querySelector("a[href]")).not.toBeNull();
      // 选中按钮本身不能是链接触发的元素，二者是兄弟关系。
      const link = row.querySelector("a[href]")!;
      expect(link.closest("button")).toBeNull();
    }
  });

  it("每个文件都能通过键盘到达选中按钮与下载链接（两者都在可聚焦序列里）", async () => {
    renderPanel();

    const user = userEvent.setup();
    const firstRow = siblingRows()[0]!;
    const selectButton = within(firstRow).getAllByRole("button")[0]!;
    const downloadLink = within(firstRow).getByRole("link");

    // 下载链接的可访问名带上文件名，读屏不必靠上下文猜是哪一个。
    expect(downloadLink).toHaveAccessibleName(/下载/);
    expect(downloadLink).toHaveAccessibleName(new RegExp(JAR.name.replace(".", "\\.")));

    selectButton.focus();
    expect(selectButton).toHaveFocus();
    await user.tab();
    expect(downloadLink).toHaveFocus();
  });

  it("点下载链接不切换选中项，点行本身才切换", async () => {
    const { onSelectFile } = renderPanel();
    const user = userEvent.setup();
    const rows = siblingRows();

    await user.click(within(rows[0]!).getByRole("link"));
    expect(onSelectFile).not.toHaveBeenCalled();

    // 第二行（pom）的选中按钮触发切换。
    await user.click(within(rows[1]!).getAllByRole("button")[0]!);
    expect(onSelectFile).toHaveBeenCalledWith(POM);
  });
});
