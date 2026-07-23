package protocol

import "github.com/gin-gonic/gin"

// artifactHandler 抽象 /repository/:repo/*artifactPath 的方法处理器，
// 由 RawHandler / MavenHandler / Dispatcher 实现，便于按仓库 format 分派。
type artifactHandler interface {
	Get(*gin.Context)
	Put(*gin.Context)
	Delete(*gin.Context)
}

// RegisterRoutes 在 r 上挂载制品协议端点：
//
//	GET|HEAD|PUT|DELETE /repository/:repo/*artifactPath
//
// h 通常为按仓库 format 分派的 Dispatcher（Raw/Maven 等），也可直接传单一格式处理器。
// mw 为作用于该组的中间件（通常是 authenticator.Optional()，已支持 Basic + Bearer）。
// GET/HEAD 走 read、PUT/DELETE 走 write，具体鉴权由 handler 内 authorize 判定。
func RegisterRoutes(r gin.IRouter, h artifactHandler, mw ...gin.HandlerFunc) {
	grp := r.Group("/repository", mw...)
	grp.GET("/:repo/*artifactPath", h.Get)
	grp.HEAD("/:repo/*artifactPath", h.Get)
	grp.PUT("/:repo/*artifactPath", h.Put)
	grp.DELETE("/:repo/*artifactPath", h.Delete)
}
