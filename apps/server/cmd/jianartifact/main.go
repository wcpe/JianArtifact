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
	"github.com/wcpe/jianartifact/apps/server/internal/scheduler"
	"github.com/wcpe/jianartifact/apps/server/web"
)

// version 由构建时注入（-ldflags "-X main.version=..."）；默认与 VERSION 文件一致。
var version = "0.12.0"

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
  backup drill <包标识>
                     恢复演练：隔离目录内真解包 + SQLite 一致性校验，
                     产出报告并落库 [--level shallow|deep]（默认 deep）
                     只读路径，不产生 restore.pending；未通过时以非零退出
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
  JIAN_BACKUP_AUTO_INTERVAL
                     自动备份作业间隔（秒，默认 0 即关闭；需显式开启）
  JIAN_BACKUP_KEEP_LAST
                     保留最近 N 份自动备份（默认 7；0 = 不按份数限制）
  JIAN_BACKUP_RETENTION_DAYS
                     自动备份保留天数（默认 30；0 = 不限天数）
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
	// 回源 Token 校验（后台「设置-安全防护」；节点本地，运行时生效）。
	originTokenGuard := func() (bool, string, string) { return svc.settingSvc.OriginTokenGuard() }
	authenticator := auth.NewAuthenticator(svc.jwt, svc.store, auth.WithAllowedHosts(allowedHosts))
	apiHandlers := svc.handlers(version, checks)

	// 各格式协议共享基础资产处理器；空格式列表时不创建任何协议处理对象。
	var rawHandler *protocol.RawHandler
	if cfg.EnabledFormats.Any() {
		rawHandler = protocol.NewRawHandler(svc.assetSvc, svc.repoSvc)
		rawHandler.SetPublishPolicy(svc.publishPolicySvc)
		rawHandler.SetQuotaGuard(svc.quotaSvc) // 仓库存储配额（预检早拒 + 提交点权威复检）
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
		httpserver.WithWriteFreeze(svc.freeze.State), // 运行时写入冻结窗口（未冻结时中间件为空操作）
		httpserver.WithHandlers(apiHandlers),
		httpserver.WithManagementSecurityAudit(apiHandlers.AuditLog),
		httpserver.WithAllowedHosts(allowedHosts),
		httpserver.WithOriginTokenGuard(originTokenGuard),
		httpserver.WithMiddleware(api.MiddlewareFunc(authenticator.Optional())),
		httpserver.WithProtocolMetric(func(c *gin.Context) {
			cacheResult, _ := c.Get(protocol.CacheResultContextKey)
			svc.dashboardSvc.RecordProtocol(domain.ProtocolMetric{CompletedAt: time.Now().UTC(), Method: c.Request.Method, Status: c.Writer.Status(), CacheResult: cacheResultString(cacheResult)})
			// Prometheus 指标——复用同一钩子累加协议请求计数（进程内，不写库）。
			svc.metricsReg.ProtocolRequest(c.Request.Method, c.Writer.Status(), cacheResultString(cacheResult))
			// 制品下载计量（只认 GET+200 的完整传输；该钩子只服务制品协议请求，
			// 路径解析与 UA 归类在服务内完成——原始 UA 串不落库）。
			svc.assetDownloadSvc.RecordDownload(c.Request.Method, c.Writer.Status(), c.Request.URL.Path, c.ClientIP(), c.Request.UserAgent(), time.Now().UTC())
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
			// Maven 网页上传仅在 Maven 启用时注册。
			if mavenHandler != nil {
				r.POST("/api/v1/repositories/:name/maven-upload", authMW, mavenHandler.UploadForm)
			}
			// 目录懒加载 tree API
			r.GET("/api/v1/repositories/:name/tree", authMW, apiHandlers.ListRepositoryTree)
			// 仓库详情下载趋势与总下载（权限同 tree；非契约端点，结构收口在
			// openapi.yaml 的 RepositoryDownloadTrendResponse，路径按 tree 先例不入契约）
			r.GET("/api/v1/repositories/:name/download-trend", authMW, apiHandlers.GetRepositoryDownloadTrend)
			// 分组下载趋势 /api/v1/observability/downloads/trend 已收口进契约，
			// 由 RegisterHandlersWithOptions 注册（勿在此重复注册，避免 gin 路由冲突）。
			// 公开接口 /api/v1/public/repositories 同样已收口进契约（响应含全局置顶名），
			// 由 RegisterHandlersWithOptions 注册，不再经此处手写注册。
			// 全局搜索
			r.GET("/api/v1/search", authMW, apiHandlers.SearchAssets)
			// 搜索索引回填状态与全量重建（仅管理员，admin 校验在 handler 内）。
			// 与上一行同组同先例——**非契约端点**，不写入 api/openapi.yaml：它们只服务运维观测
			// 与故障修复，形状会随后续批次（查询改写、suggest）演进，不为易变的内部进度形状
			// 引入对外承诺。理由详见 internal/api/search_index_handlers.go 文件头。
			r.GET("/api/v1/search/index", authMW, apiHandlers.GetSearchIndexStatus)
			r.POST("/api/v1/search/index/rebuild", authMW, apiHandlers.RebuildSearchIndex)
			// 存储体检（只读，仅管理员）。同上为**非契约端点**：它服务运维手动排障，
			// 返回「此刻磁盘上可回收的东西有多少」，形状随清理能力演进，不属于协议客户端
			// 依赖的稳定面。**刻意只读**：仓库删除与 blob 回收不可逆，自动清理会把一次
			// 误判变成数据丢失，清理仍交给既有作业按明确间隔执行。
			r.GET("/api/v1/maintenance/storage-health", authMW, apiHandlers.GetStorageHealth)
			// 定向清理与清理任务队列（仅管理员，非契约端点）。
			// 执行端点只把目标**移入回收站**，不做物理删除——误点可恢复。
			r.POST("/api/v1/maintenance/cleanup/preview", authMW, apiHandlers.PreviewCleanup)
			r.POST("/api/v1/maintenance/cleanup", authMW, apiHandlers.ExecuteCleanup)
			r.GET("/api/v1/maintenance/clean-tasks", authMW, apiHandlers.ListCleanTasks)
			r.GET("/api/v1/maintenance/clean-tasks/:id", authMW, apiHandlers.GetCleanTask)
			// 回收站（仅管理员，非契约端点）：让定向清理可逆。
			// restore 是反向操作，不做二次防护；purge / purge-all 不可逆，防护在界面上。
			r.GET("/api/v1/maintenance/trash", authMW, apiHandlers.ListTrash)
			r.POST("/api/v1/maintenance/trash/restore", authMW, apiHandlers.RestoreTrash)
			r.POST("/api/v1/maintenance/trash/purge", authMW, apiHandlers.PurgeTrash)
			r.POST("/api/v1/maintenance/trash/purge-all", authMW, apiHandlers.PurgeAllTrash)
			// 搜索建议（搜索框下拉候选：包名 + 仓库名）。
			// 同上为**非契约端点**，不写入 api/openapi.yaml：它只服务前端搜索框，候选形状会随
			// 包坐标解析口径演进，不是协议客户端依赖的稳定面。权限（管理员搜全部 / 其余
			// public ∪ 被授 read）在领域层统一判定，可读集合为空时返回 200 + 空 items 而非 403。
			r.GET("/api/v1/search/suggest", authMW, apiHandlers.SuggestSearch)
			// 匿名访问全局开关（admin，主体由 Optional 注入，handler 内校验）
			r.GET("/api/v1/settings/anonymous-access", authMW, apiHandlers.GetAnonymousAccessSetting)
			r.PUT("/api/v1/settings/anonymous-access", authMW, apiHandlers.PutAnonymousAccessSetting)
			// 设置读写端点（admin，基础配置四项：匿名开关/对外 URL/回源超时/同步间隔）
			r.GET("/api/v1/settings", authMW, apiHandlers.GetSettings)
			r.PUT("/api/v1/settings", authMW, apiHandlers.PutSettings)
			// 开源协议清单（admin 专属；清单不再打进前端 bundle，见 internal/licenses）
			r.GET("/api/v1/licenses", authMW, apiHandlers.GetLicenses)
			// 复制退役：数据面 /api/v1/cluster/sync/* 与管理面 /api/v1/cluster* 均已摘除。
			// 审计日志（分页 + 筛选：actor/action/repo/from/to，仅管理员）
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

	// 在打开数据库连接之前应用待生效的备份恢复（替换 db 文件时不能有打开的连接）。
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
	// 收尾上次进程遗留的"生成中"备份包，避免列表里留下永久幻影。
	if n, err := svc.backupSvc.ReconcileStartup(); err == nil && n > 0 {
		fmt.Fprintf(os.Stderr, "备份：标记 %d 个服务重启遗留的生成中包为失败\n", n)
	}
	// 清理任务收口：把上次进程遗留的 queued/running 标为 interrupted。
	// 不做这一步的后果不是「少一条记录」——单并发守卫以「是否存在 running」为判据，
	// 残留的 running 会让此后每一次清理都被 409 拒绝且无法自愈，表现为「清理功能坏了」。
	if n, err := svc.cleanTaskRepo.MarkInterruptedOnStartup(); err != nil {
		fmt.Fprintf(os.Stderr, "清理任务：标记重启遗留任务失败：%v\n", err)
	} else if n > 0 {
		fmt.Fprintf(os.Stderr, "清理任务：标记 %d 个服务重启遗留的任务为中断\n", n)
	}
	// 清理崩在导入中途留下的暂存目录（有 restore.pending 时不清理，
	// 那是等待本次启动替换数据库的正品，由 ApplyPendingRestore 处理）。
	if err := svc.restoreSvc.ReconcileStaleStaging(); err != nil {
		fmt.Fprintf(os.Stderr, "导入：清理残留暂存目录失败：%v\n", err)
	}
	// 清理到期未完成的分片上传会话（磁盘 + 记录）；失败仅记日志，不影响启动。
	if n, err := svc.backupUploads.ReconcileExpired(time.Now()); err != nil {
		fmt.Fprintf(os.Stderr, "分片上传：清理过期会话失败：%v\n", err)
	} else if n > 0 {
		fmt.Fprintf(os.Stderr, "分片上传：清理 %d 个过期上传会话\n", n)
	}
	// 分钟聚合与当前主机采样只由运行期定时任务驱动，启动不扫描历史数据。
	svc.dashboardSvc.Start(ctx, time.Now)
	svc.assetDownloadSvc.Start(ctx, time.Now)
	svc.hostMonitoringSvc.Start(ctx, time.Now)
	// 上游健康采样的定期落盘（每 5s 一次 + ctx 结束后最后一次 flush）。
	// 它必须在 scheduler.Start 之前挂上：回源请求随时可能开始产生采样，
	// 早一秒开始刷盘就少一段只存在内存里、等不到落盘就关停的数据。
	svc.upstreamHealthSvc.Start(ctx)
	// 维护类周期作业的统一调度器（作业已在 wiring 装配期注册完毕）。
	svc.scheduler.Start(ctx)
	// 搜索索引的后台异步回填。一次性任务、不阻塞启动（这里只是挂 goroutine），
	// 分批推进水位、可中断续跑；未完成期间搜索照常可用（走既有 LIKE 路径）。
	// 不用调度器：回填要尽快开始、只跑一次、跑完即退出，与周期作业语义相反。
	svc.searchIndexSvc.Start(ctx)

	errCh := make(chan error, 1)
	go func() {
		fmt.Printf("JianArtifact %s 正在监听 %s\n", version, addr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()
	// 服务内置 TLS 双监听（与 HTTP 共用同一 handler），供 CDN 回源 HTTPS。
	// 显式配置了 TLS 地址但证书缺失 / 格式错误 → 启动失败并明确报错，
	// 防止"以为加密了实际没有"的静默降级；未配置 TLS 地址时保持纯 HTTP。
	//
	// tlsServer 在 select 之前声明（而不是原来的 if 块内短声明）：一键重启分支也要关停它，
	// 块内短声明的变量在 select 处已不可见。
	var tlsServer *http.Server
	if cfg.TLSAddr != "" {
		built, err := buildTLSServer(cfg, newApplicationHandler(cfg, svc, assets))
		if err != nil {
			return fmt.Errorf("配置了 %s 但 TLS 启动失败：%w", config.EnvTLSAddr, err)
		}
		tlsServer = built
		go func() {
			fmt.Printf("JianArtifact %s TLS 正在监听 %s\n", version, cfg.TLSAddr)
			if err := tlsServer.ListenAndServeTLS(cfg.TLSCert, cfg.TLSKey); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- err
			}
		}()
	}

	// 一键重启的"该重启了"信号（触发点已在装配期接到领域服务上）。
	// 它由 HTTP 端点受理后延迟发出，因此这条分支就是"重启"本身：
	// 先优雅关停（与 SIGTERM 同一条路），再自替换映像。
	restartCh := svc.restartCh
	if restartCh == nil {
		// CLI 子命令不会走到这里（它们不启动 HTTP 服务）；真到了也只说明装配漏了，
		// 用一条永不触发的通道兜底，避免 select 里出现 nil 通道分支。
		restartCh = make(chan struct{})
	}

	select {
	case err := <-errCh:
		return fmt.Errorf("HTTP 服务异常：%w", err)
	case <-ctx.Done():
		fmt.Println("收到关停信号，正在优雅关停……")
		noteSearchBackfillInterrupted()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("优雅关停失败：%w", err)
		}
		fmt.Println("已关停。")
		return nil
	case <-restartCh:
		// 一键重启：**先优雅关停，再自替换**。
		//
		// 顺序必须是这个（反过来是错的，两个具体后果）：
		//  1. 新映像要在关停之后才绑定同一端口。若先 Exec，旧监听器还没关，
		//     新映像就只能绑到一个已被占用的端口上；虽然 Go 的 fd 默认带 O_CLOEXEC、
		//     内核会在 exec 时关掉旧 fd（实测可重绑），但那是"靠内核兜底"，
		//     一旦哪天某个监听器的 fd 被清掉 CLOEXEC 或被继承，就变成新映像起不来；
		//  2. 正在服务的在途请求与数据库连接需要有收尾的机会。Exec 一到，旧映像的内存状态
		//     连同 goroutine 一起消失，没有任何机会把请求写完、把事务提交完。
		//     所以这里走与 SIGTERM 完全相同的 15s 上限的 Shutdown。
		//
		// 关停失败**如实报错并返回**，绝不静默 Exec：在途请求或 SQLite 连接残留着
		// 就换映像，等于把"没关干净"这件事藏起来——新映像启动时看到的是一份半关停的现场，
		// 而日志里一个字都不会提。
		fmt.Println("收到一键重启请求，正在优雅关停并自替换……")
		noteSearchBackfillInterrupted()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("一键重启：优雅关停 HTTP 失败，已放弃自替换：%w", err)
		}
		if tlsServer != nil {
			if err := tlsServer.Shutdown(shutdownCtx); err != nil {
				return fmt.Errorf("一键重启：优雅关停 TLS 失败，已放弃自替换：%w", err)
			}
		}
		fmt.Println("已关停，正在自替换进程映像……")

		// 自替换：不执行任何外部命令，只是把本进程的映像换成 argv[0] 指向的那份
		// （**必须是 os.Args[0]，不能用 os.Executable()**——理由见 domain/restart_exec.go，
		// 用错会让"部署切换后重启"等于没有切换）。本调用成功后不会返回：
		// 新映像从 run() 的第一行重新开始，于是 ApplyPendingRestore（见本函数开头的调用）
		// 会被再执行一次——这才是"一键重启"真正的意义：备份恢复的落地就靠这一次启动。
		if err := domain.ReplaceSelf(); err != nil {
			return fmt.Errorf("一键重启：自替换失败（argv[0]=%q）：%w", os.Args[0], err)
		}
		// 理论上到不了这里（Exec 成功即换映像）。留一句防御性返回，避免将来有人把
		// ReplaceSelf 改成"可返回成功"的实现后，本函数静默地走完却没重启。
		return errors.New("一键重启：自替换未生效，进程仍在运行")
	}
}

// noteSearchBackfillInterrupted 在关停路径上说明「后台索引回填可能被本批中断」。
//
// 为什么不在关停时等它退出：回填跑在 domain.SearchIndexService 自己的 goroutine 里，
// 关停只需要停止推进水位，而**等待**会把关停时长绑在「当前批次还剩多少行」上——
// 大库上一批可能是几十万行，SIGTERM 后的 15 秒关停窗口（k8s 的 terminationGracePeriod
// 同理）根本不够，结果是关停超时被杀，反而丢掉「本批已提交到哪」之外的全部收尾动作。
//
// 不等待是安全的：水位（search_index_state.last_id）在**每批提交时**落库，被中断的那一批
// 至多回退到该批起点，下次启动从 last_id 续跑，不重复灌也不漏行；未完成期间搜索走既有
// 全表 LIKE 路径，结果仍然正确。代价只是「本批白做一次」，所以不在关停路径里等。
//
// 但仍然要打这一行日志：否则运维在「刚重启后 indexed 比关停前还少一点」时无从判断
// 是回退了还是索引坏了。
func noteSearchBackfillInterrupted() {
	fmt.Fprintln(os.Stderr, "提示：搜索索引的后台回填可能在本批中被中断；水位按批提交，下次启动将从 last_id 续跑，无需人工干预。")
}

// registerBlobGCJob 把孤立 blob 定时清理注册为调度器作业：按固定间隔执行、启动不立即扫描；
// 间隔 <= 0 或清理函数为空时不注册（禁用语义，对齐 JIAN_BLOB_GC_INTERVAL=0）。
// 复制退役后不再有 standby（此前 standby 不 GC 以免误删对端仍引用的 blob）；
// 单节点视角下未引用 blob 即可回收。
func registerBlobGCJob(sched *scheduler.Scheduler, interval time.Duration, cleanup func() (int, error)) {
	if interval <= 0 || cleanup == nil {
		return
	}
	sched.Register("blob-gc", interval, func(context.Context) error {
		removed, err := cleanup()
		if err != nil {
			return err
		}
		if removed > 0 {
			log.Printf("blob 定时清理完成：回收 %d 个未引用 blob", removed)
		}
		return nil
	})
}

// buildTLSServer 构造服务内置 TLS 的 http.Server。
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
