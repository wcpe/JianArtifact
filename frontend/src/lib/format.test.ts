// 展示辅助函数单元测试：体积格式化与错误文案提取。

import { describe, it, expect } from 'vitest';
import { formatBytes, formatCount, formatUptime, formatRelativeTime, errorMessage } from './format';
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

describe('formatCount', () => {
  it('小数值原样', () => {
    expect(formatCount(42)).toBe('42');
  });
  it('千位加分隔符', () => {
    expect(formatCount(12345)).toBe('12,345');
  });
  it('百万级多组分隔符', () => {
    expect(formatCount(1234567)).toBe('1,234,567');
  });
});

describe('formatUptime', () => {
  it('天级显示天与小时', () => {
    expect(formatUptime(3 * 86400 + 4 * 3600 + 30 * 60)).toBe('3 天 4 小时');
  });
  it('小时级显示小时与分钟', () => {
    expect(formatUptime(5 * 3600 + 12 * 60)).toBe('5 小时 12 分钟');
  });
  it('分钟级只显示分钟', () => {
    expect(formatUptime(7 * 60 + 30)).toBe('7 分钟');
  });
  it('不足一分钟显示刚刚启动', () => {
    expect(formatUptime(40)).toBe('刚刚启动');
  });
});

describe('formatRelativeTime', () => {
  const now = Date.parse('2026-06-27T12:00:00Z');
  it('一分钟内显示刚刚', () => {
    expect(formatRelativeTime('2026-06-27T11:59:30Z', now)).toBe('刚刚');
  });
  it('分钟级', () => {
    expect(formatRelativeTime('2026-06-27T11:45:00Z', now)).toBe('15 分钟前');
  });
  it('小时级', () => {
    expect(formatRelativeTime('2026-06-27T09:00:00Z', now)).toBe('3 小时前');
  });
  it('天级', () => {
    expect(formatRelativeTime('2026-06-25T12:00:00Z', now)).toBe('2 天前');
  });
  it('非法时间串回退原串', () => {
    expect(formatRelativeTime('不是时间', now)).toBe('不是时间');
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
