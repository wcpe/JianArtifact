// FR-72: 构建时生成开源依赖协议清单（Go + npm），内嵌到后端二进制并经
// admin 专属端点 GET /api/v1/licenses 返回（不再打进前端 bundle）。
// 用法：node scripts/generate-licenses.mjs（工作目录任意，路径以本文件定位）。
// 软失败：任一侧工具不可用时保留现有 JSON 并告警，不中断构建。
import { execFileSync, execSync } from "node:child_process";
import { existsSync, mkdirSync, readFileSync, readdirSync, writeFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const repoRoot = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const serverDir = join(repoRoot, "apps", "server");
const outFile = join(serverDir, "internal", "licenses", "licenses.json");

/** 运行命令返回 stdout；失败返回 null（软失败）。 */
function run(cmd, args, cwd) {
  try {
    return execFileSync(cmd, args, { cwd, encoding: "utf8", maxBuffer: 64 * 1024 * 1024 });
  } catch (e) {
    console.warn(`[licenses] ${cmd} ${args.join(" ")} 失败：${e.message}`);
    return null;
  }
}

// ---------- npm 段：pnpm licenses list --json --prod ----------
function collectNpm() {
  // pnpm 可能不在 PATH（如 corepack 托管），逐个候选尝试
  let raw = null;
  for (const cmd of ["pnpm licenses list --json --prod", "corepack pnpm licenses list --json --prod"]) {
    try {
      raw = execSync(cmd, { cwd: repoRoot, encoding: "utf8", maxBuffer: 64 * 1024 * 1024, stdio: ["ignore", "pipe", "ignore"] });
      break;
    } catch {
      raw = null;
    }
  }
  if (!raw) {
    console.warn("[licenses] pnpm licenses 不可用（已尝试 pnpm / corepack pnpm）");
    return null;
  }
  let parsed;
  try {
    parsed = JSON.parse(raw);
  } catch {
    console.warn("[licenses] pnpm licenses 输出非 JSON");
    return null;
  }
  const rows = [];
  for (const [license, pkgs] of Object.entries(parsed)) {
    if (!Array.isArray(pkgs)) continue;
    for (const pkg of pkgs) {
      rows.push({
        name: pkg.name ?? "unknown",
        version: Array.isArray(pkg.versions) ? pkg.versions.join(", ") : "",
        license: pkg.license || license || "Unknown",
        author: typeof pkg.author === "string" && pkg.author ? pkg.author : "—",
      });
    }
  }
  rows.sort((a, b) => a.name.localeCompare(b.name));
  return rows;
}

// ---------- Go 段：go mod edit -json + GOMODCACHE LICENSE 启发式 ----------

/** go module 路径转缓存目录名：大写字母 → !小写。 */
function escapeModulePath(p) {
  return p.replace(/[A-Z]/g, (c) => "!" + c.toLowerCase());
}

/** 依 LICENSE 文本关键词识别 SPDX 标识。顺序敏感：先长后短。 */
function detectLicense(text) {
  const t = text.slice(0, 4000);
  if (/Apache License\s+Version 2\.0/i.test(t)) return "Apache-2.0";
  if (/Mozilla Public License.*2\.0/is.test(t)) return "MPL-2.0";
  if (/GNU LESSER GENERAL PUBLIC LICENSE/i.test(t)) return "LGPL";
  if (/GNU GENERAL PUBLIC LICENSE/i.test(t)) return "GPL";
  if (/Permission to use, copy, modify, and\/?or distribute this software/i.test(t)) return "ISC";
  if (/MIT License|Permission is hereby granted, free of charge/i.test(t)) return "MIT";
  if (/This is free and unencumbered software released into the public domain/i.test(t))
    return "Unlicense";
  if (/Redistribution and use in source and binary forms/i.test(t)) {
    if (/neither the name/i.test(t)) return "BSD-3-Clause";
    return "BSD-2-Clause";
  }
  return "Unknown";
}

function collectGo() {
  const modJson = run("go", ["mod", "edit", "-json"], serverDir);
  const modCache = run("go", ["env", "GOMODCACHE"], serverDir)?.trim();
  if (!modJson || !modCache) return null;
  let mod;
  try {
    mod = JSON.parse(modJson);
  } catch {
    console.warn("[licenses] go mod edit -json 输出非 JSON");
    return null;
  }
  const rows = [];
  for (const req of mod.Require ?? []) {
    const { Path: path, Version: version } = req;
    let license = "Unknown";
    const dir = join(modCache, ...escapeModulePath(path).split("/")) + "@" + version;
    if (existsSync(dir)) {
      try {
        const entry = readdirSync(dir).find((f) => /^(LICEN[SC]E|COPYING)(\.|$)/i.test(f));
        if (entry) license = detectLicense(readFileSync(join(dir, entry), "utf8"));
      } catch {
        // 读取失败保持 Unknown
      }
    }
    const segs = path.split("/");
    rows.push({
      name: path,
      version,
      license,
      author: segs.length >= 2 ? segs[1] : segs[0],
    });
  }
  rows.sort((a, b) => a.name.localeCompare(b.name));
  return rows;
}

// ---------- 合并输出（任一侧失败时保留旧数据） ----------
let previous = { go: [], npm: [] };
if (existsSync(outFile)) {
  try {
    previous = JSON.parse(readFileSync(outFile, "utf8"));
  } catch {
    // 旧文件损坏则忽略
  }
}

const npm = collectNpm() ?? previous.npm ?? [];
const go = collectGo() ?? previous.go ?? [];

mkdirSync(dirname(outFile), { recursive: true });
writeFileSync(
  outFile,
  JSON.stringify({ generatedAt: new Date().toISOString(), go, npm }, null, 2) + "\n",
  "utf8",
);
console.log(`[licenses] 写入 ${outFile}：Go ${go.length} 条，npm ${npm.length} 条`);
