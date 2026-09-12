package httpserver

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
)

// WithWriteFreeze 在 HTTP 层叠加运行时写入冻结窗口：冻结期间除读方法与几个运维出口
// （登录、节点级维护命名空间、搬迁导入/上传）外，一律以 503 + write_frozen 拒绝。
// state 由全局 *domain.FreezeController.State 提供；nil 表示不启用（保持旧行为）。
func WithWriteFreeze(state func() domain.FreezeState) Option {
	return func(s *Server) { s.writeFreeze = state }
}

// writeFrozenAllowedPrefixes 是冻结窗口内放行的非读路径前缀白名单。
//
// 为什么放行导入/上传：冻结窗口的全部意义就是「停写 → 生成差包/导入 → 起服」。
// 导入与上传只写 restore-staging/ 与 restore.pending，重启才生效，不经实时写闸门，
// 不破坏冻结语义；拦掉它们等于冻结期间做不了搬迁切换（甚至冻上就解不开）。
// 用前缀而非精确路径：未来往这些命名空间加端点时不会被运维锁在门外。
var writeFrozenAllowedPrefixes = []string{
	"/api/v1/maintenance/", // 节点级运维状态（含冻结/解冻），解冻的唯一出口
	"/api/v1/backups/import",
	"/api/v1/backups/imports",
	"/api/v1/backups/uploads",
}

// writeFrozenAllowed 在冻结窗口内放行的方法/路径：
//   - 读方法（GET/HEAD/OPTIONS）一律放行；
//   - POST /api/v1/auth/login 放行：否则无法登录去解冻；
//   - 前缀命中 writeFrozenAllowedPrefixes 的路径全方法放行（维护/导入/上传出口）。
func writeFrozenAllowed(method, requestPath string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	case http.MethodPost:
		if requestPath == "/api/v1/auth/login" {
			return true // 否则无法登录去解冻
		}
	}
	for _, prefix := range writeFrozenAllowedPrefixes {
		if requestPath == prefix || strings.HasPrefix(requestPath, prefix) {
			return true
		}
	}
	return false
}

func writeFreezeMiddleware(state func() domain.FreezeState) gin.HandlerFunc {
	return func(c *gin.Context) {
		if state == nil {
			c.Next()
			return
		}
		st := state()
		if !st.Frozen {
			c.Next()
			return
		}
		if writeFrozenAllowed(c.Request.Method, c.Request.URL.Path) {
			c.Next()
			return
		}
		// 默认按「需手动解冻」表述：domain 里 until 零值即手动冻结。
		message := "写入已冻结，需手动解冻（POST /api/v1/maintenance/freeze 解冻）"
		if !st.Until.IsZero() {
			message = "写入已冻结，预计 " + st.Until.Format(time.RFC3339) + " 自动解冻"
		}
		auth.WriteError(c, http.StatusServiceUnavailable, "write_frozen", message)
		c.Abort()
	}
}
