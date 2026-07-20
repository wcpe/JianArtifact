// 从 api/openapi.yaml 加载契约，抽取组件 schema 供运行时（ajv）校验使用。
// 与 schema.gen.ts（编译期类型）同源，共同保证 mock 不偏离契约。
import { readFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

import { parse } from "yaml";

const here = dirname(fileURLToPath(import.meta.url));
// packages/devmock/src -> 仓库根 api/openapi.yaml
const specPath = resolve(here, "..", "..", "..", "api", "openapi.yaml");

interface OpenApiDoc {
  components: {
    schemas: Record<string, Record<string, unknown>>;
  };
}

/** 解析并返回 OpenAPI 契约文档（唯一真源）。 */
export function loadOpenApi(): OpenApiDoc {
  return parse(readFileSync(specPath, "utf8")) as OpenApiDoc;
}

/** 取指定组件 schema 的 JSON Schema（可直接交给 ajv 编译）。 */
export function schemaFor(name: string): Record<string, unknown> {
  const doc = loadOpenApi();
  const schema = doc.components?.schemas?.[name];
  if (!schema) {
    throw new Error(`OpenAPI 契约缺少组件 schema：${name}`);
  }
  return schema;
}

/** 返回全部组件 schema，键为 `#/components/schemas/<名>`，供 ajv 解析交叉 $ref。 */
export function allSchemas(): Record<string, Record<string, unknown>> {
  const doc = loadOpenApi();
  const out: Record<string, Record<string, unknown>> = {};
  for (const [name, schema] of Object.entries(doc.components?.schemas ?? {})) {
    out[`#/components/schemas/${name}`] = schema;
  }
  return out;
}
