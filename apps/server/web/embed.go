// Package web 内嵌前端构建产物（apps/web 的 vite 产出 dist/），供单二进制交付。
// 见 docs/adr/0005-single-binary-embed.md。
//
// dist/ 下提交了占位 index.html 以保证独立 `go build` 可用；正式构建时前端
// 产物会覆盖它（见 deploy/Dockerfile 与顶层 Makefile 的 build 目标）。
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var distFS embed.FS

// Assets 返回以 dist/ 为根的前端静态资源文件系统。
func Assets() (fs.FS, error) {
	return fs.Sub(distFS, "dist")
}
