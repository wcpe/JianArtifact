#!/usr/bin/env python3
"""清理源码中的 FR 编号引用（只保留在 docs/ 与 CHANGELOG.md）。

规则见 .claude/rules/comments.md §5。要点：FR 编号进入代码会与并行分支在同一行
产生无意义冲突，且对代码读者无价值——读者需要"为什么这样写"，不是"属于哪张需求单"。

## 核心安全约束（上一版踩过的坑）

**只改注释段，绝不触碰代码段。** 上一版在整行做替换与收尾清理，其中「删除空括号」
把代码里的 `func()` 削成了 `func`，直接造成语法错误。因此本版把每行拆成
「代码段 + 注释段」：编号删除与标点收尾**只在注释段内进行**。

对纯注释行（整行以 `//` `#` `*` `/*` 开头）处理整行；否则只处理注释起始位置之后的部分。
字符串字面量里若出现编号（用户可见文案），也按注释段同样规则处理——它们不参与语法。
"""

from __future__ import annotations

import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent

INCLUDE_SUFFIXES = {".go", ".ts", ".tsx", ".js", ".mjs", ".sql", ".css", ".json", ".yaml", ".yml"}
SEARCH_DIRS = ["apps", "packages", "scripts", "api"]
SKIP_DIRS = {"node_modules", "dist", "build", ".git", "vendor", ".tmp", "coverage", "__pycache__"}

# 完整编号 token：FR-NN、FR-NN 追加七、FR-NN §3.4、FR-25c 变体。
# 注意：三个可选组都必须带 `?`——早先漏掉最后一个 `?` 会让「§x.y」变成必需项，
# 导致只有带章节号的写法能匹配，普通 `FR-89` 反而匹配不到。
FR_TOKEN = r"FR-\d+[a-z]?(?:\s*追加[一二三四五六七八九十]+)?(?:\s*§\s*[\d.]+)?"

RE_ANY = re.compile(FR_TOKEN)
# 整段括号引用（仅注释段内使用）
RE_BRACKET = re.compile(r"[（(【\[]\s*" + FR_TOKEN + r"\s*[）)】\]]")
# 注释行首前缀
RE_LEADING = re.compile(r"^(\s*(?://+|#+|/\*+|\*)\s*)" + FR_TOKEN + r"\s*[：:]\s*")
RE_LEADING_BARE = re.compile(r"^(\s*(?://+|#+|/\*+|\*)\s*)" + FR_TOKEN + r"\s+")
# 中文之间的多余空格（删除编号后常见的断裂）
RE_CJK_GAP = re.compile(r"([\u4e00-\u9fff])\s{2,}([\u4e00-\u9fff])")
# 删除编号后只剩注释符号的行
RE_EMPTY_COMMENT = re.compile(r"^\s*(?://+|#+)\s*$")

# 仅在注释段内使用：删掉因移除编号而变空的括号、重复标点、以及标点旁的游离空格。
COMMENT_CLEANUPS = [
    (re.compile(r"[（(]\s*[）)]"), ""),
    (re.compile(r"[，,]\s*[，,]"), "，"),
    (re.compile(r"[：:]\s*[：:]"), "："),
    (re.compile(r"[ \t]+([，。、；：！？）)】])"), r"\1"),
    (re.compile(r"([（(【])\s+"), r"\1"),
    (re.compile(r"//\s*[：:]\s*"), "// "),
    # 迁移文件头的「相关：FR-06 / FR-08 / ADR-0002」形态：编号被删后会留下
    # 「相关： / / ADR-0002」或「相关：ADR-0002」，把游离的分隔符一并收掉。
    (re.compile(r"(相关[：:])\s*(?:/\s*)+"), r"\1"),
    (re.compile(r"(相关[：:])\s+"), r"\1"),        # 中文冒号后紧跟内容，不留空格
    (re.compile(r"(\S)\s*/\s*/"), r"\1 "),        # 连续斜杠
    # 注释标记后残留的孤立冒号（`-- : 内容` / `// : 内容`）
    (re.compile(r"((?://+|--+|#+|\*)\s*)[：:]\s*"), r"\1"),
    # 注释标记后残留的多余空格（`--  内容`）
    (re.compile(r"((?://+|--+)\s)[ \t]+"), r"\1"),
    (re.compile(r"相关[：:]\s*$"), "相关：见 docs/"),  # 只剩空「相关」时给可读回落
    (re.compile(r"^(\s*(?://+|#+|--+)\s*)（\s*）"), r"\1"),  # 行首残留的空括号
]

# 注释起始标记（按语言）：在代码段之后寻找第一个未处于字符串内的注释标记。
# SQL 的 `--` 必须在内，否则迁移文件的注释会被当成"代码段"而整行跳过——
# 早先漏了它，导致 40 个迁移文件的「相关：FR-xx」行全部残留。
COMMENT_MARKERS = ("//", "--", "#", "/*")
# 纯注释行的行首标记（用于判定整行是否为注释）
PURE_MARKERS = ("//", "--", "#", "/*", "*")


def split_comment(line: str) -> tuple[str, str]:
    """把一行拆成 (代码段, 注释段)。注释段包含标记本身；无注释则第二项为空串。

    只做「不在引号内」的粗略判定：足够覆盖本仓库的注释形态，且误判只会导致
    保守（不处理），不会破坏代码。
    """
    in_str: str | None = None
    escaped = False
    i = 0
    while i < len(line):
        ch = line[i]
        if in_str:
            if escaped:
                escaped = False
            elif ch == "\\":
                escaped = True
            elif ch == in_str:
                in_str = None
            i += 1
            continue
        if ch in ('"', "'", "`"):
            in_str = ch
            i += 1
            continue
        for marker in COMMENT_MARKERS:
            if line.startswith(marker, i):
                return line[:i], line[i:]
        i += 1
    return line, ""


def clean_comment_segment(seg: str) -> str:
    """在注释段内清理编号与残留标点。"""
    seg = RE_BRACKET.sub("", seg)
    seg = RE_LEADING.sub(r"\1", seg)
    if not seg.lstrip().startswith("//") and not seg.lstrip().startswith("#"):
        seg = RE_LEADING_BARE.sub(r"\1", seg)
    seg = RE_ANY.sub("", seg)
    seg = RE_CJK_GAP.sub(r"\1\2", seg)
    for pat, repl in COMMENT_CLEANUPS:
        seg = pat.sub(repl, seg)
    return seg


def clean_line(line: str) -> tuple[str, bool]:
    original = line
    body = line.rstrip("\n")
    newline = line[len(body):]

    if not RE_ANY.search(body):
        return line, False

    indent = ""
    stripped = body.lstrip()
    indent = body[: len(body) - len(stripped)]

    # 纯注释行：整行都是注释，去掉行首标记后再清理
    pure = False
    for marker in PURE_MARKERS:
        if stripped.startswith(marker):
            pure = True
            break

    if pure:
        # 纯注释行：**连标记一起**交给清理流程，这样 RE_LEADING 能在串首看到 `//` / `#`。
        # （早先把标记剥下来只清理后半段，导致前缀规则匹配不到，留下 `//：` 之类的残迹。）
        cleaned = clean_comment_segment(stripped)
        new = indent + cleaned
        if RE_EMPTY_COMMENT.match(new):
            return "", True
    else:
        code, comment = split_comment(body)
        if not comment:
            # 编号出现在代码段（例如标识符或字符串），不自动处理，交给人工。
            return line, False
        new = code + clean_comment_segment(comment)

    new = new.rstrip()
    return new + newline, new != body


def main() -> int:
    dry_run = "--apply" not in sys.argv
    changed_files = 0
    changed_lines = 0
    skipped: list[tuple[str, str]] = []

    for d in SEARCH_DIRS:
        base = ROOT / d
        if not base.is_dir():
            continue
        for path in base.rglob("*"):
            if not path.is_file() or path.suffix not in INCLUDE_SUFFIXES:
                continue
            if any(part in SKIP_DIRS for part in path.parts):
                continue
            try:
                text = path.read_text(encoding="utf-8")
            except (UnicodeDecodeError, OSError):
                continue
            if "FR-" not in text:
                continue

            out_lines = []
            file_changed = 0
            for line in text.splitlines(keepends=True):
                new, did = clean_line(line)
                if did:
                    file_changed += 1
                elif RE_ANY.search(line):
                    skipped.append((str(path.relative_to(ROOT)), line.strip()[:90]))
                out_lines.append(new)

            if file_changed:
                changed_files += 1
                changed_lines += file_changed
                rel = path.relative_to(ROOT)
                print(f"{'[预览]' if dry_run else '[写入]'} {rel}：{file_changed} 行")
                if not dry_run:
                    path.write_text("".join(out_lines), encoding="utf-8")

    print(f"\n{'预计' if dry_run else '已'}改动 {changed_files} 个文件 / {changed_lines} 行")

    if skipped:
        print(f"\n[注意] 有 {len(skipped)} 行含编号但未自动处理（编号出现在代码段，需人工确认）：")
        for rel, snip in skipped[:20]:
            print(f"  {rel}: {snip}")
        if len(skipped) > 20:
            print(f"  … 另有 {len(skipped) - 20} 行")

    if dry_run:
        print("\n这是预览模式；加 --apply 才真正写入。")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
