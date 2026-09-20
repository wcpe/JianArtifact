import { access, readFile, readdir, rm } from "node:fs/promises";
import { spawn } from "node:child_process";
import path from "node:path";

import { afterEach, describe, expect, it } from "vitest";

const webRoot = path.resolve(import.meta.dirname, "..");
const buildOutput = path.resolve(webRoot, "..", "..", ".tmp", `vite-mock-isolation-${process.pid}`);
const viteBin = path.join(webRoot, "node_modules", "vite", "bin", "vite.js");
const fixedPreviewCanaries = [
  "op_01J8X1Q9P7M4Z2A6F5C3D8H0K1",
  "sync-1286",
  "28,416",
  "采样权限不足",
  "预览记录 #",
] as const;

function runVite(args: string[]): Promise<void> {
  return new Promise((resolve, reject) => {
    const child = spawn(process.execPath, [viteBin, ...args], {
      cwd: webRoot,
      env: { ...process.env, NODE_ENV: "production" },
      stdio: "ignore",
    });
    child.once("error", reject);
    child.once("exit", (code) => {
      if (code === 0) {
        resolve();
        return;
      }
      reject(new Error(`Vite 进程异常退出：${code ?? "未知"}`));
    });
  });
}

function startViteDevServer(): Promise<{
  baseUrl: string;
  stop: () => Promise<void>;
}> {
  return new Promise((resolve, reject) => {
    const child = spawn(process.execPath, [viteBin, "--host", "127.0.0.1", "--port", "0"], {
      cwd: webRoot,
      // 本机（非 CI）运行时 Vite 会给地址加 ANSI 颜色；--port 0 时端口号被转义序列分割，
      // 导致地址正则匹配不到。统一禁用颜色，保证本地与 CI 行为一致。
      env: { ...process.env, NO_COLOR: "1" },
      stdio: ["ignore", "pipe", "pipe"],
    });
    let output = "";
    const timeout = setTimeout(() => {
      child.kill();
      reject(new Error("等待 Vite 开发服务器超时"));
    }, 30_000);
    const resolveServer = (chunk: Buffer) => {
      output += chunk.toString();
      const match = output.match(/http:\/\/127\.0\.0\.1:\d+\//);
      if (!match) {
        return;
      }
      clearTimeout(timeout);
      resolve({
        baseUrl: match[0],
        stop: () =>
          new Promise((stopResolve) => {
            child.once("exit", () => stopResolve());
            child.kill();
          }),
      });
    };

    child.stdout.on("data", resolveServer);
    child.stderr.on("data", resolveServer);
    child.once("error", (error) => {
      clearTimeout(timeout);
      reject(error);
    });
    child.once("exit", (code) => {
      clearTimeout(timeout);
      reject(new Error(`Vite 开发服务器异常退出：${code ?? "未知"}`));
    });
  });
}

async function listFiles(directory: string): Promise<string[]> {
  const entries = await readdir(directory, { withFileTypes: true });
  const paths = await Promise.all(
    entries.map(async (entry) => {
      const entryPath = path.join(directory, entry.name);
      return entry.isDirectory() ? listFiles(entryPath) : [entryPath];
    }),
  );
  return paths.flat();
}

afterEach(async () => {
  await rm(buildOutput, { force: true, recursive: true });
});

describe("Vite Mock 产物隔离", () => {
  it("生产构建不携带 worker、MSW 或固定开发预览样例，并保留 favicon", async () => {
    await runVite(["build", "--outDir", buildOutput]);

    const files = await listFiles(buildOutput);
    const contents = await Promise.all(files.map((file) => readFile(file, "utf8")));

    await expect(access(path.join(buildOutput, "mockServiceWorker.js"))).rejects.toThrow();
    expect(files.some((file) => path.basename(file).startsWith("favicon"))).toBe(true);
    expect(contents.join("\n")).not.toContain("Mock Service Worker");
    expect(contents.join("\n")).not.toContain("setupWorker");
    expect(contents.join("\n")).not.toContain("msw/browser");
    for (const canary of fixedPreviewCanaries) {
      expect(contents.join("\n")).not.toContain(canary);
    }
    // 实测：vite 5 时代 21–23s，升到 vite 7 后实测 19–69s（波动大，多数落在 46–69s）——
    // 60s 预算会超时（本用例曾以 60.03s 撞线失败）。取 120s：相对主要观测区间约 1.7–2.5 倍余量，
    // 同时仍能拦住"构建退化到数分钟"这类真问题。
  }, 120_000);

  it("开发服务器继续从既有路径提供 MSW worker", async () => {
    const server = await startViteDevServer();

    try {
      const response = await fetch(new URL("mockServiceWorker.js", server.baseUrl));
      expect(response.ok).toBe(true);
      expect(await response.text()).toContain("Mock Service Worker");

      const favicon = await fetch(new URL("src/assets/favicon.svg", server.baseUrl));
      expect(favicon.ok).toBe(true);
      expect(await favicon.text()).toContain("<svg");
    } finally {
      await server.stop();
    }
  }, 60_000);
});
