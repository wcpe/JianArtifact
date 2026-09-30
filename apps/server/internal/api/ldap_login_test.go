package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// TestLoginWithLDAPFallback 登录的目录回退语义（FR-35）：
// 本地优先；本地未通过且启用目录时再试目录；目录身份同样经绑定/建号与状态校验。
func TestLoginWithLDAPFallback(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := persistence.Open(filepath.Join(t.TempDir(), "api-ldap.db"))
	if err != nil {
		t.Fatalf("打开数据库：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("迁移数据库：%v", err)
	}
	users := repository.NewUserRepo(db)
	authSvc := domain.NewAuthService(users, repository.NewRevokedRepo(db), auth.NewJWTManager([]byte(oidcTestSecret)))

	directoryCalls := 0
	directoryResult := func(_ context.Context, user string, password string) (auth.ExternalUser, error) {
		directoryCalls++
		if user == "alice" && password == "directory-pw" {
			return auth.ExternalUser{
				Subject:  "uid=alice,ou=people,dc=example,dc=com",
				Username: "alice",
				Email:    "alice@example.com",
			}, nil
		}
		return auth.ExternalUser{}, auth.ErrLDAPAuthFailed
	}
	h := NewHandlers(Deps{
		Auth:     authSvc,
		Users:    domain.NewUserService(users),
		LDAPAuth: directoryResult,
	})

	login := func(user, password string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		body, _ := json.Marshal(map[string]string{"username": user, "password": password})
		c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(string(body)))
		c.Request.Header.Set("Content-Type", "application/json")
		h.Login(c)
		return rec
	}

	// 1) 目录侧成功：本地无此账号 → 回退目录并建号。
	rec := login("alice", "directory-pw")
	if rec.Code != http.StatusOK {
		t.Fatalf("目录登录应成功，实际 %d：%s", rec.Code, rec.Body.String())
	}
	if directoryCalls != 1 {
		t.Fatalf("应恰好回退目录一次，实际 %d", directoryCalls)
	}
	alice, err := users.GetByUsername("alice")
	if err != nil {
		t.Fatalf("目录身份应建号：%v", err)
	}
	if alice.AuthSource != "ldap" || alice.Role != "user" || alice.PasswordHash != "" {
		t.Fatalf("建号口径不符：source=%q role=%q 无本地口令=%v", alice.AuthSource, alice.Role, alice.PasswordHash == "")
	}

	// 2) 本地优先：本地口令正确时不得触碰目录。
	localID, err := users.Create("bob", "", "user")
	if err != nil {
		t.Fatalf("预建本地用户：%v", err)
	}
	localHash, err := auth.HashPassword("local-pw")
	if err != nil {
		t.Fatalf("生成口令哈希：%v", err)
	}
	if err := users.UpdatePassword(localID, localHash); err != nil {
		t.Fatalf("写入本地口令：%v", err)
	}
	before := directoryCalls
	if rec := login("bob", "local-pw"); rec.Code != http.StatusOK {
		t.Fatalf("本地登录应成功，实际 %d", rec.Code)
	}
	if directoryCalls != before {
		t.Fatal("本地口令通过时不得回退目录")
	}

	// 3) 两边都不通过：401，且对外表现与纯本地失败一致。
	if rec := login("carol", "whatever"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("两侧皆失败应 401，实际 %d", rec.Code)
	}
}
