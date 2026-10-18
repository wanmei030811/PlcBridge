// Web REST CRUD 集成测试。
//
// 覆盖：
//   - 通道 CRUD
//   - 设备 CRUD（含 nested sources/points/targets）
//   - 鉴权：未登录 401
//   - 登录 / 改密 / 注销
//   - Token 管理
//   - 仪表盘聚合
package web

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

func init() {
	gin.SetMode(gin.TestMode)
}

// newTestRouter 创建测试 router（注入 random HMAC + DB）。
func newTestRouter(t *testing.T) (*gin.Engine, func()) {
	t.Helper()
	dir := t.TempDir()
	dbPath := dir + "/test.db"
	db, err := OpenDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	cfgDir := dir + "/cfg"
	sess := NewSessionStore(RandomHMACKey())

	// 自动建账号
	if _, err := EnsureAccount(db, "admin", "Password123!"); err != nil {
		t.Fatal(err)
	}

	r := gin.New()
	RegisterRoutes(r, db, cfgDir, sess)
	return r, func() { db.Close() }
}

// jsonReq helper.
func jsonReq(method, path string, body any) *http.Request {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	return req
}

func decodeJSON(t *testing.T, resp *httptest.ResponseRecorder, out any) any {
	t.Helper()
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

// loginAndCookie 模拟登录后返回带 Cookie 的 client。
func loginAndCookie(t *testing.T, r *gin.Engine) string {
	t.Helper()
	req := jsonReq(http.MethodPost, "/api/v1/auth/login", map[string]string{
		"username": "admin",
		"password": "Password123!",
	})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("login status=%d body=%s", w.Code, w.Body.String())
	}
	cookies := w.Header().Values("Set-Cookie")
	if len(cookies) == 0 {
		t.Fatal("no cookie")
	}
	// 取第一个 cookie name=value
	first := cookies[0]
	if idx := strings.Index(first, ";"); idx > 0 {
		first = first[:idx]
	}
	if eq := strings.Index(first, "="); eq > 0 {
		return first[eq+1:]
	}
	t.Fatal("bad cookie")
	return ""
}

// authedReq 带 cookie 的请求。
func authedReq(method, path, cookie string, body any) *http.Request {
	req := jsonReq(method, path, body)
	req.Header.Set("Cookie", SessionCookieName+"="+cookie)
	return req
}

// === Tests ===

func TestAuth_LoginLogout(t *testing.T) {
	r, cleanup := newTestRouter(t)
	defer cleanup()

	// 错密码 → 401
	req := jsonReq(http.MethodPost, "/api/v1/auth/login", map[string]string{
		"username": "admin", "password": "wrong",
	})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("wrong password 应返回 401, got %d", w.Code)
	}

	// 正确
	w = httptest.NewRecorder()
	req = jsonReq(http.MethodPost, "/api/v1/auth/login", map[string]string{
		"username": "admin", "password": "Password123!",
	})
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("正确密码应 200, got %d body=%s", w.Code, w.Body.String())
	}
	cookie := loginAndCookie(t, r)

	// me
	w = httptest.NewRecorder()
	r.ServeHTTP(w, authedReq(http.MethodGet, "/api/v1/auth/me", cookie, nil))
	if w.Code != http.StatusOK {
		t.Errorf("me 应 200, got %d", w.Code)
	}

	// logout
	w = httptest.NewRecorder()
	r.ServeHTTP(w, authedReq(http.MethodPost, "/api/v1/auth/logout", cookie, nil))
	if w.Code != http.StatusOK {
		t.Errorf("logout 应 200, got %d", w.Code)
	}
}

func TestAuth_ProtectedRoutes_RequireCookie(t *testing.T) {
	r, cleanup := newTestRouter(t)
	defer cleanup()

	w := httptest.NewRecorder()
	r.ServeHTTP(w, jsonReq(http.MethodGet, "/api/v1/channels", nil))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("未登录应 401, got %d", w.Code)
	}
}

func TestChannels_CRUD(t *testing.T) {
	r, cleanup := newTestRouter(t)
	defer cleanup()
	cookie := loginAndCookie(t, r)

	// Create
	w := httptest.NewRecorder()
	r.ServeHTTP(w, authedReq(http.MethodPost, "/api/v1/channels", cookie, map[string]any{
		"channel_biz_id": "ch-s7-1",
		"role":           "collect",
		"type":           "s7",
		"address":         "192.168.1.10:102",
		"config":         `{"rack":0,"slot":1}`,
	}))
	if w.Code != http.StatusCreated {
		t.Fatalf("create channel status=%d body=%s", w.Code, w.Body.String())
	}
	t.Logf("create body=%s", w.Body.String())
	var ch ChannelDTO
	decodeJSON(t, w, &ch)
	if ch.ID == 0 || ch.ChannelBizID != "ch-s7-1" {
		t.Errorf("create 后 channel 字段不对: %+v", ch)
	}

	// Duplicate → 409
	w = httptest.NewRecorder()
	r.ServeHTTP(w, authedReq(http.MethodPost, "/api/v1/channels", cookie, map[string]any{
		"channel_biz_id": "ch-s7-1", "role": "collect", "type": "s7", "address": "x",
	}))
	if w.Code != http.StatusConflict {
		t.Errorf("重复 biz_id 应 409, got %d", w.Code)
	}

	// List
	w = httptest.NewRecorder()
	r.ServeHTTP(w, authedReq(http.MethodGet, "/api/v1/channels", cookie, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", w.Code, w.Body.String())
	}
	var listResp struct {
		Items []ChannelDTO `json:"items"`
	}
	decodeJSON(t, w, &listResp)
	if len(listResp.Items) != 1 {
		t.Errorf("len(items)=%d, want 1", len(listResp.Items))
	}

	// Update
	w = httptest.NewRecorder()
	r.ServeHTTP(w, authedReq(http.MethodPut, "/api/v1/channels/1", cookie, map[string]any{
		"channel_biz_id": "ch-s7-1", "role": "collect", "type": "s7",
		"address":   "192.168.1.11:102",
		"config":    `{"rack":0,"slot":2}`,
		"queue_size": 5000,
	}))
	if w.Code != http.StatusOK {
		t.Errorf("update 应 200, got %d body=%s", w.Code, w.Body.String())
	}

	// Delete
	w = httptest.NewRecorder()
	r.ServeHTTP(w, authedReq(http.MethodDelete, "/api/v1/channels/1", cookie, nil))
	if w.Code != http.StatusOK {
		t.Errorf("delete 应 200, got %d", w.Code)
	}
	w = httptest.NewRecorder()
	r.ServeHTTP(w, authedReq(http.MethodGet, "/api/v1/channels", cookie, nil))
	decodeJSON(t, w, &listResp)
	if len(listResp.Items) != 0 {
		t.Errorf("delete 后 items 长度=%d", len(listResp.Items))
	}
}

func TestDevices_CRUD_AndNested(t *testing.T) {
	r, cleanup := newTestRouter(t)
	defer cleanup()
	cookie := loginAndCookie(t, r)

	// 先建一个 channel（source 需要）
	w := httptest.NewRecorder()
	r.ServeHTTP(w, authedReq(http.MethodPost, "/api/v1/channels", cookie, map[string]any{
		"channel_biz_id": "ch-1",
		"role":           "collect", "type": "modbus", "address": "192.168.1.20:502",
	}))
	if w.Code != http.StatusCreated {
		t.Fatalf("create channel status=%d body=%s", w.Code, w.Body.String())
	}

	// 创建设备
	w = httptest.NewRecorder()
	r.ServeHTTP(w, authedReq(http.MethodPost, "/api/v1/devices", cookie, map[string]any{
		"device_biz_id": "dev-1", "name": "PLC #1", "enabled": true,
	}))
	if w.Code != http.StatusCreated {
		t.Fatalf("create device status=%d body=%s", w.Code, w.Body.String())
	}
	var dev DeviceDTO
	decodeJSON(t, w, &dev)
	if dev.ID == 0 {
		t.Fatal("dev.ID 缺失")
	}

	// 添加 source
	w = httptest.NewRecorder()
	r.ServeHTTP(w, authedReq(http.MethodPost, "/api/v1/sources", cookie, map[string]any{
		"source_biz_id": "src-1", "device_id": dev.ID, "channel_biz_id": "ch-1",
		"collect_interval_ms": 1000,
	}))
	if w.Code != http.StatusCreated {
		t.Fatalf("create source status=%d body=%s", w.Code, w.Body.String())
	}
	var src SourceDTO
	decodeJSON(t, w, &src)

	// 添加 point（注意：在 sources/:id/points 是只读 GET；POST /points 用 source_id 创建）
	w = httptest.NewRecorder()
	r.ServeHTTP(w, authedReq(http.MethodPost, "/api/v1/points", cookie, map[string]any{
		"point_biz_id": "pt-1", "source_id": src.ID,
		"name": "温度", "address": "40001", "data_type": "int16",
		"collect_enabled": true,
	}))
	if w.Code != http.StatusCreated {
		t.Fatalf("create point status=%d body=%s", w.Code, w.Body.String())
	}
	var pt PointDTO
	decodeJSON(t, w, &pt)
	if pt.DataType != "int16" {
		t.Errorf("point.data_type=%q, want int16", pt.DataType)
	}

	// Get device（带 nested）
	w = httptest.NewRecorder()
	r.ServeHTTP(w, authedReq(http.MethodGet, "/api/v1/devices/"+itoa(dev.ID), cookie, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("get device status=%d", w.Code)
	}
	var devFull DeviceDTO
	decodeJSON(t, w, &devFull)
	if len(devFull.Sources) != 1 || devFull.Sources[0].SourceBizID != "src-1" {
		t.Errorf("嵌套 source 不对: %+v", devFull.Sources)
	}
}

func TestTokens_CreateListRevoke(t *testing.T) {
	r, cleanup := newTestRouter(t)
	defer cleanup()
	cookie := loginAndCookie(t, r)

	// Create
	w := httptest.NewRecorder()
	r.ServeHTTP(w, authedReq(http.MethodPost, "/api/v1/tokens", cookie, map[string]any{
		"name": "test-broker",
	}))
	if w.Code != http.StatusCreated {
		t.Fatalf("create token status=%d body=%s", w.Code, w.Body.String())
	}
	var tk TokenDTO
	decodeJSON(t, w, &tk)
	if tk.Token == "" || tk.RevokedAt != nil {
		t.Errorf("新 token 应有明文、未撤销: %+v", tk)
	}

	// List
	w = httptest.NewRecorder()
	r.ServeHTTP(w, authedReq(http.MethodGet, "/api/v1/tokens", cookie, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("list status=%d", w.Code)
	}

	// Revoke
	w = httptest.NewRecorder()
	r.ServeHTTP(w, authedReq(http.MethodDelete, "/api/v1/tokens/"+itoa(tk.ID), cookie, nil))
	if w.Code != http.StatusOK {
		t.Errorf("revoke status=%d", w.Code)
	}
}

func TestDashboard_Aggregates(t *testing.T) {
	r, cleanup := newTestRouter(t)
	defer cleanup()
	cookie := loginAndCookie(t, r)

	// 空状态仪表盘
	w := httptest.NewRecorder()
	r.ServeHTTP(w, authedReq(http.MethodGet, "/api/v1/dashboard", cookie, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("dashboard status=%d", w.Code)
	}
	var d DashboardDTO
	decodeJSON(t, w, &d)
	if d.Channels.Total != 0 || d.Devices.Total != 0 {
		t.Errorf("空 db 仪表盘非零: %+v", d)
	}

	// 加一条 channel
	w = httptest.NewRecorder()
	r.ServeHTTP(w, authedReq(http.MethodPost, "/api/v1/channels", cookie, map[string]any{
		"channel_biz_id": "ch-fwd", "role": "forward", "type": "mqtt", "address": "tcp://broker:1883",
	}))
	if w.Code != http.StatusCreated {
		t.Fatalf("create channel status=%d", w.Code)
	}

	w = httptest.NewRecorder()
	r.ServeHTTP(w, authedReq(http.MethodGet, "/api/v1/dashboard", cookie, nil))
	decodeJSON(t, w, &d)
	if d.Channels.Total != 1 || d.Channels.Forward != 1 {
		t.Errorf("dashboard 计数错: %+v", d.Channels)
	}
}

func TestSnapshotExport_WritesFile(t *testing.T) {
	r, cleanup := newTestRouter(t)
	defer cleanup()
	cookie := loginAndCookie(t, r)

	// 加内容
	w := httptest.NewRecorder()
	r.ServeHTTP(w, authedReq(http.MethodPost, "/api/v1/devices", cookie, map[string]any{
		"device_biz_id": "dev-1", "name": "P", "enabled": true,
	}))
	if w.Code != http.StatusCreated {
		t.Fatalf("create dev: %d body=%s", w.Code, w.Body.String())
	}

	// 触发 export（/api/v1/snapshot 已受 RequireSession 保护）
	w = httptest.NewRecorder()
	r.ServeHTTP(w, authedReq(http.MethodPost, "/api/v1/snapshot", cookie, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("snapshot status=%d body=%s", w.Code, w.Body.String())
	}
	var snapResp struct {
		OK   bool   `json:"ok"`
		Path string `json:"path"`
	}
	decodeJSON(t, w, &snapResp)
	if !snapResp.OK {
		t.Errorf("snapshot 应返回 ok=true")
	}

	// 验证导出 JSON：顶层 slice 字段必须为 [] 而非 null
	b, err := os.ReadFile(snapResp.Path)
	if err != nil {
		t.Fatalf("read snapshot file: %v", err)
	}
	got := string(b)
	if !strings.Contains(got, `"channels": []`) {
		t.Errorf("导出 JSON 中 channels 应为 [], 实际片段: %s", got)
	}
	if !strings.Contains(got, `"tokens": []`) {
		t.Errorf("导出 JSON 中 tokens 应为 [], 实际片段: %s", got)
	}
	if strings.Contains(got, `"channels":null`) || strings.Contains(got, `"devices":null`) || strings.Contains(got, `"tokens":null`) {
		t.Errorf("导出 JSON 顶层 slice 出现 null: %s", got)
	}
}

// TestDeleteChannel_Referenced 验证：channel 删除时被 source 引用应 409。
func TestDeleteChannel_Referenced(t *testing.T) {
	r, cleanup := newTestRouter(t)
	defer cleanup()
	cookie := loginAndCookie(t, r)

	// 建 channel
	w := httptest.NewRecorder()
	r.ServeHTTP(w, authedReq(http.MethodPost, "/api/v1/channels", cookie, map[string]any{
		"channel_biz_id": "ch-x", "role": "collect", "type": "modbus", "address": "127.0.0.1:502",
	}))
	if w.Code != http.StatusCreated {
		t.Fatalf("create ch: %d %s", w.Code, w.Body.String())
	}
	// 建 device
	w = httptest.NewRecorder()
	r.ServeHTTP(w, authedReq(http.MethodPost, "/api/v1/devices", cookie, map[string]any{
		"device_biz_id": "dev-x", "name": "P", "enabled": true,
	}))
	if w.Code != http.StatusCreated {
		t.Fatalf("create dev: %d %s", w.Code, w.Body.String())
	}
	// 建 source（引用 ch-x）
	w = httptest.NewRecorder()
	r.ServeHTTP(w, authedReq(http.MethodPost, "/api/v1/sources", cookie, map[string]any{
		"source_biz_id": "src-x", "device_id": 1, "channel_biz_id": "ch-x",
	}))
	if w.Code != http.StatusCreated {
		t.Fatalf("create src: %d %s", w.Code, w.Body.String())
	}
	// 尝试删 channel → 应 409
	w = httptest.NewRecorder()
	r.ServeHTTP(w, authedReq(http.MethodDelete, "/api/v1/channels/1", cookie, nil))
	if w.Code != http.StatusConflict {
		t.Errorf("delete 引用中的 channel 应 409，实际 %d body=%s", w.Code, w.Body.String())
	}
}

// TestDeleteDevice_Cascade 验证：删除 device 应级联清掉 sources/points/targets（FK ON DELETE CASCADE）。
func TestDeleteDevice_Cascade(t *testing.T) {
	r, cleanup := newTestRouter(t)
	defer cleanup()
	cookie := loginAndCookie(t, r)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, authedReq(http.MethodPost, "/api/v1/channels", cookie, map[string]any{
		"channel_biz_id": "ch-y", "role": "collect", "type": "modbus", "address": "127.0.0.1:502",
	}))
	if w.Code != http.StatusCreated {
		t.Fatalf("create ch: %d %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	r.ServeHTTP(w, authedReq(http.MethodPost, "/api/v1/devices", cookie, map[string]any{
		"device_biz_id": "dev-y", "name": "P", "enabled": true,
	}))
	if w.Code != http.StatusCreated {
		t.Fatalf("create dev: %d %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	r.ServeHTTP(w, authedReq(http.MethodPost, "/api/v1/sources", cookie, map[string]any{
		"source_biz_id": "src-y", "device_id": 1, "channel_biz_id": "ch-y",
	}))
	if w.Code != http.StatusCreated {
		t.Fatalf("create src: %d %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	r.ServeHTTP(w, authedReq(http.MethodPost, "/api/v1/points", cookie, map[string]any{
		"point_biz_id": "pt-y", "source_id": 1, "address": "DB1.0", "data_type": "int16",
	}))
	if w.Code != http.StatusCreated {
		t.Fatalf("create pt: %d %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	r.ServeHTTP(w, authedReq(http.MethodPost, "/api/v1/channels", cookie, map[string]any{
		"channel_biz_id": "fwd-y", "role": "forward", "type": "mqtt", "address": "tcp://127.0.0.1:1883",
	}))
	if w.Code != http.StatusCreated {
		t.Fatalf("create fwd: %d %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	r.ServeHTTP(w, authedReq(http.MethodPost, "/api/v1/targets", cookie, map[string]any{
		"target_biz_id": "tgt-y", "device_id": 1, "forward_channel_biz_id": "fwd-y",
	}))
	if w.Code != http.StatusCreated {
		t.Fatalf("create tgt: %d %s", w.Code, w.Body.String())
	}

	// 删 device → 应 200，cascade 清掉
	w = httptest.NewRecorder()
	r.ServeHTTP(w, authedReq(http.MethodDelete, "/api/v1/devices/1", cookie, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("del dev 应 200, 实际 %d body=%s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	r.ServeHTTP(w, authedReq(http.MethodGet, "/api/v1/devices/1/sources", cookie, nil))
	var srcList struct {
		Items []SourceDTO `json:"items"`
	}
	decodeJSON(t, w, &srcList)
	if len(srcList.Items) != 0 {
		t.Errorf("device 删除后 sources 应被 cascade 清空, 实际 %d", len(srcList.Items))
	}
}

// TestSessionGC_ExpiredEntryRemoved 验证：过期 entry 在 Validate 时被 GC。
func TestSessionGC_ExpiredEntryRemoved(t *testing.T) {
	key := RandomHMACKey()
	sess := NewSessionStore(key)
	defer sess.Stop()

	// 注入一个已过期 entry
	expiredToken := "expired-token"
	sess.mu.Lock()
	sess.sessions[expiredToken] = sessionEntry{
		username:  "alice",
		expiresAt: time.Now().Add(-time.Hour),
	}
	sess.mu.Unlock()

	if got := sess.Len(); got != 1 {
		t.Fatalf("注入后 len=%d", got)
	}
	// 合法 sig（先过 HMAC），触发 GC
	sig := sess.sign(expiredToken)
	if _, err := sess.Validate(expiredToken + "." + sig); err == nil {
		t.Errorf("过期 token 应返回 ErrUnauthorized")
	}
	if got := sess.Len(); got != 0 {
		t.Errorf("过期 entry 应被 GC，实际 len=%d", got)
	}
}

// TestSessionStore_StopTwiceSafe 验证：Stop 多次调用安全。
func TestSessionStore_StopTwiceSafe(t *testing.T) {
	key := RandomHMACKey()
	sess := NewSessionStore(key)
	sess.Stop()
	sess.Stop()
}

// TestHMACKeyShortPanics 验证：短 key 立即 panic，不静默续 0。
func TestHMACKeyShortPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Errorf("短 key 应 panic")
		}
	}()
	_ = NewSessionStore([]byte("short"))
}

// TestMountSPA_AssetsAndFallback 验证：SPA 静态资源可访问、unknown GET 走 index.html、未知 /api/ 走 JSON 404。
//
// dist/ 已经在仓库里（vite build 产物），因此 embed FS 始终存在；这里直接挂 SPA 后断言。
func TestMountSPA_AssetsAndFallback(t *testing.T) {
	r := gin.New()
	MountSPA(r)

	// 0) 校验 embed FS 真的有内容（hash 经常变，按 glob 找一个即可）
	entries, err := fs.ReadDir(distFS, "dist/assets")
	if err != nil {
		t.Fatalf("read embed dist/assets: %v", err)
	}
	var jsAsset, cssAsset string
	for _, e := range entries {
		name := e.Name()
		if strings.HasSuffix(name, ".js") && !strings.HasPrefix(name, "_") && !strings.HasPrefix(name, "index") {
			if jsAsset == "" {
				jsAsset = name
			}
		}
		if strings.HasSuffix(name, ".css") && strings.HasPrefix(name, "index") {
			if cssAsset == "" {
				cssAsset = name
			}
		}
	}
	if jsAsset == "" {
		// fallback: 任意 .js
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".js") {
				jsAsset = e.Name()
				break
			}
		}
	}
	if jsAsset == "" {
		t.Fatalf("embed dist/assets 中找不到 .js 文件")
	}

	// 1) 根路径 = index.html
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != http.StatusOK {
		t.Errorf("GET / 应 200, got %d", w.Code)
	}
	if !strings.Contains(w.Header().Get("Content-Type"), "text/html") {
		t.Errorf("GET / 应 text/html, got %q", w.Header().Get("Content-Type"))
	}
	if !strings.Contains(w.Body.String(), "<div id=\"app\">") {
		t.Errorf("GET / 应返回 index.html（含 <div id=app>），body=%s", w.Body.String())
	}

	// 2) 静态资源：实际存在的 .js 资产
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/assets/"+jsAsset, nil))
	if w.Code != http.StatusOK {
		t.Errorf("GET /assets/%s 应 200, got %d body=%s", jsAsset, w.Code, w.Body.String())
	}
	if !strings.Contains(w.Header().Get("Content-Type"), "javascript") {
		t.Errorf("JS Content-Type 错: %q", w.Header().Get("Content-Type"))
	}

	// 3) 子目录静态资源（css，可选）
	if cssAsset != "" {
		w = httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/assets/"+cssAsset, nil))
		if w.Code != http.StatusOK {
			t.Errorf("GET /assets/%s 应 200, got %d body=%s", cssAsset, w.Code, w.Body.String())
		}
	}

	// 4) 未知路径走 SPA fallback
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/dashboard", nil))
	if w.Code != http.StatusOK {
		t.Errorf("SPA fallback GET /dashboard 应 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "<div id=\"app\">") {
		t.Errorf("SPA fallback 应返回 index.html")
	}

	// 5) /api/ unknown → JSON 404
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/unknown", nil))
	if w.Code != http.StatusNotFound {
		t.Errorf("GET /api/unknown 应 404 JSON, got %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "api route not found") {
		t.Errorf("/api/ fallback 应返回 JSON 'api route not found', got %s", w.Body.String())
	}

	// 6) 不存在的静态资产 → 404
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/assets/__no_such_file__.js", nil))
	if w.Code != http.StatusNotFound {
		t.Errorf("GET /assets/__no_such_file__.js 应 404, got %d", w.Code)
	}

	// 7) 路径穿越被拒
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/assets/../../etc/passwd", nil))
	if w.Code == http.StatusOK {
		t.Errorf("路径穿越应被拒，不能 200")
	}
}

// TestCreateChannel_RoleTypeValidation 测试 createChannel 的 role/type 校验：
//   - role 非法 → 400
//   - role 与 type 不匹配 → 400
//   - 合法组合通过
func TestCreateChannel_RoleTypeValidation(t *testing.T) {
	r, cleanup := newTestRouter(t)
	defer cleanup()
	cookie := loginAndCookie(t, r)

	cases := []struct {
		name    string
		role    string
		typ     string
		wantBad bool
	}{
		{"collect+s7 ok", "collect", "s7", false},
		{"collect+modbus ok", "collect", "modbus", false},
		{"collect+opcua ok", "collect", "opcua", false},
		{"forward+mqtt ok", "forward", "mqtt", false},
		{"forward+influxdb ok", "forward", "influxdb", false},
		{"collect+mqtt 不匹配", "collect", "mqtt", true},
		{"forward+s7 不匹配", "forward", "s7", true},
		{"role 非法", "weird", "s7", true},
		{"type 非法", "collect", "made-up", true},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := map[string]any{
				"channel_biz_id": "test_chan_" + strings.ReplaceAll(strings.ReplaceAll(c.name, " ", "_"), "/", "_") + "_" + uuid.NewString()[:8],
				"role":           c.role,
				"type":           c.typ,
				"address":        "127.0.0.1:0",
			}
			w := httptest.NewRecorder()
			r.ServeHTTP(w, authedReq(http.MethodPost, "/api/v1/channels", cookie, body))
			if c.wantBad && w.Code == http.StatusCreated {
				t.Errorf("case %s: 应失败但 201; body=%s", c.name, w.Body.String())
			}
			if !c.wantBad && w.Code != http.StatusCreated {
				t.Errorf("case %s (i=%d): 应 201 但 %d; body=%s", c.name, i, w.Code, w.Body.String())
			}
		})
	}
}

// TestCreateSource_ChannelNotExist 测试引用不存在 channel_biz_id 时返回 400 而不是 500。
func TestCreateSource_ChannelNotExist(t *testing.T) {
	r, cleanup := newTestRouter(t)
	defer cleanup()
	cookie := loginAndCookie(t, r)

	devID := createTestDevice(t, r, cookie, "dev_"+uuid.NewString()[:8], "test-device", true)

	body := map[string]any{
		"source_biz_id":      "src_" + uuid.NewString()[:8],
		"device_id":          devID,
		"channel_biz_id":     "nonexistent-channel",
		"collect_enabled":    true,
		"write_enabled":      false,
		"collect_interval_ms": 1000,
		"timeout_ms":         300,
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, authedReq(http.MethodPost, "/api/v1/sources", cookie, body))
	if w.Code != http.StatusBadRequest {
		t.Errorf("引用不存在 channel 应 400, got %d body=%s", w.Code, w.Body.String())
	}
}

// TestCreateSource_ChannelRoleMismatch 测试 source 引用 forward channel 应被拒。
func TestCreateSource_ChannelRoleMismatch(t *testing.T) {
	r, cleanup := newTestRouter(t)
	defer cleanup()
	cookie := loginAndCookie(t, r)

	devID := createTestDevice(t, r, cookie, "dev_"+uuid.NewString()[:8], "test-device", true)
	chID := createTestChannel(t, r, cookie, "ch_"+uuid.NewString()[:8], "forward", "mqtt", "127.0.0.1:1883")

	body := map[string]any{
		"source_biz_id":      "src_" + uuid.NewString()[:8],
		"device_id":          devID,
		"channel_biz_id":     chID,
		"collect_enabled":    true,
		"write_enabled":      false,
		"collect_interval_ms": 1000,
		"timeout_ms":         300,
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, authedReq(http.MethodPost, "/api/v1/sources", cookie, body))
	if w.Code != http.StatusBadRequest {
		t.Errorf("source 引用 forward channel 应 400, got %d body=%s", w.Code, w.Body.String())
	}
}

// TestCreatePoint_InvalidDataType 测试 point.data_type 校验。
func TestCreatePoint_InvalidDataType(t *testing.T) {
	r, cleanup := newTestRouter(t)
	defer cleanup()
	cookie := loginAndCookie(t, r)

	devID := createTestDevice(t, r, cookie, "dev_"+uuid.NewString()[:8], "test-device", true)
	chID := createTestChannel(t, r, cookie, "ch_"+uuid.NewString()[:8], "collect", "s7", "127.0.0.1:102")
	srcID := createTestSource(t, r, cookie, "src_"+uuid.NewString()[:8], devID, chID)

	body := map[string]any{
		"point_biz_id":    "pt_" + uuid.NewString()[:8],
		"source_id":       srcID,
		"name":            "温度",
		"address":         "DB1.DBD0",
		"data_type":       "unknown_type",
		"collect_enabled": true,
		"write_enabled":   false,
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, authedReq(http.MethodPost, "/api/v1/points", cookie, body))
	if w.Code != http.StatusBadRequest {
		t.Errorf("非法 data_type 应 400, got %d body=%s", w.Code, w.Body.String())
	}
}

// TestParseID_RejectsNonPositive 测试 /api/v1/channels/0 / /-1 应 400。
func TestParseID_RejectsNonPositive(t *testing.T) {
	r, cleanup := newTestRouter(t)
	defer cleanup()
	cookie := loginAndCookie(t, r)

	for _, p := range []string{"/api/v1/channels/0", "/api/v1/channels/-1", "/api/v1/channels/abc"} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, authedReq(http.MethodGet, p, cookie, nil))
		if w.Code != http.StatusBadRequest {
			t.Errorf("GET %s 应 400, got %d", p, w.Code)
		}
	}
}

// === 测试 helper（独立 uuid 后缀避免 UNIQUE 冲突）===

func createTestDevice(t *testing.T, r http.Handler, cookie string, bizID, name string, enabled bool) int64 {
	t.Helper()
	body := map[string]any{"device_biz_id": bizID, "name": name, "enabled": enabled}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, authedReq(http.MethodPost, "/api/v1/devices", cookie, body))
	if w.Code != http.StatusCreated {
		t.Fatalf("createTestDevice 失败: %d body=%s", w.Code, w.Body.String())
	}
	var d DeviceDTO
	_ = json.Unmarshal(w.Body.Bytes(), &d)
	return d.ID
}

func createTestChannel(t *testing.T, r http.Handler, cookie, bizID, role, typ, addr string) string {
	t.Helper()
	body := map[string]any{"channel_biz_id": bizID, "role": role, "type": typ, "address": addr}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, authedReq(http.MethodPost, "/api/v1/channels", cookie, body))
	if w.Code != http.StatusCreated {
		t.Fatalf("createTestChannel 失败: %d body=%s", w.Code, w.Body.String())
	}
	return bizID
}

func createTestSource(t *testing.T, r http.Handler, cookie, bizID string, devID int64, chBizID string) int64 {
	t.Helper()
	body := map[string]any{
		"source_biz_id":      bizID,
		"device_id":          devID,
		"channel_biz_id":     chBizID,
		"collect_enabled":    true,
		"write_enabled":      false,
		"collect_interval_ms": 1000,
		"timeout_ms":         300,
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, authedReq(http.MethodPost, "/api/v1/sources", cookie, body))
	if w.Code != http.StatusCreated {
		t.Fatalf("createTestSource 失败: %d body=%s", w.Code, w.Body.String())
	}
	var s SourceDTO
	_ = json.Unmarshal(w.Body.Bytes(), &s)
	return s.ID
}

// 编译期断言
var _ = uuid.NewString
var _ = context.Background
