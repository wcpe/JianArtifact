// Package main 是 JianArtifact 后端的入口。
//
// 0.1.0 工程基座：加载监听地址、装配 HTTP 服务（契约路由 + 健康 / 就绪端点 +
// 内嵌前端），以优雅关停运行。协议端点、管理 API、SQLite 元数据与 blob 存储、
// Nexus 迁移随 M1 后续版本（0.2.0+）迭代落地。
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/api"
	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/config"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/formats"
	"github.com/wcpe/jianartifact/apps/server/internal/httpserver"
	"github.com/wcpe/jianartifact/apps/server/internal/protocol"
	"github.com/wcpe/jianartifact/apps/server/web"
)

// version 由构建时注入（-ldflags "-X main.version=..."）；默认与 VERSION 文件一致。
var version = "0.8.1-dev"

func main() {
	// 子命令分发器：run/serve（启动 HTTP 服务）/ status（在线探测或离线静态信息）/
	// admin reset（离线重置管理员）/ healthcheck（容器自探活）/ help（打印用法）。
	// 无子命令或未知命令均打印用法，避免误触发启动服务。
	args := os.Args[1:]
	if len(args) == 0 {
		usage(os.Stdout)
		return
	}
	switch args[0] {
	case "run", "serve":
		if err := run(); err != nil {
			fmt.Fprintln(os.Stderr, "启动失败：", err)
			os.Exit(1)
		}
	case "status":
		if err := statusCmd(); err != nil {
			fmt.Fprintln(os.Stderr, "status 子命令失败：", err)
			os.Exit(1)
		}
	case "admin":
		if err := adminCmd(args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, "admin 子命令失败：", err)
			os.Exit(1)
		}
	case "backup":
		if err := backupCmd(args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, "backup 子命令失败：", err)
			os.Exit(1)
		}
	case "healthcheck":
		if err := healthcheck(); err != nil {
			fmt.Fprintln(os.Stderr, "探活失败：", err)
			os.Exit(1)
		}
	case "help", "-h", "--help":
		usage(os.Stdout)
	default:
		fmt.Fprintf(os.Stderr, "未知命令：%s\n\n", args[0])
		usage(os.Stderr)
		os.Exit(2)
	}
}

// usage 打印子命令清单与用法说明。
func usage(w io.Writer) {
	_, _ = fmt.Fprintf(w, `JianArtifact %s — 制品仓库服务

用法：
  jianartifact <命令> [参数]

命令：
  run                启动 HTTP 服务（监听 JIAN_HTTP_ADDR，默认 :8080）
  status             打印运行时状态（服务在跑）或离线静态信息
  admin reset        重置 / 创建管理员账号与口令（离线直连 SQLite）
                     [--username <名>] [--password <口令>]
  admin backfill-checksums
                     回填历史资产缺失的 sha1/md5（从 blob 流式计算并写库）
                     [--batch <N>] [--all]
  admin backfill-times
                     从源 Nexus 拉取资产时间（blobCreated/lastModified）回填
                     created_at/updated_at，与源完全对齐
                     --url <源基址> [--repos <a,b>] [--cred <user:pass>] [--batch <N>]
  admin emit-asset-times
                     为全部资产重新登记带创建/更新时间的资产变更记录
  backup create      生成节点备份包（SQLite 一致性快照 + 内容寻址 blob）
                     [--mode hot|frozen] [--label <备注>] [--base <基线包标识>]
                     frozen 假定本地写入已停止；运行中请改用 Web 的冻结窗口
                     --base 以该基线包生成增量差包（只含新增 blob 与新 db）
  backup list        列出本机备份包 [--json]
  backup verify <包标识|归档路径>
                     校验备份包完整性 [--deep]（deep 逐 blob 比对内容摘要）
  backup link <包标识>
                     签发带时效的下载链接，供新机器直接拉取
                     [--ttl 30m] [--base https://对外地址]
  backup delete <包标识>
                     删除备份包（被增量包引用的基线不可删）
  backup import <归档路径>
                     导入节点备份包（校验 → 暂存 → 重启替换，循环闭合搬迁）
                     [--overwrite] [--deep] [--yes]
                     目标非空需 --overwrite；--deep 逐 blob 比对内容摘要；
                     交互终端下目标非空会二次确认，--yes 跳过；无 TTY 时
                     --overwrite 即视为已确认。导入后需重启服务方可生效。
  healthcheck        对本地 /readyz 探活，供容器健康检查
  help               显示本帮助

环境变量：
  JIAN_HTTP_ADDR     HTTP 监听地址:端口（默认 :8080）
  JIAN_DATA_DIR      数据根目录（默认 ./data；存放 SQLite 与 blob）
  JIAN_JWT_SECRET    JWT(HS256) 签名密钥（缺省时生成并持久化到数据目录）
  JIAN_PUBLIC_URL    对外基础 URL（下载链接与 usage 片段使用）
  JIAN_ENABLED_FORMATS
                     启用的协议格式，逗号分隔（缺省 raw,maven,npm）
  JIAN_UPSTREAM_TIMEOUT
                     proxy 回源整体超时（秒，默认 30）
  JIAN_BLOB_GC_INTERVAL
                     遗留 blob 清理间隔（秒，默认 86400；0 禁用）
  JIAN_MIGRATION_CREDENTIAL_KEY
                     在线迁移凭据 AES-256-GCM 密钥（Base64 32 字节）
  JIAN_TLS_ADDR      HTTPS 监听地址（为空则不启用 HTTPS）
  JIAN_TLS_CERT      TLS 证书 PEM 路径（配置 JIAN_TLS_ADDR 时必填）
  JIAN_TLS_KEY       TLS 私钥 PEM 路径（配置 JIAN_TLS_ADDR 时必填）

示例：
  jianartifact run
  jianartifact admin reset --username admin
  jianartifact admin backfill-checksums --all
  jianartifact status
`, version)
}

// healthcheck 对本进程监听地址发起 GET /readyz，返回 200 视为就绪（退出 0），否则报错。
func healthcheck() error {
	_, port, err := net.SplitHostPort(listenAddr())
	if err != nil || port == "" {
		port = "8080"
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%s/readyz", port))
	if err != nil {
		return err
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("/readyz 返回 %d", resp.StatusCode)
	}
	return nil
}

// listenAddr 返回 HTTP 监听地址（环境变量 JIAN_HTTP_ADDR，默认 :8080）。
func listenAddr() string {
	if addr := os.Getenv("JIAN_HTTP_ADDR"); addr != "" {
		return addr
	}
	return ":8080"
}

// blobWritableCheck 返回一个就绪自检：探测 blob 目录可写（写入并删除探针文件）。
func blobWritableCheck(blobDir string) httpserver.ReadinessCheck {
	return func() error {
		probe := filepath.Join(blobDir, ".readyz-probe")
		if err := os.WriteFile(probe, []byte("ok"), 0o600); err != nil {
			return fmt.Errorf("blob 目录不可写：%w", err)
		}
		_ = os.Remove(probe)
		return nil
	}
}

// newApplicationHandler 按启动期格式集合构造完整 HTTP 服务。
// 未启用格式不构造其专属 handler，也不注册其原生协议路由。
func newApplicationHandler(cfg *config.Config, svc *appServices, assets fs.FS) http.Handler {
	checks := []func() error{svc.db.Ping, blobWritableCheck(cfg.BlobDir)}
	// 允许访问的域名白名单（后台设置，节点本地；空列表不限制）。
	allowedHosts := func() []string { return svc.settingSvc.AllowedHostsList() }
	// FR-130：回源 Token 校验（后台「设置-安全防护」；节点本地，运行时生效）。
	originTokenGuard := func() (bool, string, string) { return svc.settingSvc.OriginTokenGuard() }
	authenticator := auth.NewAuthenticator(svc.jwt, svc.store, auth.WithAllowedHosts(allowedHosts))
	apiHandlers := svc.handlers(version, checks)

	// 各格式协议共享基础资产处理器；空格式列表时不创建任何协议处理对象。
	var rawHandler *protocol.RawHandler
	if cfg.EnabledFormats.Any() {
		rawHandler = protocol.NewRawHandler(svc.assetSvc, svc.repoSvc)
		rawHandler.SetPublishPolicy(svc.publishPolicySvc)
		rawHandler.SetAudit(apiHandlers.AuditLog)
		rawHandler.SetOperationAudit(apiHandlers.ProtocolAssetOperationAudit)
	}

	var dispatcher *protocol.Dispatcher
	var mavenHandler *protocol.MavenHandler
	if cfg.EnabledFormats.Has("raw") || cfg.EnabledFormats.Has("maven") {
		if cfg.EnabledFormats.Has("maven") {
			mavenHandler = protocol.NewMavenHandler(rawHandler)
		}
		dispatcher = protocol.NewDispatcher(svc.repoSvc, rawHandler, mavenHandler, cfg.EnabledFormats)
	}

	var npmHandler *protocol.NpmHandler
	if cfg.EnabledFormats.Has("npm") {
		npmHandler = protocol.NewNpmHandler(rawHandler, svc.store, svc.tokenSvc, svc.publicURL)
		npmHandler.SetPublicURLFn(func() string { return svc.settingSvc.PublicURL() })
		// npm 仓库经 /repository/ 通用路径时保持完整 npm 语义（发布 tarball、
		// 版本合并与 dist.tarball 重写），与 /npm/ 前缀一致。
		if dispatcher != nil {
			dispatcher.SetNpm(npmHandler)
		}
	}

	var ociHandler *protocol.OCIHandler
	if cfg.EnabledFormats.Has("docker") {
		ociHandler = protocol.NewOCIHandler(rawHandler, svc.ociSvc)
	}

	var cargoHandler *protocol.CargoHandler
	if cfg.EnabledFormats.Has("cargo") {
		cargoHandler = protocol.NewCargoHandler(rawHandler, svc.cargoSvc, cfg.PublicURL)
	}

	var pypiHandler *protocol.PypiHandler
	if cfg.EnabledFormats.Has("pypi") {
		pypiHandler = protocol.NewPypiHandler(rawHandler, svc.formatMetadataSvc, svc.publicURL)
	}

	var goProxyHandler *protocol.GoProxyHandler
	if cfg.EnabledFormats.Has("gomod") {
		goProxyHandler = protocol.NewGoProxyHandler(rawHandler)
	}

	var nugetHandler *protocol.NuGetHandler
	if cfg.EnabledFormats.Has("nuget") {
		nugetHandler = protocol.NewNuGetHandler(rawHandler, svc.formatMetadataSvc, svc.publicURL)
	}

	srv := httpserver.New(version,
		httpserver.WithReadinessCheck(svc.db.Ping),
		httpserver.WithReadinessCheck(blobWritableCheck(cfg.BlobDir)),
		httpserver.WithWriteFreeze(svc.freeze.State), // FR-135：运行时写入冻结窗口（未冻结时中间件为空操作）
		httpserver.WithHandlers(apiHandlers),
		httpserver.WithManagementSecurityAudit(apiHandlers.AuditLog),
		httpserver.WithAllowedHosts(allowedHosts),
		httpserver.WithOriginTokenGuard(originTokenGuard),
		httpserver.WithMiddleware(api.MiddlewareFunc(authenticator.Optional())),
		httpserver.WithProtocolMetric(func(c *gin.Context) {
			cacheResult, _ := c.Get("jianartifact.protocol.cache_result")
			svc.dashboardSvc.RecordProtocol(domain.ProtocolMetric{CompletedAt: time.Now().UTC(), Method: c.Request.Method, Status: c.Writer.Status(), CacheResult: cacheResultString(cacheResult)})
		}),
		httpserver.WithProtocolRoutes(func(r gin.IRouter) {
			protocolMW := authenticator.Protocol().Optional()
			cargoProtocolMW := authenticator.CargoProtocol().Optional()
			authMW := authenticator.Optional()
			if dispatcher != nil {
				protocol.RegisterRoutes(r, dispatcher, protocolMW)
			}
			if npmHandler != nil {
				protocol.RegisterNpmRoutes(r, npmHandler, protocolMW)
			}
			if ociHandler != nil {
				protocol.RegisterOCIRoutes(r, ociHandler, protocolMW)
			}
			if cargoHandler != nil {
				protocol.RegisterCargoRoutes(r, cargoHandler, cargoProtocolMW)
			}
			if pypiHandler != nil {
				protocol.RegisterPypiRoutes(r, pypiHandler, protocolMW)
			}
			if goProxyHandler != nil {
				protocol.RegisterGoProxyRoutes(r, goProxyHandler, protocolMW)
			}
			if nugetHandler != nil {
				protocol.RegisterNuGetRoutes(r, nugetHandler, protocolMW)
			}
			// 迁移辅助（主体由 Optional 注入，admin 在 handler 内校验）。
			r.POST("/api/v1/migrations/offline-index/scan", authMW, apiHandlers.StartOfflineDirIndex)
			r.GET("/api/v1/migrations/offline-index", authMW, apiHandlers.GetOfflineDirIndex)
			r.POST("/api/v1/migrations/offline-index/cancel", authMW, apiHandlers.CancelOfflineDirIndex)
			// 运维端点
			r.POST("/api/v1/repositories/:name/cleanup", authMW, apiHandlers.CleanupEmptyMavenArtifacts)
			// FR-73: Maven 网页上传仅在 Maven 启用时注册。
			if mavenHandler != nil {
				r.POST("/api/v1/repositories/:name/maven-upload", authMW, mavenHandler.UploadForm)
			}
			// FR-54: 目录懒加载 tree API
			r.GET("/api/v1/repositories/:name/tree", authMW, apiHandlers.ListRepositoryTree)
			// 公开接口（无需认证）
			r.GET("/api/v1/public/repositories", apiHandlers.ListPublicRepositories)
			// FR-30: 全局搜索
			r.GET("/api/v1/search", authMW, apiHandlers.SearchAssets)
			// FR-66: 匿名访问全局开关（admin，主体由 Optional 注入，handler 内校验）
			r.GET("/api/v1/settings/anonymous-access", authMW, apiHandlers.GetAnonymousAccessSetting)
			r.PUT("/api/v1/settings/anonymous-access", authMW, apiHandlers.PutAnonymousAccessSetting)
			// FR-89: 设置读写端点（admin，基础配置四项：匿名开关/对外 URL/回源超时/同步间隔）
			r.GET("/api/v1/settings", authMW, apiHandlers.GetSettings)
			r.PUT("/api/v1/settings", authMW, apiHandlers.PutSettings)
			// 开源协议清单（admin 专属；清单不再打进前端 bundle，见 internal/licenses）
			r.GET("/api/v1/licenses", authMW, apiHandlers.GetLicenses)
			// 复制退役：数据面 /api/v1/cluster/sync/* 与管理面 /api/v1/cluster* 均已摘除。
			// FR-38: 审计日志（分页 + 筛选：actor/action/repo/from/to，仅管理员）
			r.GET("/api/v1/audit-logs", authMW, apiHandlers.GetAuditLogs)
		}),
		httpserver.WithProtocolPrefixes(formats.AllPrefixes()...),
	)
	return srv.Handler(assets)
}

func cacheResultString(value any) string {
	result, _ := value.(string)
	if result == "hit" || result == "miss" {
		return result
	}
	return ""
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("加载配置：%w", err)
	}

	// FR-137：在打开数据库连接之前应用待生效的备份恢复（替换 db 文件时不能有打开的连接）。
	if applied, outcome, err := domain.ApplyPendingRestore(cfg.DataDir, cfg.DBPath, cfg.BlobDir); err != nil {
		return fmt.Errorf("应用待生效的备份恢复：%w", err)
	} else if applied {
		fmt.Fprintf(os.Stderr, "已应用待生效的备份恢复：%s\n", outcome.Summary())
	}

	svc, err := openServices(cfg)
	if err != nil {
		return err
	}
	defer func() { _ = svc.db.Close() }()

	assets, err := web.Assets()
	if err != nil {
		return fmt.Errorf("加载内嵌前端资源：%w", err)
	}

	httpServer := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           newApplicationHandler(cfg, svc, assets),
		ReadHeaderTimeout: 10 * time.Second,
	}
	addr := cfg.HTTPAddr

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 复制退役：出站同步调度器已随集群功能移除。
	// FR-132：收尾上次进程遗留的"生成中"备份包，避免列表里留下永久幻影。
	if n, err := svc.backupSvc.ReconcileStartup(); err == nil && n > 0 {
		fmt.Fprintf(os.Stderr, "备份：标记 %d 个服务重启遗留的生成中包为失败\n", n)
	}
	// FR-137：清理崩在导入中途留下的暂存目录（有 restore.pending 时不清理，
	// 那是等待本次启动替换数据库的正品，由 ApplyPendingRestore 处理）。
	if err := svc.restoreSvc.ReconcileStaleStaging(); err != nil {
		fmt.Fprintf(os.Stderr, "导入：清理残留暂存目录失败：%v\n", err)
	}
	// FR-137：清理到期未完成的分片上传会话（磁盘 + 记录）；失败仅记日志，不影响启动。
	if n, err := svc.backupUploads.ReconcileExpired(time.Now()); err != nil {
		fmt.Fprintf(os.Stderr, "分片上传：清理过期会话失败：%v\n", err)
	} else if n > 0 {
		fmt.Fprintf(os.Stderr, "分片上传：清理 %d 个过期上传会话\n", n)
	}
	// FR-53/120：分钟聚合与当前主机采样只由运行期定时任务驱动，启动不扫描历史数据。
	svc.dashboardSvc.Start(ctx, time.Now)
	svc.hostMonitoringSvc.Start(ctx, time.Now)
	startBlobGCTask(ctx, cfg.BlobGCInterval, svc.assetSvc.CleanupUnreferencedBlobs)

	errCh := make(chan error, 1)
	go func() {
		fmt.Printf("JianArtifact %s 正在监听 %s\n", version, addr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()
	// FR-131：服务内置 TLS 双监听（与 HTTP 共用同一 handler），供 CDN 回源 HTTPS。
	// 显式配置了 TLS 地址但证书缺失 / 格式错误 → 启动失败并明确报错，
	// 防止"以为加密了实际没有"的静默降级；未配置 TLS 地址时保持纯 HTTP。
	if cfg.TLSAddr != "" {
		tlsServer, err := buildTLSServer(cfg, newApplicationHandler(cfg, svc, assets))
		if err != nil {
			return fmt.Errorf("配置了 %s 但 TLS 启动失败：%w", config.EnvTLSAddr, err)
		}
		go func() {
			fmt.Printf("JianArtifact %s TLS 正在监听 %s\n", version, cfg.TLSAddr)
			if err := tlsServer.ListenAndServeTLS(cfg.TLSCert, cfg.TLSKey); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- err
			}
		}()
	}

	select {
	case err := <-errCh:
		return fmt.Errorf("HTTP 服务异常：%w", err)
	case <-ctx.Done():
		fmt.Println("收到关停信号，正在优雅关停……")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("优雅关停失败：%w", err)
		}
		fmt.Println("已关停。")
		return nil
	}
}

// startBlobGCTask 仅在 primary 节点按固定间隔清理未引用 blob；启动阶段不立即扫描。
func startBlobGCTask(ctx context.Context, interval time.Duration, cleanup func() (int, error)) {
	// 复制退役后不再有 standby（此前 standby 不 GC 以免误删对端仍引用的 blob）；
	// 单节点视角下未引用 blob 即可回收。
	if interval <= 0 || cleanup == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if ctx.Err() != nil {
					return
				}
				removed, err := cleanup()
				if err != nil {
					log.Printf("blob 定时清理失败：%v", err)
				} else if removed > 0 {
					log.Printf("blob 定时清理完成：回收 %d 个未引用 blob", removed)
				}
			}
		}
	}()
}

// buildTLSServer 构造服务内置 TLS 的 http.Server(FR-131)。
// 先加载并校验 PEM 证书对;失败返回错误,由调用方决定整体启动失败(防静默降级)。
func buildTLSServer(cfg *config.Config, handler http.Handler) (*http.Server, error) {
	if strings.TrimSpace(cfg.TLSCert) == "" || strings.TrimSpace(cfg.TLSKey) == "" {
		return nil, fmt.Errorf("配置了 %s 但缺少 %s / %s", config.EnvTLSAddr, config.EnvTLSCert, config.EnvTLSKey)
	}
	if _, err := tls.LoadX509KeyPair(cfg.TLSCert, cfg.TLSKey); err != nil {
		return nil, fmt.Errorf("加载 TLS 证书失败(%s/%s)：%w", cfg.TLSCert, cfg.TLSKey, err)
	}
	return &http.Server{
		Addr:              cfg.TLSAddr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}, nil
}
