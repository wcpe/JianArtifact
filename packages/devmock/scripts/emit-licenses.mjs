// 把后端真实内嵌的开源协议清单转换为 devmock 的静态模块。
//
// 背景：`/api/v1/licenses` 是非契约管理面端点，响应体由
// `scripts/generate-licenses.mjs` 产出的 apps/server/internal/licenses/licenses.json
// 在运行期返回。开发态若手工维护一份样例，很快就会与真实依赖漂移
// （曾长期停在 go 2 条 + npm 2 条，且 @mantine/core 版本号过期）。
//
// 用法：node packages/devmock/scripts/emit-licenses.mjs
// 软失败：真实清单不存在时保留现有 licenses.gen.ts 并告警，不中断构建。
import { existsSync, readFileSync, writeFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const repoRoot = resolve(dirname(fileURLToPath(import.meta.url)), "..", "..", "..");
const source = join(repoRoot, "apps", "server", "internal", "licenses", "licenses.json");
const outFile = join(repoRoot, "packages", "devmock", "src", "licenses.gen.ts");

if (!existsSync(source)) {
  console.warn(
    `[licenses] 未找到 ${source}，保留现有 licenses.gen.ts。请先执行 pnpm licenses:gen 生成真实清单。`,
  );
  process.exit(0);
}

const raw = JSON.parse(readFileSync(source, "utf8"));
const go = Array.isArray(raw.go) ? raw.go : [];
const npm = Array.isArray(raw.npm) ? raw.npm : [];
const generatedAt =
  typeof raw.generatedAt === "string" ? raw.generatedAt : new Date().toISOString();

/** 归一化条目，避免源文件缺字段时产出 undefined。 */
function normalize(entries) {
  return entries.map((entry) => ({
    name: String(entry?.name ?? "—"),
    version: String(entry?.version ?? "—"),
    license: String(entry?.license ?? "—"),
    author: String(entry?.author ?? "—"),
  }));
}

const body = `// 由 packages/devmock/scripts/emit-licenses.mjs 生成，请勿手改。
// 数据源：apps/server/internal/licenses/licenses.json（scripts/generate-licenses.mjs 产出），
// 使开发态「开源协议」页与真实后端内嵌清单同源，不再手工维护易过期的样例。
export interface MockLicenseEntry {
  name: string;
  version: string;
  license: string;
  author: string;
}

export interface MockLicenseManifest {
  generatedAt: string;
  go: MockLicenseEntry[];
  npm: MockLicenseEntry[];
}

export const MOCK_LICENSES: MockLicenseManifest = {
  generatedAt: ${JSON.stringify(generatedAt)},
  go: ${JSON.stringify(normalize(go))},
  npm: ${JSON.stringify(normalize(npm))},
};
`;

writeFileSync(outFile, body, "utf8");
console.log(`[licenses] 已写入 ${outFile}（go ${go.length} / npm ${npm.length}）`);
