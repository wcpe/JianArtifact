import react from "@vitejs/plugin-react";
import { defineConfig } from "vite";

// 验收站为独立静态站点，仅供开发/评审预览与验收，不内嵌进后端二进制。
export default defineConfig({
  plugins: [react()],
  build: {
    outDir: "dist",
    emptyOutDir: true,
  },
});
