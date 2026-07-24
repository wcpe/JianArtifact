// 剪贴板写入：优先 Clipboard API；HTTP 非安全上下文下降级 execCommand。

/**
 * 将文本写入系统剪贴板。
 * - HTTPS / localhost：navigator.clipboard.writeText
 * - 普通 HTTP：临时 textarea + document.execCommand("copy")
 * 返回是否成功（失败时调用方可 toast 提示）。
 */
export async function copyToClipboard(text: string): Promise<boolean> {
  if (typeof text !== "string") {
    return false;
  }
  // 安全上下文且 Clipboard API 可用
  if (
    typeof navigator !== "undefined" &&
    typeof navigator.clipboard?.writeText === "function" &&
    (typeof window === "undefined" || window.isSecureContext !== false)
  ) {
    try {
      await navigator.clipboard.writeText(text);
      return true;
    } catch {
      // 权限拒绝等：继续降级
    }
  }
  return copyViaExecCommand(text);
}

/** 旧式复制：临时 textarea + execCommand（HTTP 可用）。 */
function copyViaExecCommand(text: string): boolean {
  if (typeof document === "undefined") {
    return false;
  }
  const ta = document.createElement("textarea");
  ta.value = text;
  ta.setAttribute("readonly", "");
  ta.style.position = "fixed";
  ta.style.left = "-9999px";
  ta.style.top = "0";
  document.body.appendChild(ta);
  ta.focus();
  ta.select();
  let ok = false;
  try {
    ok = document.execCommand("copy");
  } catch {
    ok = false;
  }
  document.body.removeChild(ta);
  return ok;
}
