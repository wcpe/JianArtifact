// npm 经 /repository/:repo/*artifactPath 通用制品路径的适配层。
//
// 真实 npm 客户端按用户配置的 registry 基址（`…/repository/<repo>/`）发 publish 与
// 拉取；此前 npm 仓库被 Dispatcher 回落 RawHandler，packument 被当普通文件写入、
// `_attachments` 内的 tarball 静默丢失（0.8.0 验收 BUG-1）。适配器把通配参数
// artifactPath 映射为 NpmHandler 期望的 rest（gin 通配参数对两者都给解码后路径，
// scoped 包名 `@scope%2fname` 与 `@scope/name` 落到同一资产路径），使完整 npm 语义
// （版本合并、tarball 落库、dist.tarball 按请求基址重写、unpublish）在两条前缀一致。
package protocol

import "github.com/gin-gonic/gin"

// npmArtifactAdapter 实现 artifactHandler，转发到 NpmHandler 并映射路径参数。
type npmArtifactAdapter struct {
	npm *NpmHandler
}

func (a npmArtifactAdapter) Get(c *gin.Context)    { a.mapRest(c); a.npm.Get(c) }
func (a npmArtifactAdapter) Put(c *gin.Context)    { a.mapRest(c); a.npm.Put(c) }
func (a npmArtifactAdapter) Delete(c *gin.Context) { a.mapRest(c); a.npm.Delete(c) }

// mapRest 把 /repository/ 路由的 artifactPath 参数以 rest 键追加，供 NpmHandler 读取。
func (a npmArtifactAdapter) mapRest(c *gin.Context) {
	if rest := c.Param("artifactPath"); rest != "" {
		c.Params = append(c.Params, gin.Param{Key: "rest", Value: rest})
	}
}
