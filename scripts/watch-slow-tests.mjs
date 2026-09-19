#!/usr/bin/env node
// 慢用例观测：对已知的负载敏感用例记录单次耗时，超阈值时在 CI 里发告警。
//
// 背景：test/AppRoutes.test.tsx 的「登录访问 /host-monitoring」用例依赖图表首屏渲染，
// 42 文件并行时会偶发撞上等待上限。治理办法是「放宽阈值到真实耗时之上 + 观测」：
// 放宽会让测试对**性能退化**失去敏感度（首屏从 2s 退化到 11s 测试仍绿），
// 所以这里补上观测——它是软告警（不 fail CI），只把退化暴露出来。
//
// 说明（诚实口径）：单次运行只有 1 个样本，因此这里是「单次耗时阈值告警」，不是严格 P95。
// 真正跨运行的 P95 需要把每次结果持久化（后续可用 CI artifact 累积后再算），此处不做。
//
// 用法：node scripts/watch-slow-tests.mjs
// 环境变量：SLOW_TEST_BUDGET_MS 覆盖默认阈值（毫秒，默认 10000）。
import { spawnSync } from "node:child_process";
import { mkdirSync, readFileSync, rmSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const repoRoot = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const webDir = join(repoRoot, "apps", "web");
const reportDir = join(repoRoot, ".tmp");
const reportFile = join(reportDir, "slow-watch.json");

/** 被观测的用例：文件名 + 用例名子串（避开完整中文标题，减少匹配脆弱性）。 */
const WATCHED = [
  { file: "test/AppRoutes.test.tsx", namePart: "/host-monitoring" },
];

const budgetMs = Number(process.env.SLOW_TEST_BUDGET_MS ?? 10_000);

mkdirSync(reportDir, { recursive: true });

let warned = 0;

for (const target of WATCHED) {
  const run = spawnSync(
    process.execPath,
    [
      join(webDir, "node_modules", "vitest", "vitest.mjs"),
      "run",
      target.file,
      "--reporter=json",
      `--outputFile=${reportFile}`,
    ],
    { cwd: webDir, encoding: "utf8", stdio: ["ignore", "ignore", "pipe"] },
  );

  if (run.status !== 0) {
    console.warn(
      `::warning::慢用例观测：${target.file} 未能通过（退出码 ${run.status ?? "未知"}），跳过耗时统计。`,
    );
    console.warn((run.stderr ?? "").split("\n").slice(-3).join("\n"));
    continue;
  }

  let report;
  try {
    report = JSON.parse(readFileSync(reportFile, "utf8"));
  } catch {
    console.warn(`::warning::慢用例观测：无法解析 ${target.file} 的报告，跳过耗时统计。`);
    continue;
  }

  const durations = [];
  for (const suite of report.testResults ?? []) {
    for (const assertion of suite.assertionResults ?? []) {
      if (!assertion.fullName?.includes(target.namePart)) continue;
      if (typeof assertion.duration === "number") {
        durations.push({ fullName: assertion.fullName, duration: assertion.duration });
      }
    }
  }

  if (durations.length === 0) {
    console.warn(
      `::warning::慢用例观测：在 ${target.file} 里没找到匹配「${target.namePart}」的用例，` +
        "可能已改名或被删除——观测目标失效，请同步更新 scripts/watch-slow-tests.mjs。",
    );
    continue;
  }

  for (const { fullName, duration } of durations) {
    const ms = Math.round(duration);
    const label = `${target.file} › ${fullName}`;
    if (ms >= budgetMs) {
      warned += 1;
      console.warn(
        `::warning::慢用例告警：${label} 耗时 ${ms}ms，超过预算 ${budgetMs}ms。` +
          `该用例的等待上限已被放宽，超时不再失败——若不是本机负载，请检查是否引入性能退化。`,
      );
    } else {
      console.log(`慢用例观测：${label} 耗时 ${ms}ms（预算 ${budgetMs}ms，正常）。`);
    }
  }
}

rmSync(reportFile, { force: true });

if (warned > 0) {
  console.log(`慢用例观测结束：${warned} 条超过预算（仅告警，未阻断质量门）。`);
} else {
  console.log("慢用例观测结束：全部在预算内。");
}
