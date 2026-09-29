// 审计分页回归：服务端按契约对「未执行精确 COUNT」返回 totalCount=-1
// （见 openapi：-1 表示未执行精确计数），并以 nextCursor 表示「还有更多」。
// 分页器必须仅凭 nextCursor 就能前进——此前它按 totalCount 计算总页数，
// 恒得 1 页，导致 goToPage 拒绝一切翻页（分页失效）。
import { renderHook, waitFor, act } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { describe, expect, it, vi } from "vitest";

import "../../i18n";

const listAuditEvents = vi.hoisted(() => vi.fn());
const getAuditSummary = vi.hoisted(() => vi.fn());
const getAuditAttentionNotifications = vi.hoisted(() => vi.fn());

vi.mock("../../api/endpoints", () => ({
  listAuditEvents,
  getAuditSummary,
  getAuditAttentionNotifications,
}));

import { AUDIT_PAGE_SIZE, useAuditQuery } from "./useAuditQuery";

// 契约合法的一页：总数未知（-1）但明确告知还有下一页（nextCursor）。
function page(offset: number, nextCursor?: string) {
  return {
    items: [],
    snapshot: "snapshot",
    snapshotAt: "2026-09-29T08:00:00Z",
    totalCount: -1,
    nextCursor,
    // 断言只关心分页器，不构造条目明细。
    __offset: offset,
  };
}

function wrapper({ children }: { children: React.ReactNode }) {
  return <MemoryRouter>{children}</MemoryRouter>;
}

describe("审计分页器与 unknown total", () => {
  it("总数未知时仍可前进到下一页", async () => {
    listAuditEvents.mockImplementation((args: { offset: number }) =>
      Promise.resolve(page(args.offset, args.offset === 0 ? "50" : undefined)),
    );
    getAuditSummary.mockResolvedValue({ totalCount: 0 });
    getAuditAttentionNotifications.mockResolvedValue({ items: [] });

    const { result } = renderHook(() => useAuditQuery(), { wrapper });
    // 等首页数据落地：服务端给了 nextCursor 而总数未知，可达页数应为 2（而非恒 1）。
    await waitFor(() => expect(result.current.pagination.totalPages).toBe(2));

    // 关键断言：总数未知（-1）但服务端给了 nextCursor，就必须允许翻到第 2 页。
    act(() => result.current.pagination.goToPage(2));
    await waitFor(() =>
      expect(listAuditEvents).toHaveBeenLastCalledWith(
        expect.objectContaining({ offset: AUDIT_PAGE_SIZE }),
      ),
    );
    expect(result.current.pagination.page).toBe(2);
  });

  it("末页（无 nextCursor）不得继续前进", async () => {
    listAuditEvents.mockImplementation((args: { offset: number }) =>
      Promise.resolve(page(args.offset)),
    );
    getAuditSummary.mockResolvedValue({ totalCount: 0 });
    getAuditAttentionNotifications.mockResolvedValue({ items: [] });

    const { result } = renderHook(() => useAuditQuery(), { wrapper });
    await waitFor(() => expect(listAuditEvents).toHaveBeenCalled());

    act(() => result.current.pagination.goToPage(2));
    expect(result.current.pagination.page).toBe(1);
  });
});
