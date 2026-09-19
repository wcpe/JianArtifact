// 界面语言策略：解析优先级（用户显式选择 > 浏览器语言 > 路由默认）、偏好缓存与 locale 标记。
//
// 「公开页按浏览器语言、管理页与 /setup 默认中文」——管理页是登录后的操作界面，
// 运维不该被浏览器语言悄悄换掉；公开页面向访客，按浏览器语言更自然。
// 用户一旦在页眉显式选过语言，该选择在任意路由上都优先，并被记住。
export type Language = "zh" | "en";

/** 语言偏好的 localStorage 键（沿用全站 jianartifact.* 前缀）。 */
export const LANGUAGE_STORAGE_KEY = "jianartifact.language";

/** 支持的语言（用于校验缓存值与遍历切换）。 */
export const LANGUAGES: readonly Language[] = ["zh", "en"];

/** 语言 → BCP-47 标记。所有 toLocaleString / Intl.* / Mantine 日期组件的唯一取值来源。 */
export function toLocaleTag(language: Language): string {
  return language === "en" ? "en-US" : "zh-CN";
}

/** 语言的自称（切换入口上显示的名称，不随当前语言翻译）。 */
export function languageLabel(language: Language): string {
  return language === "en" ? "English" : "中文";
}

function isLanguage(value: unknown): value is Language {
  return value === "zh" || value === "en";
}

/**
 * 浏览器语言 → 本应用语言：`zh*`（zh、zh-CN、zh-Hant…）都归中文，其余归英文。
 * 取 `navigator.languages` 里**第一个**能匹配的项，都没有时看 `navigator.language`；
 * 无 `navigator`（SSR / 测试裁剪）时回退中文（默认语言）。
 */
export function resolveBrowserLanguage(): Language {
  if (typeof navigator === "undefined") return "zh";
  const candidates = [...(navigator.languages ?? []), navigator.language].filter(Boolean);
  for (const tag of candidates) {
    if (typeof tag !== "string") continue;
    if (/^zh\b/i.test(tag)) return "zh";
    // 只有明确是英文才落英文；其余语言（ja、de…）没有对应资源包，按中文兜底，
    // 避免把一个看不懂英文的访客推进英文界面。
    if (/^en\b/i.test(tag)) return "en";
  }
  return "zh";
}

/** 读取用户显式选择的语言；无偏好或存储不可用（隐私模式）时返回 null。 */
export function readStoredLanguage(): Language | null {
  try {
    const raw = localStorage.getItem(LANGUAGE_STORAGE_KEY);
    return isLanguage(raw) ? raw : null;
  } catch {
    return null;
  }
}

/** 记住用户显式选择的语言；存储不可用（隐私模式）时静默跳过。 */
export function writeStoredLanguage(language: Language): void {
  try {
    localStorage.setItem(LANGUAGE_STORAGE_KEY, language);
  } catch {
    // 隐私模式下写入会抛异常，语言仍在当前会话内生效，不阻断交互。
  }
}

/**
 * 公开路径（按浏览器语言）：匿名即可访问的浏览面。
 * 用显式路径表而不是「是否已登录」判定——被 RequireAuth 拦在公开页之外的匿名用户，
 * 与真正登录后的管理页用户，默认语言应当不同。
 * 统一去掉尾斜杠存储（`/p/`），比对时再补回，以免出现 `/p//maven-public` 双斜杠失配。
 */
const PUBLIC_PREFIXES = ["/repositories", "/search", "/p"];

/** 判断路径是否为公开浏览路径（`/repositories/:name/acl` 例外，它是管理页）。 */
export function isPublicPath(pathname: string): boolean {
  if (/^\/repositories\/[^/]+\/acl(\/|$)/.test(pathname)) return false;
  // 精确等于前缀，或前缀后紧跟斜杠——避免 /repositoriesXYZ 这类不存在的路径被误判公开。
  return PUBLIC_PREFIXES.some((prefix) => pathname === prefix || pathname.startsWith(`${prefix}/`));
}

/**
 * 解析最终语言：用户显式选择 > 浏览器语言（公开路径）/ 中文（管理路径与 /setup）。
 * 传 `stored` 可显式指定偏好（测试与初始化时用），省略则读 localStorage。
 */
export function resolveLanguage(options: { pathname: string; stored?: Language | null }): Language {
  const stored = options.stored === undefined ? readStoredLanguage() : options.stored;
  if (stored) return stored;
  return isPublicPath(options.pathname) ? resolveBrowserLanguage() : "zh";
}

/** 在两种支持的语言间轮换（页眉切换入口用）。 */
export function nextLanguage(language: Language): Language {
  const index = LANGUAGES.indexOf(language);
  return LANGUAGES[(index + 1) % LANGUAGES.length]!;
}
