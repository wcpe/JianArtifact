package domain

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/archive"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// seedBackupPackage 直接登记一个已完成的包（不生成包体），并回填创建时间。
// 保留策略只关心登记行的 origin / status / created_at，不需要真实包体。
func (f *backupFixture) seedBackupPackage(t *testing.T, packageID, origin string, createdAt time.Time) {
	t.Helper()
	if err := f.repo.Create(repository.BackupPackage{
		PackageID: packageID,
		Mode:      repository.BackupModeHot,
		Status:    repository.BackupStatusDone,
		Origin:    origin,
		CreatedAt: createdAt.UTC().Format(time.RFC3339Nano),
	}); err != nil {
		t.Fatalf("登记备份包 %s：%v", packageID, err)
	}
}

// newRetentionFixture 建一个带保留策略服务的夹具，并允许固定"当前时间"。
func newRetentionFixture(t *testing.T, opts BackupRetentionOptions, now time.Time) (*backupFixture, *BackupRetentionService) {
	t.Helper()
	f := newBackupFixture(t)
	svc := NewBackupRetentionService(f.svc, f.repo, opts)
	svc.now = func() time.Time { return now.UTC() }
	return f, svc
}

// TestBackupRetentionKeepsExactlyN 覆盖份数边界：恰好 N 份时一份都不删；
// 多出的一份被删，且删的是最老的那份。
func TestBackupRetentionKeepsExactlyN(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)

	t.Run("恰好N份不删", func(t *testing.T) {
		f, svc := newRetentionFixture(t, BackupRetentionOptions{KeepLast: 3}, now)
		for i := 0; i < 3; i++ {
			f.seedBackupPackage(t, "bk-"+string(rune('a'+i)), repository.BackupOriginAuto, now.Add(-time.Duration(i)*time.Hour))
		}
		res, err := svc.Run()
		if err != nil {
			t.Fatalf("Run：%v", err)
		}
		if res.Deleted != 0 || res.Skipped != 0 {
			t.Fatalf("恰好 N 份不应删除任何包，实际 %+v", res)
		}
		if res.Candidates != 3 {
			t.Fatalf("候选数 = %d，期望 3", res.Candidates)
		}
	})

	t.Run("多出一份删最老", func(t *testing.T) {
		f, svc := newRetentionFixture(t, BackupRetentionOptions{KeepLast: 3}, now)
		// bk-oldest 最老，应被删；其余三份保留。
		f.seedBackupPackage(t, "bk-oldest", repository.BackupOriginAuto, now.Add(-4*time.Hour))
		for i := 0; i < 3; i++ {
			f.seedBackupPackage(t, "bk-"+string(rune('a'+i)), repository.BackupOriginAuto, now.Add(-time.Duration(i)*time.Hour))
		}
		res, err := svc.Run()
		if err != nil {
			t.Fatalf("Run：%v", err)
		}
		if res.Deleted != 1 {
			t.Fatalf("淘汰数 = %d，期望 1（%+v）", res.Deleted, res)
		}
		if _, err := f.svc.Get("bk-oldest"); err == nil {
			t.Fatal("最老的一份应被淘汰")
		}
		for i := 0; i < 3; i++ {
			if _, err := f.svc.Get("bk-" + string(rune('a'+i))); err != nil {
				t.Fatalf("最近 3 份应保留，%s 查询失败：%v", "bk-"+string(rune('a'+i)), err)
			}
		}
	})
}

// TestBackupRetentionKeepsExactlyNDays 覆盖天数边界：恰好 N 天内的保留；
// 早于 N 天的淘汰。
func TestBackupRetentionKeepsExactlyNDays(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	// 不设份数上限，只让天数生效。
	f, svc := newRetentionFixture(t, BackupRetentionOptions{RetentionDays: 7}, now)

	// 恰好 7 天前（now - 7d）：不早于 cutoff，保留。
	f.seedBackupPackage(t, "bk-exactly-7d", repository.BackupOriginAuto, now.AddDate(0, 0, -7))
	// 7 天零 1 秒前：早于 cutoff，淘汰。
	f.seedBackupPackage(t, "bk-over-7d", repository.BackupOriginAuto, now.AddDate(0, 0, -7).Add(-time.Second))
	// 3 天前：保留。
	f.seedBackupPackage(t, "bk-3d", repository.BackupOriginAuto, now.AddDate(0, 0, -3))

	res, err := svc.Run()
	if err != nil {
		t.Fatalf("Run：%v", err)
	}
	if res.Deleted != 1 {
		t.Fatalf("淘汰数 = %d，期望 1（%+v）", res.Deleted, res)
	}
	if _, err := f.svc.Get("bk-over-7d"); err == nil {
		t.Fatal("超过保留天数的包应被淘汰")
	}
	for _, id := range []string{"bk-exactly-7d", "bk-3d"} {
		if _, err := f.svc.Get(id); err != nil {
			t.Fatalf("%s 应保留：%v", id, err)
		}
	}
}

// TestBackupRetentionTakesStricterOfCountAndAge 覆盖两条上限交叉：取更严格者。
// 三种组合各验一遍——只触发份数、只触发天数、两条都触发。
func TestBackupRetentionTakesStricterOfCountAndAge(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)

	t.Run("只触发份数", func(t *testing.T) {
		// 10 份都在 1 天内（不触发天数），份数上限 2 → 删 8。
		f, svc := newRetentionFixture(t, BackupRetentionOptions{KeepLast: 2, RetentionDays: 30}, now)
		for i := 0; i < 10; i++ {
			f.seedBackupPackage(t, "bk-"+string(rune('a'+i)), repository.BackupOriginAuto, now.Add(-time.Duration(i)*time.Minute))
		}
		res, err := svc.Run()
		if err != nil {
			t.Fatalf("Run：%v", err)
		}
		if res.Deleted != 8 {
			t.Fatalf("淘汰数 = %d，期望 8（%+v）", res.Deleted, res)
		}
	})

	t.Run("只触发天数", func(t *testing.T) {
		// 3 份都很老（都超 30 天），份数上限 10（不触发）→ 删 3。
		f, svc := newRetentionFixture(t, BackupRetentionOptions{KeepLast: 10, RetentionDays: 30}, now)
		for i := 0; i < 3; i++ {
			f.seedBackupPackage(t, "bk-"+string(rune('a'+i)), repository.BackupOriginAuto, now.AddDate(0, 0, -40-i))
		}
		res, err := svc.Run()
		if err != nil {
			t.Fatalf("Run：%v", err)
		}
		if res.Deleted != 3 {
			t.Fatalf("淘汰数 = %d，期望 3（%+v）", res.Deleted, res)
		}
	})

	t.Run("两条都触发取更严格", func(t *testing.T) {
		// 5 份：最近 2 份在 3 天内；第 3、4 份在 3~40 天内（仅份数超限）；
		// 第 5 份在 40 天前（两条都超限）。份数上限 2、天数 30 → 删 3。
		f, svc := newRetentionFixture(t, BackupRetentionOptions{KeepLast: 2, RetentionDays: 30}, now)
		f.seedBackupPackage(t, "bk-0", repository.BackupOriginAuto, now.Add(-time.Hour))
		f.seedBackupPackage(t, "bk-1", repository.BackupOriginAuto, now.Add(-2*time.Hour))
		f.seedBackupPackage(t, "bk-2", repository.BackupOriginAuto, now.AddDate(0, 0, -3))
		f.seedBackupPackage(t, "bk-3", repository.BackupOriginAuto, now.AddDate(0, 0, -20))
		f.seedBackupPackage(t, "bk-4", repository.BackupOriginAuto, now.AddDate(0, 0, -40))

		res, err := svc.Run()
		if err != nil {
			t.Fatalf("Run：%v", err)
		}
		if res.Deleted != 3 {
			t.Fatalf("淘汰数 = %d，期望 3（%+v）", res.Deleted, res)
		}
		for _, id := range []string{"bk-0", "bk-1"} {
			if _, err := f.svc.Get(id); err != nil {
				t.Fatalf("%s 应保留：%v", id, err)
			}
		}
	})
}

// TestBackupRetentionNeverDeletesManualPackages 覆盖最关键的安全约束：
// 手动包永不自动删除，即使它最老、即使份数/天数都严重超限。
func TestBackupRetentionNeverDeletesManualPackages(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	f, svc := newRetentionFixture(t, BackupRetentionOptions{KeepLast: 1, RetentionDays: 1}, now)

	// 三个手动包，全都极老（远超任何阈值）。
	f.seedBackupPackage(t, "bk-manual-oldest", repository.BackupOriginManual, now.AddDate(0, 0, -365))
	f.seedBackupPackage(t, "bk-manual-mid", repository.BackupOriginManual, now.AddDate(0, 0, -200))
	f.seedBackupPackage(t, "bk-manual-new", repository.BackupOriginManual, now.AddDate(0, 0, -100))
	// 两个自动包，供策略正常淘汰。
	f.seedBackupPackage(t, "bk-auto-1", repository.BackupOriginAuto, now.Add(-time.Hour))
	f.seedBackupPackage(t, "bk-auto-2", repository.BackupOriginAuto, now.Add(-2*time.Hour))

	res, err := svc.Run()
	if err != nil {
		t.Fatalf("Run：%v", err)
	}
	// 候选集只应含 2 个自动包（手动包根本不进候选）。
	if res.Candidates != 2 {
		t.Fatalf("候选数 = %d，期望 2（手动包不得进入候选集）", res.Candidates)
	}
	if res.Deleted != 1 {
		t.Fatalf("淘汰数 = %d，期望 1（只删较老的自动包）", res.Deleted)
	}
	for _, id := range []string{"bk-manual-oldest", "bk-manual-mid", "bk-manual-new", "bk-auto-1"} {
		if _, err := f.svc.Get(id); err != nil {
			t.Fatalf("%s 必须保留：%v", id, err)
		}
	}
	if _, err := f.svc.Get("bk-auto-2"); err == nil {
		t.Fatal("超出份数上限的自动包应被淘汰")
	}
}

// TestBackupRetentionSkipsBaseReferencedByDelta 覆盖基线保护：
// 被差包引用为基线的自动包跳过不删，且计入 Skipped（不是失败、也不是静默忽略）。
func TestBackupRetentionSkipsBaseReferencedByDelta(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	// 份数上限 1：三个自动包里有 2 个超限，但其中一个被差包引用。
	f, svc := newRetentionFixture(t, BackupRetentionOptions{KeepLast: 1}, now)

	f.seedBackupPackage(t, "bk-auto-new", repository.BackupOriginAuto, now.Add(-time.Hour))
	f.seedBackupPackage(t, "bk-auto-base", repository.BackupOriginAuto, now.Add(-2*time.Hour))
	f.seedBackupPackage(t, "bk-auto-plain", repository.BackupOriginAuto, now.Add(-3*time.Hour))
	// 一个手动差包以 bk-auto-base 为基线（差包本身是 manual，不受自动淘汰影响）。
	if err := f.repo.Create(repository.BackupPackage{
		PackageID:     "bk-delta",
		Mode:          repository.BackupModeHot,
		Status:        repository.BackupStatusDone,
		Origin:        repository.BackupOriginManual,
		BasePackageID: "bk-auto-base",
	}); err != nil {
		t.Fatalf("登记差包：%v", err)
	}

	res, err := svc.Run()
	if err != nil {
		t.Fatalf("Run：%v", err)
	}
	if res.Deleted != 1 {
		t.Fatalf("淘汰数 = %d，期望 1（只删无引用的那个）", res.Deleted)
	}
	if res.Skipped != 1 {
		t.Fatalf("跳过数 = %d，期望 1（被差包引用的基线必须计入跳过）", res.Skipped)
	}
	if _, err := f.svc.Get("bk-auto-base"); err != nil {
		t.Fatalf("被差包引用的基线不得删除：%v", err)
	}
	if _, err := f.svc.Get("bk-delta"); err != nil {
		t.Fatalf("差包本身（manual）不得删除：%v", err)
	}
	if _, err := f.svc.Get("bk-auto-plain"); err == nil {
		t.Fatal("无引用的超限自动包应被淘汰")
	}
}

// TestBackupRetentionDisabledWhenBothUnlimited 覆盖 0 值语义：
// 两条都配 0（不限制）时策略是空操作，一份都不删。
func TestBackupRetentionDisabledWhenBothUnlimited(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	f, svc := newRetentionFixture(t, BackupRetentionOptions{KeepLast: 0, RetentionDays: 0}, now)
	f.seedBackupPackage(t, "bk-a", repository.BackupOriginAuto, now.AddDate(0, 0, -365))
	f.seedBackupPackage(t, "bk-b", repository.BackupOriginAuto, now.AddDate(0, 0, -400))

	res, err := svc.Run()
	if err != nil {
		t.Fatalf("Run：%v", err)
	}
	if !res.Empty() || res.Deleted != 0 {
		t.Fatalf("两条都不限制时不得删除任何包，实际 %+v", res)
	}
	if res.Candidates != 0 {
		t.Fatalf("策略未启用时不应做候选集查询，实际 Candidates=%d", res.Candidates)
	}
}

// TestBackupRetentionIgnoresIncompletePackages 覆盖状态过滤：
// 生成中 / 失败的自动包不计入份数（否则会挤掉一份真正可用的备份），也不被淘汰。
func TestBackupRetentionIgnoresIncompletePackages(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	f, svc := newRetentionFixture(t, BackupRetentionOptions{KeepLast: 1}, now)

	f.seedBackupPackage(t, "bk-done", repository.BackupOriginAuto, now.Add(-time.Hour))
	// 一个更老的失败包：不应进候选、不应被删、也不占份数。
	if err := f.repo.Create(repository.BackupPackage{
		PackageID: "bk-failed", Mode: repository.BackupModeHot,
		Status: repository.BackupStatusFailed, Origin: repository.BackupOriginAuto,
		CreatedAt: now.Add(-10 * time.Hour).Format(time.RFC3339Nano),
	}); err != nil {
		t.Fatalf("登记失败包：%v", err)
	}

	res, err := svc.Run()
	if err != nil {
		t.Fatalf("Run：%v", err)
	}
	if res.Candidates != 1 {
		t.Fatalf("候选数 = %d，期望 1（失败包不进候选）", res.Candidates)
	}
	if res.Deleted != 0 {
		t.Fatalf("淘汰数 = %d，期望 0", res.Deleted)
	}
	if _, err := f.svc.Get("bk-failed"); err != nil {
		t.Fatalf("失败包不属本策略范围，不得被删：%v", err)
	}
}

// TestBackupRetentionLeavesNoOrphanSidecar 覆盖淘汰的磁盘收尾：
// 淘汰一份真实包后，它的侧车索引必须随之消失，目录里不得留下孤儿 .index。
//
// 这是"保留策略兜住磁盘占用"的一部分：只删包体不删侧车，索引会随周期数只增不减。
// 本用例走真实生成路径（seedAssets + Generate），因为侧车只有生成成功才会写出。
func TestBackupRetentionLeavesNoOrphanSidecar(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	f := newBackupFixture(t)
	f.seedAssets(t, "payload")

	// 生成两份真实自动包（各带包体与侧车），把创建时间回填成可判定的先后顺序。
	var ids []string
	for i := 0; i < 2; i++ {
		rec, err := f.svc.Generate(t.Context(), CreateBackupOptions{Mode: archive.ModeHot, Origin: repository.BackupOriginAuto})
		if err != nil {
			t.Fatalf("Generate：%v", err)
		}
		ids = append(ids, rec.PackageID)
	}
	svc := NewBackupRetentionService(f.svc, f.repo, BackupRetentionOptions{KeepLast: 1})
	svc.now = func() time.Time { return now.UTC() }
	// 直接回填 created_at：真实生成路径写的是"现在"，两次生成间隔太短，
	// 无法稳定区分先后；这里只需登记行的时间可判定，包体与侧车已是真实产物。
	for i, id := range ids {
		if _, err := f.db.Exec(`UPDATE backup_package SET created_at=? WHERE package_id=?`,
			now.Add(-time.Duration(2-i)*time.Hour).Format(time.RFC3339Nano), id); err != nil {
			t.Fatalf("回填创建时间 %s：%v", id, err)
		}
	}

	res, err := svc.Run()
	if err != nil {
		t.Fatalf("Run：%v", err)
	}
	if res.Deleted != 1 {
		t.Fatalf("淘汰数 = %d，期望 1（%+v）", res.Deleted, res)
	}
	if _, err := os.Stat(f.svc.SidecarPath(ids[0])); !os.IsNotExist(err) {
		t.Fatalf("被淘汰的包不得留下侧车索引，实际 err=%v", err)
	}
	if _, err := os.Stat(f.svc.PackagePath(ids[0])); !os.IsNotExist(err) {
		t.Fatalf("被淘汰的包体应消失，实际 err=%v", err)
	}
	// 保留的那份包体与侧车都必须完好，否则"淘汰"就变成了破坏可用备份。
	for _, p := range []string{f.svc.PackagePath(ids[1]), f.svc.SidecarPath(ids[1])} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("保留的包文件应完好（%s）：%v", filepath.Base(p), err)
		}
	}
}

// TestBackupGenerateRecordsOrigin 覆盖来源落库：
// 显式 auto 写成 auto；空值与未知值都收敛为 manual（保守地永不被自动淘汰）。
func TestBackupGenerateRecordsOrigin(t *testing.T) {
	t.Parallel()
	f := newBackupFixture(t)
	f.seedAssets(t, "payload")

	cases := []struct {
		name string
		give string
		want string
	}{
		{name: "显式自动", give: repository.BackupOriginAuto, want: repository.BackupOriginAuto},
		{name: "显式手动", give: repository.BackupOriginManual, want: repository.BackupOriginManual},
		{name: "空值回落手动", give: "", want: repository.BackupOriginManual},
		{name: "未知值回落手动", give: "whatever", want: repository.BackupOriginManual},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec, err := f.svc.Generate(t.Context(), CreateBackupOptions{Mode: archive.ModeHot, Origin: tc.give})
			if err != nil {
				t.Fatalf("Generate：%v", err)
			}
			if rec.Origin != tc.want {
				t.Fatalf("返回值 Origin = %q，期望 %q", rec.Origin, tc.want)
			}
			// 库内登记必须与返回值一致（否则列表显示与实际淘汰范围会分歧）。
			got, err := f.svc.Get(rec.PackageID)
			if err != nil {
				t.Fatalf("Get：%v", err)
			}
			if got.Origin != tc.want {
				t.Fatalf("登记行 Origin = %q，期望 %q", got.Origin, tc.want)
			}
		})
	}
}

// TestBackupDeleteRemovesVerificationReports 覆盖级联：
// 包被删除时其演练报告一并消失（报告随包存在）。
func TestBackupDeleteRemovesVerificationReports(t *testing.T) {
	t.Parallel()
	f := newBackupFixture(t)
	verifyRepo := repository.NewBackupVerificationRepo(f.db)
	drillSvc := NewBackupDrillService(f.svc, verifyRepo, f.dataDir)
	f.seedAssets(t, "payload")

	rec, err := f.svc.Generate(t.Context(), CreateBackupOptions{Mode: archive.ModeHot})
	if err != nil {
		t.Fatalf("Generate：%v", err)
	}
	if _, err := drillSvc.Drill(t.Context(), rec.PackageID, DrillBackupOptions{Level: repository.VerificationLevelShallow}); err != nil {
		t.Fatalf("Drill：%v", err)
	}
	if _, total, err := drillSvc.ListVerifications(rec.PackageID, 10, 0); err != nil || total != 1 {
		t.Fatalf("演练报告应已落库：total=%d err=%v", total, err)
	}
	if err := f.svc.Delete(rec.PackageID); err != nil {
		t.Fatalf("Delete：%v", err)
	}
	if total, err := verifyRepo.CountByPackage(rec.PackageID); err != nil || total != 0 {
		t.Fatalf("包删除后报告应随级联清除：total=%d err=%v", total, err)
	}
	// 临时目录不得残留。
	if _, err := os.Stat(drillSvc.StagingRoot()); err == nil {
		entries, _ := os.ReadDir(drillSvc.StagingRoot())
		if len(entries) != 0 {
			t.Fatalf("演练临时目录应被清理，残留 %d 项", len(entries))
		}
	}
}
