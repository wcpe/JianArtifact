// 展示辅助函数单元测试：体积格式化与错误文案提取。

import { describe, it, expect } from 'vitest';
import { formatBytes, errorMessage } from './format';
import { ApiError } from '../api/client';

describe('formatBytes', () => {
  it('小于 1KB 显示字节', () => {
    expect(formatBytes(512)).toBe('512 B');
  });
  it('KB 级别保留两位小数', () => {
    expect(formatBytes(1536)).toBe('1.50 KB');
  });
  it('MB 级别换算正确', () => {
    expect(formatBytes(5 * 1024 * 1024)).toBe('5.00 MB');
  });
});

describe('errorMessage', () => {
  it('从 ApiError 提取文案', () => {
    expect(errorMessage(new ApiError(404, 'not_found', '资源不存在'))).toBe('资源不存在');
  });
  it('从普通 Error 提取文案', () => {
    expect(errorMessage(new Error('网络错误'))).toBe('网络错误');
  });
  it('未知类型回退默认文案', () => {
    expect(errorMessage('字符串')).toBe('发生未知错误');
  });
});
