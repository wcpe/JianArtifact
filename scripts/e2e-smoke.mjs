#!/usr/bin/env node
// 真实后端联调冒烟：对运行中的单二进制（默认 :8099）驱动全链路 API。
// 跨平台（Windows/Linux/macOS），仅依赖 Node >=18 自带的 fetch/Buffer；仅用于本地验收，不纳入交付。
// 覆盖 0.2.0 全链路 + 0.3.0 扩展：Raw/Maven/npm hosted 发布+拉取、制品浏览+使用片段（FR-13~16）；
//   可选原生客户端（mvn/npm，自动探测提示）与 proxy/group 回源（--include-proxy，需外网）。
// 用法：node scripts/e2e-smoke.mjs [--include-proxy]
import { spawnSync } from "node:child_process";

const base = "http://127.0.0.1:8099";
const includeProxy = process.argv.includes("--include-proxy");
let pass = 0;
let fail = 0;

const C = { green: "\x1b[32m", red: "\x1b[31m", yellow: "\x1b[33m", dim: "\x1b[2;33m", cyan: "\x1b[36m", reset: "\x1b[0m" };

function check(name, cond, detail) {
  if (cond) {
    console.log(`${C.green}PASS${C.reset}  ${name}`);
    pass++;
  } else {
    console.log(`${C.red}FAIL${C.reset}  ${name}  -> ${detail}`);
    fail++;
  }
}

// JSON 请求：Bearer 令牌可选，body 为对象时序列化为 JSON。返回 { code, body(解析后), raw(原文) }。
async function req(method, path, token, body) {
  const headers = {};
  if (token) headers["Authorization"] = `Bearer ${token}`;
  let payload;
  if (body !== undefined && body !== null) {
    payload = JSON.stringify(body);
    headers["Content-Type"] = "application/json";
  }
  const res = await fetch(base + path, { method, headers, body: payload });
  const raw = await res.text();
  let obj = null;
  try {
    obj = raw ? JSON.parse(raw) : null;
  } catch {
    /* 非 JSON 响应，保持 obj=null */
  }
  return { code: res.status, body: obj, raw };
}

// 原始请求：自定义 Authorization 头与 Content-Type，body 原样发送。返回 { code, raw }。
// fetch 的 res.text() 恒以 UTF-8 解码，天然规避 PowerShell byte[] 比较陷阱。
async function reqRaw(method, path, authHeader, body, contentType) {
  const headers = {};
  if (authHeader) headers["Authorization"] = authHeader;
  if (contentType) headers["Content-Type"] = contentType;
  const res = await fetch(base + path, { method, headers, body: body ?? undefined });
  const raw = await res.text();
  return { code: res.status, raw };
}

// 跨平台探测命令是否可用（where/which 返回 0 表示存在）；仅作提示，不计入通过/失败。
function hasCmd(cmd) {
  const finder = process.platform === "win32" ? "where" : "which";
  const res = spawnSync(finder, [cmd], { stdio: "ignore" });
  return res.status === 0;
}

const b64 = (s) => Buffer.from(s, "utf8").toString("base64");

async function main() {
  let r;

  // 1) 就绪 + 空库状态
  r = await req("GET", "/readyz", null, null);
  check("readyz 200", r.code === 200, r.code);
  r = await req("GET", "/api/v1/status", null, null);
  check("空库 status initialized=false userCount=0", r.body?.initialized === false && r.body?.userCount === 0, r.raw);

  // 2) 自举首个管理员
  r = await req("POST", "/api/v1/auth/bootstrap", null, { username: "root", password: "Password1" });
  check("自举 201 + 返回 token", r.code === 201 && !!r.body?.token, r.code);
  let adminTok = r.body?.token;
  check("自举用户角色 admin", r.body?.user?.role === "admin", r.raw);

  // 3) 已初始化再自举 → 409
  r = await req("POST", "/api/v1/auth/bootstrap", null, { username: "x", password: "yyyyyyyy" });
  check("重复自举 409", r.code === 409, r.code);
  r = await req("GET", "/api/v1/status", null, null);
  check("自举后 initialized=true userCount=1", r.body?.initialized === true && r.body?.userCount === 1, r.raw);

  // 4) 未认证受保护端点 → 401
  r = await req("GET", "/api/v1/users", null, null);
  check("未认证列用户 401", r.code === 401, r.code);

  // 5) 登录（错误口令 401 / 正确口令 200）
  r = await req("POST", "/api/v1/auth/login", null, { username: "root", password: "wrong" });
  check("错误口令登录 401", r.code === 401, r.code);
  r = await req("POST", "/api/v1/auth/login", null, { username: "root", password: "Password1" });
  check("登录 200 + token", r.code === 200 && !!r.body?.token, r.code);
  adminTok = r.body?.token;

  // 6) 管理员列用户 + 建普通用户
  r = await req("GET", "/api/v1/users", adminTok, null);
  check("管理员列用户 200", r.code === 200 && r.body?.total >= 1, r.raw);
  r = await req("POST", "/api/v1/users", adminTok, { username: "alice", password: "Password1", role: "user" });
  check("建用户 alice 201", r.code === 201 && r.body?.role === "user", r.raw);
  const aliceId = r.body?.id;
  check("响应体不含口令哈希", !/passwordHash|PasswordHash|argon2/.test(r.raw), r.raw);

  // 7) alice 登录 + 越权列用户 403
  r = await req("POST", "/api/v1/auth/login", null, { username: "alice", password: "Password1" });
  check("alice 登录 200", r.code === 200, r.code);
  let aliceTok = r.body?.token;
  r = await req("GET", "/api/v1/users", aliceTok, null);
  check("alice 列用户 403", r.code === 403, r.code);

  // 8) alice 改自己口令（允许）、改他人（403）
  r = await req("POST", `/api/v1/users/${aliceId}/password`, aliceTok, { password: "Password2" });
  check("alice 改自己口令 2xx", r.code >= 200 && r.code < 300, r.code);
  r = await req("POST", "/api/v1/users/1/password", aliceTok, { password: "Password2" });
  check("alice 改他人口令 403", r.code === 403, r.code);
  r = await req("POST", "/api/v1/auth/login", null, { username: "alice", password: "Password2" });
  check("改密后新口令可登录", r.code === 200, r.code);

  // 9) API Token 明文仅回显一次
  r = await req("POST", "/api/v1/tokens", adminTok, { name: "ci" });
  check("建 Token 201 + 明文", r.code === 201 && !!r.body?.token, r.raw);
  const plain = r.body?.token;
  check("Token 明文含 jat_ 前缀", typeof plain === "string" && plain.startsWith("jat_"), plain);
  r = await req("GET", "/api/v1/tokens", adminTok, null);
  check("列 Token 不含明文", !r.raw.includes(plain), r.raw);

  // 10) 仓库 CRUD + ACL
  r = await req("POST", "/api/v1/repositories", adminTok, { name: "maven-releases", format: "maven", type: "hosted", visibility: "private" });
  check("建仓库 201", r.code === 201, r.raw);
  r = await req("GET", "/api/v1/repositories", aliceTok, null);
  check("alice 看不到 private 仓库", r.body?.total === 0, r.raw);
  r = await req("PUT", "/api/v1/repositories/maven-releases/acl", adminTok, { items: [{ subjectId: aliceId, action: "read" }] });
  check("写 ACL 授 alice read 2xx", r.code >= 200 && r.code < 300, r.code);
  r = await req("GET", "/api/v1/repositories", aliceTok, null);
  check("授 read 后 alice 可见仓库", r.body?.total === 1, r.raw);
  r = await req("GET", "/api/v1/repositories/maven-releases/acl", aliceTok, null);
  check("alice 无 admin 读 ACL 403", r.code === 403, r.code);

  // 10.5) Raw hosted 发布 / 拉取闭环（0.3.0）
  r = await req("POST", "/api/v1/repositories", adminTok, { name: "raw-hosted", format: "raw", type: "hosted", visibility: "private" });
  check("建 raw hosted 仓库 201", r.code === 201, r.raw);
  const rawBody = "raw hosted smoke payload";
  r = await reqRaw("PUT", "/repository/raw-hosted/dir/app.txt", `Bearer ${adminTok}`, rawBody, "text/plain");
  check("Bearer PUT 制品 201", r.code === 201, r.code);
  r = await reqRaw("GET", "/repository/raw-hosted/dir/app.txt", `Bearer ${adminTok}`, null, null);
  check("Bearer GET 制品 200 + 字节一致", r.code === 200 && r.raw === rawBody, r.raw);
  const basic = "Basic " + b64(`token:${plain}`);
  r = await reqRaw("PUT", "/repository/raw-hosted/dir/via-basic.bin", basic, "basic auth payload", "application/octet-stream");
  check("Basic PUT 制品 201", r.code === 201, r.code);
  r = await reqRaw("GET", "/repository/raw-hosted/dir/via-basic.bin", basic, null, null);
  check("Basic GET 制品 200 + 字节一致", r.code === 200 && r.raw === "basic auth payload", r.raw);
  r = await reqRaw("GET", "/repository/raw-hosted/dir/app.txt", null, null, null);
  check("私有仓匿名读 401", r.code === 401, r.code);
  r = await reqRaw("GET", "/repository/raw-hosted/missing/none", `Bearer ${adminTok}`, null, null);
  check("未知路径 404", r.code === 404, r.code);
  r = await req("POST", "/api/v1/repositories", adminTok, { name: "raw-public", format: "raw", type: "hosted", visibility: "public" });
  check("建 raw public 仓库 201", r.code === 201, r.raw);
  r = await reqRaw("PUT", "/repository/raw-public/hello.txt", `Bearer ${adminTok}`, "public payload", "text/plain");
  check("public 仓 PUT 201", r.code === 201, r.code);
  r = await reqRaw("GET", "/repository/raw-public/hello.txt", null, null, null);
  check("public 仓匿名读 200 + 字节一致", r.code === 200 && r.raw === "public payload", r.raw);

  // 10.6) 制品浏览 + 使用片段（0.3.0 / FR-16）
  r = await req("GET", "/api/v1/repositories/raw-public/assets", adminTok, null);
  check("浏览 raw-public assets 200 + 含 hello.txt", r.code === 200 && (r.body?.items ?? []).some((i) => i.path === "hello.txt"), r.raw);
  r = await req("GET", "/api/v1/repositories/raw-public/assets?prefix=none/", adminTok, null);
  check("浏览 prefix 过滤命中空集 total=0", r.code === 200 && r.body?.total === 0, r.raw);
  r = await req("GET", "/api/v1/repositories/raw-public/usage", adminTok, null);
  check("raw usage 200 + format=raw + 含片段", r.code === 200 && r.body?.format === "raw" && (r.body?.snippets?.length ?? 0) >= 1, r.raw);
  r = await req("GET", "/api/v1/repositories/raw-public/usage", null, null);
  check("public 仓 usage 匿名可读 200", r.code === 200, r.code);
  r = await req("GET", "/api/v1/repositories/maven-releases/usage", aliceTok, null);
  check("无 admin 的成员读 private usage（read 授权）200", r.code === 200 && r.body?.format === "maven", r.raw);
  r = await req("GET", "/api/v1/repositories/ghost-none/usage", adminTok, null);
  check("未知仓库 usage 404", r.code === 404, r.code);

  // 10.7) Maven hosted deploy + resolve 闭环（0.3.0 / FR-14）
  r = await req("POST", "/api/v1/repositories", adminTok, { name: "mvn-hosted", format: "maven", type: "hosted", visibility: "public" });
  check("建 maven hosted 仓库 201", r.code === 201, r.raw);
  const pom = "<project><modelVersion>4.0.0</modelVersion><groupId>com.example</groupId><artifactId>app</artifactId><version>1.0.0</version></project>";
  const jar = "PK-fake-jar-bytes-0.3.0";
  r = await reqRaw("PUT", "/repository/mvn-hosted/com/example/app/1.0.0/app-1.0.0.pom", `Bearer ${adminTok}`, pom, "application/xml");
  check("deploy pom 201", r.code === 201, r.code);
  r = await reqRaw("PUT", "/repository/mvn-hosted/com/example/app/1.0.0/app-1.0.0.jar", `Bearer ${adminTok}`, jar, "application/java-archive");
  check("deploy jar 201", r.code === 201, r.code);
  r = await reqRaw("GET", "/repository/mvn-hosted/com/example/app/1.0.0/app-1.0.0.jar", null, null, null);
  check("resolve jar 200 + 字节一致", r.code === 200 && r.raw === jar, r.raw);
  r = await reqRaw("GET", "/repository/mvn-hosted/com/example/app/1.0.0/app-1.0.0.jar.sha1", null, null, null);
  check("缺失 .sha1 现算返回 200 + 40 hex", r.code === 200 && /^[0-9a-f]{40}$/.test(r.raw.trim()), r.raw);

  // 10.8) npm publish + install 闭环（0.3.0 / FR-15）
  r = await req("POST", "/api/v1/repositories", adminTok, { name: "npm-hosted", format: "npm", type: "hosted", visibility: "public" });
  check("建 npm hosted 仓库 201", r.code === 201, r.raw);
  const tarBytes = "npm-fake-tarball-0.3.0";
  const tarB64 = b64(tarBytes);
  const publishBody = JSON.stringify({
    _id: "demo-pkg",
    name: "demo-pkg",
    "dist-tags": { latest: "1.0.0" },
    versions: { "1.0.0": { name: "demo-pkg", version: "1.0.0", dist: { tarball: "http://x/demo-pkg/-/demo-pkg-1.0.0.tgz" } } },
    _attachments: { "demo-pkg-1.0.0.tgz": { content_type: "application/octet-stream", data: tarB64, length: Buffer.byteLength(tarBytes, "utf8") } },
  });
  r = await reqRaw("PUT", "/npm/npm-hosted/demo-pkg", `Bearer ${adminTok}`, publishBody, "application/json");
  check("npm publish 2xx", r.code >= 200 && r.code < 300, r.code);
  r = await reqRaw("GET", "/npm/npm-hosted/demo-pkg", null, null, null);
  let pk = null;
  try {
    pk = r.raw ? JSON.parse(r.raw) : null;
  } catch {
    /* 保持 pk=null */
  }
  check("install packument 200 + latest=1.0.0", r.code === 200 && pk?.["dist-tags"]?.latest === "1.0.0", r.raw);
  check("packument tarball 重写为本仓地址", /\/npm\/npm-hosted\/demo-pkg\/-\/demo-pkg-1\.0\.0\.tgz$/.test(pk?.versions?.["1.0.0"]?.dist?.tarball ?? ""), r.raw);
  r = await reqRaw("GET", "/npm/npm-hosted/demo-pkg/-/demo-pkg-1.0.0.tgz", null, null, null);
  check("install tarball 200 + 字节一致", r.code === 200 && r.raw === tarBytes, r.raw);

  // 10.9) 原生客户端 roundtrip（自动探测 mvn/npm；缺失则跳过，不计失败）
  if (hasCmd("mvn")) {
    console.log(`${C.yellow}INFO${C.reset}  探测到 mvn，可手动执行：mvn deploy -DaltDeploymentRepository=mvn-hosted::default::${base}/repository/mvn-hosted`);
  } else {
    console.log(`${C.dim}SKIP${C.reset}  未探测到 mvn，跳过原生 Maven roundtrip（HTTP 层已覆盖 deploy+resolve）`);
  }
  if (hasCmd("npm")) {
    console.log(`${C.yellow}INFO${C.reset}  探测到 npm，可手动执行：npm install demo-pkg --registry ${base}/npm/npm-hosted/`);
  } else {
    console.log(`${C.dim}SKIP${C.reset}  未探测到 npm，跳过原生 npm roundtrip（HTTP 层已覆盖 publish+install）`);
  }

  // 10.10) proxy/group 回源（可选，需外网；--include-proxy 开启）
  if (includeProxy) {
    r = await req("POST", "/api/v1/repositories", adminTok, { name: "npm-proxy", format: "npm", type: "proxy", visibility: "public", remoteUrl: "https://registry.npmjs.org" });
    check("建 npm proxy 仓库 201", r.code === 201, r.raw);
    r = await reqRaw("GET", "/npm/npm-proxy/left-pad", null, null, null);
    check("npm proxy 回源 packument 200", r.code === 200 && /"name"\s*:\s*"left-pad"/.test(r.raw), r.code);
    r = await req("POST", "/api/v1/repositories", adminTok, { name: "npm-group", format: "npm", type: "group", visibility: "public", members: ["npm-hosted", "npm-proxy"] });
    check("建 npm group 仓库 201", r.code === 201, r.raw);
    r = await reqRaw("GET", "/npm/npm-group/demo-pkg", null, null, null);
    check("npm group 命中本地成员 200 + latest", r.code === 200 && /"latest"\s*:\s*"1.0.0"/.test(r.raw), r.code);
  } else {
    console.log(`${C.dim}SKIP${C.reset}  未加 --include-proxy，跳过 proxy/group 外网回源（可用 --include-proxy 开启）`);
  }

  // 11) 登出后旧 token 失效 401
  r = await req("POST", "/api/v1/auth/logout", adminTok, null);
  check("登出 2xx", r.code >= 200 && r.code < 300, r.code);
  r = await req("GET", "/api/v1/users", adminTok, null);
  check("登出后旧 token 401", r.code === 401, r.code);

  // 12) 内嵌 SPA 首页
  r = await req("GET", "/", null, null);
  check("SPA 首页 200 + HTML", r.code === 200 && (/id="root"/.test(r.raw) || /<!doctype html/i.test(r.raw)), r.code);

  console.log("");
  console.log(`${C.cyan}==== E2E 结果：${pass} 通过 / ${fail} 失败 ====${C.reset}`);
  process.exit(fail > 0 ? 1 : 0);
}

main().catch((err) => {
  console.error(`${C.red}E2E 运行异常${C.reset}：${err?.stack || err}`);
  process.exit(1);
});
