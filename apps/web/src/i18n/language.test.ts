// 语言策略单测：解析优先级（用户显式选择 > 浏览器语言 > 路由默认）、偏好缓存与 locale 标记。
// 这些用例自行覆盖 navigator 与 localStorage，不依赖 test/setup.ts 的中文基线。
import { afterEach, describe, expect, it } from "vitest";

import {
  LANGUAGE_STORAGE_KEY,
  isPublicPath,
  nextLanguage,
  readStoredLanguage,
  resolveBrowserLanguage,
  resolveLanguage,
  toLocaleTag,
  writeStoredLanguage,
} from "./language";

/** 临时把浏览器语言桩成给定值，返回还原函数。 */
function withBrowserLanguages(languages: string[]): () => void {
  const originals = {
    language: Object.getOwnPropertyDescriptor(window.navigator, "language"),
    languages: Object.getOwnPropertyDescriptor(window.navigator, "languages"),
  };
  Object.defineProperty(window.navigator, "language", {
    configurable: true,
    get: () => languages[0] ?? "",
  });
  Object.defineProperty(window.navigator, "languages", {
    configurable: true,
    get: () => languages,
  });
  return () => {
    for (const [key, descriptor] of Object.entries(originals)) {
      if (descriptor) {
        Object.defineProperty(window.navigator, key, descriptor);
      } else {
        delete (window.navigator as unknown as Record<string, unknown>)[key];
      }
    }
  };
}

afterEach(() => {
  localStorage.removeItem(LANGUAGE_STORAGE_KEY);
});

describe("locale 标记", () => {
  it("中文与英文各对应一个 BCP-47 标记", () => {
    expect(toLocaleTag("zh")).toBe("zh-CN");
    expect(toLocaleTag("en")).toBe("en-US");
  });

  it("切换在两种语言间轮换", () => {
    expect(nextLanguage("zh")).toBe("en");
    expect(nextLanguage("en")).toBe("zh");
  });
});

describe("浏览器语言解析", () => {
  it("zh 的各种变体都归中文", () => {
    for (const tag of ["zh", "zh-CN", "zh-Hans", "zh-Hant-TW"]) {
      const restore = withBrowserLanguages([tag]);
      expect(resolveBrowserLanguage()).toBe("zh");
      restore();
    }
  });

  it("英文变体归英文", () => {
    for (const tag of ["en", "en-US", "en-GB"]) {
      const restore = withBrowserLanguages([tag]);
      expect(resolveBrowserLanguage()).toBe("en");
      restore();
    }
  });

  it("取第一个能匹配的语言，而不是只看首个", () => {
    const restore = withBrowserLanguages(["de-DE", "en-US"]);
    // de 无对应资源包，继续往后找，命中 en。
    expect(resolveBrowserLanguage()).toBe("en");
    restore();
  });

  it("没有可识别的语言时回退中文", () => {
    const restore = withBrowserLanguages(["ja-JP"]);
    expect(resolveBrowserLanguage()).toBe("zh");
    restore();
  });
});

describe("公开路径判定", () => {
  it("浏览面是公开路径", () => {
    for (const path of [
      "/repositories",
      "/repositories/maven-public",
      "/search",
      "/p/maven-public",
    ]) {
      expect(isPublicPath(path)).toBe(true);
    }
  });

  it("管理面不是公开路径（含仓库 ACL 这一例外）", () => {
    for (const path of ["/dashboard", "/users", "/setup", "/repositories/maven-public/acl"]) {
      expect(isPublicPath(path)).toBe(false);
    }
  });
});

describe("语言解析优先级", () => {
  it("用户显式选择优先于浏览器语言与路由默认", () => {
    const restore = withBrowserLanguages(["en-US"]);
    // 即便浏览器是英文、路径是管理页，显式选择的中文仍然生效。
    expect(resolveLanguage({ pathname: "/repositories", stored: "zh" })).toBe("zh");
    expect(resolveLanguage({ pathname: "/dashboard", stored: "en" })).toBe("en");
    restore();
  });

  it("无偏好时公开路径跟随浏览器语言", () => {
    const restore = withBrowserLanguages(["en-US"]);
    expect(resolveLanguage({ pathname: "/repositories", stored: null })).toBe("en");
    restore();
    const restoreZh = withBrowserLanguages(["zh-CN"]);
    expect(resolveLanguage({ pathname: "/search", stored: null })).toBe("zh");
    restoreZh();
  });

  it("无偏好时管理页与 /setup 默认中文（不跟随浏览器语言）", () => {
    const restore = withBrowserLanguages(["en-US"]);
    for (const path of ["/dashboard", "/users", "/setup", "/migrations"]) {
      expect(resolveLanguage({ pathname: path, stored: null })).toBe("zh");
    }
    restore();
  });
});

describe("偏好缓存", () => {
  it("写入后可读回，非法值视为无偏好", () => {
    expect(readStoredLanguage()).toBeNull();
    writeStoredLanguage("en");
    expect(readStoredLanguage()).toBe("en");
    localStorage.setItem(LANGUAGE_STORAGE_KEY, "klingon");
    expect(readStoredLanguage()).toBeNull();
  });

  it("存储不可用时静默降级，不抛异常", () => {
    const original = Object.getOwnPropertyDescriptor(window, "localStorage");
    Object.defineProperty(window, "localStorage", {
      configurable: true,
      get: () => {
        throw new Error("隐私模式");
      },
    });
    try {
      expect(readStoredLanguage()).toBeNull();
      expect(() => writeStoredLanguage("en")).not.toThrow();
    } finally {
      if (original) Object.defineProperty(window, "localStorage", original);
    }
  });
});
