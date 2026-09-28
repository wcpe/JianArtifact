// 仓库置顶：**服务端持久化**（用户级 + 全局兜底，见迁移 0043）。
//
// 数据来源（读）：
// - 登录用户：`GET /me/pinned-repositories` 读自己的置顶（跨设备一致，不再"总会掉"）；
// - 匿名 / 未登录：同一端点回退**全局置顶**（管理员维护），使公开页也有稳定的置顶顺序。
//
// 与旧实现的差异：不再以 localStorage 为真源。旧键 `jianartifact.pinnedRepos` 仅作
// **只读降级**——服务端不可用（离线 / 老后端 404）时用浏览器里残留的旧置顶名兜底，
// 保证排序不退化；降级结果不回写，服务端恢复后立即被权威数据覆盖。
//
// 写入（`toggle`）：契约按 **repositoryId** 持久化（重命名后置顶不丢），而调用点传的是
// 仓库名（保持既有签名）。名字 → ID 由置顶响应自带的 ids/names 对应关系 + 一次仓库列表
// 解析得到；写入为**覆盖式**（整体替换），采用乐观更新 + 失败回滚。
import { useCallback, useEffect, useRef, useState } from "react";

import { ApiError } from "../api/client";
import {
  getMyPinnedRepositories,
  listAllRepositories,
  listPublicRepositories,
  putMyPinnedRepositories,
} from "../api/endpoints";
import { invalidateAsyncCache, useAsync } from "./useAsync";

export const PINNED_REPOS_KEY = "jianartifact.pinnedRepos";

/** 置顶状态：名字（有序）+ 名字到 ID 的映射（供写入换算）。 */
interface PinnedSnapshot {
  names: string[];
  idsByName: Record<string, number>;
}

/** 旧版本地置顶键：只读降级用（服务端不可用时的兜底），不再写入。 */
function readLegacyLocalPinned(): string[] {
  try {
    const raw = localStorage.getItem(PINNED_REPOS_KEY);
    if (!raw) return [];
    const parsed: unknown = JSON.parse(raw);
    return Array.isArray(parsed) ? parsed.filter((v): v is string => typeof v === "string") : [];
  } catch {
    return [];
  }
}

/**
 * 读取置顶快照：登录读用户级、匿名回退全局（同一端点）。
 * 端点不可用时依次降级到公开列表的 pinnedNames、再退到旧 localStorage 只读快照。
 */
async function loadPinnedSnapshot(): Promise<PinnedSnapshot> {
  try {
    const res = await getMyPinnedRepositories();
    const names = res.names ?? [];
    const ids = res.repositoryIds ?? [];
    const idsByName: Record<string, number> = {};
    names.forEach((name, index) => {
      if (typeof ids[index] === "number") idsByName[name] = ids[index]!;
    });
    // 服务端可用：即使为空集合也是权威答案（用户可能确实没有置顶）。
    return { names, idsByName };
  } catch {
    /* 落到下面的降级路径 */
  }
  try {
    const res = await listPublicRepositories();
    if (res.pinnedNames) return { names: res.pinnedNames, idsByName: {} };
  } catch {
    /* 公开列表也不可用：继续降级 */
  }
  return { names: readLegacyLocalPinned(), idsByName: {} };
}

export interface UsePinnedReposResult {
  /** 当前生效的置顶仓库名（有序，顺序即置顶顺序）。 */
  pinned: string[];
  isPinned: (name: string) => boolean;
  /**
   * 切换置顶：乐观更新 + 失败回滚。仅登录用户可以置顶——匿名调用会因 401 抛出，
   * 由调用方按 `ApiError.status === 401` 提示「需登录」（匿名只读全局置顶作为兜底）。
   */
  toggle: (name: string) => Promise<void>;
  /** 置顶的仓库排到最前（保持原相对顺序；ES2019+ Array.sort 稳定）。 */
  sortPinnedFirst: <T extends { name: string }>(items: T[]) => T[];
  /** 写入已生效但后台重拉失败（权威数据未刷新），供调用方提示；无失败为 null。 */
  refreshError: ApiError | null;
}

/**
 * 置顶偏好钩子：读服务端、写服务端（覆盖式）。
 *
 * 乐观更新：点击后先把名字集合改到本地（UI 立即响应），PUT 成功后重拉权威数据；
 * 失败则丢弃乐观层（自动回落到服务端已知状态）并把错误抛给调用方做提示。
 */
export function usePinnedRepos(): UsePinnedReposResult {
  const state = useAsync(loadPinnedSnapshot, [], {
    cacheKey: "pinned-repositories",
    keepPreviousData: true,
  });
  // 乐观层：非 null 时表示本地已先行变更、等待服务端确认。
  const [optimistic, setOptimistic] = useState<string[] | null>(null);
  // 写入后重拉：记住重拉前的快照对象，data 引用变化即视为新快照落地（见下方 effect）。
  const snapshotBeforeReloadRef = useRef<PinnedSnapshot | null>(null);
  // 名字 → ID 映射：先取置顶响应自带的对应关系，缺口再经仓库列表补全。
  const idsRef = useRef<Record<string, number>>({});

  const serverSnapshot = state.data;
  const reloadPinned = state.reload;
  idsRef.current = { ...idsRef.current, ...(serverSnapshot?.idsByName ?? {}) };
  const pinned = optimistic ?? serverSnapshot?.names ?? [];

  const isPinned = (name: string): boolean => pinned.includes(name);

  /** 置顶的仓库排到最前（保持原相对顺序；ES2019+ Array.sort 稳定）。 */
  const sortPinnedFirst = <T extends { name: string }>(items: T[]): T[] => {
    const pinnedSet = new Set(pinned);
    return [...items].sort(
      (a, b) => (pinnedSet.has(a.name) ? 0 : 1) - (pinnedSet.has(b.name) ? 0 : 1),
    );
  };

  const resolveIDs = useCallback(async (names: string[]): Promise<number[]> => {
    const missing = names.filter((name) => typeof idsRef.current[name] !== "number");
    if (missing.length > 0) {
      // 一次列表查询补全全部缺口（别名一并登记，使经别名传名也能命中）。
      try {
        const list = await listAllRepositories();
        for (const repo of list.items) {
          if (typeof repo.id !== "number") continue;
          idsRef.current[repo.name] = repo.id;
          for (const alias of repo.aliases ?? []) idsRef.current[alias] = repo.id;
        }
      } catch {
        /* 列表不可用：缺口以 0 占位，服务端会以 404 拒绝并触发回滚提示。 */
      }
    }
    return names.map((name) => idsRef.current[name] ?? 0);
  }, []);

  const toggle = useCallback(
    async (name: string): Promise<void> => {
      const current = optimistic ?? serverSnapshot?.names ?? [];
      const next = current.includes(name)
        ? current.filter((item) => item !== name)
        : [...current, name];
      setOptimistic(next);
      try {
        await putMyPinnedRepositories(await resolveIDs(next));
        // 服务端已接受：丢弃乐观层并重拉权威数据（保证与其它标签页 / 设备一致）。
      // 写入已生效：先失效缓存再重拉，否则重拉会先回放 60s 内的旧快照；
      // 乐观层保留到新快照落地再丢弃，重拉期间界面不得闪回写入前的旧值。
      invalidateAsyncCache("pinned-repositories");
      snapshotBeforeReloadRef.current = serverSnapshot;
      reloadPinned();
      } catch (err) {
        setOptimistic(null);
        throw err;
      }
    },
    [optimistic, serverSnapshot, resolveIDs, reloadPinned],
  );

  // 重拉的新快照落地（data 引用变化）或重拉失败时丢弃乐观层：此前界面一直显示乐观值，
  // 避免缓存回放 / 旧快照把刚保存的置顶顶回去；失败时由 refreshError 提示，不会无声停留。
  useEffect(() => {
    if (optimistic === null) return;
    const landed =
      snapshotBeforeReloadRef.current !== null &&
      state.data !== snapshotBeforeReloadRef.current;
    if (landed || state.refreshError) {
      snapshotBeforeReloadRef.current = null;
      setOptimistic(null);
    }
  }, [optimistic, state.data, state.refreshError]);

  // 返回签名与旧实现一致（调用点无需改动）：pinned / isPinned / toggle / sortPinnedFirst。
  return { pinned, isPinned, toggle, sortPinnedFirst, refreshError: state.refreshError };
}
