// 「操作聚合」类型分区回归：非制品类管理动作（令牌 / 设置 / 仓库 / 用户）此前
// 全部落进兜底「其他」，一屏只能看到「其他」一大坨，既看不出操作类型、也无法按类型筛选。
// 本测试锁定这些常见管理动作各自归入独立类型；「其他」只留真正无法分类的杂项。
import { describe, expect, it } from "vitest";

import { operationKindOf } from "./operations";

describe("operationKindOf 的类型分区", () => {
  it("制品动作仍按上传 / 删除 / 移动分区", () => {
    expect(operationKindOf("asset.put")).toBe("upload");
    expect(operationKindOf("asset.upload")).toBe("upload");
    expect(operationKindOf("asset.delete")).toBe("delete");
    expect(operationKindOf("asset.move")).toBe("move");
    expect(operationKindOf("asset.rename")).toBe("move");
  });

  it("令牌动作归「令牌」，不再进兜底「其他」", () => {
    expect(operationKindOf("token.create")).toBe("token");
    expect(operationKindOf("token.delete")).toBe("token");
  });

  it("设置动作归「设置」", () => {
    expect(operationKindOf("setting.update")).toBe("setting");
    expect(operationKindOf("setting.set")).toBe("setting");
  });

  it("仓库与用户动作各自成类", () => {
    expect(operationKindOf("repo.create")).toBe("repo");
    expect(operationKindOf("repo.delete")).toBe("repo");
    expect(operationKindOf("repo.online")).toBe("repo");
    expect(operationKindOf("user.create")).toBe("user");
    expect(operationKindOf("user.delete")).toBe("user");
  });

  it("真正无法分类的杂项仍归「其他」，未知动作不抛错", () => {
    expect(operationKindOf("migration.start")).toBe("other");
    expect(operationKindOf("")).toBe("other");
    expect(operationKindOf(undefined)).toBe("other");
    expect(operationKindOf(42)).toBe("other");
  });

  it("大小写与空白不容错错配：统一按小写比较", () => {
    expect(operationKindOf(" TOKEN.CREATE ")).toBe("token");
    expect(operationKindOf("Setting.Update")).toBe("setting");
  });
});
