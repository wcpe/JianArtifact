package domain

import (
	"errors"
	"fmt"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// TestFailStageMapsEveryDomainError 是「错误码被吞成 internal」的回归守卫（FR-40 追加十）。
//
// # 曾经的 bug
//
// failStage 的 switch 只认 5 种错误，其余一律归为 internal。于是用户恢复失败时
// 只看到「服务内部错误，请稍后重试或查看日志」，真实原因（基线缺失、已有导入在跑、
// 包未就绪…）被完全丢弃，日志里也没有对应记录——"恢复一直失败却查不出原因"就是这么来的。
//
// 本用例逐个断言：每种可自行处置的领域错误都必须映射到**它自己的**错误码，
// 绝不能落到 internal。
func TestFailStageMapsEveryDomainError(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		err      error
		wantCode string
	}{
		{"目标非空", fmt.Errorf("%w：请用 --overwrite 覆盖", ErrRestoreTargetNotEmpty), "target_not_empty"},
		{"版本不兼容", fmt.Errorf("%w（包 99 / 本地 48）", ErrRestoreIncompatible), "incompatible"},
		{"待重启叠加", fmt.Errorf("%w（包 bk-x 待生效）", ErrRestorePending), "restore_pending"},
		{"畸形差包", fmt.Errorf("%w：差包缺少 expected 摘要", ErrRestoreIncrementalUnsupported), "manifest_invalid"},
		{"规模超限", fmt.Errorf("%w", ErrRestoreTooLarge), "package_oversize"},
		// 以下三个此前全部落到 internal。
		{"基线缺失", fmt.Errorf("%w：基线包 bk-base 未就位", ErrRestoreBaseMissing), "base_missing"},
		{"已有导入在跑", ErrRestoreInProgress, "import_in_progress"},
		{"包未就绪", fmt.Errorf("%w：%s 当前状态 packing", ErrRestorePackageNotDone, "bk-x"), "package_not_ready"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newBackupFixture(t)
			repo := repository.NewBackupImportRepo(f.db)
			// 建一条 queued 记录作为被收尾的对象。
			rec := repository.BackupImport{
				ImportID: "imp-test-" + tc.wantCode,
				Origin:   repository.ImportOriginCLI,
				Status:   repository.ImportStatusQueued,
			}
			if err := repo.Create(rec); err != nil {
				t.Fatalf("建导入记录：%v", err)
			}

			failStage(repo, rec.ImportID, tc.err)

			got, err := repo.Get(rec.ImportID)
			if err != nil {
				t.Fatalf("读导入记录：%v", err)
			}
			if got.Status != repository.ImportStatusFailed {
				t.Fatalf("状态应为 failed，实得 %s", got.Status)
			}
			if got.ErrorCode == "" {
				t.Fatal("失败必须带 errorCode")
			}
			if got.ErrorCode != tc.wantCode {
				t.Fatalf("errorCode 应为 %q，实得 %q（此前会被吞成 internal）", tc.wantCode, got.ErrorCode)
			}
			// 原始错误文本必须保留：光有错误码不足以定位。
			if got.ErrorSummary == "" {
				t.Fatal("失败必须保留原始错误文本")
			}
		})
	}
}

// TestFailStageUnknownErrorKeepsReason 覆盖未知错误的兜底：
// 仍标 internal，但**必须把原始原因写进 error 字段**，而不是只有一句套话。
func TestFailStageUnknownErrorKeepsReason(t *testing.T) {
	t.Parallel()
	f := newBackupFixture(t)
	repo := repository.NewBackupImportRepo(f.db)
	rec := repository.BackupImport{
		ImportID: "imp-test-unknown",
		Origin:   repository.ImportOriginCLI,
		Status:   repository.ImportStatusQueued,
	}
	if err := repo.Create(rec); err != nil {
		t.Fatalf("建导入记录：%v", err)
	}

	// 一个不属于任何已知领域错误的故障。
	failStage(repo, rec.ImportID, errors.New("解包暂存目录：permission denied"))

	got, err := repo.Get(rec.ImportID)
	if err != nil {
		t.Fatalf("读导入记录：%v", err)
	}
	if got.ErrorCode != "internal" {
		t.Fatalf("未知错误应标 internal，实得 %q", got.ErrorCode)
	}
	if got.ErrorSummary == "" {
		t.Fatal("未知错误也必须保留原始文本")
	}
	if want := "permission denied"; !contains(got.ErrorSummary, want) {
		t.Fatalf("原始原因应保留在 error 里（含 %q），实得 %q", want, got.ErrorSummary)
	}
}
