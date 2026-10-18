// 内嵌 H5 SPA（前端构建产物 dist/）。
//
// 构建命令（Vite）：
//   cd web && npm install && npm run build
//   输出 dist/ 自动嵌入到 plcbridge-web 二进制
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
)

//go:embed all:dist
var distFS embed.FS

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
		// dist 必须存在（go:embed 编译期保证）；缺失即 panic
		panic(err)
	}
	indexBytes, err := readIndex(sub)
	if err != nil {
		panic(err)
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
			c.JSON(http.StatusNotFound, gin.H{"error": "api route not found"})
			return
		}
		if c.Request.Method != http.MethodGet && c.Request.Method != http.MethodHead && c.Request.Method != http.MethodOptions {
			c.JSON(http.StatusMethodNotAllowed, gin.H{"error": "method not allowed"})
			return
		}
		// 关键：index.html 强制不缓存。否则浏览器用旧 index.html 引用旧 hash 的
		// /assets/channels-*.js chunk，导致前端代码不更新。/assets/* 自身也 no-store
		// 兜底，但 index.html 不缓存是最干净的解。
		noCache(c)
		c.Data(http.StatusOK, "text/html; charset=utf-8", indexBytes)
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
