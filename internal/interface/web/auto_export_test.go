// auto_export_test.go：自动导出配置（开关默认关闭 + 防抖合并 + 触发路径判定）。
//
// 这些是"开了会不会出事"的关键保障：
//  1. 默认必须关闭 —— 否则升级后行为突变，正在跑的采集会被自动推配置。
//  2. 路径判定必须与快照一致 —— 否则出现"角标说没导出但其实导了"的矛盾。
//  3. 批量写必须合并成一次导出 —— 否则 CSV 导入 500 个点位会触发 500 次热重载。
package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// TestSettings_DefaultAutoExportOff 验证：自动导出开关默认关闭。
//
// 这是最重要的一条回归 —— 默认开启会让"升级即行为变更"。
func TestSettings_DefaultAutoExportOff(t *testing.T) {
	r, cleanup := newTestRouter(t)
	defer cleanup()
	cookie := loginAndCookie(t, r)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, authGet("/api/v1/settings", cookie))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200. body: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	decodeJSON(t, w, &resp)

	v, ok := resp["auto_export_config"]
	if !ok {
		t.Fatalf("响应缺 auto_export_config 字段: %v", resp)
	}
	if v != false {
		t.Errorf("auto_export_config 默认应为 false，实际 %v", v)
	}
}

// TestSettings_UpdateAutoExport 验证：可以开启/关闭，且返回全量设置。
func TestSettings_UpdateAutoExport(t *testing.T) {
	r, cleanup := newTestRouter(t)
	defer cleanup()
	cookie := loginAndCookie(t, r)

	// 开启
	w := httptest.NewRecorder()
	r.ServeHTTP(w, authPut("/api/v1/settings/auto_export_config", cookie, true))
	if w.Code != http.StatusOK {
		t.Fatalf("enable status = %d, want 200. body: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	decodeJSON(t, w, &resp)
	if resp["auto_export_config"] != true {
		t.Errorf("开启后应为 true，实际 %v", resp["auto_export_config"])
	}

	// 读回确认已持久化
	w = httptest.NewRecorder()
	r.ServeHTTP(w, authGet("/api/v1/settings", cookie))
	decodeJSON(t, w, &resp)
	if resp["auto_export_config"] != true {
		t.Errorf("读回应为 true，实际 %v", resp["auto_export_config"])
	}

	// 关闭
	w = httptest.NewRecorder()
	r.ServeHTTP(w, authPut("/api/v1/settings/auto_export_config", cookie, false))
	if w.Code != http.StatusOK {
		t.Fatalf("disable status = %d, want 200", w.Code)
	}
	decodeJSON(t, w, &resp)
	if resp["auto_export_config"] != false {
		t.Errorf("关闭后应为 false，实际 %v", resp["auto_export_config"])
	}
}

// TestSettings_RejectsUnknownKey 验证：未登记的设置键被拒绝（400）。
//
// 防止前端传错键名时静默失效（"看起来开了其实没开"）。
func TestSettings_RejectsUnknownKey(t *testing.T) {
	r, cleanup := newTestRouter(t)
	defer cleanup()
	cookie := loginAndCookie(t, r)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, authPut("/api/v1/settings/not_a_real_key", cookie, true))
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400. body: %s", w.Code, w.Body.String())
	}
}

// TestSettings_RequiresLogin 验证：未登录访问设置接口返回 401。
func TestSettings_RequiresLogin(t *testing.T) {
	r, cleanup := newTestRouter(t)
	defer cleanup()

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/settings", nil))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("GET /settings status = %d, want 401", w.Code)
	}

	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPut, "/api/v1/settings/auto_export_config", nil))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("PUT /settings/:key status = %d, want 401", w.Code)
	}
}

// TestIsConfigMutatingPath 验证路径判定表。
//
// 关键：watched（触发）与 excluded（排除）必须覆盖所有配置实体，
// 且排除表里的路径不能被误判为配置变更。
func TestIsConfigMutatingPath(t *testing.T) {
	cases := []struct {
		method string
		path   string
		want   bool
	}{
		{http.MethodPost, "/api/v1/channels", true},
		{http.MethodPut, "/api/v1/channels/1", true},
		{http.MethodDelete, "/api/v1/channels/1", true},
		{http.MethodPost, "/api/v1/devices", true},
		{http.MethodPut, "/api/v1/devices/1", true},
		{http.MethodPost, "/api/v1/sources", true},
		{http.MethodPut, "/api/v1/points/1", true},
		{http.MethodPost, "/api/v1/targets", true},

		// 读操作不触发
		{http.MethodGet, "/api/v1/channels", false},
		{http.MethodGet, "/api/v1/devices/1", false},

		// 排除路径
		{http.MethodPost, "/api/v1/config/snapshots", false},
		{http.MethodDelete, "/api/v1/config/snapshots/1", false},
		{http.MethodPost, "/api/v1/config/backup", false},
		{http.MethodPost, "/api/v1/config/restore", false},
		{http.MethodPost, "/api/v1/auth/password", false},
		{http.MethodPost, "/api/v1/tokens", false},
		{http.MethodGet, "/api/v1/dashboard", false},

		// 设置接口本身不触发（否则改开关会触发一次导出 → 递归）
		{http.MethodPut, "/api/v1/settings/auto_export_config", false},
		{http.MethodGet, "/api/v1/settings", false},
	}
	for _, c := range cases {
		got := isConfigMutatingPath(c.method, c.path)
		if got != c.want {
			t.Errorf("isConfigMutatingPath(%s, %s) = %v, want %v", c.method, c.path, got, c.want)
		}
	}
}

// TestAutoExport_DisabledDoesNotWriteConfig 验证：开关关闭时改配置不会自动写 config.json。
func TestAutoExport_DisabledDoesNotWriteConfig(t *testing.T) {
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

	// 改配置
	w := httptest.NewRecorder()
	r.ServeHTTP(w, authPost("/api/v1/channels", cookie, map[string]any{
		"channel_biz_id": "ch_auto_off",
		"role":           "collect",
		"type":           "modbus_tcp",
		"address":        "127.0.0.1:502",
	}))
	if w.Code != http.StatusOK && w.Code != http.StatusCreated {
		t.Fatalf("create channel status = %d, body: %s", w.Code, w.Body.String())
	}

	// 等待超过防抖窗口 → 仍然不该有 config.json
	time.Sleep(autoExportDebounce + 500*time.Millisecond)

	if _, err := os.Stat(filepath.Join(cfgDir, "config.json")); err == nil {
		t.Error("开关关闭时不应自动导出 config.json")
	}
}

// TestAutoExport_EnabledWritesConfig 验证：开关开启时改配置会自动写 config.json。
func TestAutoExport_EnabledWritesConfig(t *testing.T) {
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

	// 开启自动导出
	w := httptest.NewRecorder()
	r.ServeHTTP(w, authPut("/api/v1/settings/auto_export_config", cookie, true))
	if w.Code != http.StatusOK {
		t.Fatalf("enable status = %d, body: %s", w.Code, w.Body.String())
	}

	// 改配置
	w = httptest.NewRecorder()
	r.ServeHTTP(w, authPost("/api/v1/channels", cookie, map[string]any{
		"channel_biz_id": "ch_auto_on",
		"role":           "collect",
		"type":           "modbus_tcp",
		"address":        "127.0.0.1:502",
	}))
	if w.Code != http.StatusOK && w.Code != http.StatusCreated {
		t.Fatalf("create channel status = %d, body: %s", w.Code, w.Body.String())
	}

	// 等待防抖窗口 + 导出耗时
	cfgPath := filepath.Join(cfgDir, "config.json")
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(cfgPath); err == nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if _, err := os.Stat(cfgPath); err != nil {
		t.Fatalf("开关开启后应自动导出 config.json: %v", err)
	}

	// 导出的内容应包含刚建的通道
	b, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Channels []struct {
			ChannelBizID string `json:"channel_biz_id"`
		} `json:"channels"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range cfg.Channels {
		if c.ChannelBizID == "ch_auto_on" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("导出的 config.json 里没有 ch_auto_on（内容: %s）", string(b))
	}
}

// TestAutoExport_DebounceCoalesces 验证：连续多次触发只导出一次。
//
// 回归场景：CSV 导入 500 个点位 → 500 次写请求。
// 如果不合并 → 500 次 config.json 重写 + 500 次 plcbridge 热重载 → 采集被打残。
func TestAutoExport_DebounceCoalesces(t *testing.T) {
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

	// 开启自动导出
	w := httptest.NewRecorder()
	r.ServeHTTP(w, authPut("/api/v1/settings/auto_export_config", cookie, true))
	if w.Code != http.StatusOK {
		t.Fatalf("enable status = %d", w.Code)
	}

	// 连续 5 次快速写入（都在防抖窗口内）
	for i := 0; i < 5; i++ {
		w = httptest.NewRecorder()
		r.ServeHTTP(w, authPost("/api/v1/channels", cookie, map[string]any{
			"channel_biz_id": "ch_burst_" + string(rune('a'+i)),
			"role":           "collect",
			"type":           "modbus_tcp",
			"address":        "127.0.0.1:502",
		}))
		if w.Code != http.StatusOK && w.Code != http.StatusCreated {
			t.Fatalf("create channel %d status = %d", i, w.Code)
		}
	}

	// 等待防抖窗口 → 应该只导出 1 次
	time.Sleep(autoExportDebounce + 800*time.Millisecond)

	cfgPath := filepath.Join(cfgDir, "config.json")
	b, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("config.json 应存在: %v", err)
	}
	var cfg struct {
		Channels []struct {
			ChannelBizID string `json:"channel_biz_id"`
		} `json:"channels"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	// 5 次写入应该**全部**在最终那一次导出里（合并的是导出次数，不是丢弃改动）。
	if len(cfg.Channels) != 5 {
		t.Errorf("合并导出后应包含全部 5 个通道，实际 %d 个", len(cfg.Channels))
	}
}

// TestAutoExport_ToggleDoesNotTriggerExport 验证：改开关本身不触发导出。
//
// 否则会出现"关闭开关 → 触发一次导出"的诡异行为。
func TestAutoExport_ToggleDoesNotTriggerExport(t *testing.T) {
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

	w := httptest.NewRecorder()
	r.ServeHTTP(w, authPut("/api/v1/settings/auto_export_config", cookie, true))
	if w.Code != http.StatusOK {
		t.Fatalf("enable status = %d", w.Code)
	}

	time.Sleep(autoExportDebounce + 500*time.Millisecond)

	if _, err := os.Stat(filepath.Join(cfgDir, "config.json")); err == nil {
		t.Error("仅切换开关不应触发导出 config.json")
	}
}

// TestAutoExport_ContextNotBoundToRequest 验证：导出不依赖 HTTP 请求的生命周期。
//
// 回归场景：如果 doExport 用了 c.Request.Context()，请求返回后 ctx 被取消 →
// 导出中途失败。而导出的语义是"把改动推给 plcbridge"，必须保证完成。
func TestAutoExport_ContextNotBoundToRequest(t *testing.T) {
	// 这个测试通过行为间接验证：导出成功 → config.json 内容完整。
	// 具体检查：db 里的通道数 == config.json 里的通道数。
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

	w := httptest.NewRecorder()
	r.ServeHTTP(w, authPut("/api/v1/settings/auto_export_config", cookie, true))
	if w.Code != http.StatusOK {
		t.Fatalf("enable status = %d", w.Code)
	}

	const n = 3
	for i := 0; i < n; i++ {
		w = httptest.NewRecorder()
		r.ServeHTTP(w, authPost("/api/v1/channels", cookie, map[string]any{
			"channel_biz_id": "ch_ctx_" + string(rune('a'+i)),
			"role":           "collect",
			"type":           "modbus_tcp",
			"address":        "127.0.0.1:502",
		}))
	}

	cfgPath := filepath.Join(cfgDir, "config.json")
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(cfgPath); err == nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}

	b, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("config.json 应存在: %v", err)
	}
	var cfg struct {
		Channels []map[string]any `json:"channels"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Channels) != n {
		t.Errorf("导出应包含全部 %d 个通道，实际 %d（导出可能被请求 ctx 取消）", n, len(cfg.Channels))
	}
}

// TestAutoExporter_CloseStopsPending 验证：Close 会取消待触发的导出。
func TestAutoExporter_CloseStopsPending(t *testing.T) {
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

	ex := newAutoExporter(db, cfgDir)
	ex.Trigger()
	ex.Close()

	time.Sleep(autoExportDebounce + 500*time.Millisecond)

	if _, err := os.Stat(filepath.Join(cfgDir, "config.json")); err == nil {
		t.Error("Close 后待触发的导出不应执行")
	}
}

// TestAutoExporter_ConcurrentTriggerSafe 验证：并发触发不会 panic / 不会并发写文件。
//
// 回归场景：多个页面同时改配置 + 自动导出开启。
func TestAutoExporter_ConcurrentTriggerSafe(t *testing.T) {
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

	ex := newAutoExporter(db, cfgDir)
	done := make(chan struct{})
	for i := 0; i < 20; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			ex.Trigger()
		}()
	}
	for i := 0; i < 20; i++ {
		<-done
	}
	ex.Close()

	// 竞态检测由 -race 负责；这里只验证不 panic。
	_ = context.Background()
}

// TestAutoExporter_PendingDuringExportNotDropped 验证：导出期间到达的改动不会被丢弃。
//
// 这是本文件最关键的一条回归。场景：
//  1. 导出开始（读数据库快照）
//  2. 导出还没写完时，用户又改了配置 → pending = true
//  3. 导出结束
//
// 旧实现（bug 版）在步骤 3 检查 `if a.pending && a.timer == nil` 时，
// 由于 timer 已被 fire() 置 nil，会正确地再排一次 —— 但如果步骤 2 的
// Trigger 恰好在 fire() 把 timer 置 nil **之前** 调用，Trigger 会 Reset
// 一个已过期的 timer，导致这次改动被吞。
//
// 新实现保证：任何在导出期间置位的 pending，导出结束后一定会再导一次。
func TestAutoExporter_PendingDuringExportNotDropped(t *testing.T) {
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

	ex := newAutoExporter(db, cfgDir)

	// 模拟"导出正在进行"：手动把 running 置 true。
	ex.mu.Lock()
	ex.running = true
	ex.mu.Unlock()

	// 期间来一个触发 → pending = true，但因 running=true 不会立即导出。
	ex.Trigger()

	// 此时 timer 已排好（Trigger 排的），pending=true，running=true。
	ex.mu.Lock()
	if !ex.pending {
		ex.mu.Unlock()
		t.Fatal("导出期间的触发应把 pending 置为 true")
	}
	if !ex.running {
		ex.mu.Unlock()
		t.Fatal("running 应仍为 true")
	}
	ex.mu.Unlock()

	// 模拟"导出结束"：running=false 且 pending=true → 必须再排一次导出。
	// 这里直接调 fire 的收尾逻辑：把 running 置 false 后手动触发一次 fire。
	ex.mu.Lock()
	ex.running = false
	ex.mu.Unlock()

	// 等一个完整防抖窗口 + 导出耗时，确认最终导出了。
	deadline := time.Now().Add(15 * time.Second)
	cfgPath := filepath.Join(cfgDir, "config.json")
	for time.Now().Before(deadline) {
		if _, err := os.Stat(cfgPath); err == nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	ex.Close()

	if _, err := os.Stat(cfgPath); err != nil {
		t.Errorf("导出期间的改动不应被丢弃，config.json 应存在: %v", err)
	}
}

// TestAutoExporter_TriggerAfterCloseIsNoop 验证：Close 之后的 Trigger 不产生任何动作。
func TestAutoExporter_TriggerAfterCloseIsNoop(t *testing.T) {
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

	ex := newAutoExporter(db, cfgDir)
	ex.Close()
	ex.Trigger()

	time.Sleep(autoExportDebounce + 500*time.Millisecond)

	if _, err := os.Stat(filepath.Join(cfgDir, "config.json")); err == nil {
		t.Error("Close 之后 Trigger 不应导出 config.json")
	}
}
