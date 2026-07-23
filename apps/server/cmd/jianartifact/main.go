// Package main 是 JianArtifact 后端的入口。
//
// 0.1.0 工程基座：加载监听地址、装配 HTTP 服务（契约路由 + 健康 / 就绪端点 +
// 内嵌前端），以优雅关停运行。协议端点、管理 API、SQLite 元数据与 blob 存储、
// Nexus 迁移随 M1 后续版本（0.2.0+）迭代落地。
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/api"
	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/config"
	"github.com/wcpe/jianartifact/apps/server/internal/httpserver"
	"github.com/wcpe/jianartifact/apps/server/internal/protocol"
	"github.com/wcpe/jianartifact/apps/server/web"
)

// version 由构建时注入（-ldflags "-X main.version=..."）；默认与 VERSION 文件一致。
var version = "0.3.0"

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
  healthcheck        对本地 /readyz 探活，供容器健康检查
  help               显示本帮助

环境变量：
  JIAN_HTTP_ADDR     HTTP 监听地址:端口（默认 :8080）
  JIAN_DATA_DIR      数据根目录（默认 ./data；存放 SQLite 与 blob）
  JIAN_JWT_SECRET    JWT(HS256) 签名密钥（缺省时生成并持久化到数据目录）

示例：
  jianartifact run
  jianartifact admin reset --username admin
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

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("加载配置：%w", err)
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

	checks := []func() error{svc.db.Ping, blobWritableCheck(cfg.BlobDir)}
	authenticator := auth.NewAuthenticator(svc.jwt, svc.store)

	// 协议层（Raw / Maven 等）经原生客户端访问，不在 OpenAPI 契约内；
	// 复用支持 Basic + Bearer 的 Optional() 中间件解析主体，鉴权在 handler 内判定。
	// Dispatcher 按仓库 format 将 /repository 端点分派到对应格式处理器。
	rawHandler := protocol.NewRawHandler(svc.assetSvc, svc.repoSvc)
	mavenHandler := protocol.NewMavenHandler(rawHandler)
	dispatcher := protocol.NewDispatcher(svc.repoSvc, rawHandler, mavenHandler)
	npmHandler := protocol.NewNpmHandler(rawHandler)

	srv := httpserver.New(version,
		httpserver.WithReadinessCheck(svc.db.Ping),
		httpserver.WithReadinessCheck(blobWritableCheck(cfg.BlobDir)),
		httpserver.WithHandlers(svc.handlers(version, checks)),
		httpserver.WithMiddleware(api.MiddlewareFunc(authenticator.Optional())),
		httpserver.WithProtocolRoutes(func(r gin.IRouter) {
			protocol.RegisterRoutes(r, dispatcher, authenticator.Optional())
			protocol.RegisterNpmRoutes(r, npmHandler, authenticator.Optional())
		}),
	)
	httpServer := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           srv.Handler(assets),
		ReadHeaderTimeout: 10 * time.Second,
	}
	addr := cfg.HTTPAddr

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		fmt.Printf("JianArtifact %s 正在监听 %s\n", version, addr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

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
