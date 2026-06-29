// 新增 MSW handlers 的契约 / 有状态强断言（FR-116/119，ADR-0035）。
//
// 重点验证此前未覆盖、导致 Mock 模式登录后被登出的管理员端点：
// 管理员访问返 2xx（绝不 401）、响应结构对齐 types.ts、PATCH / CRUD 有状态。
// 走真实 client.ts 发请求，被 src/test/setup.ts 全局装载的 MSW server 拦截；
// 每用例前 reset + 经 loginAs 取管理员会话。

import { describe, it, expect, beforeEach } from 'vitest';
import { seed } from './store';
import { loginAs } from './auth';
import * as api from '../../api/endpoints';

describe('新增 MSW handlers（管理员端点不返 401）', () => {
  beforeEach(async () => {
    seed();
    await loginAs('admin', 'admin123');
  });

  it('GET /settings/dynamic 返 200 带各节结构', async () => {
    const cfg = await api.getDynamicConfig();
    expect(cfg.limits).toBeDefined();
    expect(cfg.audit.retention_days).toBeTypeOf('number');
    expect(cfg.vuln).toHaveProperty('enabled');
    expect(cfg.auth).toHaveProperty('session_ttl_secs');
  });

  it('PATCH /settings/dynamic 有状态：写入后再读回新值', async () => {
    const before = await api.getDynamicConfig();
    const patched = { ...before, vuln: { ...before.vuln, enabled: !before.vuln.enabled } };
    const resp = await api.updateDynamicConfig(patched);
    expect(resp.vuln.enabled).toBe(!before.vuln.enabled);
    const after = await api.getDynamicConfig();
    expect(after.vuln.enabled).toBe(!before.vuln.enabled);
  });

  it('GET /protection/config 返 200，PATCH 有状态', async () => {
    const cfg = await api.getProtectionConfig();
    expect(cfg.rate_limit).toHaveProperty('enabled');
    const patched = { ...cfg, rate_limit: { ...cfg.rate_limit, enabled: !cfg.rate_limit.enabled } };
    const resp = await api.updateProtectionConfig(patched);
    expect(resp.rate_limit.enabled).toBe(!cfg.rate_limit.enabled);
    const after = await api.getProtectionConfig();
    expect(after.rate_limit.enabled).toBe(!cfg.rate_limit.enabled);
  });

  it('GET /protection/status 返 200 带窗内计数', async () => {
    const status = await api.protectionStatus();
    expect(status.window_counts.length).toBeGreaterThan(0);
    expect(status.active_banned_ips).toBe(0);
  });

  it('GET /protection/alerts 返空分页', async () => {
    const page = await api.listProtectionAlerts();
    expect(page.items).toEqual([]);
    expect(page.total).toBe(0);
  });

  it('GET /update/check 返 200「已是最新」（不 409）', async () => {
    const check = await api.checkUpdate();
    expect(check.update_available).toBe(false);
    expect(check.current_version).toBe(check.latest_version);
  });

  it('POST /update/apply 与 /update/rollback 返成功形态', async () => {
    expect((await api.applyUpdate()).status).toBe('ok');
    expect((await api.rollbackUpdate()).status).toBe('ok');
  });

  it('POST /system/restart 与 /system/shutdown 返 {status}', async () => {
    expect((await api.systemRestart()).status).toBe('ok');
    expect((await api.systemShutdown()).status).toBe('ok');
  });

  it('GET /monitor/metrics 返指定指标的时序点数组', async () => {
    const series = await api.getMetricSeries('host.cpu_percent', {
      from: Date.now() - 3600_000,
      to: Date.now(),
    });
    expect(series.metric).toBe('host.cpu_percent');
    expect(series.points.length).toBeGreaterThan(0);
    expect(series.points[0]).toHaveProperty('ts');
    expect(series.points[0]).toHaveProperty('value');
  });

  it('GET /analytics/usage 返聚合总览', async () => {
    const usage = await api.usageAnalytics(3);
    expect(usage.total_access).toBeTypeOf('number');
    expect(Array.isArray(usage.top_downloads)).toBe(true);
    expect(Array.isArray(usage.repo_usage)).toBe(true);
  });

  it('GET /system-logs 返分页日志、按级别过滤', async () => {
    const all = await api.listSystemLogs();
    expect(all.items.length).toBeGreaterThan(0);
    const warns = await api.listSystemLogs({ level: 'WARN' });
    expect(warns.items.every((l) => l.level === 'WARN')).toBe(true);
  });

  it('用户组 CRUD 有状态：建组 → 列出 → 加成员 → 删组', async () => {
    const before = await api.listGroups();
    const created = await api.createGroup('qa-team');
    const after = await api.listGroups();
    expect(after.length).toBe(before.length + 1);

    await api.addGroupMember(created.id, 'u-admin');
    const members = await api.listGroupMembers(created.id);
    expect(members.some((m) => m.user_id === 'u-admin')).toBe(true);

    await api.deleteGroup(created.id);
    expect((await api.listGroups()).some((g) => g.id === created.id)).toBe(false);
  });

  it('重复组名返回 409', async () => {
    await expect(api.createGroup('developers')).rejects.toMatchObject({ status: 409 });
  });

  it('仓库组 ACL CRUD 有状态', async () => {
    const repos = await api.listRepositories();
    const repo = repos[0];
    const groups = await api.listGroups();
    const created = await api.createGroupAcl(repo.id, groups[0].id, 'read');
    const list = await api.listGroupAcl(repo.id);
    expect(list.some((a) => a.id === created.id)).toBe(true);
    await api.deleteGroupAcl(repo.id, created.id);
    expect((await api.listGroupAcl(repo.id)).some((a) => a.id === created.id)).toBe(false);
  });

  it('Nexus 迁移：预览返示例、在线迁移建任务可列出与控制', async () => {
    const preview = await api.previewNexusRepositories({ base_url: 'https://nexus.example' });
    expect(preview.length).toBeGreaterThan(0);

    const created = await api.migrateNexusOnline({
      base_url: 'https://nexus.example',
      repositories: [{ source: 'maven-internal' }],
    });
    expect(created.job_id).toBeTruthy();

    const jobs = await api.listMigrationJobs();
    expect(jobs.some((j) => j.job_id === created.job_id)).toBe(true);

    const job = await api.getMigrationJob(created.job_id);
    expect(job.job_id).toBe(created.job_id);

    // 任务控制为幂等空操作（200/204），不抛错。
    await expect(api.pauseMigrationJob(created.job_id)).resolves.toBeUndefined();
    await expect(api.resumeMigrationJob(created.job_id)).resolves.toBeUndefined();
    await expect(api.cancelMigrationJob(created.job_id)).resolves.toBeUndefined();
  });
});
