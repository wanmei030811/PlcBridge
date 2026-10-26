// config_status_test.go：配置脏状态端点（GET /api/v1/config/status）的测试。
//
// 为什么需要这个端点：
//
//	用户改完配置后必须点「导出配置」把 SQLite 状态写进 config.json，
//	plcbridge 才会热重载。前端顶栏按钮靠这个端点显示"有未导出的改动"角标，
//	所以判定逻辑（dirty / pending_changes）必须准确，不能只靠肉眼。
package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// authReq 构造带登录 Cookie 的 GET 请求。
func authGet(path, cookie string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Cookie", SessionCookieName+"="+cookie)
	return req
}

// authPost 构造带登录 Cookie 的 POST 请求。
func authPost(path, cookie string, body any) *http.Request {
	req := jsonReq(http.MethodPost, path, body)
	req.Header.Set("Cookie", SessionCookieName+"="+cookie)
	return req
}

// authPut 构造带登录 Cookie 的 PUT 请求。
func authPut(path, cookie string, body any) *http.Request {
	req := jsonReq(http.MethodPut, path, body)
	req.Header.Set("Cookie", SessionCookieName+"="+cookie)
	return req
}

// TestConfigStatus_CleanWhenNoChanges 验证：没有配置改动时 dirty=false。
func TestConfigStatus_CleanWhenNoChanges(t *testing.T) {
	r, cleanup := newTestRouter(t)
	defer cleanup()
	cookie := loginAndCookie(t, r)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, authGet("/api/v1/config/status", cookie))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200. body: %s", w.Code, w.Body.String())
	}
	var resp configStatusResp
	decodeJSON(t, w, &resp)
	if !resp.OK {
		t.Errorf("ok = false, want true")
	}
	if resp.Dirty {
		t.Errorf("没有任何改动时 dirty 应为 false（last_change=%d last_export=%d）",
			resp.LastChangeMs, resp.LastExportMs)
	}
}

// TestConfigStatus_DirtyAfterChange 验证：改了配置后 dirty=true，且 pending_changes>0。
//
// 这是核心回归：角标逻辑依赖它。
func TestConfigStatus_DirtyAfterChange(t *testing.T) {
	r, cleanup := newTestRouter(t)
	defer cleanup()
	cookie := loginAndCookie(t, r)

	// 先查一次基线（此时还没建通道）
	get := func() configStatusResp {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, authGet("/api/v1/config/status", cookie))
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200. body: %s", w.Code, w.Body.String())
		}
		var resp configStatusResp
		decodeJSON(t, w, &resp)
		return resp
	}

	before := get()
	if before.Dirty {
		t.Fatalf("改动前 dirty 不应为 true")
	}

	// 建一个通道（会写 updated_at = now）
	w := httptest.NewRecorder()
	body := map[string]any{
		"channel_biz_id": "ch_status_test",
		"role":           "collect",
		"type":           "modbus_tcp",
		"address":        "127.0.0.1:502",
	}
	r.ServeHTTP(w, authPost("/api/v1/channels", cookie, body))
	if w.Code != http.StatusOK && w.Code != http.StatusCreated {
		t.Fatalf("create channel status = %d, body: %s", w.Code, w.Body.String())
	}

	// updated_at 是毫秒级；确保与 baseline 有可见差异。
	time.Sleep(5 * time.Millisecond)

	after := get()
	if !after.Dirty {
		t.Errorf("改了配置后 dirty 应为 true（last_change=%d last_export=%d）",
			after.LastChangeMs, after.LastExportMs)
	}
	if after.PendingChanges < 1 {
		t.Errorf("pending_changes = %d, want >= 1", after.PendingChanges)
	}
	if after.LastChangeMs <= before.LastChangeMs {
		t.Errorf("last_change_ms 应递增: before=%d after=%d",
			before.LastChangeMs, after.LastChangeMs)
	}
}

// TestConfigStatus_CleanAfterExport 验证：导出 config.json 后 dirty 变回 false。
//
// 这是完整闭环：改配置 → dirty → 导出 → clean。
func TestConfigStatus_CleanAfterExport(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	db, err := OpenDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	cfgDir := filepath.Join(dir, "cfg")
	sess := NewSessionStore(RandomHMACKey())
	if _, err := EnsureAccount(db, "admin", "Password123!"); err != nil {
		t.Fatal(err)
	}

	r := gin.New()
	RegisterRoutes(r, db, cfgDir, sess)
	cookie := loginAndCookie(t, r)

	// 建通道 → 产生改动
	w := httptest.NewRecorder()
	r.ServeHTTP(w, authPost("/api/v1/channels", cookie, map[string]any{
		"channel_biz_id": "ch_export_test",
		"role":           "collect",
		"type":           "modbus_tcp",
		"address":        "127.0.0.1:502",
	}))
	if w.Code != http.StatusOK && w.Code != http.StatusCreated {
		t.Fatalf("create channel status = %d, body: %s", w.Code, w.Body.String())
	}

	// 导出 config.json
	w = httptest.NewRecorder()
	r.ServeHTTP(w, authPost("/api/v1/snapshot", cookie, map[string]any{}))
	if w.Code != http.StatusOK {
		t.Fatalf("snapshot status = %d, body: %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(cfgDir, "config.json")); err != nil {
		t.Fatalf("config.json 未生成: %v", err)
	}

	// 导出后应干净
	w = httptest.NewRecorder()
	r.ServeHTTP(w, authGet("/api/v1/config/status", cookie))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200. body: %s", w.Code, w.Body.String())
	}
	var resp configStatusResp
	decodeJSON(t, w, &resp)
	if resp.Dirty {
		t.Errorf("导出后 dirty 应为 false（last_change=%d last_export=%d）",
			resp.LastChangeMs, resp.LastExportMs)
	}
	if resp.LastExportMs == 0 {
		t.Errorf("last_export_ms 不应为 0（config.json 已存在）")
	}
}

// TestConfigStatus_RequiresLogin 验证：未登录访问返回 401。
func TestConfigStatus_RequiresLogin(t *testing.T) {
	r, cleanup := newTestRouter(t)
	defer cleanup()

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/config/status", nil))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

// TestConfigStatus_ResponseShape 验证响应字段名与前端 api/config.ts 对齐。
//
// 字段名写错 → 前端拿到 undefined → 角标不显示（静默失效），很难查。
func TestConfigStatus_ResponseShape(t *testing.T) {
	r, cleanup := newTestRouter(t)
	defer cleanup()
	cookie := loginAndCookie(t, r)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, authGet("/api/v1/config/status", cookie))

	var raw map[string]any
	decodeJSON(t, w, &raw)

	for _, key := range []string{"ok", "config_path", "last_export_ms", "last_change_ms", "dirty", "pending_changes"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("响应缺字段 %s（前端 api/config.ts 依赖它）", key)
		}
	}
	b, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) == 0 {
		t.Error("响应体为空")
	}
}
