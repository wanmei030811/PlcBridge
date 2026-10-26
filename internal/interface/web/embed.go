// 内嵌 H5 SPA（前端构建产物 dist/）。
//
// 构建命令（Vite）：
//
//	cd web && npm install && npm run build
//	输出 dist/ 自动嵌入到 plcbridge-web 二进制
//
// 路由策略（SPA fallback）：
//   - /api/v1/*  → 不进 SPA（由 Gin 处理）
//   - /assets/*  → 静态资源（由 embed FS serve）
//   - 其它 GET   → dist/index.html（前端接管）
package web

import (
	"embed"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/plcbridge/plcbridge/internal/infra/logging"
)

//go:embed all:dist
var distFS embed.FS

// spaFallbackHTML 是「前端产物未内嵌」时的占位页。
//
// 触发条件：二进制构建时 internal/interface/web/dist/ 里没有 index.html。
// 常见原因有两个：
//   - 只跑了 go build，没跑 web 的 npm run build（最常见）
//   - 干净克隆直接 go build（dist/ 只有 .gitkeep 占位文件）
//
// 为什么不能 panic（2026-10-26 修复）：
//
//	web 进程除了提供仪表盘 UI，还提供配置/通道/设备/点位等全部核心 API。
//	前端缺失属于「降级」，不属于「进程起不来」——原来的 panic 会让进程直接退出，
//	supervisor 随后进入无限重启循环，结果连 API 和健康检查一起不可用。
//	现在改成：进程照常起来，/api/v1/* 正常，页面显示这张占位页并给出修复步骤。
//
// 详见 md/deploy-first-time.md —— 文档里写的预期行为本来就是
// 「若 internal/interface/web/dist/ 为空，页面能开但没有 UI 资源」。
const spaFallbackHTML = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>PlcBridge Web — 前端产物缺失</title>
<style>
  body { font-family: -apple-system, "Segoe UI", "Microsoft YaHei", sans-serif;
         margin: 0; padding: 48px 24px; background: #f5f7fa; color: #1f2329; }
  .card { max-width: 700px; margin: 0 auto; background: #fff; border-radius: 10px;
          padding: 32px 36px; box-shadow: 0 2px 12px rgba(0,0,0,.08); }
  h1 { font-size: 20px; margin: 0 0 14px; }
  p { line-height: 1.8; margin: 12px 0; }
  pre { background: #f2f3f5; padding: 14px 18px; border-radius: 6px; overflow-x: auto; }
  code { background: #f2f3f5; padding: 2px 6px; border-radius: 4px; }
  .tip { color: #646a73; font-size: 13px; }
</style>
</head>
<body>
<div class="card">
  <h1>前端资源未内嵌，页面 UI 暂不可用</h1>
  <p>本二进制内嵌的 <code>internal/interface/web/dist/</code> 中没有 <code>index.html</code>，
     因此无法渲染仪表盘界面。API 服务（<code>/api/v1/*</code>）与健康检查仍然正常工作。</p>
  <p>修复方式：</p>
  <pre>cd web
npm install
npm run build
cd ..
go build -o bin/plcbridge-web.exe ./cmd/plcbridge-web</pre>
  <p class="tip">重新构建后需重启 plcbridge-web 进程才能生效。</p>
</div>
</body>
</html>
`

// MountSPA 把嵌入的 SPA 挂到 Gin：
//   - /assets/...      静态资源（嵌入 FS）
//   - /                index.html
//   - 其它 GET         index.html（SPA 接管）
//
// FileServer 的 fs root 是 dist/，因此静态资源 URL.Path 必须以 "assets/" 开头
// （这样 Open 后实际指向 dist/assets/<rel>）。我们直接把 gin 的 *filepath 拼成
// "assets/<rel>" 给 FileServer，不再用 StripPrefix。
func MountSPA(r *gin.Engine) {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		// dist 缺失意味着编译期的 go:embed 锚点没了，属于构建事故。
		// 这里不 panic：进程还挂着 API，硬退出等于把整个服务搞挂。
		// 记日志 + 走占位页，让运维能通过 /api/v1/health 发现问题。
		logging.Error("SPA dist 目录缺失（构建事故）").Err(err).Send()
		mountFallbackSPA(r)
		return
	}
	indexBytes, err := readIndex(sub)
	if err != nil {
		// 最常见原因：二进制构建时没跑 npm run build，dist/ 里只有 .gitkeep。
		// 同上，降级而不是 panic（web 进程还提供全部配置 API）。
		logging.Warn("SPA index.html 缺失，降级为占位页").Err(err).Send()
		mountFallbackSPA(r)
		return
	}
	// 注：早期版本用 http.FileServer + gin header 注入，但 FileServer 内部会对
	// 缓存相关 header 做处理，noCache(c) 写在 ServeHTTP 之前有时会被覆盖。
	// 现在改为自己从 embed FS 读文件并直接写响应，缓存头可被强制应用。
	// SPA 静态资源是 vite 带 hash 的（文件名变了即内容变），且 MVP 阶段
	// 现场更新频繁——直接禁用浏览器缓存避免老 chunk 导致 SPA mount 失败。
	noCache := func(c *gin.Context) {
		c.Header("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
		c.Header("Pragma", "no-cache")
		c.Header("Expires", "0")
	}

	// 静态资源：URL.Path = "assets/<rel>"（gin 通配 *filepath 含前导 /）。
	// 自己读文件 + 强制 no-cache，避免浏览器缓存老 chunk 出现 Dashboard 还在用旧代码
	// （http.FileServer + gin 的 header 注入顺序在某些路径下会被覆盖）。
	r.GET("/assets/*filepath", func(c *gin.Context) {
		rel := c.Param("filepath")
		// 防路径穿越
		if strings.Contains(rel, "..") {
			c.Status(http.StatusBadRequest)
			return
		}
		full := path.Join("assets", rel)
		f, err := sub.Open(full)
		if err != nil {
			c.Status(http.StatusNotFound)
			return
		}
		defer f.Close()
		stat, err := f.Stat()
		if err != nil || stat.IsDir() {
			c.Status(http.StatusNotFound)
			return
		}
		// 强制不缓存（hash 变了文件名也变，缓存价值低）
		c.Header("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
		c.Header("Pragma", "no-cache")
		c.Header("Expires", "0")
		// Vite 输出文件名自带 .js/.css/.svg/.png 等后缀，按后缀给 MIME
		ext := path.Ext(rel)
		mime := mimeByExt(ext)
		if mime != "" {
			c.Header("Content-Type", mime)
		}
		c.Status(http.StatusOK)
		_, _ = io.Copy(c.Writer, f)
	})
	r.HEAD("/assets/*filepath", func(c *gin.Context) {
		rel := c.Param("filepath")
		if strings.Contains(rel, "..") {
			c.Status(http.StatusBadRequest)
			return
		}
		full := path.Join("assets", rel)
		f, err := sub.Open(full)
		if err != nil {
			c.Status(http.StatusNotFound)
			return
		}
		defer f.Close()
		stat, err := f.Stat()
		if err != nil || stat.IsDir() {
			c.Status(http.StatusNotFound)
			return
		}
		c.Header("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
		c.Header("Pragma", "no-cache")
		c.Header("Expires", "0")
		ext := path.Ext(rel)
		mime := mimeByExt(ext)
		if mime != "" {
			c.Header("Content-Type", mime)
		}
		c.Status(http.StatusOK)
	})

	// 根路径 = index.html
	r.GET("/", func(c *gin.Context) {
		noCache(c)
		c.Data(http.StatusOK, "text/html; charset=utf-8", indexBytes)
	})

	// SPA fallback：所有非 /api/* 的 GET/HEAD/OPTIONS 都返回 index.html（前端 SPA 接管路由）
	// OPTIONS 也走 SPA fallback，避免被当作 405 返回（即使 CORS 中间件已处理 OPTIONS，MountSPA 仍要兜底）
	r.NoRoute(func(c *gin.Context) {
		if strings.HasPrefix(c.Request.URL.Path, "/api/") {
			// Plan2 4.1：404 也走 {code, msg} 统一格式
			WriteError(c, ErrAPINotFound.With("api route not found"))
			return
		}
		if c.Request.Method != http.MethodGet && c.Request.Method != http.MethodHead && c.Request.Method != http.MethodOptions {
			// Plan2 4.1：405 也走 {code, msg} 统一格式
			WriteError(c, &APIError{
				Code: "method_not_allowed", Message: "method not allowed", HTTPStatus: http.StatusMethodNotAllowed,
			})
			return
		}
		// 关键：index.html 强制不缓存。否则浏览器用旧 index.html 引用旧 hash 的
		// /assets/channels-*.js chunk，导致前端代码不更新。/assets/* 自身也 no-store
		// 兜底，但 index.html 不缓存是最干净的解。
		noCache(c)
		c.Data(http.StatusOK, "text/html; charset=utf-8", indexBytes)
	})
}

// mountFallbackSPA 在前端产物缺失时挂载占位页。
//
// 行为：
//   - GET /            → 占位页（说明如何修复）
//   - GET /assets/...  → 404（确实没有资源）
//   - 其它 GET/HEAD    → 占位页（SPA fallback 语义保持一致）
//
// API 路由（/api/v1/*）由 Gin 的其它 handler 正常处理，不受影响。
func mountFallbackSPA(r *gin.Engine) {
	r.GET("/assets/*filepath", func(c *gin.Context) {
		c.Status(http.StatusNotFound)
	})
	r.HEAD("/assets/*filepath", func(c *gin.Context) {
		c.Status(http.StatusNotFound)
	})
	r.GET("/", func(c *gin.Context) {
		c.Header("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
		c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(spaFallbackHTML))
	})
	r.NoRoute(func(c *gin.Context) {
		if strings.HasPrefix(c.Request.URL.Path, "/api/") {
			WriteError(c, ErrAPINotFound.With("api route not found"))
			return
		}
		if c.Request.Method != http.MethodGet && c.Request.Method != http.MethodHead && c.Request.Method != http.MethodOptions {
			WriteError(c, &APIError{
				Code: "method_not_allowed", Message: "method not allowed", HTTPStatus: http.StatusMethodNotAllowed,
			})
			return
		}
		c.Header("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
		c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(spaFallbackHTML))
	})
}

func readIndex(fsys fs.FS) ([]byte, error) {
	f, err := fsys.Open("index.html")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

// mimeByExt 给静态资源按后缀返回 MIME。Vite build 产物常见类型即可。
func mimeByExt(ext string) string {
	switch strings.ToLower(ext) {
	case ".js", ".mjs":
		return "application/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".html":
		return "text/html; charset=utf-8"
	case ".json":
		return "application/json; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".ico":
		return "image/x-icon"
	case ".woff":
		return "font/woff"
	case ".woff2":
		return "font/woff2"
	case ".ttf":
		return "font/ttf"
	case ".map":
		return "application/json; charset=utf-8"
	default:
		return ""
	}
}

// assetFilePath 抽出 helper，备用于 build 阶段资源定位（当前未用，留接口）。
func assetFilePath(p string) string { return path.Join("assets", p) }
