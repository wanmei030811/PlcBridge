// Plan2 主题 A 3.3 + 3.4 + 3.5: GET /api/v1/protocols 端到端测试。
//
// 验证：
//  1. 无 auth 也可访问 → 200（Plan2 3.4：协议元数据公开，无敏感信息）
//  2. 响应 shape 正确：{collect: [...], forward: [...]}
//  3. 每个 entry 含 name / version / role / config_schema
//  4. 已知 MVP 协议（s7 / modbus_tcp / opcua）+ Target（mqtt / influxdb）都在
//  5. 错误方法（POST）被拒
//  6. Plan2 3.5 fake 协议也在响应中（通过 import _ fake 子包触发注册）
package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/plcbridge/plcbridge/internal/infra/config"
	"github.com/plcbridge/plcbridge/internal/infra/pool"
	_ "github.com/plcbridge/plcbridge/internal/infra/protocol/fake"    // Plan2 3.5
	_ "github.com/plcbridge/plcbridge/internal/infra/protocol/plugins" // 注册内置协议
	_ "github.com/plcbridge/plcbridge/internal/infra/target/plugins"   // 注册内置 Target
)

func init() {
	// 减少测试输出
	gin.SetMode(gin.TestMode)
}

// newTestServerForProtocols 构造一个最小可用的 Server（不启动 listener）。
//
// 依赖：plugin init() 自注册已生效 → protocol.AllInfo() / target.AllInfo() 返回 MVP 内置项。
func newTestServerForProtocols(t *testing.T) http.Handler {
	t.Helper()
	store := config.NewStore("")
	pools := pool.NewPools(pool.Config{CollectSize: 1, ForwardSize: 1, WriteSize: 1, ConfigSize: 1})
	return NewServer(store, pools, ":0").routes()
}

// TestListProtocols_PublicOK_NoAuth 验证无 auth 也能 200 + 正确 shape。
//
// Plan2 3.4：协议元数据公开（与 /health 同等），前端通过 web 子进程同源拉取不需要 service token。
func TestListProtocols_PublicOK_NoAuth(t *testing.T) {
	r := newTestServerForProtocols(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/protocols", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("HTTP 状态 = %d, want 200. body: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Collect []map[string]any `json:"collect"`
		Forward []map[string]any `json:"forward"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("JSON 解析失败: %v. body: %s", err, w.Body.String())
	}

	// 数量下限
	if len(resp.Collect) < 3 {
		t.Errorf("collect 数量 = %d, want >= 3", len(resp.Collect))
	}
	if len(resp.Forward) < 2 {
		t.Errorf("forward 数量 = %d, want >= 2", len(resp.Forward))
	}

	// 字段完整性 + role 正确
	for i, e := range resp.Collect {
		for _, key := range []string{"name", "version", "role", "config_schema"} {
			if _, ok := e[key]; !ok {
				t.Errorf("collect[%d] 缺字段 %q", i, key)
			}
		}
		if e["role"] != "collect" {
			t.Errorf("collect[%d].role = %v, want \"collect\"", i, e["role"])
		}
	}
	for i, e := range resp.Forward {
		for _, key := range []string{"name", "version", "role", "config_schema"} {
			if _, ok := e[key]; !ok {
				t.Errorf("forward[%d] 缺字段 %q", i, key)
			}
		}
		if e["role"] != "forward" {
			t.Errorf("forward[%d].role = %v, want \"forward\"", i, e["role"])
		}
	}

	// 已知 MVP 协议都在
	knownCollect := map[string]bool{"s7": false, "modbus_tcp": false, "opcua": false}
	for _, e := range resp.Collect {
		if name, ok := e["name"].(string); ok {
			if _, known := knownCollect[name]; known {
				knownCollect[name] = true
			}
		}
	}
	for name, seen := range knownCollect {
		if !seen {
			t.Errorf("collect 缺已知协议 %q", name)
		}
	}
	knownForward := map[string]bool{"mqtt": false, "influxdb": false}
	for _, e := range resp.Forward {
		if name, ok := e["name"].(string); ok {
			if _, known := knownForward[name]; known {
				knownForward[name] = true
			}
		}
	}
	for name, seen := range knownForward {
		if !seen {
			t.Errorf("forward 缺已知 Target %q", name)
		}
	}

	// Plan2 3.5 验收：fake 协议必须在 collect 列表中（证明 import _ fake 子包 → init() 自注册 → /api/v1/protocols 可见）
	fakeSeen := false
	for _, e := range resp.Collect {
		if e["name"] == "fake" {
			fakeSeen = true
			break
		}
	}
	if !fakeSeen {
		t.Errorf("Plan2 3.5：collect 列表缺 fake（import _ fake 子包未生效？）")
	}
}

// TestListProtocols_WrongMethod 验证 POST 等被拒（路由只注册 GET）。
func TestListProtocols_WrongMethod(t *testing.T) {
	r := newTestServerForProtocols(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/protocols", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code == http.StatusOK {
		t.Errorf("POST /api/v1/protocols 不应成功 (got 200)")
	}
	// gin 对未注册 method 返回 404 或 405
	if w.Code != http.StatusNotFound && w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /api/v1/protocols 应返回 404/405, got %d", w.Code)
	}
}
