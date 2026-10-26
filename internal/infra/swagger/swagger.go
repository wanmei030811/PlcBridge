// Package swagger provides embedded OpenAPI/Swagger UI for plcbridge.
//
// 设计要点（2026-10-24 修正）：
//
//	旧实现：init() 时按"exe 同级目录/api/openapi.yaml → cwd/api/openapi.yaml"找文件。
//	问题：部署方式一变（容器 / systemd WorkingDirectory / 交叉编译后拷 exe）就找不到，
//	      /api/openapi.yaml 返回空 body，Swagger UI 白屏，且**没有任何报错**。
//	新实现：spec 由 api 包 go:embed 内嵌，二进制自包含，恒可用。
//
//	API 包路径：github.com/plcbridge/plcbridge/api
//	（api/openapi.yaml 是对外契约的唯一事实源，embed.go 在同目录做唯一嵌入点）
package swagger

import (
	"embed"
	"io/fs"
	"net/http"

	apispec "github.com/plcbridge/plcbridge/api"
)

// FS is the swagger-assets virtual filesystem.
//
//go:embed assets
var assets embed.FS

// assetsFS 是以 assets/ 为根的文件系统。
//
// 为什么需要 fs.Sub：assets embed.FS 的根是包目录（里面还有一个 assets/ 子目录），
// 直接 http.FS(assets) 会让 FileServer 的根变成包目录 → 请求 /swagger/ 时
// 列出的是 "assets/" 目录项而不是 Swagger UI 页面。
// 用 fs.Sub 剥掉这层，根才真正落在 assets/index.html 上。
//
// 回归背景（2026-10-24）：之前没做 Sub，/swagger/ 返回的是目录列表，
// Swagger UI 根本没显示出来（而且不报错，很难发现）。
var assetsFS = func() fs.FS {
	sub, err := fs.Sub(assets, "assets")
	if err != nil {
		// go:embed 编译期保证 assets 目录存在；缺失即 panic
		panic(err)
	}
	return sub
}()

// openAPISpec 是内嵌的 OpenAPI YAML 原文。
var openAPISpec = apispec.OpenAPIYAML

// Handler returns an http.Handler that serves the Swagger UI.
// Strips the /swagger/ prefix so index.html and assets load correctly.
func Handler() http.Handler {
	return http.StripPrefix("/swagger/", http.FileServer(http.FS(assetsFS)))
}

// OpenAPISpec returns the raw OpenAPI YAML bytes.
func OpenAPISpec() []byte { return openAPISpec }

// OpenAPIHandler returns an http.Handler that serves the OpenAPI YAML spec.
func OpenAPIHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-yaml; charset=utf-8")
		w.Write(openAPISpec)
	})
}
