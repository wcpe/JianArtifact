// 搜索框的候选下拉（规格 docs/specs/search-index-and-performance.md §3.9）。
//
// 这里刻意把「输入框 + 下拉」封成一个自管请求的组件，而不是继续在 SearchPage 里堆状态：
//   - 请求生命周期（防抖 / 中止 / 乱序保护）只属于这一块，与结果区的 useAsync（的
//     进度与取消）完全无关，混在一处会让两条 cancellation 语义互相干扰；
//   - 下拉的开合、高亮与键盘导航是一组必须同源的状态，拆开就会出现「高亮在、面板已关」。
//
// 三条硬约束（规格 §3.9）：
//  1. **防抖 ≤ 300ms**：连续键入只发最后一次请求；去空白后长度 < 2 不发请求（1 字符全是噪声）；
//  2. **旧响应不得覆盖新响应**：每次发起前 abort 上一次，并用请求序号兜住「abort 晚于响应到达」
//     的竞态；
//  3. **无候选不弹面板**：不渲染空面板与「无结果」占位行——那会让光标附近突然出现一块空白。
//
// 另外：仅键入**不触发搜索**（PRD 明确不做输入即搜），结果区只由提交动作刷新。
import { TextInput } from "@mantine/core";
import { IconSearch } from "@tabler/icons-react";
import {
  useCallback,
  useEffect,
  useId,
  useRef,
  useState,
  type CSSProperties,
  type FocusEvent,
  type KeyboardEvent,
} from "react";
import { useTranslation } from "react-i18next";

import { suggestSearch, type SearchSuggestion } from "../../api/endpoints";

/** 防抖窗口：规格要求 ≤ 300ms，取 250ms 留出一帧余量。 */
export const SUGGEST_DEBOUNCE_MS = 250;

/** 发请求的最短输入长度（去空白后）：1 字符的候选全是噪声（规格 §3.9）。 */
export const SUGGEST_MIN_CHARS = 2;

/** 单次请求的候选上限：与后端夹取上限一致，避免两处口径漂移。 */
export const SUGGEST_LIMIT = 10;

export interface SearchSuggestInputProps {
  value: string;
  /** 输入变化（含候选确认后的回填）。 */
  onChange: (value: string) => void;
  /** 确认包名候选 / 直接回车：把最终表达式交给调用方发起搜索。 */
  onSearch: (value: string) => void;
  /** 确认仓库名候选：调用方写回 `repo:` 表达式，但**不**立即搜索。 */
  onPickRepository: (name: string) => void;
  placeholder: string;
  style?: CSSProperties;
}

export function SearchSuggestInput({
  value,
  onChange,
  onSearch,
  onPickRepository,
  placeholder,
  style,
}: SearchSuggestInputProps) {
  const { t } = useTranslation();
  const [items, setItems] = useState<SearchSuggestion[]>([]);
  const [open, setOpen] = useState(false);
  const [activeIndex, setActiveIndex] = useState(-1);
  // 组件根节点：判断「点击外部」时用，避免把面板内的点击当成外部而先关面板。
  const rootRef = useRef<HTMLDivElement | null>(null);
  // 在途请求的中止器与序号。序号是乱序保护的最后一道闸：即使某次响应在 abort 之后才落地
  // （例如适配层已把结果放进微任务队列），序号不匹配也不会写进状态。
  const controllerRef = useRef<AbortController | null>(null);
  const seqRef = useRef(0);
  // 是否允许为「这一次值变化」发请求。
  //
  // 只在用户真的改动输入框时放行，用来挡掉两类非用户来源的变化：
  //   - 首屏 / 路由跳转把 URL 的 q 同步进输入框（否则进搜索页就自动弹一块候选面板）；
  //   - 确认候选后父组件回填表达式（否则面板会在 250ms 后自己重新弹出来，而此刻用户
  //     要的是「继续追加关键词」或「立刻看结果」）。
  const allowFetchRef = useRef(false);
  const listboxId = useId();

  const close = useCallback(() => {
    setOpen(false);
    setActiveIndex(-1);
  }, []);

  // 输入变化：去空白后足够长才发请求；否则立刻作废在途请求并清空候选。
  useEffect(() => {
    // 非用户来源的值变化（URL 同步 / 候选回填）：不发请求，也不弹面板。
    const fromTyping = allowFetchRef.current;
    allowFetchRef.current = false;
    const q = value.trim();
    const pending = controllerRef.current;
    if (!fromTyping || q.length < SUGGEST_MIN_CHARS) {
      // 短输入、清空或非用户变化：不必等防抖，立即收面板并中止上一次
      // （否则旧候选会在 250ms 后才消失）。
      seqRef.current += 1;
      pending?.abort();
      controllerRef.current = null;
      setItems([]);
      setOpen(false);
      setActiveIndex(-1);
      return;
    }
    const seq = seqRef.current + 1;
    seqRef.current = seq;
    // 输入一变就丢掉旧高亮：否则方向键会从「上一个输入的高亮项」继续移动，落到语义不同的候选上。
    setActiveIndex(-1);
    const timer = window.setTimeout(() => {
      // 每次发起前中止上一次：旧请求的响应不再有任何价值。
      // 必须读 ref 的**当前值**而不是 effect 运行时捕获的快照——连续键入时
      // 定时器可能晚于 Effect 执行 250ms，届时 ref 已被后续 Effect 更新，
      // 用旧快照会 abort 一个早已结束的 controller，而在途的新请求跑完全程。
      controllerRef.current?.abort();
      const controller = new AbortController();
      controllerRef.current = controller;
      void suggestSearch({ q, limit: SUGGEST_LIMIT, signal: controller.signal })
        .then((result) => {
          // 乱序保护：序号不是最新的一律丢弃（旧响应晚回不得覆盖新候选）。
          if (seqRef.current !== seq) {
            return;
          }
          setItems(result.items);
          // 无候选不弹面板：空列表等于「没有建议」，不渲染占位行。
          setOpen(result.items.length > 0);
          setActiveIndex(-1);
        })
        .catch(() => {
          if (seqRef.current !== seq) {
            return;
          }
          // suggest 是辅助功能：失败时静默收起，不弹错误——用户仍可直接回车搜索。
          setItems([]);
          setOpen(false);
          setActiveIndex(-1);
        });
    }, SUGGEST_DEBOUNCE_MS);
    return () => {
      window.clearTimeout(timer);
    };
  }, [value]);

  // 卸载时中止在途请求。
  useEffect(() => {
    return () => {
      controllerRef.current?.abort();
    };
  }, []);

  // 点击外部收起（规格 §3.9）。用 mousedown 而不是 click：后者在输入框失焦后仍会到达，
  // 会让「点候选」与「点外部」的判定顺序变得依赖浏览器实现。
  useEffect(() => {
    if (!open) {
      return;
    }
    const onPointerDown = (event: MouseEvent) => {
      if (!rootRef.current?.contains(event.target as Node)) {
        close();
      }
    };
    document.addEventListener("mousedown", onPointerDown);
    return () => document.removeEventListener("mousedown", onPointerDown);
  }, [open, close]);

  const pick = (item: SearchSuggestion) => {
    close();
    if (item.type === "repository") {
      // 仓库名候选：写回 repo: 表达式但不搜索——用户可以继续追加关键词。
      onPickRepository(item.value);
      return;
    }
    // 包名候选：写入输入框并直接发起搜索（对齐既有 handleSubmit）。
    onChange(item.value);
    onSearch(item.value);
  };

  const handleKeyDown = (event: KeyboardEvent<HTMLInputElement>) => {
    switch (event.key) {
      case "ArrowDown":
        if (items.length === 0) {
          return;
        }
        // 阻止光标跳到行尾：这里的方向键只服务于候选导航。
        event.preventDefault();
        setOpen(true);
        setActiveIndex((current) => (current + 1 >= items.length ? 0 : current + 1));
        return;
      case "ArrowUp":
        if (items.length === 0) {
          return;
        }
        event.preventDefault();
        setOpen(true);
        setActiveIndex((current) => (current <= 0 ? items.length - 1 : current - 1));
        return;
      case "Enter": {
        const picked = open && activeIndex >= 0 ? items[activeIndex] : undefined;
        if (picked) {
          event.preventDefault();
          pick(picked);
          return;
        }
        // 没有高亮项时回车就是「提交搜索」，与既有行为一致。
        onSearch(value);
        return;
      }
      case "Escape":
        if (open) {
          event.preventDefault();
          close();
        }
        return;
      default:
    }
  };

  const activeId = activeIndex >= 0 ? `${listboxId}-option-${activeIndex}` : undefined;

  // 焦点整体离开组件时收起面板（键盘 Tab 走开的主路径）。
  //
  // 只用 mousedown 判断「点击外部」是不够的：键盘用户按 Tab 离开不会产生 mousedown，
  // 面板会一直展开并盖住下方的筛选区，aria-expanded 也还停在 true。
  // 用 focusout（React 的 onBlur 即为冒泡版）配合 relatedTarget 判定：焦点仍在根节点内
  // （例如从输入框移到面板里的可聚焦元素）时不动；真正离开才关闭。
  // relatedTarget 为 null（焦点落到 document / 浏览器 UI）同样视为离开。
  // 鼠标点选候选的路径不受影响：面板项的 mousedown 已 preventDefault，焦点不会先离开。
  const handleBlur = (event: FocusEvent<HTMLDivElement>) => {
    if (!rootRef.current?.contains(event.relatedTarget as Node | null)) {
      close();
    }
  };

  return (
    <div ref={rootRef} style={{ position: "relative", ...style }} onBlur={handleBlur}>
      <TextInput
        placeholder={placeholder}
        // 可访问名称：只有 placeholder 时读屏软件不会把它当成一个具名输入框，
        // 且 placeholder 会随输入消失。统一用搜索框的既有文案键。
        aria-label={t("search.placeholder")}
        leftSection={<IconSearch size={16} />}
        value={value}
        onChange={(e) => {
          // 用户键入：放行这一次值变化对应的 suggest 请求。
          allowFetchRef.current = true;
          onChange(e.currentTarget.value);
        }}
        onKeyDown={handleKeyDown}
        // 组合框语义：展开态、候选列表与高亮项都要能被读屏软件跟随。
        role="combobox"
        aria-expanded={open}
        aria-controls={listboxId}
        aria-autocomplete="list"
        aria-activedescendant={activeId}
      />
      {open && items.length > 0 && (
        <div
          id={listboxId}
          role="listbox"
          aria-label={t("search.suggestLabel")}
          data-testid="search-suggest-listbox"
          style={{
            position: "absolute",
            top: "calc(100% + 4px)",
            left: 0,
            right: 0,
            zIndex: 300,
            background: "var(--mantine-color-body)",
            border: "1px solid var(--mantine-color-default-border)",
            borderRadius: 8,
            boxShadow: "var(--mantine-shadow-md)",
            maxHeight: 320,
            overflowY: "auto",
          }}
        >
          {items.map((item, index) => (
            <div
              key={`${item.type}:${item.value}`}
              id={`${listboxId}-option-${index}`}
              role="option"
              aria-selected={index === activeIndex}
              data-testid="search-suggest-option"
              data-type={item.type}
              data-value={item.value}
              // mousedown 时阻止默认行为：保住输入框焦点，避免 blur 先收起面板让点击落空。
              onMouseDown={(event) => {
                event.preventDefault();
                pick(item);
              }}
              onMouseEnter={() => setActiveIndex(index)}
              style={{
                display: "flex",
                alignItems: "center",
                gap: 8,
                padding: "6px 10px",
                cursor: "pointer",
                background:
                  index === activeIndex ? "var(--mantine-color-default-hover)" : "transparent",
              }}
            >
              <span style={{ flex: 1, minWidth: 0, overflow: "hidden", textOverflow: "ellipsis" }}>
                {item.value}
              </span>
              <span
                style={{
                  flexShrink: 0,
                  fontSize: 11,
                  color: "var(--mantine-color-dimmed)",
                }}
              >
                {item.type === "repository"
                  ? t("search.suggestTypeRepository")
                  : t("search.suggestTypePackage")}
              </span>
            </div>
          ))}
        </div>
      )}
    </div>
  );
}
