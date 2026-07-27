// 开源协议清单端点（非契约）：返回内嵌的依赖协议清单，仅管理员可见。
package api

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/wcpe/jianartifact/apps/server/internal/licenses"
)

// GetLicenses 返回构建时生成的 Go/npm 依赖协议清单（内嵌 JSON 原样透传）。
// 清单含精确依赖版本，属侦察情报，故仅限管理员（未认证 401、非管理员 403）。
func (h *Handlers) GetLicenses(c *gin.Context) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	c.Data(http.StatusOK, "application/json; charset=utf-8", licenses.Data())
}
