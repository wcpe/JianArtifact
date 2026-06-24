// 权限动作展示辅助单元测试：四级动作的选项、标签与配色映射。

import { describe, it, expect } from 'vitest';
import { PERMISSION_OPTIONS, permissionColor, permissionLabel } from './permissions';
import type { Permission } from '../api/types';

describe('权限动作展示辅助', () => {
  it('下拉选项覆盖四级动作且顺序由低到高', () => {
    expect(PERMISSION_OPTIONS.map((o) => o.value)).toEqual(['read', 'write', 'delete', 'admin']);
  });

  it('每个动作有对应中文短标签', () => {
    expect(permissionLabel('read')).toBe('读');
    expect(permissionLabel('write')).toBe('写');
    expect(permissionLabel('delete')).toBe('删除');
    expect(permissionLabel('admin')).toBe('管理');
  });

  it('每个动作有非空徽章配色', () => {
    const actions: Permission[] = ['read', 'write', 'delete', 'admin'];
    for (const action of actions) {
      expect(permissionColor(action)).toBeTruthy();
    }
  });
});
