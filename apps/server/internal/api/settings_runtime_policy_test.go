package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// 本文件覆盖运行期策略 13 项的**设置面**：夹取口径、回显口径，以及「保存后回调真的被触发」。
//
// 为什么要分两层测：夹取函数的边界口径（0 / 负数 / 超上限）直接决定存进表里的值，
// 而存进去的值就是运行时会用到的值；回调是否正确分发则由 cmd/jianartifact 的装配级用例
// 单独盯（那里才看得到真实的调度器与服务）。

// TestClampRuntimePolicyBoundaries 覆盖 13 项的夹取边界。
//
// 三类口径必须分开验证，不能笼统断言"越界就回退默认"：
//   - **0 有语义**（10 项）：0 原样保留，负数回落默认，超上限夹取；
//   - **不允许 0**（宽限期）：0 与负数都回落默认——零宽限会误删正在拷入的包；
//   - **0 回退默认**（搜索预算 / 断路器退避）：0 与负数都回落默认。
func TestClampRuntimePolicyBoundaries(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		p    policyClamp
		in   int64
		want int64
	}{
		// 一、0 有语义的作业间隔：0 是禁用，必须原样保留。
		{"blob-gc 间隔 0 保留（禁用）", blobGCIntervalClamp, 0, 0},
		{"blob-gc 间隔负数回落默认 86400", blobGCIntervalClamp, -1, defaultBlobGCIntervalSecs},
		{"blob-gc 间隔默认值 86400 原样保留", blobGCIntervalClamp, 86400, 86400},
		{"blob-gc 间隔超上限夹到 30 天", blobGCIntervalClamp, 99999999, maxBackupAutoIntervalSecs},
		{"storage-cleanup 间隔 0 保留", storageCleanupIntervalClamp, 0, 0},
		{"upstream-probe 间隔 0 保留（关闭探测）", upstreamProbeIntervalClamp, 0, 0},
		{"upstream-probe 间隔 60 原样保留", upstreamProbeIntervalClamp, 60, 60},

		// 二、宽限期不允许 0：0 与负数都回落默认 1 小时。
		{"宽限期 0 回落默认 1（零宽限会误删在写的包）", backupOrphanGraceClamp, 0, defaultBackupOrphanGraceHours},
		{"宽限期负数回落默认 1", backupOrphanGraceClamp, -3, defaultBackupOrphanGraceHours},
		{"宽限期 12 原样保留", backupOrphanGraceClamp, 12, 12},
		{"宽限期超上限夹到 8760", backupOrphanGraceClamp, 999999, maxConfigHours},

		// 三、保留天数 / 临时文件存活：0 = 禁用该子步骤，合法。
		{"元数据保留 0 保留（不裁剪）", storageMetadataRetentionClamp, 0, 0},
		{"元数据保留负数回落默认 7 天", storageMetadataRetentionClamp, -1, defaultStorageMetadataRetentionDays},
		{"元数据保留 30 原样保留", storageMetadataRetentionClamp, 30, 30},
		{"元数据保留超上限夹到 365 天", storageMetadataRetentionClamp, 10000, maxConfigDays},
		{"临时文件存活 0 保留（不清理）", storageTempMaxAgeClamp, 0, 0},
		{"回收站保留 0 保留（不自动清理）", trashRetentionClamp, 0, 0},
		{"回收站保留 7 原样保留", trashRetentionClamp, 7, 7},

		// 四、回收站上限（字节）：0 = 不限制，合法。
		{"回收站上限 0 保留（不限制）", trashMaxBytesClamp, 0, 0},
		{"回收站上限 2 GiB 原样保留", trashMaxBytesClamp, 2 << 30, 2 << 30},
		{"回收站上限负数回落默认 2 GiB", trashMaxBytesClamp, -1, defaultTrashMaxBytes},
		{"回收站上限超上限夹到 1 TiB", trashMaxBytesClamp, 1 << 50, maxTrashMaxBytes},

		// 五、无禁用语义的两项：0 与负数都回落默认。
		{"断路器退避 0 回落默认 40（0 等于不阻止）", autoBlockBaseClamp, 0, defaultAutoBlockBaseSecs},
		{"断路器退避负数回落默认 40", autoBlockBaseClamp, -10, defaultAutoBlockBaseSecs},
		{"断路器退避超上限夹到 3600", autoBlockBaseClamp, 99999, maxConfigSecs},
		{"搜索预算 0 回落默认 10（关掉超时等于放开无界查询）", searchTimeoutClamp, 0, defaultSearchTimeoutSecs},
		{"搜索预算负数回落默认 10", searchTimeoutClamp, -5, defaultSearchTimeoutSecs},
		{"搜索预算 1 原样保留", searchTimeoutClamp, 1, 1},

		// 六、容量缓存 TTL：0 = 禁用缓存，合法。
		{"容量缓存 0 保留（禁用缓存）", capacityCacheTTLClamp, 0, 0},
		{"容量缓存负数回落默认 30", capacityCacheTTLClamp, -1, defaultCapacityCacheTTLSecs},
		{"容量缓存超上限夹到 3600", capacityCacheTTLClamp, 86400, maxConfigSecs},
	}
	for _, tc := range cases {
		if got := clampPolicy(tc.p, tc.in); got != tc.want {
			t.Errorf("%s：入参 %d 得 %d，期望 %d", tc.name, tc.in, got, tc.want)
		}
	}
}

// TestRuntimePolicyDefaultsAreNotEmpty 钉住 api 层展示默认值的关键几条。
//
// 这组常量与 config 层的默认值各存一份（config 的不导出，本包读不到），只能靠断言防止
// 单侧改动：把 config 的 blob-gc 默认从 86400 改成 3600，本文件的 86400 仍在，
// 届时"播种失败时界面显示什么"就会与"实际在跑什么"不一致。
// 这里只钉最关键的几条（作业间隔必须是"每天一次"量级），完整同值校验需要装配层视角，
// 见 cmd/jianartifact 的播种用例。
func TestRuntimePolicyDefaultsAreNotEmpty(t *testing.T) {
	t.Parallel()
	// 五类作业间隔的默认值必须是"每天一次"这个量级，不能是 0 或个位数秒：
	// 0 会让升级上来的实例在播种失败时静默关掉全部清理作业。
	for name, v := range map[string]int{
		"blob-gc":               defaultBlobGCIntervalSecs,
		"storage-cleanup":       defaultStorageCleanupIntervalSecs,
		"backup-orphan-cleanup": defaultBackupOrphanCleanupIntervalSecs,
		"trash-gc":              defaultTrashGCIntervalSecs,
	} {
		if v != 86400 {
			t.Errorf("%s 默认间隔应为 86400 秒（24 小时），实际 %d", name, v)
		}
	}
	if defaultUpstreamProbeIntervalSecs != 60 {
		t.Errorf("上游探测默认间隔应为 60 秒，实际 %d", defaultUpstreamProbeIntervalSecs)
	}
	if defaultBackupOrphanGraceHours != 1 {
		t.Errorf("孤儿包宽限期默认应为 1 小时，实际 %d", defaultBackupOrphanGraceHours)
	}
	if defaultTrashMaxBytes != 2<<30 {
		t.Errorf("回收站上限默认应为 2 GiB，实际 %d", defaultTrashMaxBytes)
	}
}

// TestSettingsSnapshotRuntimePolicyDefaults 钉住「13 项键缺失时界面显示什么」。
//
// 全部回退到与 config 同值的默认值，而不是 0：0 在这些项上多数表示「禁用」，
// 显示 0 会让运维以为清理作业已经关了，而实际上它们正按默认间隔在跑。
func TestSettingsSnapshotRuntimePolicyDefaults(t *testing.T) {
	t.Parallel()
	_, _, _, router, _ := newTuningTestEnv(t)

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/settings", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET 应 200，得 %d：%s", rec.Code, rec.Body.String())
	}
	var got SettingsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("解析响应：%v", err)
	}
	checks := []struct {
		name string
		got  int64
		want int64
	}{
		{"blobGcInterval", int64(got.BlobGCInterval), defaultBlobGCIntervalSecs},
		{"storageCleanupInterval", int64(got.StorageCleanupInterval), defaultStorageCleanupIntervalSecs},
		{"backupOrphanCleanupInterval", int64(got.BackupOrphanCleanupInterval), defaultBackupOrphanCleanupIntervalSecs},
		{"trashGcInterval", int64(got.TrashGCInterval), defaultTrashGCIntervalSecs},
		{"upstreamProbeInterval", int64(got.UpstreamProbeInterval), defaultUpstreamProbeIntervalSecs},
		{"backupOrphanGraceHours", int64(got.BackupOrphanGraceHours), defaultBackupOrphanGraceHours},
		{"storageMetadataRetentionDays", int64(got.StorageMetadataRetentionDays), defaultStorageMetadataRetentionDays},
		{"storageTempMaxAgeHours", int64(got.StorageTempMaxAgeHours), defaultStorageTempMaxAgeHours},
		{"trashRetentionDays", int64(got.TrashRetentionDays), defaultTrashRetentionDays},
		{"trashMaxBytes", got.TrashMaxBytes, defaultTrashMaxBytes},
		{"autoBlockBaseSeconds", int64(got.AutoBlockBaseSeconds), defaultAutoBlockBaseSecs},
		{"searchTimeoutSeconds", int64(got.SearchTimeoutSeconds), defaultSearchTimeoutSecs},
		{"capacityCacheTtlSeconds", int64(got.CapacityCacheTTLSeconds), defaultCapacityCacheTTLSecs},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s 键缺失时应回填默认 %d，实际 %d", c.name, c.want, c.got)
		}
	}
}

// TestPutSettingsRuntimePolicyEcho 端到端确认：13 项照实写库并按夹取后的值回显，
// 其中 0（禁用 / 不限制）必须**原样回显 0**，否则界面无法把开关拨回关闭态。
func TestPutSettingsRuntimePolicyEcho(t *testing.T) {
	t.Parallel()
	_, _, _, router, _ := newTuningTestEnv(t)

	// 一次性写入 13 项，其中多数显式取 0（禁用语义）。
	body := `{
		"blobGcInterval": 0,
		"storageCleanupInterval": 3600,
		"backupOrphanCleanupInterval": 7200,
		"trashGcInterval": 0,
		"upstreamProbeInterval": 0,
		"backupOrphanGraceHours": 3,
		"storageMetadataRetentionDays": 0,
		"storageTempMaxAgeHours": 0,
		"trashRetentionDays": 0,
		"trashMaxBytes": 0,
		"autoBlockBaseSeconds": 120,
		"searchTimeoutSeconds": 5,
		"capacityCacheTtlSeconds": 0
	}`
	resp, code := putSettingsJSON(t, router, body)
	if code != http.StatusOK {
		t.Fatalf("PUT 应 200，得 %d", code)
	}
	if resp.BlobGCInterval != 0 {
		t.Errorf("blobGcInterval 应回显 0（禁用），实际 %d", resp.BlobGCInterval)
	}
	if resp.StorageCleanupInterval != 3600 {
		t.Errorf("storageCleanupInterval 应回显 3600，实际 %d", resp.StorageCleanupInterval)
	}
	if resp.BackupOrphanCleanupInterval != 7200 {
		t.Errorf("backupOrphanCleanupInterval 应回显 7200，实际 %d", resp.BackupOrphanCleanupInterval)
	}
	if resp.TrashGCInterval != 0 {
		t.Errorf("trashGcInterval 应回显 0，实际 %d", resp.TrashGCInterval)
	}
	if resp.UpstreamProbeInterval != 0 {
		t.Errorf("upstreamProbeInterval 应回显 0（关闭），实际 %d", resp.UpstreamProbeInterval)
	}
	if resp.BackupOrphanGraceHours != 3 {
		t.Errorf("backupOrphanGraceHours 应回显 3，实际 %d", resp.BackupOrphanGraceHours)
	}
	if resp.StorageMetadataRetentionDays != 0 {
		t.Errorf("storageMetadataRetentionDays 应回显 0（不裁剪），实际 %d", resp.StorageMetadataRetentionDays)
	}
	if resp.StorageTempMaxAgeHours != 0 {
		t.Errorf("storageTempMaxAgeHours 应回显 0，实际 %d", resp.StorageTempMaxAgeHours)
	}
	if resp.TrashRetentionDays != 0 {
		t.Errorf("trashRetentionDays 应回显 0（不自动清理），实际 %d", resp.TrashRetentionDays)
	}
	if resp.TrashMaxBytes != 0 {
		t.Errorf("trashMaxBytes 应回显 0（不限制），实际 %d", resp.TrashMaxBytes)
	}
	if resp.AutoBlockBaseSeconds != 120 {
		t.Errorf("autoBlockBaseSeconds 应回显 120，实际 %d", resp.AutoBlockBaseSeconds)
	}
	if resp.SearchTimeoutSeconds != 5 {
		t.Errorf("searchTimeoutSeconds 应回显 5，实际 %d", resp.SearchTimeoutSeconds)
	}
	if resp.CapacityCacheTTLSeconds != 0 {
		t.Errorf("capacityCacheTtlSeconds 应回显 0（禁用缓存），实际 %d", resp.CapacityCacheTTLSeconds)
	}
}

// TestPutSettingsRuntimePolicyClampOverHTTP 确认越界输入在**存进表之前**就被夹取，
// 且「传哪个改哪个」语义不变：未提交的项保持原值。
func TestPutSettingsRuntimePolicyClampOverHTTP(t *testing.T) {
	t.Parallel()
	_, _, _, router, _ := newTuningTestEnv(t)

	// 宽限期传 0：必须回落到默认 1 小时（零宽限会误删正在拷入的包）。
	resp, code := putSettingsJSON(t, router, `{"backupOrphanGraceHours":0}`)
	if code != http.StatusOK {
		t.Fatalf("PUT 宽限期 0 应 200，得 %d", code)
	}
	if resp.BackupOrphanGraceHours != defaultBackupOrphanGraceHours {
		t.Fatalf("宽限期 0 应回落默认 %d，实际 %d", defaultBackupOrphanGraceHours, resp.BackupOrphanGraceHours)
	}

	// 搜索预算传 0：本项无禁用语义，回落默认 10 秒。
	resp, code = putSettingsJSON(t, router, `{"searchTimeoutSeconds":0}`)
	if code != http.StatusOK {
		t.Fatalf("PUT 搜索预算 0 应 200，得 %d", code)
	}
	if resp.SearchTimeoutSeconds != defaultSearchTimeoutSecs {
		t.Fatalf("搜索预算 0 应回落默认 %d，实际 %d", defaultSearchTimeoutSecs, resp.SearchTimeoutSeconds)
	}

	// 作业间隔传超大值：夹到 30 天上界，而不是拒绝保存。
	resp, code = putSettingsJSON(t, router, `{"blobGcInterval":99999999}`)
	if code != http.StatusOK {
		t.Fatalf("PUT 超大间隔应 200（夹取而非报错），得 %d", code)
	}
	if resp.BlobGCInterval != maxBackupAutoIntervalSecs {
		t.Fatalf("超大间隔应夹到 %d，实际 %d", maxBackupAutoIntervalSecs, resp.BlobGCInterval)
	}

	// 只提交搜索预算时，前面写过的宽限期与 blob 间隔不得被改动。
	resp, code = putSettingsJSON(t, router, `{"searchTimeoutSeconds":8}`)
	if code != http.StatusOK {
		t.Fatalf("PUT 搜索预算 8 应 200，得 %d", code)
	}
	if resp.BackupOrphanGraceHours != defaultBackupOrphanGraceHours {
		t.Errorf("未提交的宽限期不应被改动：期望 %d，实际 %d", defaultBackupOrphanGraceHours, resp.BackupOrphanGraceHours)
	}
	if resp.BlobGCInterval != maxBackupAutoIntervalSecs {
		t.Errorf("未提交的 blob 间隔不应被改动：期望 %d，实际 %d", maxBackupAutoIntervalSecs, resp.BlobGCInterval)
	}
}

// TestPutSettingsRuntimePolicyCallbacks 验证 8 项时长类 + 1 项容量类回调**只对提交的字段**触发，
// 且传入的是夹取后的值。
//
// 为什么必须验证「只对提交的字段触发」：把未提交的项也推一遍，装配层会以为运维刚改过它，
// 对作业间隔而言那意味着一次无谓的 SetInterval（进而影响下一轮的起算时刻）。
func TestPutSettingsRuntimePolicyCallbacks(t *testing.T) {
	t.Parallel()

	type call struct {
		key string
		d   time.Duration
	}
	var (
		calls    []call
		capacity []int64
	)
	h := NewHandlers(Deps{
		Settings: domain.NewSettingService(repository.NewSettingRepo(openAPITestDB(t))),
		OnRuntimePolicyChange: func(key string, d time.Duration) {
			calls = append(calls, call{key, d})
		},
		OnTrashCapacityChange: func(maxBytes int64) {
			capacity = append(capacity, maxBytes)
		},
	})
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("auth.principal", &auth.Principal{Role: "admin", Username: "admin", UserID: 1})
		c.Next()
	})
	router.PUT("/api/v1/settings", h.PutSettings)

	// 只提交 3 项时长类 + 1 项容量类。
	body := `{"blobGcInterval":1800,"searchTimeoutSeconds":7,"capacityCacheTtlSeconds":0,"trashMaxBytes":1048576}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/v1/settings", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT 应 200，得 %d：%s", rec.Code, rec.Body.String())
	}

	want := map[string]time.Duration{
		domain.SettingKeyBlobGCInterval:          1800 * time.Second,
		domain.SettingKeySearchTimeoutSeconds:    7 * time.Second,
		domain.SettingKeyCapacityCacheTTLSeconds: 0,
	}
	if len(calls) != len(want) {
		t.Fatalf("应有 %d 次时长类回调，实际 %d 次：%+v", len(want), len(calls), calls)
	}
	for _, c := range calls {
		expected, ok := want[c.key]
		if !ok {
			t.Errorf("收到未提交字段的回调：%s", c.key)
			continue
		}
		if c.d != expected {
			t.Errorf("%s 回调值 = %s，期望 %s", c.key, c.d, expected)
		}
	}
	if len(capacity) != 1 || capacity[0] != 1048576 {
		t.Fatalf("回收站容量回调应收到 1 次 1048576，实际 %v", capacity)
	}
}

// TestSettingsSnapshotWithoutServiceDoesNotPanic 钉住「未装配 SettingService 时不打崩」。
//
// settingServiceOrZero() 会返回一个零值 SettingService（内部仓储为 nil），
// 而 settingsSnapshot 仍会逐项读取回填。没有 nil 守卫时 nil.Get 会 panic，
// 让整个 GET /settings 变成 500。本次把读取项从 3 个扩到 16 个，触发面被放大，
// 因此单独钉一条。
func TestSettingsSnapshotWithoutServiceDoesNotPanic(t *testing.T) {
	t.Parallel()
	h := NewHandlers(Deps{}) // 刻意不给 Settings

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("未装配设置服务时快照不得 panic：%v", r)
		}
	}()
	snap := h.settingsSnapshot()
	// 各项应回退到默认值而不是零值。
	if snap.GroupMemberTimeout <= 0 || snap.NegativeCacheTTL <= 0 {
		t.Fatalf("应回退默认值，实际 budget=%d ttl=%d", snap.GroupMemberTimeout, snap.NegativeCacheTTL)
	}
}
