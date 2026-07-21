// 验收站画廊测试：默认渲染首个展台、可切换分区、含全部注册展台导航。
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { App } from "../src/App";
import { sections } from "../src/sections";

describe("Gallery 验收站画廊", () => {
  it("默认渲染首个展台（设计令牌）", () => {
    render(<App />);
    expect(screen.getByTestId("section-tokens")).toBeInTheDocument();
  });

  it("导航含全部注册展台", () => {
    render(<App />);
    const nav = within(screen.getByRole("navigation"));
    for (const section of sections) {
      expect(nav.getByText(section.label)).toBeInTheDocument();
    }
  });

  it("点击导航切换到状态态展台", async () => {
    const user = userEvent.setup();
    render(<App />);
    await user.click(within(screen.getByRole("navigation")).getByText("状态态"));
    expect(screen.getByTestId("section-states")).toBeInTheDocument();
    expect(screen.queryByTestId("section-tokens")).not.toBeInTheDocument();
  });
});
