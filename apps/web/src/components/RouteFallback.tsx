// FR-70：路由级代码分割的 Suspense 占位——居中 loader，避免路由切换白屏。
import { Center, Loader } from "@mantine/core";

export function RouteFallback() {
  return (
    <Center mih={240}>
      <Loader size="sm" />
    </Center>
  );
}
