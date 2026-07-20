// @jianartifact/devmock —— 双端 Mock 与场景包入口。
// 据 api/openapi.yaml 做契约比对：schema.gen.ts 提供编译期类型，openapi.ts 提供运行期校验源。
export * from "./handlers";
export { loadOpenApi, schemaFor } from "./openapi";
