/// <reference types="vitest/config" />
import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';

// 前端构建配置：产物输出到 frontend/dist，供后端 rust-embed 编译期嵌入。
// 开发期 Vite dev server 把 /api、/v2、/health 代理到本地后端（默认 8080），
// 以便前后端分离开发；生产构建后前端与后端同源（相对 /api/v1）。
export default defineConfig({
  plugins: [react()],
  server: {
    proxy: {
      '/api': 'http://127.0.0.1:8080',
      '/v2': 'http://127.0.0.1:8080',
      '/health': 'http://127.0.0.1:8080',
    },
  },
  build: {
    outDir: 'dist',
    emptyOutDir: true,
  },
  test: {
    globals: true,
    environment: 'jsdom',
    setupFiles: ['./src/test/setup.ts'],
    css: false,
  },
});
