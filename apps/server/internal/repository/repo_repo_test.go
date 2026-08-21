package repository_test

import (
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// newRepoRepo 打开临时 SQLite、迁移并返回仓库 Repo。
func newRepoRepo(t *testing.T) *repository.RepoRepo {
	t.Helper()
	db, err := persistence.Open(filepath.Join(t.TempDir(), "repo.db"))
	if err != nil {
		t.Fatalf("打开数据库：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("迁移：%v", err)
	}
	return repository.NewRepoRepo(db)
}

// TestRepoConfigRoundtrip 结构化配置经 Create 落库、GetByName 读回后应无损往返。
func TestRepoConfigRoundtrip(t *testing.T) {
	repos := newRepoRepo(t)

	proxyCfg := repository.RepositoryConfig{RemoteURL: "https://repo.example.com/maven"}
	proxyJSON, err := repository.EncodeRepositoryConfig(proxyCfg)
	if err != nil {
		t.Fatalf("编码 proxy 配置：%v", err)
	}
	if _, err := repos.Create("maven-proxy", "maven", "proxy", "private", proxyJSON); err != nil {
		t.Fatalf("建 proxy 仓库：%v", err)
	}
	got, err := repos.GetByName("maven-proxy")
	if err != nil {
		t.Fatalf("取 proxy 仓库：%v", err)
	}
	decoded, err := got.DecodeConfig()
	if err != nil {
		t.Fatalf("解码 proxy 配置：%v", err)
	}
	if decoded.RemoteURL != proxyCfg.RemoteURL {
		t.Errorf("remoteUrl 往返失真：得 %q，期望 %q", decoded.RemoteURL, proxyCfg.RemoteURL)
	}

	groupCfg := repository.RepositoryConfig{Members: []string{"maven-proxy", "maven-hosted"}}
	groupJSON, err := repository.EncodeRepositoryConfig(groupCfg)
	if err != nil {
		t.Fatalf("编码 group 配置：%v", err)
	}
	if _, err := repos.Create("maven-group", "maven", "group", "private", groupJSON); err != nil {
		t.Fatalf("建 group 仓库：%v", err)
	}
	got, err = repos.GetByName("maven-group")
	if err != nil {
		t.Fatalf("取 group 仓库：%v", err)
	}
	decoded, err = got.DecodeConfig()
	if err != nil {
		t.Fatalf("解码 group 配置：%v", err)
	}
	if !reflect.DeepEqual(decoded.Members, groupCfg.Members) {
		t.Errorf("members 往返失真：得 %v，期望 %v", decoded.Members, groupCfg.Members)
	}
}

// TestRepoDefaultConfigEmptyObject Create 传空 config 时应落库为空对象，解码为空配置。
func TestRepoDefaultConfigEmptyObject(t *testing.T) {
	repos := newRepoRepo(t)
	if _, err := repos.Create("raw-hosted", "raw", "hosted", "private", ""); err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	got, err := repos.GetByName("raw-hosted")
	if err != nil {
		t.Fatalf("取仓库：%v", err)
	}
	if got.Config != "{}" {
		t.Errorf("空 config 应落库为 {}，得 %q", got.Config)
	}
	decoded, err := got.DecodeConfig()
	if err != nil {
		t.Fatalf("解码配置：%v", err)
	}
	if decoded.RemoteURL != "" || len(decoded.Members) != 0 {
		t.Errorf("空 config 应解码为空配置，得 %+v", decoded)
	}
}

// TestRepoUpdateConfig UpdateConfig 应覆盖写入结构化配置。
func TestRepoUpdateConfig(t *testing.T) {
	repos := newRepoRepo(t)
	if _, err := repos.Create("raw-proxy", "raw", "proxy", "private", ""); err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	cfg := repository.RepositoryConfig{RemoteURL: "https://example.org/raw"}
	cfgJSON, err := repository.EncodeRepositoryConfig(cfg)
	if err != nil {
		t.Fatalf("编码配置：%v", err)
	}
	if err := repos.UpdateConfig("raw-proxy", cfgJSON); err != nil {
		t.Fatalf("更新配置：%v", err)
	}
	got, err := repos.GetByName("raw-proxy")
	if err != nil {
		t.Fatalf("取仓库：%v", err)
	}
	decoded, err := got.DecodeConfig()
	if err != nil {
		t.Fatalf("解码配置：%v", err)
	}
	if decoded.RemoteURL != cfg.RemoteURL {
		t.Errorf("UpdateConfig 未生效：得 %q，期望 %q", decoded.RemoteURL, cfg.RemoteURL)
	}
}

// TestRepoOnlinePersistence 新建仓库默认在线（online=true）；SetOnline 置离线后
// 持久化读回一致（FR-113）。
func TestRepoOnlinePersistence(t *testing.T) {
	repos := newRepoRepo(t)
	if _, err := repos.Create("raw-proxy", "raw", "proxy", "private", ""); err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	got, err := repos.GetByName("raw-proxy")
	if err != nil {
		t.Fatalf("取仓库：%v", err)
	}
	if !got.Online {
		t.Errorf("新建仓库应默认 online=true，得 %v", got.Online)
	}

	// 置离线 → 读回 false。
	if err := repos.SetOnline("raw-proxy", false); err != nil {
		t.Fatalf("SetOnline(false)：%v", err)
	}
	got, err = repos.GetByName("raw-proxy")
	if err != nil {
		t.Fatalf("取仓库：%v", err)
	}
	if got.Online {
		t.Errorf("SetOnline(false) 后应 online=false，得 %v", got.Online)
	}

	// 置回在线 → 读回 true。
	if err := repos.SetOnline("raw-proxy", true); err != nil {
		t.Fatalf("SetOnline(true)：%v", err)
	}
	got, err = repos.GetByName("raw-proxy")
	if err != nil {
		t.Fatalf("取仓库：%v", err)
	}
	if !got.Online {
		t.Errorf("SetOnline(true) 后应 online=true，得 %v", got.Online)
	}

	// List 也带出 online 字段。
	items, err := repos.List(10, 0)
	if err != nil {
		t.Fatalf("List：%v", err)
	}
	if len(items) != 1 || !items[0].Online {
		t.Errorf("List 应带出 online=true，得 %+v", items)
	}
}

// TestRepoSetOnlineNotFound 对不存在的仓库 SetOnline 应返回 ErrNotFound。
func TestRepoSetOnlineNotFound(t *testing.T) {
	repos := newRepoRepo(t)
	err := repos.SetOnline("ghost", false)
	if !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("SetOnline 不存在的仓库应 ErrNotFound，得 %v", err)
	}
}
