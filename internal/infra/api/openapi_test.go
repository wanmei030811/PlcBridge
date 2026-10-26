// Plan3 3.9 + 2026-10-24 补齐：OpenAPI spec 与实际路由的一致性测试。
//
// 背景：2026-10-24 核对 Plan3 交付状态时发现 api/openapi.yaml 漏了 4 个已实现的端点
//
//	（/points/import、/points/export、/devices/{id}/clone、/channels/runtime），
//	Swagger UI 上看不到，用户按文档调会 404。
//
// 这个测试的作用是**锁住**这件事：新增 /api/v1 路由时，要么在 openapi.yaml 里补文档，
// 要么这个测试失败 —— 避免 spec 再次悄悄漂移。
package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/plcbridge/plcbridge/internal/infra/config"
	"github.com/plcbridge/plcbridge/internal/infra/pool"
)

// loadOpenAPISpec 读 api/openapi.yaml。
//
// 查找顺序：
//  1. 环境变量 PLCBRIDGE_OPENAPI（测试/CI 可显式指定）
//  2. 从本包目录向上找 api/openapi.yaml（go test 的工作目录是包目录）
func loadOpenAPISpec(t *testing.T) string {
	t.Helper()

	if p := os.Getenv("PLCBRIDGE_OPENAPI"); p != "" {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("读取 PLCBRIDGE_OPENAPI=%s 失败: %v", p, err)
		}
		return string(b)
	}

	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	for i := 0; i < 6; i++ {
		p := filepath.Join(dir, "api", "openapi.yaml")
		if b, err := os.ReadFile(p); err == nil {
			return string(b)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Skip("找不到 api/openapi.yaml（跳过 spec 一致性测试）")
	return ""
}

// specPathRE 匹配 openapi.yaml 里的 path 条目（如 "  /api/v1/points/import:"）。
var specPathRE = regexp.MustCompile(`(?m)^  (/[^\s:]+):\s*$`)

// specPaths 提取 spec 里声明的所有 path。
func specPaths(spec string) map[string]bool {
	out := map[string]bool{}
	for _, m := range specPathRE.FindAllStringSubmatch(spec, -1) {
		out[m[1]] = true
	}
	return out
}

// routePathRE 从 gin 路由里提取路径模板（如 "/api/v1/points/:id"）。
var routePathRE = regexp.MustCompile(`/api/v1/[^\s]*`)

// registeredAPIPaths 返回 Server 上注册的所有 /api/v1 路径。
//
// 返回两个 map：
//   - raw   ：原始路径模板（用于报错展示，保留 :id 形态）
//   - norm  ：占位符归一化后的路径（用于与 spec 对比）
func registeredAPIPaths(r *gin.Engine) (raw map[string]bool, norm map[string]bool) {
	raw = map[string]bool{}
	norm = map[string]bool{}
	for _, ri := range r.Routes() {
		p := ri.Path
		if !strings.HasPrefix(p, "/api/v1/") {
			continue
		}
		raw[p] = true
		norm[specPathToPattern(p)] = true
	}
	return raw, norm
}

// specPathToPattern 把 spec 里的 {id} 占位符转成统一的占位符形式，便于与 gin 路由对比。
//
// gin 用 ":id"，OpenAPI 用 "{id}"，两者语义相同但字面不同，
// 所以两边都要归一化成同一个占位符再比。
func specPathToPattern(p string) string {
	return regexp.MustCompile(`\{[^}]+\}|:[^/]+`).ReplaceAllString(p, "{}")
}

// TestOpenAPISpec_CoversRegisteredRoutes 验证 openapi.yaml 覆盖了所有已注册的 /api/v1 路由。
//
// 对比方式：把 spec 里的 {id} 换成 :x，再和 gin 的 :id 路由模板比对。
// 这样 "/api/v1/devices/{id}" 能匹配 gin 的 "/api/v1/devices/:id"。
func TestOpenAPISpec_CoversRegisteredRoutes(t *testing.T) {
	spec := loadOpenAPISpec(t)
	declared := specPaths(spec)

	store := config.NewStore("")
	pools := pool.NewPools(pool.Config{CollectSize: 1, ForwardSize: 1, WriteSize: 1, ConfigSize: 1})
	h := NewServer(store, pools, ":0").routes()
	r, ok := h.(*gin.Engine)
	if !ok {
		t.Fatalf("routes() 返回 %T，期望 *gin.Engine", h)
	}

	registeredRaw, registered := registeredAPIPaths(r)

	// spec 里声明的 path 模板（占位符统一成 :x）
	declaredPatterns := map[string]bool{}
	for p := range declared {
		declaredPatterns[specPathToPattern(p)] = true
	}

	var missing []string
	for p := range registered {
		if declaredPatterns[p] {
			continue
		}
		missing = append(missing, p)
	}
	if len(missing) > 0 {
		// 报错时用原始路径（保留 :id 形态），更贴近开发者看到的路由。
		var rawMissing []string
		for p := range registeredRaw {
			if !declaredPatterns[specPathToPattern(p)] {
				rawMissing = append(rawMissing, p)
			}
		}
		sort.Strings(rawMissing)
		t.Errorf("openapi.yaml 漏了 %d 个已注册的路由（Swagger UI 上看不到）：\n  %s",
			len(rawMissing), strings.Join(rawMissing, "\n  "))
	}
}

// TestOpenAPISpec_HasPlan3Endpoints 显式锁定 Plan3 新增的关键端点。
//
// 回归测试（2026-10-24）：这 4 个端点已实现但漏在 spec 外。
//
// 注意路径必须与**实际路由**一致：
//   - /points/import、/points/export、/devices/{id}/clone 在 web 端（:8080）
//   - 通道运行时状态在 web 端是 /plcbridge/channels/runtime
//     （早先 spec 写的是 /channels/runtime，实际 400 —— 文档漂移）
func TestOpenAPISpec_HasPlan3Endpoints(t *testing.T) {
	spec := loadOpenAPISpec(t)
	declared := specPaths(spec)

	want := []string{
		"/api/v1/points/import",              // Plan3 3.1 CSV 导入
		"/api/v1/points/export",              // Plan3 3.1 CSV 导出
		"/api/v1/devices/{id}/clone",         // Plan3 3.7 设备克隆
		"/api/v1/plcbridge/channels/runtime", // Plan3 3.2 通道运行时状态
	}
	for _, p := range want {
		if !declared[p] {
			t.Errorf("openapi.yaml 缺端点 %s", p)
		}
	}
}

// TestOpenAPISpec_DiagDocumentsNewFields 验证 diag 端点文档里有新加的字段。
//
// 回归测试（2026-10-24）：后端加了 type / format / last_error，但 spec 里还停留在旧字段集。
func TestOpenAPISpec_DiagDocumentsNewFields(t *testing.T) {
	spec := loadOpenAPISpec(t)

	// 取出 /api/v1/targets/{biz_id}/diag 这一段的文本。
	idx := strings.Index(spec, "  /api/v1/targets/{biz_id}/diag:")
	if idx < 0 {
		t.Fatal("openapi.yaml 缺 /api/v1/targets/{biz_id}/diag")
	}
	rest := spec[idx:]
	if next := strings.Index(rest[1:], "\n  /api/"); next >= 0 {
		rest = rest[:next+1]
	}

	for _, field := range []string{"format:", "last_error:", "queue_len:", "dropped_total:"} {
		if !strings.Contains(rest, field) {
			t.Errorf("diag 端点文档缺字段 %s", field)
		}
	}
}

// TestOpenAPIYAML_ServedByServer 验证 /api/openapi.yaml 能正常吐出 YAML。
func TestOpenAPIYAML_ServedByServer(t *testing.T) {
	r := newTestServerForProtocols(t)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/openapi.yaml", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200. body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "openapi:") {
		t.Errorf("响应不含 OpenAPI 文档（body 前 200 字符: %s）", truncate(body, 200))
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// TestOpenAPISpec_CoversWebRoutes 验证 openapi.yaml 覆盖了 web 端（:8080）的 /api/v1 路由。
//
// 回归测试（2026-10-24）：spec 写的是 /api/v1/channels/runtime，
// 但 web 端实际注册的是 /api/v1/plcbridge/channels/runtime ——
// 用户按 Swagger 文档调 → 400。
//
// 根因：TestOpenAPISpec_CoversRegisteredRoutes 只校验了**核心进程**（:8082）的路由，
// 而 spec 描述的是整个产品（web + 核心）。web 端的路由从不被校验 → 文档漂移无人发现。
//
// 这个测试把 web 端路由也纳入校验范围。
func TestOpenAPISpec_CoversWebRoutes(t *testing.T) {
	spec := loadOpenAPISpec(t)
	declaredPatterns := map[string]bool{}
	for p := range specPaths(spec) {
		declaredPatterns[specPathToPattern(p)] = true
	}

	// web 端路由由 internal/interface/web 注册，这里通过读取注册表文本
	// 拿到路径清单，避免 api 包 import web 包造成循环依赖。
	webRoutes := extractWebRoutePaths(t)

	var missing []string
	for _, p := range webRoutes {
		if declaredPatterns[specPathToPattern(p)] {
			continue
		}
		missing = append(missing, p)
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("openapi.yaml 漏了 %d 个 web 端路由（Swagger UI 上看不到）：\n  %s",
			len(missing), strings.Join(missing, "\n  "))
	}
}

// extractWebRoutePaths 从 web 包的路由注册代码里提取 /api/v1 路径。
//
// 做法：读 internal/interface/web/web.go，正则匹配 auth.GET/POST/PUT/DELETE("/...")。
// 这不是最优雅的方式（理想是导出路由表），但避免了 api ↔ web 的循环依赖，
// 且 web.go 的注册写法是稳定的。
func extractWebRoutePaths(t *testing.T) []string {
	t.Helper()

	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	var src []byte
	for i := 0; i < 6; i++ {
		p := filepath.Join(dir, "internal", "interface", "web", "web.go")
		if b, err := os.ReadFile(p); err == nil {
			src = b
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	if src == nil {
		t.Skip("找不到 internal/interface/web/web.go（跳过 web 路由覆盖测试）")
	}

	// 匹配 auth.METHOD("/path" 与 r.METHOD("/path"
	re := regexp.MustCompile(`(?:auth|r)\.(?:GET|POST|PUT|DELETE)\("(/api/v1[^"]*)"`)
	seen := map[string]bool{}
	var out []string
	for _, m := range re.FindAllStringSubmatch(string(src), -1) {
		if seen[m[1]] {
			continue
		}
		seen[m[1]] = true
		out = append(out, m[1])
	}
	if len(out) == 0 {
		t.Fatal("没从 web.go 里解析出任何 /api/v1 路由（正则可能失效）")
	}
	return out
}
