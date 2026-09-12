// FR-105：将文件树节点转换为后端统一资产操作目标。
import type { AssetOperationTarget, AssetOperationTargetType } from "../api/types";

export function assetOperationTarget(
  format: string,
  path: string,
  kind: "file" | "dir",
): AssetOperationTarget {
  if (format === "raw") {
    return { type: "raw_path", path };
  }
  if (format === "maven") {
    if (kind === "file") {
      return { type: "asset_path", path };
    }
    const parts = path.split("/").filter(Boolean);
    const type: AssetOperationTargetType = parts.length >= 3 ? "maven_version" : "maven_artifact";
    return { type, path };
  }
  if (format === "npm") {
    return { type: path.includes("/-/") ? "asset_path" : "npm_package", path };
  }
  return { type: "asset_path", path };
}

export function operationSupportsPathMutation(format: string): boolean {
  return format === "raw";
}
