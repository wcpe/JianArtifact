// Package main 是 JianArtifact 后端的入口。
//
// 当前为 0.1.0 工程基座骨架：仅提供最小启动与健康端点占位，
// 协议端点、管理 API、Nexus 迁移、前端 embed 随 M1 迭代落地。
package main

import (
	"fmt"
	"os"
)

// version 由构建时注入；默认与 VERSION 文件一致。
var version = "0.1.0"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "启动失败：", err)
		os.Exit(1)
	}
}

func run() error {
	// TODO(M1/0.1.0)：加载配置、初始化 SQLite 元数据与文件系统 blob 存储、
	// 装配 Gin 路由（协议端点 + 管理 API + /healthz /readyz）、embed 前端 dist。
	fmt.Printf("JianArtifact %s — 工程基座骨架，尚未提供服务能力。\n", version)
	return nil
}
