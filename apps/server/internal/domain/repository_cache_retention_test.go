package domain_test

import (
	"errors"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// TestRepositoryCreateCacheRetentionValidation 代理缓存保留只对 proxy 有意义：
// hosted/group 携带非 0 值、proxy 携带负值都必须被拒（对外映射为 400），
// proxy 的 0（关闭）与非 0（开启）都必须放行。
func TestRepositoryCreateCacheRetentionValidation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name               string
		format             string
		typ                string
		remoteURL          string
		members            []string
		cacheRetentionDays int
		wantErr            bool
	}{
		{name: "proxy 关闭", format: "raw", typ: "proxy", remoteURL: "https://example.org/raw", cacheRetentionDays: 0},
		{name: "proxy 开启", format: "raw", typ: "proxy", remoteURL: "https://example.org/raw", cacheRetentionDays: 30},
		{name: "proxy 负值非法", format: "raw", typ: "proxy", remoteURL: "https://example.org/raw", cacheRetentionDays: -1, wantErr: true},
		{name: "hosted 开启非法", format: "raw", typ: "hosted", cacheRetentionDays: 7, wantErr: true},
		{name: "hosted 关闭", format: "raw", typ: "hosted", cacheRetentionDays: 0},
		{name: "group 开启非法", format: "raw", typ: "group", members: []string{"raw-hosted-member"}, cacheRetentionDays: 7, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// 每个子用例自带独立的临时 SQLite 与仓库命名空间，彼此无共享状态，可并行。
			t.Parallel()
			db := newTestDB(t)
			repoRepo := repository.NewRepoRepo(db)
			repoSvc := domain.NewRepositoryService(repoRepo, repository.NewAclRepo(db), repository.NewAssetRepo(db),
				domain.NewSettingService(repository.NewSettingRepo(db)), repository.NewUserRepo(db))
			if tc.typ == "group" {
				if _, err := repoSvc.Create("raw-hosted-member", "raw", "hosted", "private", "", repository.RepositoryConfig{}); err != nil {
					t.Fatalf("创建 group 成员仓库：%v", err)
				}
			}
			cfg := repository.RepositoryConfig{
				RemoteURL:          tc.remoteURL,
				Members:            tc.members,
				CacheRetentionDays: tc.cacheRetentionDays,
			}
			_, err := repoSvc.Create("raw-target", tc.format, tc.typ, "private", "", cfg)
			if tc.wantErr {
				if !errors.Is(err, domain.ErrValidation) {
					t.Fatalf("期望校验失败（ErrValidation），实际 err=%v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("期望创建成功，实际 err=%v", err)
			}
			repo, err := repoSvc.Get("raw-target")
			if err != nil {
				t.Fatalf("读回仓库：%v", err)
			}
			decoded, err := repo.DecodeConfig()
			if err != nil {
				t.Fatalf("解析仓库配置：%v", err)
			}
			if decoded.CacheRetentionDays != tc.cacheRetentionDays {
				t.Fatalf("cacheRetentionDays 回读=%d，期望=%d", decoded.CacheRetentionDays, tc.cacheRetentionDays)
			}
		})
	}
}
