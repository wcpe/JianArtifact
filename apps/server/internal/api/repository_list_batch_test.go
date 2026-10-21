package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// 本文件钉住仓库列表端点（GET /api/v1/repositories）的逐仓鉴权批量化：
//
//  1. **可见集合必须与逐仓库判定完全一致**——公开仓库、用户直授、组授权、无权限四种情况
//     逐一对照，另用仍然保留在领域层的逐仓判定做对照物；
//  2. **语句数不随仓库数增长**——直接在驱动层数语句，仓库数翻倍时请求发出的语句数必须不变。
//     只断言结果正确的测试发现不了「改完还是 N 次查询」。
//
// 匿名与管理员两条路径也一并锁住：前者受全局开关约束，后者本就不做过滤。

// repoListBatchEnv 是带语句计数的仓库列表测试环境。
type repoListBatchEnv struct {
	handlers *Handlers
	repoSvc  *domain.RepositoryService
	repoRepo *repository.RepoRepo
	acls     *repository.AclRepo
	users    *repository.UserRepo
	groups   *repository.UserGroupRepo
	settings *domain.SettingService
	counter  *int64
}

// newRepoListBatchEnv 装配一套「计数数据库 + 已接线仓库服务」的环境。
func newRepoListBatchEnv(t *testing.T) *repoListBatchEnv {
	t.Helper()
	gin.SetMode(gin.ReleaseMode)
	db, counter, err := persistence.OpenCountingForTest(filepath.Join(t.TempDir(), "api-list-batch.db"))
	if err != nil {
		t.Fatalf("打开计数数据库：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("迁移：%v", err)
	}
	repoRepo := repository.NewRepoRepo(db)
	acls := repository.NewAclRepo(db)
	users := repository.NewUserRepo(db)
	groups := repository.NewUserGroupRepo(db)
	settings := domain.NewSettingService(repository.NewSettingRepo(db))
	// 注入组仓储，使「组被授 read」这条分支真的被走到（未接线时组分支会被静默跳过）。
	repoSvc := domain.NewRepositoryService(repoRepo, acls, repository.NewAssetRepo(db), settings, users)
	repoSvc.SetUserGroupRepo(groups)
	return &repoListBatchEnv{
		handlers: NewHandlers(Deps{Repos: repoSvc, Settings: settings}),
		repoSvc:  repoSvc,
		repoRepo: repoRepo,
		acls:     acls,
		users:    users,
		groups:   groups,
		settings: settings,
		counter:  counter,
	}
}

// seedRepo 建一个 raw hosted 仓库并返回其快照。
func (e *repoListBatchEnv) seedRepo(t *testing.T, name, visibility string) *repository.Repository {
	t.Helper()
	repo, err := e.repoSvc.Create(name, "raw", "hosted", visibility, "", repository.RepositoryConfig{})
	if err != nil {
		t.Fatalf("建仓库 %s：%v", name, err)
	}
	return repo
}

// seedUser 建一个用户并返回其 ID。
func (e *repoListBatchEnv) seedUser(t *testing.T, name, role string) int64 {
	t.Helper()
	id, err := e.users.Create(name, "hash", role)
	if err != nil {
		t.Fatalf("建用户 %s：%v", name, err)
	}
	return id
}

// countStatements 把计数器归零后执行 fn，返回 fn 期间发出的语句数。
func (e *repoListBatchEnv) countStatements(fn func()) int64 {
	atomic.StoreInt64(e.counter, 0)
	fn()
	return atomic.LoadInt64(e.counter)
}

// sessionPrincipal 构造一个会话登录主体。
func sessionPrincipal(userID int64, role string) *auth.Principal {
	return &auth.Principal{UserID: userID, Role: role, Kind: auth.KindSession}
}

// getRepoList 发起一次仓库列表请求，返回响应。
func getRepoList(e *repoListBatchEnv, principal *auth.Principal, path string) *httptest.ResponseRecorder {
	router := gin.New()
	router.Use(func(c *gin.Context) {
		if principal != nil {
			c.Set("auth.principal", principal)
		}
	})
	siw := ServerInterfaceWrapper{Handler: e.handlers}
	router.GET("/api/v1/repositories", siw.ListRepositories)

	req := httptest.NewRequest(http.MethodGet, path, strings.NewReader(""))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// decodeRepoList 解析响应体为仓库列表（响应结构必须保持不变，故直接按契约类型解析）。
func decodeRepoList(t *testing.T, rec *httptest.ResponseRecorder) RepositoryList {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("仓库列表应 200，得 %d：%s", rec.Code, rec.Body.String())
	}
	var out RepositoryList
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析仓库列表失败：%v：%s", err, rec.Body.String())
	}
	return out
}

// repoListNames 取响应中的仓库名（排序后便于集合比较）。
func repoListNames(out RepositoryList) []string {
	names := make([]string, 0, len(out.Items))
	for _, item := range out.Items {
		names = append(names, item.Name)
	}
	sort.Strings(names)
	return names
}

// perRepoReadableNames 用领域层仍保留的逐仓库判定（改造前的实现方式）算出期望可读集合，
// 作为批量判定的对照物：只要某天两条路径分叉，这里就会先炸。
func (e *repoListBatchEnv) perRepoReadableNames(t *testing.T, subject repository.Subject) []string {
	t.Helper()
	rows, err := e.repoRepo.List(1000, 0)
	if err != nil {
		t.Fatalf("列举仓库：%v", err)
	}
	names := make([]string, 0, len(rows))
	for i := range rows {
		ok, err := e.repoSvc.CanAccessResolved(&rows[i], subject, repository.ActionRead)
		if err != nil {
			t.Fatalf("逐仓库判定 %s：%v", rows[i].Name, err)
		}
		if ok {
			names = append(names, rows[i].Name)
		}
	}
	sort.Strings(names)
	return names
}

// equalNameSets 比较两个有序名称切片。
func equalNameSets(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// setupVisibilityTopology 铺设四类可见性来源：
//
//	vis-public  公开仓库（任何人可读）
//	vis-direct  private，直接授 member read
//	vis-group   private，仅授 dev 组 read
//	vis-denied  private，无任何 ACL（任何人都不该看到）
func (e *repoListBatchEnv) setupVisibilityTopology(t *testing.T) (memberID, strangerID int64) {
	t.Helper()
	memberID = e.seedUser(t, "list-member", "user")
	strangerID = e.seedUser(t, "list-stranger", "user")
	devGroup, err := e.groups.Create("list-dev", "可读组")
	if err != nil {
		t.Fatalf("建组：%v", err)
	}
	if err := e.groups.AddMember(devGroup, memberID); err != nil {
		t.Fatalf("加入组成员：%v", err)
	}

	publicRepo := e.seedRepo(t, "vis-public", "public")
	directRepo := e.seedRepo(t, "vis-direct", "private")
	groupRepo := e.seedRepo(t, "vis-group", "private")
	e.seedRepo(t, "vis-denied", "private")
	_ = publicRepo

	if err := e.acls.Replace(directRepo.ID, []repository.Acl{{SubjectID: memberID, Action: repository.ActionRead}}); err != nil {
		t.Fatalf("写直授 ACL：%v", err)
	}
	if err := e.acls.Replace(groupRepo.ID, []repository.Acl{{
		SubjectType: repository.SubjectTypeGroup, SubjectGroupID: devGroup, Action: repository.ActionRead}}); err != nil {
		t.Fatalf("写组授 ACL：%v", err)
	}
	return memberID, strangerID
}

// TestRepositoryListVisibilityMatchesPerRepoAuthorization 是非管理员路径的语义对照：
// 公开仓库 + 直授 + 组授三条通路都要命中，无权限的私有仓库不得出现，
// 且结果与逐仓库判定（改造前的实现）逐条一致。
func TestRepositoryListVisibilityMatchesPerRepoAuthorization(t *testing.T) {
	env := newRepoListBatchEnv(t)
	memberID, strangerID := env.setupVisibilityTopology(t)

	cases := []struct {
		name    string
		userID  int64
		want    []string
		comment string
	}{
		{
			name:    "普通用户（含组授权）",
			userID:  memberID,
			want:    []string{"vis-direct", "vis-group", "vis-public"},
			comment: "公开 / 直授 read / 组授 read 三条通路都应命中，无权限的私有仓库不得出现",
		},
		{
			name:    "普通用户（无任何授权）",
			userID:  strangerID,
			want:    []string{"vis-public"},
			comment: "只有公开仓库可见",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := getRepoList(env, sessionPrincipal(tc.userID, "user"), "/api/v1/repositories?page_size=100")
			out := decodeRepoList(t, rec)
			got := repoListNames(out)
			if !equalNameSets(got, tc.want) {
				t.Fatalf("%s：\n期望 = %v\n实际 = %v", tc.comment, tc.want, got)
			}
			if out.Total != len(tc.want) {
				t.Fatalf("total 应为可见仓库总数 %d，得 %d", len(tc.want), out.Total)
			}
			// 与逐仓库判定对照：改造前后必须是同一集合。
			subject, err := env.repoSvc.SubjectFor(tc.userID)
			if err != nil {
				t.Fatalf("展开主体：%v", err)
			}
			if want := env.perRepoReadableNames(t, subject); !equalNameSets(got, want) {
				t.Fatalf("批量判定与逐仓库判定不一致：\n批量   = %v\n逐仓库 = %v", got, want)
			}
		})
	}
}

// TestRepositoryListStatementCountIndependentOfRepoCount 是本次改动的核心断言：
// 仓库数从 N 增到 2N 时，请求发出的语句数必须不变（三类主体各验一次）。
// 同时把逐仓库判定的语句数也量出来，证明它确实随仓库数线性增长——即这里消掉的正是 N+1。
func TestRepositoryListStatementCountIndependentOfRepoCount(t *testing.T) {
	const small, large = 8, 16 // 都远小于单批 100，避免分批次数成为变量

	type principalCase struct {
		name  string
		build func(env *repoListBatchEnv, t *testing.T) *auth.Principal
		// maxStatements 是常数上限：改动若退化成逐仓查询，16 个仓库就会发出 16+ 条。
		maxStatements int64
	}
	cases := []principalCase{
		{
			name: "非管理员（含组授权）",
			build: func(env *repoListBatchEnv, t *testing.T) *auth.Principal {
				userID := env.seedUser(t, "count-member", "user")
				groupID, err := env.groups.Create("count-dev", "计数组")
				if err != nil {
					t.Fatalf("建组：%v", err)
				}
				if err := env.groups.AddMember(groupID, userID); err != nil {
					t.Fatalf("加入组成员：%v", err)
				}
				return sessionPrincipal(userID, "user")
			},
			maxStatements: 12,
		},
		{
			name: "匿名",
			build: func(*repoListBatchEnv, *testing.T) *auth.Principal {
				return nil
			},
			maxStatements: 12,
		},
		{
			name: "管理员",
			build: func(env *repoListBatchEnv, t *testing.T) *auth.Principal {
				return sessionPrincipal(env.seedUser(t, "count-admin", "admin"), "admin")
			},
			maxStatements: 12,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			measure := func(repoCount int) (int64, int64) {
				env := newRepoListBatchEnv(t)
				for i := 0; i < repoCount; i++ {
					visibility := "private"
					if i%2 == 0 {
						visibility = "public"
					}
					env.seedRepo(t, fmt.Sprintf("count-repo-%03d", i), visibility)
				}
				principal := tc.build(env, t)
				var code int
				n := env.countStatements(func() {
					code = getRepoList(env, principal, "/api/v1/repositories?page_size=100").Code
				})
				if code != http.StatusOK {
					t.Fatalf("仓库列表应 200，得 %d", code)
				}
				// 逐仓库判定（改造前的写法）在同一批行上的语句数，用于证明 N+1 真实存在。
				rows, err := env.repoRepo.List(1000, 0)
				if err != nil {
					t.Fatalf("列举仓库：%v", err)
				}
				subject := repository.AnonymousSubject()
				if principal != nil {
					if subject, err = env.repoSvc.SubjectFor(principal.UserID); err != nil {
						t.Fatalf("展开主体：%v", err)
					}
				}
				legacy := env.countStatements(func() {
					for i := range rows {
						if _, err := env.repoSvc.CanAccessResolved(&rows[i], subject, repository.ActionRead); err != nil {
							t.Fatalf("逐仓库判定：%v", err)
						}
					}
				})
				return n, legacy
			}

			smallN, smallLegacy := measure(small)
			largeN, largeLegacy := measure(large)
			t.Logf("%d 个仓库：请求语句数=%d，逐仓判定语句数=%d", small, smallN, smallLegacy)
			t.Logf("%d 个仓库：请求语句数=%d，逐仓判定语句数=%d", large, largeN, largeLegacy)

			if smallN != largeN {
				t.Fatalf("仓库数翻倍后请求语句数应保持不变：%d 个仓库=%d 条，%d 个仓库=%d 条",
					small, smallN, large, largeN)
			}
			if smallN > tc.maxStatements {
				t.Fatalf("请求语句数应为常数且不超过 %d 条，%d 个仓库实测 %d 条",
					tc.maxStatements, small, smallN)
			}
			if largeLegacy <= smallLegacy {
				t.Fatalf("逐仓库判定的语句数应随仓库数增长（这正是被消掉的 N+1）：%d 个仓库=%d 条，%d 个仓库=%d 条",
					small, smallLegacy, large, largeLegacy)
			}
		})
	}
}

// TestRepositoryListAnonymousPathUnchanged 锁住匿名路径：开关关闭时 401（行为不变），
// 开关开启时只返回公开仓库，且与逐仓库匿名判定一致。
func TestRepositoryListAnonymousPathUnchanged(t *testing.T) {
	env := newRepoListBatchEnv(t)
	env.setupVisibilityTopology(t)

	rec := getRepoList(env, nil, "/api/v1/repositories?page_size=100")
	out := decodeRepoList(t, rec)
	if got, want := repoListNames(out), []string{"vis-public"}; !equalNameSets(got, want) {
		t.Fatalf("匿名应只看到公开仓库：\n期望 = %v\n实际 = %v", want, got)
	}
	if want := env.perRepoReadableNames(t, repository.AnonymousSubject()); !equalNameSets(repoListNames(out), want) {
		t.Fatalf("匿名批量判定与逐仓库判定不一致：\n批量 = %v\n逐仓库 = %v", repoListNames(out), want)
	}

	if err := env.settings.SetAnonymousAccessEnabled(false); err != nil {
		t.Fatalf("关闭匿名访问：%v", err)
	}
	rec = getRepoList(env, nil, "/api/v1/repositories?page_size=100")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("匿名访问关闭后应 401，得 %d：%s", rec.Code, rec.Body.String())
	}
}

// TestRepositoryListAdminPathUnaffected 锁住管理员路径：不做可见性过滤，
// 私有仓库（含无任何授权的）一并返回，total 等于全量。
func TestRepositoryListAdminPathUnaffected(t *testing.T) {
	env := newRepoListBatchEnv(t)
	adminID := env.seedUser(t, "list-admin", "admin")
	env.setupVisibilityTopology(t)

	rec := getRepoList(env, sessionPrincipal(adminID, "admin"), "/api/v1/repositories?page_size=100")
	out := decodeRepoList(t, rec)
	want := []string{"vis-denied", "vis-direct", "vis-group", "vis-public"}
	if got := repoListNames(out); !equalNameSets(got, want) {
		t.Fatalf("管理员应看到全部仓库：\n期望 = %v\n实际 = %v", want, got)
	}
	if out.Total != len(want) {
		t.Fatalf("管理员 total 应为全量 %d，得 %d", len(want), out.Total)
	}
	// 连接状态为管理面信息（FR-114）：管理员响应保留该字段（hosted 仓库为 nil，此处只验证
	// 端点未因批量化改动丢失既有字段处理，故只断言解析成功且条目完整）。
	for _, item := range out.Items {
		if item.Id == 0 || item.Visibility == "" {
			t.Fatalf("管理员条目字段缺失：%+v", item)
		}
	}
}
