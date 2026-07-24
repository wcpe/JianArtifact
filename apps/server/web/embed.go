// Package web 内嵌前端构建产物（apps/web 的 vite 产出 dist/），供单二进制交付。
// 见 docs/adr/0005-single-binary-embed.md。
//
// dist/ 内容不入库：构建流程（scripts/check.sh、顶层 Makefile build、deploy/Dockerfile）
// 会在编译前把 apps/web 的 vite 产物同步/复制到此处再编译。因此未先构建前端而直接
// `go build` 会因 all:dist 无文件而失败，属预期——请走 make build / check.sh / Docker 构建。
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
