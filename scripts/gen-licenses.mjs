#!/usr/bin/env node
// 开源许可清单构建期生成脚本（FR-102，ADR-0025）。
//
// 在构建二进制**前**扫描全部依赖（Rust crates + 前端 npm，含运行时与开发依赖）的
// 名 / 版本 / 许可证 / 作者，合并为一份结构化 JSON 写入 src/licenses/data.generated.json，
// 由后端 licenses 模块经 include_str! 编译期嵌入。
//
// 数据来源（均为本地 / 构建期，不外发、不 phone-home，守 ADR-0009）：
// - Rust 运行时：cargo about generate --format json（按 about.toml 的 accepted 清单）。
// - Rust 开发：cargo metadata 全图与运行时集合的差集（dev-only crate），license/authors 取 metadata。
// - 前端：pnpm licenses list --json（全量）与 --prod（运行时），全量 − 运行时 = 开发依赖。
//
// 工具未装 / 扫描失败时按生态降级（跳过该生态、记 WARN），仍写出已得部分；
// 全部失败则写占位（generated=false），不阻断构建。

import { execFileSync, execSync } from 'node:child_process';
import { writeFileSync, mkdirSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const 仓库根 = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const 前端目录 = join(仓库根, 'frontend');
const 输出路径 = join(仓库根, 'src', 'licenses', 'data.generated.json');

/** 运行命令并返回 stdout 文本；失败抛出（由各采集函数捕获降级）。 */
function 运行(cmd, args, options = {}) {
  const 公共 = {
    cwd: options.cwd ?? 仓库根,
    encoding: 'utf8',
    maxBuffer: 64 * 1024 * 1024,
    stdio: ['ignore', 'pipe', 'inherit'],
  };
  // Windows 下 pnpm 为 .cmd 包装脚本，无法被 execFileSync 直接 spawn；经 shell 运行。
  // 命令与参数均为脚本内静态字面量（无外部输入），shell 拼接安全。
  if (process.platform === 'win32' && cmd === 'pnpm') {
    return execSync([cmd, ...args].join(' '), 公共);
  }
  return execFileSync(cmd, args, 公共);
}

/** 规范化作者：cargo authors 为数组、pnpm author 为字符串，统一为单行字符串。 */
function 规范化作者(author) {
  if (Array.isArray(author)) return author.join(', ');
  return typeof author === 'string' ? author : '';
}

/** 采集 Rust crate 许可（运行时 + 开发）。失败返回空数组。 */
function 采集_rust() {
  let about;
  let meta;
  try {
    about = JSON.parse(运行('cargo', ['about', 'generate', '--format', 'json', '--offline']));
  } catch (err) {
    console.warn(`[gen-licenses] WARN: cargo-about 扫描失败，跳过 Rust 许可：${err.message}`);
    return [];
  }
  try {
    meta = JSON.parse(运行('cargo', ['metadata', '--format-version', '1', '--locked']));
  } catch (err) {
    console.warn(`[gen-licenses] WARN: cargo metadata 失败，仅取 Rust 运行时许可：${err.message}`);
    meta = null;
  }

  // cargo-about 默认依赖图为运行时（normal + build，排除 dev-dependencies）
  const 运行时 = new Map();
  for (const c of about.crates ?? []) {
    const p = c.package ?? {};
    const 键 = `${p.name}@${p.version}`;
    运行时.set(键, {
      name: p.name,
      version: p.version,
      license: c.license ?? p.license ?? '',
      author: 规范化作者(p.authors),
      kind: 'runtime',
      source: 'rust',
    });
  }

  const 条目 = [...运行时.values()];

  // 开发依赖 = cargo metadata 全图 − 运行时集合（按 name@version）
  if (meta) {
    for (const p of meta.packages ?? []) {
      const 键 = `${p.name}@${p.version}`;
      if (运行时.has(键)) continue;
      // 跳过本项目自身（root crate 非第三方依赖）
      if (p.source == null) continue;
      条目.push({
        name: p.name,
        version: p.version,
        license: p.license ?? '',
        author: 规范化作者(p.authors),
        kind: 'dev',
        source: 'rust',
      });
    }
  }
  return 条目;
}

/** 把 pnpm licenses list --json 输出（按许可分组）摊平为逐包条目。 */
function 摊平_pnpm(grouped) {
  const out = [];
  for (const lic of Object.keys(grouped)) {
    for (const e of grouped[lic]) {
      const versions = e.versions && e.versions.length > 0 ? e.versions : [''];
      for (const v of versions) {
        out.push({
          name: e.name,
          version: v,
          license: e.license || lic,
          author: 规范化作者(e.author),
        });
      }
    }
  }
  return out;
}

/** 采集前端 npm 许可（运行时 + 开发）。失败返回空数组。 */
function 采集_frontend() {
  let 全量;
  let 运行时;
  try {
    全量 = 摊平_pnpm(
      JSON.parse(运行('pnpm', ['licenses', 'list', '--json'], { cwd: 前端目录 })),
    );
  } catch (err) {
    console.warn(`[gen-licenses] WARN: pnpm licenses（全量）失败，跳过前端许可：${err.message}`);
    return [];
  }
  try {
    运行时 = 摊平_pnpm(
      JSON.parse(运行('pnpm', ['licenses', 'list', '--prod', '--json'], { cwd: 前端目录 })),
    );
  } catch (err) {
    console.warn(`[gen-licenses] WARN: pnpm licenses（运行时）失败，前端全部按开发依赖记：${err.message}`);
    运行时 = [];
  }

  const 运行时键 = new Set(运行时.map((e) => `${e.name}@${e.version}`));
  return 全量.map((e) => ({
    ...e,
    kind: 运行时键.has(`${e.name}@${e.version}`) ? 'runtime' : 'dev',
    source: 'frontend',
  }));
}

/** 合并去重、排序、计算汇总。 */
function 构建清单(条目列表) {
  // 按 source+name+version 去重（不同生态可能同名，加 source 区分）
  const 去重 = new Map();
  for (const e of 条目列表) {
    去重.set(`${e.source}:${e.name}@${e.version}`, e);
  }
  const entries = [...去重.values()].sort((a, b) => {
    // 运行时在前、开发在后；同类按 source 再按名排序
    if (a.kind !== b.kind) return a.kind === 'runtime' ? -1 : 1;
    if (a.source !== b.source) return a.source < b.source ? -1 : 1;
    return a.name.localeCompare(b.name);
  });

  const runtime = entries.filter((e) => e.kind === 'runtime').length;
  const dev = entries.length - runtime;
  const licenses = new Set(entries.map((e) => e.license).filter(Boolean)).size;

  return {
    generated: entries.length > 0,
    entries,
    summary: { total: entries.length, runtime, dev, licenses },
  };
}

function main() {
  const 条目 = [...采集_rust(), ...采集_frontend()];
  const 清单 = 构建清单(条目);

  mkdirSync(dirname(输出路径), { recursive: true });
  writeFileSync(输出路径, `${JSON.stringify(清单, null, 2)}\n`, 'utf8');

  console.log(
    `[gen-licenses] 已生成 ${输出路径}：共 ${清单.summary.total} 项` +
      `（运行时 ${清单.summary.runtime} / 开发 ${清单.summary.dev}，许可证 ${清单.summary.licenses} 种，generated=${清单.generated}）`,
  );
}

main();
