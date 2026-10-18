// health + ready 端点测试（Phase 1.5）。
package web

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// === /api/v1/health（liveness）测试 ===

func TestHealthHandler_OK(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/api/v1/health", healthHandler(nil))

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/health", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status: got %d, want 200", w.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf("status: got %v, want ok", body["status"])
	}
	process, ok := body["process"].(map[string]any)
	if !ok {
		t.Fatal("process 字段缺失")
	}
	// 必填字段
	for _, k := range []string{"pid", "started_at_ms", "uptime_ms", "num_goroutine", "go_version", "num_cpu"} {
		if _, ok := process[k]; !ok {
			t.Errorf("process.%s 缺失", k)
		}
	}
	if v, ok := body["version"]; !ok || v == "" {
		t.Errorf("version 字段应存在且非空")
	}
}

func TestHealthHandler_DBPingOK(t *testing.T) {
	// 用 sql.Open(":memory:") 拿一个能 Ping 成功的 db
	db, _ := sql.Open("sqlite", ":memory:")
	defer db.Close()

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/api/v1/health", healthHandler(db))

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/health", nil)
	r.ServeHTTP(w, req)

	var body map[string]any
	json.Unmarshal(w.Body.Bytes(), &body)
	if body["db"] != "ok" {
		t.Errorf("db field: got %v, want ok", body["db"])
	}
	if body["status"] != "ok" {
		t.Errorf("status: got %v, want ok", body["status"])
	}
}

// stubDBPinger 实现 Ping 用于模拟 DB 失败
type stubDBPinger struct{ err error }

func (s *stubDBPinger) Ping() error { return s.err }

// === /api/v1/health/ready 测试 ===

func TestReadyHandler_AllOK(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/ready", readyHandler(&stubDBPinger{err: nil}))

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/ready", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status: got %d, want 200", w.Code)
	}
	var body map[string]any
	json.Unmarshal(w.Body.Bytes(), &body)
	if body["status"] != "ready" {
		t.Errorf("status: got %v, want ready", body["status"])
	}
	checks := body["checks"].(map[string]any)
	if checks["db"].(map[string]any)["status"] != "ok" {
		t.Errorf("db check should be ok")
	}
}

func TestReadyHandler_DBPingFails(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/ready", readyHandler(&stubDBPinger{err: errors.New("db locked")}))

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/ready", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status: got %d, want 503", w.Code)
	}
	var body map[string]any
	json.Unmarshal(w.Body.Bytes(), &body)
	if body["status"] != "unready" {
		t.Errorf("status: got %v, want unready", body["status"])
	}
	checks := body["checks"].(map[string]any)
	if checks["db"].(map[string]any)["status"] != "fail" {
		t.Errorf("db check should be fail")
	}
}

func TestReadyHandler_NilDB(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/ready", readyHandler(nil))

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/ready", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("nil db: got %d, want 503", w.Code)
	}
	var body map[string]any
	json.Unmarshal(w.Body.Bytes(), &body)
	if body["status"] != "unready" {
		t.Errorf("nil db should be unready")
	}
}

func TestReadyHandler_WithConfigProvider(t *testing.T) {
	// 注入 mock config provider 返回 ok
	SetConfigHealthProvider(&fakeConfigHealth{ok: true})
	defer SetConfigHealthProvider(nil) // 测试间清理

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/ready", readyHandler(&stubDBPinger{err: nil}))

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/ready", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("with ok config: got %d, want 200", w.Code)
	}
	var body map[string]any
	json.Unmarshal(w.Body.Bytes(), &body)
	checks := body["checks"].(map[string]any)
	if checks["config"].(map[string]any)["status"] != "ok" {
		t.Errorf("config check should be ok")
	}
}

func TestReadyHandler_ConfigFails(t *testing.T) {
	SetConfigHealthProvider(&fakeConfigHealth{ok: false})
	defer SetConfigHealthProvider(nil)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/ready", readyHandler(&stubDBPinger{err: nil}))

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/ready", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("config fail: got %d, want 503", w.Code)
	}
}

// === SetVersion 测试 ===

func TestSetVersion(t *testing.T) {
	orig := Version
	defer func() { Version = orig }()

	SetVersion("v1.2.3")
	if Version != "v1.2.3" {
		t.Errorf("SetVersion: got %q, want v1.2.3", Version)
	}
	SetVersion("") // 空字符串不修改
	if Version != "v1.2.3" {
		t.Errorf("empty SetVersion should not clear")
	}
}

// === helper ===

type fakeConfigHealth struct{ ok bool }

func (f *fakeConfigHealth) Check() gin.H {
	if f.ok {
		return gin.H{"status": "ok"}
	}
	return gin.H{"status": "fail", "error": "config 未加载"}
}
