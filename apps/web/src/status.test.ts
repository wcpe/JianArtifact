import { describe, expect, it } from "vitest";

import { describeStatus } from "./status";

describe("describeStatus", () => {
  it("将契约状态枚举映射为中文标签", () => {
    expect(describeStatus("ok")).toBe("运行正常");
    expect(describeStatus("degraded")).toBe("降级运行");
    expect(describeStatus("unavailable")).toBe("不可用");
  });
});
