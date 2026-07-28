// 高级搜索表达式的前端解析 / 拼装（语法与后端 domain/search_query.go 保持一致）：
//   词 / -词 / "引号短语" / repo: / -repo: / format: / -format: / ext: / -ext:
// SearchPage 用它把输入框表达式与筛选面板做双向同步。

export interface SearchExpression {
  terms: string[]; // 普通关键词（含引号短语，已去引号）
  excludeTerms: string[]; // -词 负筛选
  repos: string[]; // repo: 限定
  notRepos: string[]; // -repo: 排除
  formats: string[]; // format: 限定
  exts: string[]; // ext: 包含扩展名
  notExts: string[]; // -ext: 排除扩展名
}

export function emptyExpression(): SearchExpression {
  return {
    terms: [],
    excludeTerms: [],
    repos: [],
    notRepos: [],
    formats: [],
    exts: [],
    notExts: [],
  };
}

// tokenize 按空白切词；双引号内空白不切分，引号本身丢弃。
function tokenize(q: string): string[] {
  const tokens: string[] = [];
  let buf = "";
  let inQuote = false;
  for (const ch of q) {
    if (ch === '"') {
      inQuote = !inQuote;
    } else if (!inQuote && /\s/.test(ch)) {
      if (buf) {
        tokens.push(buf);
        buf = "";
      }
    } else {
      buf += ch;
    }
  }
  if (buf) tokens.push(buf);
  return tokens;
}

/** 解析高级搜索表达式；空值 token（如孤立的 "repo:"）静默丢弃。 */
export function parseSearchExpression(q: string): SearchExpression {
  const e = emptyExpression();
  for (let tok of tokenize(q)) {
    let neg = false;
    if (tok.startsWith("-") && tok.length > 1) {
      neg = true;
      tok = tok.slice(1);
    }
    const lower = tok.toLowerCase();
    if (lower.startsWith("repo:")) {
      const name = tok.slice(5).trim();
      if (name) (neg ? e.notRepos : e.repos).push(name);
    } else if (lower.startsWith("format:")) {
      const f = tok.slice(7).trim().toLowerCase();
      if (f) {
        if (!neg) e.formats.push(f);
        // -format: 面板不暴露，丢弃负格式以简化双向同步（手输仍由后端生效）
      }
    } else if (lower.startsWith("ext:")) {
      const x = tok.slice(4).trim().replace(/^\./, "").toLowerCase();
      if (x) (neg ? e.notExts : e.exts).push(x);
    } else {
      (neg ? e.excludeTerms : e.terms).push(tok);
    }
  }
  return e;
}

// quoteTerm 含空白的词加引号还原。
function quoteTerm(t: string): string {
  return /\s/.test(t) ? `"${t}"` : t;
}

/** 把结构化条件拼回表达式字符串（顺序：关键词 → 排除词 → repo → format → ext）。 */
export function buildSearchExpression(e: SearchExpression): string {
  const parts: string[] = [];
  for (const t of e.terms) parts.push(quoteTerm(t));
  for (const t of e.excludeTerms) parts.push(`-${quoteTerm(t)}`);
  for (const r of e.repos) parts.push(`repo:${r}`);
  for (const r of e.notRepos) parts.push(`-repo:${r}`);
  for (const f of e.formats) parts.push(`format:${f}`);
  for (const x of e.exts) parts.push(`ext:${x}`);
  for (const x of e.notExts) parts.push(`-ext:${x}`);
  return parts.join(" ");
}

/** 逗号分隔的输入框值 → 去空数组。 */
export function splitCsv(value: string): string[] {
  return value
    .split(",")
    .map((s) => s.trim())
    .filter(Boolean);
}

/** 常见校验和 / 签名文件扩展名（一键负筛选）。 */
export const CHECKSUM_EXTS = ["sha1", "md5", "sha256", "sha512", "asc"];
