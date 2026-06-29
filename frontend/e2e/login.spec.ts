import { test, expect } from '@playwright/test';

// 登录流 E2E（FR-118，ADR-0036）：用 Mock 模式种子凭据登录、落到仪表盘。
// 种子管理员凭据：admin / admin123（见 src/test/mocks/store.ts seed()）。
test.describe('登录流', () => {
  test('管理员登录成功 → 进入仪表盘、私有仓库可见', async ({ page }) => {
    await page.goto('/login');

    // 填表单（标签：用户名 / 口令）并提交。
    await page.getByLabel('用户名').fill('admin');
    await page.getByLabel('口令').fill('admin123');
    await page.getByRole('button', { name: '登录' }).click();

    // 登录后落地到仪表盘（标题「仪表盘」）。
    await expect(page.getByRole('heading', { name: '仪表盘' })).toBeVisible();

    // 经应用内导航进仓库列表（避免整页刷新——Mock 内存 store 会随刷新重置丢会话）。
    // 登录用户可见此前对匿名隐藏的私有仓库。
    await page.locator('a[aria-label="仓库"]').click();
    await expect(page).toHaveURL(/\/repositories$/);
    await expect(page.getByText('maven-releases')).toBeVisible();
    await expect(page.getByText('npm-proxy')).toBeVisible();
    await expect(page.getByText('docker-internal')).toBeVisible();
  });

  test('错误口令登录失败 → 仍停留登录页', async ({ page }) => {
    await page.goto('/login');
    await page.getByLabel('用户名').fill('admin');
    await page.getByLabel('口令').fill('wrong-password');
    await page.getByRole('button', { name: '登录' }).click();

    // 凭据错误不应进入仪表盘，仍可见登录表单。
    await expect(page.getByRole('button', { name: '登录' })).toBeVisible();
    await expect(page.getByRole('heading', { name: '仪表盘' })).toHaveCount(0);
  });
});
