package main

import (
	"fmt"

	"github.com/wcpe/jianartifact/apps/server/internal/api"
	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/blobstore"
	"github.com/wcpe/jianartifact/apps/server/internal/config"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/migration/offindex"
	"github.com/wcpe/jianartifact/apps/server/internal/migration/runner"
	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
	"github.com/wcpe/jianartifact/apps/server/internal/upstream"
)

// appServices 汇集装配后的持久化连接、领域服务与鉴权依赖，供 run 及 CLI 子命令复用。
type appServices struct {
	db           *persistence.DB
	users        *repository.UserRepo
	authSvc      *domain.AuthService
	userSvc      *domain.UserService
	tokenSvc     *domain.TokenService
	repoSvc      *domain.RepositoryService
	assetSvc     *domain.AssetService
	migrationSvc *domain.MigrationService
	store        auth.Store
	jwt          *auth.JWTManager
}

// openServices 打开数据库、执行迁移并装配领域服务。调用方负责在返回的 db 上 Close。
func openServices(cfg *config.Config) (*appServices, error) {
	db, err := persistence.Open(cfg.DBPath)
	if err != nil {
		return nil, err
	}
	if err := db.Migrate(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("应用数据库迁移：%w", err)
	}

	userRepo := repository.NewUserRepo(db)
	tokenRepo := repository.NewTokenRepo(db)
	revokedRepo := repository.NewRevokedRepo(db)
	repoRepo := repository.NewRepoRepo(db)
	aclRepo := repository.NewAclRepo(db)
	assetRepo := repository.NewAssetRepo(db)
	migrationTaskRepo := repository.NewMigrationTaskRepo(db)

	jwtMgr := auth.NewJWTManager(cfg.JWTSecret)
	blobs := blobstore.NewStore(cfg.BlobDir)
	upstreamClient := upstream.NewClient(cfg.UpstreamTimeout)
	repoSvc := domain.NewRepositoryService(repoRepo, aclRepo, assetRepo)
	assetSvc := domain.NewAssetService(repoRepo, assetRepo, blobs, upstreamClient)

	offlineIndexRepo := repository.NewOfflineIndexRepo(db)
	offlineScanner := offindex.New(offlineIndexRepo)

	migRunner := runner.New(
		runner.TaskStoreAdapter{Repo: migrationTaskRepo},
		runner.AssetServiceAdapter{Assets: assetSvc, Repos: repoRepo, AssetR: assetRepo},
		runner.RepoAdminAdapter{Repos: repoSvc},
	)
	migRunner.SetOfflineIndex(offlineIndexRepo)
	migrationSvc := domain.NewMigrationService(migrationTaskRepo, migRunner)
	migrationSvc.SetOfflineIndex(offlineIndexRepo, offlineScanner)

	// 进程崩溃回收：残留 running → failed，等人 resume（ADR-0012）。
	if _, err := migrationSvc.FailInterruptedRunning(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("回收中断的迁移任务：%w", err)
	}

	return &appServices{
		db:           db,
		users:        userRepo,
		authSvc:      domain.NewAuthService(userRepo, revokedRepo, jwtMgr),
		userSvc:      domain.NewUserService(userRepo),
		tokenSvc:     domain.NewTokenService(tokenRepo),
		repoSvc:      repoSvc,
		assetSvc:     assetSvc,
		migrationSvc: migrationSvc,
		store:        domain.NewAuthStore(userRepo, tokenRepo, revokedRepo),
		jwt:          jwtMgr,
	}, nil
}

// handlers 用给定版本与就绪检查构造 api.Handlers。
func (s *appServices) handlers(version string, checks []func() error) *api.Handlers {
	return api.NewHandlers(api.Deps{
		Version:    version,
		Checks:     checks,
		Migration:  s.db.CurrentVersion,
		Auth:       s.authSvc,
		Users:      s.userSvc,
		Tokens:     s.tokenSvc,
		Repos:      s.repoSvc,
		Migrations: s.migrationSvc,
	})
}
