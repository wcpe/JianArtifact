import { test, expect } from '@playwright/test';

// 管理员会话保持 E2E（FR-116/119，ADR-0035/0036）：回归此前「Mock 模式登录后立刻被登出」的缺陷。
// 根因为管理员页 / 外壳轮询的若干端点未被 mock 覆盖、返 401，触发「会话过期→登出」。
// 本规格登录 admin 后停留 3 秒，断言：① 仍为登录态（管理员导航项可见、未退回匿名）；
// ② 期间无 401 响应。种子凭据：admin / admin123（见 src/test/mocks/store.ts seed()）。
test.describe('管理员会话保持（Mock 模式）', () => {
  test('登录后停留 3 秒不被登出、无 401', async ({ page }) => {
    // 收集页面期间出现的 401 响应（任一即视为失败）。
    const unauthorized: string[] = [];
    page.on('response', (resp) => {
      if (resp.status() === 401) {
        unauthorized.push(resp.url());
      }
    });

    await page.goto('/login');
    await page.getByLabel('用户名').fill('admin');
    await page.getByLabel('口令').fill('admin123');
    await page.getByRole('button', { name: '登录' }).click();

    // 登录后落地仪表盘。
    await expect(page.getByRole('heading', { name: '仪表盘' })).toBeVisible();

    // 展开导航，确保宽态下管理员入口的文字标签可见可断言。
    await page.getByRole('button', { name: '切换导航展开收起' }).click();

    // 停留 3 秒，期间仪表盘 / 外壳会发起若干轮询（主机健康、检查更新、防护状态、动态配置等）。
    await page.waitForTimeout(3000);

    // ① 仍为登录态：管理员专属导航项（设置 / 系统 / 用户与组）仍在 → 未被登出退回匿名。
    // NavLink 标签以文本节点呈现，定位到导航地标内的文字即可（按角色 link 不稳定）。
    const nav = page.getByRole('navigation');
    await expect(nav.getByText('设置', { exact: true })).toBeVisible();
    await expect(nav.getByText('系统', { exact: true })).toBeVisible();
    await expect(nav.getByText('用户与组', { exact: true })).toBeVisible();
    // 页眉显示登出入口（登录态），而非匿名态的「登录」按钮。
    await expect(page.getByRole('button', { name: '登出' })).toBeVisible();

    // ② 期间无 401。
    expect(unauthorized, `出现 401 响应：${unauthorized.join(', ')}`).toEqual([]);
  });
});
