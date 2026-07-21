// @jianartifact/devmock —— 双端 Mock 与场景包入口。
// 据 api/openapi.yaml 做契约比对：schema.gen.ts 提供编译期类型，openapi.ts 提供运行期校验源。
// 纯响应函数（handlers）供契约测试；MSW 拦截层（msw/store）供 web 双端 Mock。
// 浏览器 / Node worker 分别经子路径 "./browser" "./node" 导出，避免交叉引入运行时。
export * from "./handlers";
export { loadOpenApi, schemaFor } from "./openapi";
export { handlers } from "./msw";
export { store, resetStore, emptyStore, MOCK_TOKEN } from "./store";
