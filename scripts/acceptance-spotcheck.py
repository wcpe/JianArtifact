#!/usr/bin/env python3
"""发布后实机验收抽查：对已部署实例跑一组回归断言。

用途：每次发布部署到实例（t1 测试服 / 正式站）后，用它确认关键修复确实在
运行实例上生效，而不是只看探活和版本号。断言覆盖的是**永久性回归**——
写进契约的行为（如「字段漏传即 400」）在任何后续版本上都应继续成立。

用法：
    BASE=http://<实例地址> \\
    TEST_ADMIN_USER=<管理员> TEST_ADMIN_PASSWORD=<口令> \\
    python scripts/acceptance-spotcheck.py

不传凭据时只跑免认证断言（首页、探活、匿名公开仓库），适合正式站抽查。

判定：全部断言通过退出码 0，否则 1（可直接用于流水线或部署脚本的收尾步骤）。
"""

import json
import os
import sys
import time
import urllib.error
import urllib.request

BASE = os.environ.get("BASE", "http://127.0.0.1:8080").rstrip("/")
USER = os.environ.get("TEST_ADMIN_USER", "")
PWD = os.environ.get("TEST_ADMIN_PASSWORD", "")

_results: list[bool] = []


def call(method: str, path: str, body=None, token: str | None = None):
    """发一次请求，返回 (状态码, 响应体, 耗时毫秒)。4xx/5xx 也当正常结果返回。"""
    data = None if body is None else json.dumps(body).encode()
    req = urllib.request.Request(BASE + path, data=data, method=method)
    if data is not None:
        req.add_header("Content-Type", "application/json")
    if token:
        req.add_header("Authorization", "Bearer " + token)
    started = time.time()
    try:
        with urllib.request.urlopen(req, timeout=90) as resp:
            return resp.status, resp.read(), (time.time() - started) * 1000
    except urllib.error.HTTPError as exc:
        return exc.code, exc.read(), (time.time() - started) * 1000


def check(name: str, ok: bool, detail: str) -> None:
    _results.append(ok)
    print(f"{'PASS' if ok else 'FAIL'}  {name}  |  {detail}")


def anonymous_checks() -> None:
    """免认证断言：任何环境都能跑。"""
    code, payload, _ = call("GET", "/")
    check("前端首页可访问", code == 200 and b'<div id="root">' in payload, f"HTTP {code} {len(payload)}B")

    code, _, _ = call("GET", "/readyz")
    check("就绪探活", code == 200, f"HTTP {code}")

    # 版本号按认证状态脱敏：匿名必须拿到空串（防指纹）。空串是预期行为，
    # 不是「版本没注入」——别把这里的空串读成故障。
    code, payload, _ = call("GET", "/healthz")
    version = json.loads(payload).get("version", None) if code == 200 else None
    check("匿名探针不泄露版本号", code == 200 and version == "", f"HTTP {code} version={version!r}")

    # 公开仓库列表：关闭全局匿名访问时为 401，属正常配置，不算失败
    code, _, _ = call("GET", "/api/v1/public/repositories")
    check("公开仓库端点", code in (200, 401), f"HTTP {code}")


def authenticated_checks() -> None:
    """管理员断言：覆盖契约化的回归点。"""
    code, payload, _ = call("POST", "/api/v1/auth/login", {"username": USER, "password": PWD})
    if code != 200:
        check("管理员登录", False, f"HTTP {code} {payload[:120]!r}")
        return
    token = json.loads(payload)["token"]
    check("管理员登录", True, "拿到 token")

    # 版本号按认证状态脱敏：已认证必须拿到真实版本（注入失效时会退化成空串，
    # 而空串在部署核验时会被误读成「没版本」，故在此锁定）
    code, payload, _ = call("GET", "/api/v1/status", token=token)
    version = json.loads(payload).get("version", "") if code == 200 else ""
    check("认证请求可见版本号", code == 200 and version != "", f"HTTP {code} version={version!r}")

    # 用户列表分页：page_size 必须生效且返回 total
    code, payload, _ = call("GET", "/api/v1/users?page=1&page_size=1", token=token)
    body = json.loads(payload) if code == 200 else {}
    check(
        "用户列表分页生效",
        code == 200 and len(body.get("items", [])) == 1 and "total" in body,
        f"HTTP {code} items={len(body.get('items', []))}",
    )

    # 置顶：字段漏传必须 400（漏传若按空数组处理会静默清空既有置顶）
    code, payload, _ = call("PUT", "/api/v1/settings/pinned-repositories", {}, token=token)
    check("置顶字段漏传返回 400", code == 400, f"HTTP {code}")

    # 置顶：超上限必须 400，且不得改动既有集合
    code, payload, _ = call("GET", "/api/v1/settings/pinned-repositories", token=token)
    original = json.loads(payload).get("repositoryIds", []) if code == 200 else None
    if original is None:
        check("置顶读取", False, f"HTTP {code}")
    else:
        code2, payload2, _ = call(
            "PUT", "/api/v1/settings/pinned-repositories", {"repositoryIds": original}, token=token
        )
        after = json.loads(payload2).get("repositoryIds", []) if code2 == 200 else None
        check("置顶原样回写", code2 == 200 and after == original, f"HTTP {code2} 原={original} 后={after}")

        code, _, _ = call(
            "PUT",
            "/api/v1/settings/pinned-repositories",
            {"repositoryIds": list(range(1, 202))},
            token=token,
        )
        code3, payload3, _ = call("GET", "/api/v1/settings/pinned-repositories", token=token)
        now = json.loads(payload3).get("repositoryIds", []) if code3 == 200 else None
        check("置顶超上限返回 400", code == 400, f"HTTP {code}")
        check("超限请求不改动既有置顶", now == original, f"现={now}")

    # 主机监控 24h 视图必须是小时桶：约 25 个采样点、约 20KB
    # （曾回归为约 1440 个分钟点 / 约 1MB / 2.5~9.5s）
    code, payload, ms = call("GET", "/api/v1/observability/host", token=token)
    bucket, samples = None, None
    if code == 200:
        body = json.loads(payload)
        bucket, samples = body.get("effectiveBucket"), len(body.get("samples", []))
    check(
        "主机监控 24h 为小时桶",
        code == 200 and bucket == "hour" and samples is not None and samples <= 26,
        f"HTTP {code} bucket={bucket} samples={samples} {len(payload)}B {ms:.0f}ms",
    )

    # 下载趋势：默认 24h 小时桶；按未知仓库过滤按契约返回 404
    code, payload, ms = call("GET", "/api/v1/observability/downloads/trend", token=token)
    points = len(json.loads(payload).get("points", [])) if code == 200 else None
    check(
        "下载趋势 24h 为小时桶",
        code == 200 and points is not None and points <= 26 and len(payload) < 200_000,
        f"HTTP {code} points={points} {len(payload)}B {ms:.0f}ms",
    )

    code, _, _ = call("GET", "/api/v1/observability/downloads/trend?repo=__no_such_repo__", token=token)
    check("趋势按未知仓库过滤返回 404", code == 404, f"HTTP {code}")

    # 审计：主列表与按未知仓库过滤都不得 5xx
    code, payload, _ = call("GET", "/api/v1/observability/audit/events?limit=5", token=token)
    check("审计事件主列表", code == 200, f"HTTP {code} {len(payload)}B")

    code, _, _ = call(
        "GET", "/api/v1/observability/audit/events?limit=5&repository=__no_such_repo__", token=token
    )
    check("审计按未知仓库过滤不报错", code == 200, f"HTTP {code}")


def main() -> int:
    print(f"实例：{BASE}\n")
    anonymous_checks()
    if USER and PWD:
        print()
        authenticated_checks()
    else:
        print("\n（未提供 TEST_ADMIN_USER/TEST_ADMIN_PASSWORD，跳过管理员断言）")

    passed = sum(1 for r in _results if r)
    print(f"\n结果：{passed}/{len(_results)} 通过")
    return 0 if passed == len(_results) else 1


if __name__ == "__main__":
    sys.exit(main())
