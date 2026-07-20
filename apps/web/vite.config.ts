import react from "@vitejs/plugin-react";
import { defineConfig } from "vite";

// 前端产物内嵌进 Go 二进制（见 apps/server/web/embed.go）。
// 默认 base "/" 配合后端 SPA 回退，保证深层前端路由下静态资源路径正确。
export default defineConfig({
  plugins: [react()],
  build: {
    outDir: "dist",
    emptyOutDir: true,
  },
});
