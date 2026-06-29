import { test, expect } from '@playwright/test';

// 公开浏览 E2E（FR-118，ADR-0036）：匿名访客无须登录即可浏览公开仓库。
// 跑在前端 Mock 模式（VITE_MOCK=true）上，种子数据含 public 仓库 maven-releases
// 与 private 仓库 npm-proxy / docker-internal；匿名应只见 public 仓库。
test.describe('公开浏览（匿名）', () => {
  test('匿名访客落地到公开仓库列表、只见 public 仓库', async ({ page }) => {
    // 落地路由对匿名重定向到 /repositories（公开浏览）。
    await page.goto('/');
    await expect(page).toHaveURL(/\/repositories$/);

    // 公开仓库 maven-releases 可见。
    await expect(page.getByText('maven-releases')).toBeVisible();

    // 私有仓库对匿名不可见（不泄露存在性）。
    await expect(page.getByText('npm-proxy')).toHaveCount(0);
    await expect(page.getByText('docker-internal')).toHaveCount(0);

    // 页眉显示「登录」入口（匿名态）。
    await expect(page.getByRole('button', { name: '登录' })).toBeVisible();
  });
});
