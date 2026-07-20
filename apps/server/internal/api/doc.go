// Package api 承载据 api/openapi.yaml 由 oapi-codegen 生成的类型与 Gin server 接口。
//
// 契约（api/openapi.yaml）是唯一真源（见 docs/adr/0004-design-first-openapi.md）。
// 生成产物 api.gen.go 不得手改；契约变更后在 apps/server 下运行 `task gen` 重生成。
package api

//go:generate go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.8.0 -config cfg.yaml ../../../api/openapi.yaml
