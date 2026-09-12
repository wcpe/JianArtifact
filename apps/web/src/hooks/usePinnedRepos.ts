// 仓库置顶：本地偏好（localStorage，按浏览器记忆），与服务端仓库数据解耦。
// 供仓库列表与仪表盘状态面板共用：置顶的仓库排到最前，并显示图钉标记。
import { useLocalStorage } from "@mantine/hooks";

export const PINNED_REPOS_KEY = "jianartifact.pinnedRepos";

export function usePinnedRepos() {
  const [pinned, setPinned] = useLocalStorage<string[]>({
    key: PINNED_REPOS_KEY,
    defaultValue: [],
    getInitialValueInEffect: false,
  });

  const isPinned = (name: string): boolean => pinned.includes(name);

  const toggle = (name: string): void =>
    setPinned((current) =>
      current.includes(name) ? current.filter((item) => item !== name) : [...current, name],
    );

  /** 置顶的仓库排到最前（保持原相对顺序；ES2019+ Array.sort 稳定）。 */
  const sortPinnedFirst = <T extends { name: string }>(items: T[]): T[] =>
    [...items].sort((a, b) => (isPinned(a.name) ? 0 : 1) - (isPinned(b.name) ? 0 : 1));

  return { pinned, isPinned, toggle, sortPinnedFirst };
}
