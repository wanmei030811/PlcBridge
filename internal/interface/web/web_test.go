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
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	_ "github.com/plcbridge/plcbridge/internal/infra/protocol/plugins" // 注册内置协议
	_ "github.com/plcbridge/plcbridge/internal/infra/target/plugins"   // 注册内置 Target
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
		"address":        "192.168.1.10:102",
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
		"address":    "192.168.1.11:102",
		"config":     `{"rack":0,"slot":2}`,
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
	var tk TokenCreated
	decodeJSON(t, w, &tk)
	if tk.Token == "" {
		t.Errorf("新 token 应返回明文: %+v", tk)
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
// 注意（2026-10-26）：本测试必须同时适配两种构建状态——
//   - 本地/发布构建：dist/ 里有 vite 产物（index.html + assets/）→ 断言完整 SPA 行为
//   - 干净克隆：CI 只跑 go build，从不跑 npm run build，dist/ 里只有 .gitkeep 占位
//     → 此时 embed 里没有 index.html，MountSPA 走降级占位页，断言改查占位页
//
// 所以这里先探测 embed 里到底有没有产物，再决定断言哪一套行为。
func TestMountSPA_AssetsAndFallback(t *testing.T) {
	r := gin.New()
	MountSPA(r)

	// 0) 探测 embed FS 的实际状态
	entries, err := fs.ReadDir(distFS, "dist/assets")
	_ = err // assets 目录不存在（干净克隆）不算失败，下面按 index.html 是否存在分支
	_, indexErr := fs.Stat(distFS, "dist/index.html")
	hasIndex := indexErr == nil

	if !hasIndex {
		// ---- 干净克隆分支：占位页降级 ----
		t.Log("embed 无 index.html（干净克隆状态），验证降级占位页")

		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
		if w.Code != http.StatusOK {
			t.Errorf("降级态 GET / 应 200, got %d", w.Code)
		}
		if !strings.Contains(w.Body.String(), "前端资源未内嵌") {
			t.Errorf("降级态 GET / 应返回占位页，body=%s", w.Body.String())
		}

		// 未知路径同样走占位页
		w = httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/dashboard", nil))
		if w.Code != http.StatusOK {
			t.Errorf("降级态 SPA fallback GET /dashboard 应 200, got %d", w.Code)
		}

		// /api/ unknown → JSON 404（API 不受前端缺失影响）
		w = httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/unknown", nil))
		if w.Code != http.StatusNotFound {
			t.Errorf("降级态 GET /api/unknown 应 404 JSON, got %d body=%s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "api route not found") {
			t.Errorf("降级态 /api/ fallback 应返回 JSON 'api route not found', got %s", w.Body.String())
		}
		return
	}

	// ---- 完整产物分支：断言完整 SPA 行为 ----
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

// TestMountSPA_FallbackWhenAssetsMissing 验证：前端产物缺失时走占位页降级，
// 而不是 panic（2026-10-26 修复）。
//
// 背景：web 进程除了 UI，还提供配置/通道/设备/点位等全部核心 API。
// 前端缺失只是「降级」，不该让进程退出——原来的 panic 会让 supervisor 无限重启，
// 结果连 API 和健康检查一起不可用。
//
// 本测试直接调 mountFallbackSPA，不依赖 embed 的实际内容，因此在任何构建状态下都能跑。
func TestMountSPA_FallbackWhenAssetsMissing(t *testing.T) {
	r := gin.New()
	mountFallbackSPA(r)

	// 1) 根路径返回占位页（200，而不是 panic）
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != http.StatusOK {
		t.Errorf("降级态 GET / 应 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "前端资源未内嵌") {
		t.Errorf("降级态 GET / 应返回占位页，body=%s", w.Body.String())
	}
	if !strings.Contains(w.Header().Get("Content-Type"), "text/html") {
		t.Errorf("降级态 GET / 应 text/html, got %q", w.Header().Get("Content-Type"))
	}
	// 占位页必须 no-store，否则修好前端后浏览器还拿旧占位页
	if !strings.Contains(w.Header().Get("Cache-Control"), "no-store") {
		t.Errorf("降级态 GET / 应带 no-store, got %q", w.Header().Get("Cache-Control"))
	}

	// 2) SPA fallback：未知路径也返回占位页
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/dashboard", nil))
	if w.Code != http.StatusOK {
		t.Errorf("降级态 GET /dashboard 应 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "前端资源未内嵌") {
		t.Errorf("降级态 SPA fallback 应返回占位页")
	}

	// 3) 静态资源 404（确实没有产物）
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/assets/index-abc123.js", nil))
	if w.Code != http.StatusNotFound {
		t.Errorf("降级态 GET /assets/xxx.js 应 404, got %d", w.Code)
	}

	// 4) /api/unknown → JSON 404（API 语义不受前端缺失影响）
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/unknown", nil))
	if w.Code != http.StatusNotFound {
		t.Errorf("降级态 GET /api/unknown 应 404, got %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "api route not found") {
		t.Errorf("降级态 /api/ fallback 应返回 JSON 'api route not found', got %s", w.Body.String())
	}

	// 5) 非 GET/HEAD/OPTIONS → 405 JSON
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/whatever", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("降级态 POST /whatever 应 405, got %d", w.Code)
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
		{"collect+modbus ok（modbus 是 modbus_tcp 别名）", "collect", "modbus", false}, // Plan2 3.5：handler 入口归一化
		{"collect+modbus_tcp ok", "collect", "modbus_tcp", false},
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

// TestUpdateChannel_TypeValidation 验证 updateChannel 也校验 type（bugfix）。
//
// 历史 bug：5.3 把 type 校验下沉后，updateChannel 漏了 type 检查，导致可通过
// PUT 把 channel.type 改成 "made-up" 等未注册的协议名。
func TestUpdateChannel_TypeValidation(t *testing.T) {
	r, cleanup := newTestRouter(t)
	defer cleanup()
	cookie := loginAndCookie(t, r)

	// 先建一个合法 channel
	bizID := "ch_update_type_" + uuid.NewString()[:8]
	w := httptest.NewRecorder()
	r.ServeHTTP(w, authedReq(http.MethodPost, "/api/v1/channels", cookie, map[string]any{
		"channel_biz_id": bizID, "role": "collect", "type": "s7", "address": "127.0.0.1:0",
	}))
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	var ch ChannelDTO
	_ = json.Unmarshal(w.Body.Bytes(), &ch)

	// 尝试把 type 改成未注册的 "made-up" → 应 400
	w = httptest.NewRecorder()
	r.ServeHTTP(w, authedReq(http.MethodPut, "/api/v1/channels/"+strconv.FormatInt(ch.ID, 10), cookie, map[string]any{
		"channel_biz_id": bizID, "role": "collect", "type": "made-up", "address": "127.0.0.1:0",
	}))
	if w.Code != http.StatusBadRequest {
		t.Errorf("update type 改成 made-up 应 400, got %d body=%s", w.Code, w.Body.String())
	}
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body["code"] != "invalid_param" {
		t.Errorf("code 应 invalid_param, got %v", body["code"])
	}
}

// TestCreateSource_ChannelNotExist 测试引用不存在 channel_biz_id 时返回 400 而不是 500。
func TestCreateSource_ChannelNotExist(t *testing.T) {
	r, cleanup := newTestRouter(t)
	defer cleanup()
	cookie := loginAndCookie(t, r)

	devID := createTestDevice(t, r, cookie, "dev_"+uuid.NewString()[:8], "test-device", true)

	body := map[string]any{
		"source_biz_id":       "src_" + uuid.NewString()[:8],
		"device_id":           devID,
		"channel_biz_id":      "nonexistent-channel",
		"collect_enabled":     true,
		"write_enabled":       false,
		"collect_interval_ms": 1000,
		"timeout_ms":          300,
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, authedReq(http.MethodPost, "/api/v1/sources", cookie, body))
	if w.Code != http.StatusNotFound {
		t.Errorf("引用不存在 channel 应 404 (channel_not_found), got %d body=%s", w.Code, w.Body.String())
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
		"source_biz_id":       "src_" + uuid.NewString()[:8],
		"device_id":           devID,
		"channel_biz_id":      chID,
		"collect_enabled":     true,
		"write_enabled":       false,
		"collect_interval_ms": 1000,
		"timeout_ms":          300,
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

// TestErrorResponse_BusinessValidation_UseCodeMsg 验证 Plan2 4.1：
//
//	业务校验错误（400/404/409）走 WriteError，响应体必须是 {code, msg} 结构。
//	不查 data.error 字段（handleStoreErr 仍是旧格式，过渡期）。
func TestErrorResponse_BusinessValidation_UseCodeMsg(t *testing.T) {
	r, cleanup := newTestRouter(t)
	defer cleanup()
	cookie := loginAndCookie(t, r)

	type expect struct {
		name       string
		method     string
		path       string
		body       any
		wantStatus int
		wantCode   string // 期望的 code 字段值，"" 表示只校验有 code 字段
		noAuth     bool   // true = 不带 session cookie
	}
	cases := []expect{
		{
			name:       "create channel 缺字段 → invalid_param",
			method:     http.MethodPost,
			path:       "/api/v1/channels",
			body:       map[string]any{"channel_biz_id": "ch-bad"},
			wantStatus: http.StatusBadRequest,
			wantCode:   "invalid_param",
		},
		{
			name:       "create channel role 非法 → invalid_param",
			method:     http.MethodPost,
			path:       "/api/v1/channels",
			body:       map[string]any{"channel_biz_id": "x", "role": "weird", "type": "s7", "address": "1.1.1.1:0"},
			wantStatus: http.StatusBadRequest,
			wantCode:   "invalid_param",
		},
		{
			name:       "create channel type 未知 → invalid_param",
			method:     http.MethodPost,
			path:       "/api/v1/channels",
			body:       map[string]any{"channel_biz_id": "x2", "role": "collect", "type": "made-up", "address": "1.1.1.1:0"},
			wantStatus: http.StatusBadRequest,
			wantCode:   "invalid_param",
		},
		{
			name:       "get channel/0 → invalid_param",
			method:     http.MethodGet,
			path:       "/api/v1/channels/0",
			body:       nil,
			wantStatus: http.StatusBadRequest,
			wantCode:   "invalid_param",
		},
		{
			name:   "create source 引用不存在 channel → channel_not_found",
			method: http.MethodPost,
			path:   "/api/v1/sources",
			body: map[string]any{
				"source_biz_id": "src_" + uuid.NewString()[:8],
				"device_id":     1, "channel_biz_id": "nonexistent-ch",
			},
			wantStatus: http.StatusNotFound,
			wantCode:   "channel_not_found",
		},
		{
			name:       "create device 缺 name → invalid_param",
			method:     http.MethodPost,
			path:       "/api/v1/devices",
			body:       map[string]any{"device_biz_id": "d-x"},
			wantStatus: http.StatusBadRequest,
			wantCode:   "invalid_param",
		},
		{
			name:   "create point 未知 data_type → invalid_param",
			method: http.MethodPost,
			path:   "/api/v1/points",
			body: map[string]any{
				"point_biz_id": "pt_" + uuid.NewString()[:8], "source_id": 1,
				"address": "DB1.0", "data_type": "unknown_type",
			},
			wantStatus: http.StatusBadRequest,
			wantCode:   "invalid_param",
		},
		{
			name:       "未登录 → unauthorized",
			method:     http.MethodGet,
			path:       "/api/v1/channels",
			body:       nil,
			wantStatus: http.StatusUnauthorized,
			wantCode:   "unauthorized",
			noAuth:     true,
		},
		{
			name:       "登录错密码 → invalid_credentials",
			method:     http.MethodPost,
			path:       "/api/v1/auth/login",
			body:       map[string]string{"username": "admin", "password": "wrong"},
			wantStatus: http.StatusUnauthorized,
			wantCode:   "invalid_credentials",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			// 登录接口是公开的；其余接口都需要带 session cookie。
			if c.path == "/api/v1/auth/login" || c.noAuth {
				r.ServeHTTP(w, jsonReq(c.method, c.path, c.body))
			} else {
				r.ServeHTTP(w, authedReq(c.method, c.path, cookie, c.body))
			}
			if w.Code != c.wantStatus {
				t.Fatalf("status: got %d, want %d (body=%s)", w.Code, c.wantStatus, w.Body.String())
			}
			var body map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("body not JSON: %v (raw=%s)", err, w.Body.String())
			}
			code, ok := body["code"].(string)
			if !ok || code == "" {
				t.Errorf("response 缺少 code 字段: %s", w.Body.String())
			}
			if c.wantCode != "" && code != c.wantCode {
				t.Errorf("code: got %q, want %q (body=%s)", code, c.wantCode, w.Body.String())
			}
			msg, ok := body["msg"].(string)
			if !ok || msg == "" {
				t.Errorf("response 缺少 msg 字段: %s", w.Body.String())
			}
		})
	}
}

// TestErrorResponse_HandleStoreErr_AlsoUnified 验证 Plan2 4.3：
//
//	handleStoreErr 不再保留旧格式，全部走 {code, msg}。
//	错误体系彻底统一，过渡期结束。
func TestErrorResponse_HandleStoreErr_AlsoUnified(t *testing.T) {
	r, cleanup := newTestRouter(t)
	defer cleanup()
	cookie := loginAndCookie(t, r)

	// 触发 store 路径的 404：GET 不存在的 channel
	w := httptest.NewRecorder()
	r.ServeHTTP(w, authedReq(http.MethodGet, "/api/v1/channels/9999", cookie, nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("GET 不存在 channel 应 404, got %d", w.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	// 4.3 切换后：code=not_found, msg="资源不存在"（ErrAPINotFound 的默认文案）
	if body["code"] != "not_found" {
		t.Errorf("code: got %v, want not_found (body=%s)", body["code"], w.Body.String())
	}
	if msg, ok := body["msg"].(string); !ok || msg == "" {
		t.Errorf("缺 msg 字段: %s", w.Body.String())
	}
	// 4.3 后：旧 error 字段不再有
	if _, hasOld := body["error"]; hasOld {
		t.Errorf("4.3 后响应体不应再含旧 error 字段: %s", w.Body.String())
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
		"source_biz_id":       bizID,
		"device_id":           devID,
		"channel_biz_id":      chBizID,
		"collect_enabled":     true,
		"write_enabled":       false,
		"collect_interval_ms": 1000,
		"timeout_ms":          300,
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
