// Package licenses 内嵌构建时生成的开源依赖协议清单（scripts/generate-licenses.mjs 产出）。
// 清单经 admin 专属端点 GET /api/v1/licenses 返回，不再打进前端 bundle，
// 避免依赖名与版本清单随静态资源公开暴露（降低已知漏洞的侦察价值）。
package licenses

import _ "embed"

//go:embed licenses.json
var data []byte

// Data 返回协议清单 JSON 原始字节（{generatedAt, go: [...], npm: [...]}）。
func Data() []byte {
	return data
}
