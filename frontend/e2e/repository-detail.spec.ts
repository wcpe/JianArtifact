import { test, expect } from '@playwright/test';

// 仓库详情 E2E（FR-118，ADR-0036）：登录后进仓库详情、浏览文件树。
// maven-releases（public，hosted）种子含制品 com/example/app/... 与 com/example/lib/...。
test.describe('仓库详情浏览', () => {
  test('登录后从列表进入 maven-releases 详情、浏览页签显示文件树', async ({ page }) => {
    // 先登录（仪表盘出现即认为会话就绪）。
    await page.goto('/login');
    await page.getByLabel('用户名').fill('admin');
    await page.getByLabel('口令').fill('admin123');
    await page.getByRole('button', { name: '登录' }).click();
    await expect(page.getByRole('heading', { name: '仪表盘' })).toBeVisible();

    // 经应用内导航进仓库列表并点开 maven-releases（避免整页刷新重置 Mock store）。
    await page.locator('a[aria-label="仓库"]').click();
    await expect(page).toHaveURL(/\/repositories$/);
    await page.getByText('maven-releases', { exact: true }).click();

    // 进入详情页：URL 落到 /repository?id=...，浏览页签可见。
    await expect(page).toHaveURL(/\/repository\?id=/);
    await expect(page.getByRole('tab', { name: '浏览' })).toBeVisible();

    // 文件树呈现制品坐标的顶层目录段（com）。
    await expect(page.getByText('com', { exact: true }).first()).toBeVisible();
  });
});
