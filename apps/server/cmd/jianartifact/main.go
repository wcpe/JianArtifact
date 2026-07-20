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
	"syscall"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/httpserver"
	"github.com/wcpe/jianartifact/apps/server/web"
)

// version 由构建时注入（-ldflags "-X main.version=..."）；默认与 VERSION 文件一致。
var version = "0.1.0"

func main() {
	// healthcheck 子命令：供 distroless 容器（无 shell/curl）自探活，探测本地 /readyz。
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		if err := healthcheck(); err != nil {
			fmt.Fprintln(os.Stderr, "探活失败：", err)
			os.Exit(1)
		}
		return
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "启动失败：", err)
		os.Exit(1)
	}
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

func run() error {
	addr := listenAddr()

	assets, err := web.Assets()
	if err != nil {
		return fmt.Errorf("加载内嵌前端资源：%w", err)
	}

	srv := httpserver.New(version)
	httpServer := &http.Server{
		Addr:              addr,
		Handler:           srv.Handler(assets),
		ReadHeaderTimeout: 10 * time.Second,
	}

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
