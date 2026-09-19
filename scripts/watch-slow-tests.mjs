#!/usr/bin/env node
// 慢用例观测：对已知的负载敏感用例记录耗时、累积跨运行历史并计算 P95，超阈值时告警。
//
// 背景：test/AppRoutes.test.tsx 的「登录访问 /host-monitoring」用例依赖图表首屏渲染，
// 42 文件并行时会偶发撞上等待上限。治理办法是「放宽阈值到真实耗时之上 + 观测」：
// 放宽会让测试对**性能退化**失去敏感度（首屏从 2s 退化到 11s 测试仍然绿），
// 所以这里补上观测——它是软告警（不 fail CI），只把退化持续暴露出来。
//
// 两层判据：
//   1. 单次：本次耗时超 SLOW_TEST_BUDGET_MS（默认 10s）→ 告警；
//   2. 历史：同一用例的**跨运行 P95** 超 SLOW_TEST_P95_BUDGET_MS（默认同上）→ 告警。
//      这一层才真正有意义：单次偶发慢会被质量门噪音淹没，而 P95 能把持续退化顶出来。
//
// 历史从哪来（诚实口径）：历史文件只是本地文件——.tmp/slow-history.json。
// CI 里由 .github/workflows/ci.yml 用 actions/cache 在质量门前 restore、质量门后 save，
// 从而实现跨运行累积。GitHub cache 是**尽力而为**（可能 miss、可能被清理），
// 因此历史缺失或样本很少时，脚本会明确打印「样本不足，无法给出可信 P95」而不装作正常。
//
// 用法：node scripts/watch-slow-tests.mjs [--print-history]
// 环境变量：
//   SLOW_TEST_BUDGET_MS      单次耗时告警阈值（毫秒，默认 10000）
//   SLOW_TEST_P95_BUDGET_MS  历史 P95 告警阈值（毫秒，默认取 SLOW_TEST_BUDGET_MS）
//   SLOW_TEST_HISTORY        历史文件路径（默认 <repo>/.tmp/slow-history.json）
//   SLOW_TEST_WINDOW         每个用例保留的采样条数上限（默认 200）
import { existsSync, mkdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { spawnSync } from "node:child_process";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const repoRoot = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const webDir = join(repoRoot, "apps", "web");
const reportDir = join(repoRoot, ".tmp");
const reportFile = join(reportDir, "slow-watch.json");
const historyFile = process.env.SLOW_TEST_HISTORY ?? join(reportDir, "slow-history.json");

/** 被观测的用例：文件名 + 用例名子串（避开完整中文标题，减少匹配脆弱性）。 */
const WATCHED = [
  { file: "test/AppRoutes.test.tsx", namePart: "/host-monitoring" },
  // 这处的等待上限同样被放宽过（60s → 120s），不能只观测前者。
  { file: "test/ViteMockIsolation.test.ts", namePart: "生产构建" },
];

/** 数值型环境变量：非法值直接失败，避免 NaN 让所有判据静默失效却仍报绿。 */
function numericEnv(name, fallback) {
  if (process.env[name] === undefined) return fallback;
  const parsed = Number(process.env[name]);
  if (!Number.isFinite(parsed) || parsed <= 0) {
    console.error(
      `::error::慢用例观测：环境变量 ${name}=${process.env[name]} 不是正数，判据无法生效。`,
    );
    process.exit(1);
  }
  return parsed;
}

const budgetMs = numericEnv("SLOW_TEST_BUDGET_MS", 10_000);
const p95BudgetMs = numericEnv("SLOW_TEST_P95_BUDGET_MS", budgetMs);
const windowSize = Math.floor(numericEnv("SLOW_TEST_WINDOW", 200));
/** 给出「可信 P95」所需的最小样本数；低于此值只做提示，不作为判据。 */
const MIN_SAMPLES_FOR_P95 = 5;

/** 最近邻向上取整的百分位（线性插值会低估尾部，这里取更保守的一侧）。 */
function percentile(sorted, p) {
  if (sorted.length === 1) return sorted[0];
  const rank = Math.ceil((p / 100) * sorted.length) - 1;
  return sorted[Math.min(Math.max(rank, 0), sorted.length - 1)];
}

function loadHistory() {
  if (!existsSync(historyFile)) return [];
  try {
    const parsed = JSON.parse(readFileSync(historyFile, "utf8"));
    return Array.isArray(parsed) ? parsed : [];
  } catch {
    console.warn(`::warning::慢用例观测：历史文件 ${historyFile} 解析失败，本次按空历史处理。`);
    return [];
  }
}

function saveHistory(history) {
  mkdirSync(dirname(historyFile), { recursive: true });
  writeFileSync(historyFile, JSON.stringify(history, null, 2), "utf8");
}

const printHistory = process.argv.includes("--print-history");

if (printHistory) {
  const history = loadHistory();
  const byKey = new Map();
  for (const sample of history) {
    if (!byKey.has(sample.key ?? `${sample.file}::${sample.fullName}`)) {
      byKey.set(sample.key ?? `${sample.file}::${sample.fullName}`, []);
    }
    byKey.get(sample.key ?? `${sample.file}::${sample.fullName}`).push(sample.duration);
  }
  if (byKey.size === 0) {
    console.log("慢用例历史：暂无采样。");
  } else {
    console.log(`慢用例历史（${history.length} 条采样，文件 ${historyFile}）：`);
    for (const [name, durations] of byKey) {
      const sorted = [...durations].sort((a, b) => a - b);
      const p95 = percentile(sorted, 95);
      console.log(
        `  ${name}：n=${sorted.length} min=${Math.round(sorted[0])}ms ` +
          `median=${Math.round(percentile(sorted, 50))}ms p95=${Math.round(p95)}ms max=${Math.round(sorted[sorted.length - 1])}ms`,
      );
    }
  }
  process.exit(0);
}

mkdirSync(reportDir, { recursive: true });
const history = loadHistory();
let warnings = 0;
/** 因样本不足而**未**给出判据的项数：不计入告警，但必须在终局显式报出来。 */
let skipped = 0;

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
    warnings += 1;
    console.warn(
      `::warning::慢用例观测：${target.file} 未能通过（退出码 ${run.status ?? "未知"}），跳过本次采样。`,
    );
    console.warn((run.stderr ?? "").split("\n").slice(-3).join("\n"));
    continue;
  }

  let report;
  try {
    report = JSON.parse(readFileSync(reportFile, "utf8"));
  } catch {
    warnings += 1;
    console.warn(`::warning::慢用例观测：无法解析 ${target.file} 的报告，跳过本次采样。`);
    continue;
  }

  const matched = [];
  for (const suite of report.testResults ?? []) {
    for (const assertion of suite.assertionResults ?? []) {
      if (!assertion.fullName?.includes(target.namePart)) continue;
      if (typeof assertion.duration === "number") {
        matched.push({ fullName: assertion.fullName, duration: assertion.duration });
      }
    }
  }

  if (matched.length === 0) {
    warnings += 1;
    console.warn(
      `::warning::慢用例观测：在 ${target.file} 里没找到匹配「${target.namePart}」的用例，` +
        "可能已改名或被删除——观测目标失效，请同步更新 scripts/watch-slow-tests.mjs。",
    );
    continue;
  }

  for (const { fullName, duration } of matched) {
    const ms = Math.round(duration);
    const label = `${target.file} › ${fullName}`;

    // 采样入库（含本次），按窗口裁剪，落盘后供后续运行累积。
    const key = `${target.file}::${fullName}`;
    history.push({ key, file: target.file, fullName, duration: ms, at: new Date().toISOString() });
    const kept = new Map();
    for (const sample of history) {
      if (!kept.has(sample.key)) kept.set(sample.key, []);
      kept.get(sample.key).push(sample);
    }
    const trimmed = [];
    for (const samples of kept.values()) {
      trimmed.push(...samples.slice(-windowSize));
    }
    history.length = 0;
    history.push(...trimmed);
    saveHistory(history);

    if (ms >= budgetMs) {
      warnings += 1;
      console.warn(
        `::warning::慢用例告警（单次）：${label} 本次耗时 ${ms}ms，超过单次预算 ${budgetMs}ms。`,
      );
    } else {
      console.log(`慢用例观测：${label} 本次 ${ms}ms（单次预算 ${budgetMs}ms）。`);
    }

    // 跨运行 P95：这一层才是退化逃逸的兜网。
    const samples = history
      .filter((sample) => sample.key === key)
      .map((sample) => sample.duration)
      .sort((a, b) => a - b);
    if (samples.length < MIN_SAMPLES_FOR_P95) {
      skipped += 1;
      console.log(
        `慢用例观测：${label} 历史样本 n=${samples.length}（< ${MIN_SAMPLES_FOR_P95}），` +
          "不足以给出可信 P95，跳过该项判据（CI cache 未命中时会出现此情况）。",
      );
      continue;
    }
    const p95 = percentile(samples, 95);
    if (p95 >= p95BudgetMs) {
      warnings += 1;
      console.warn(
        `::warning::慢用例告警（P95）：${label} 历史 P95=${Math.round(p95)}ms（n=${samples.length}），` +
          `超过 P95 预算 ${p95BudgetMs}ms——很可能是持续退化而非单次抖动，请排查图表首屏耗时。`,
      );
    } else {
      console.log(
        `慢用例观测：${label} 历史 P95=${Math.round(p95)}ms（n=${samples.length}，预算 ${p95BudgetMs}ms）。`,
      );
    }
  }
}

rmSync(reportFile, { force: true });

if (warnings > 0) {
  console.log(
    `慢用例观测结束：${warnings} 条告警` +
      (skipped > 0 ? `，另有 ${skipped} 项因样本不足未给判据` : "") +
      "（仅告警，未阻断质量门）。",
  );
} else if (skipped > 0) {
  // 不能说"全部在预算内"——被跳过的不等于合格的。
  console.log(
    `慢用例观测结束：无告警，但有 ${skipped} 项因样本不足**未给出判据**，结论不等于"性能正常"。`,
  );
} else {
  console.log("慢用例观测结束：全部在预算内。");
}
