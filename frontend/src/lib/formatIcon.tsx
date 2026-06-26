// 仓库格式专属图标（FR-93）：为文件树的仓库根 / 文件叶子按格式渲染专属 icon。
// 仅用现有 @tabler/icons-react，不新增依赖。

import {
  IconBox,
  IconBrandDocker,
  IconBrandGolang,
  IconBrandNpm,
  IconBrandPython,
  IconBrandRust,
  IconCoffee,
  IconPackage,
  type IconProps,
} from '@tabler/icons-react';
import type { ComponentType } from 'react';
import type { RepoFormat } from '../api/types';

/** 各仓库格式对应的图标组件（无专属图标的格式回退到通用包裹箱）。 */
const FORMAT_ICONS: Record<RepoFormat, ComponentType<IconProps>> = {
  maven: IconCoffee,
  npm: IconBrandNpm,
  docker: IconBrandDocker,
  pypi: IconBrandPython,
  cargo: IconBrandRust,
  go: IconBrandGolang,
  nuget: IconPackage,
  raw: IconBox,
};

/** 取某仓库格式的图标组件（默认 16px，可经 props 覆盖）。 */
export function FormatIcon({ format, ...props }: { format: RepoFormat } & IconProps) {
  const Icon = FORMAT_ICONS[format] ?? IconBox;
  return <Icon size={16} {...props} />;
}
