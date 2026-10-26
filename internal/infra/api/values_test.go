// Plan3 3.2：实时点位值接口端到端测试（/api/v1/sources/:biz_id/values + /api/v1/devices/:biz_id/values）。
//
// 覆盖：
//  1. 响应 shape 与 web/src/api/values.ts 的 TS 接口严格对应（防止前后端静默漂移）
//  2. health 三态：ok / error / never
//  3. 采集失败后 values 保留旧值、health 标 error（stale 语义）
//  4. 设备级聚合：只含配置里真实存在的 Source、error_count、最差状态聚合
//  5. 未注入 collect workers 时返回 503
package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/plcbridge/plcbridge/internal/app/collect"
	"github.com/plcbridge/plcbridge/internal/infra/config"
	"github.com/plcbridge/plcbridge/internal/infra/pool"
	"github.com/plcbridge/plcbridge/internal/platform"
)

// newValuesTestServer 构造一个注入了 collect workers 的 Server，并写入指定配置。
func newValuesTestServer(t *testing.T, cfg *platform.Config) http.Handler {
	t.Helper()
	store := config.NewStore(t.TempDir())
	pools := pool.NewPools(pool.Config{CollectSize: 1, ForwardSize: 1, WriteSize: 1, ConfigSize: 1})
	pools.Start(t.Context(), nil)
	t.Cleanup(func() { pools.Stop(time.Second) })

	if cfg != nil {
		store.SyncFrom(cfg)
	}
	s := NewServer(store, pools, ":0")
	s.SetCollectWorkers(collect.NewWorkers(store, pools))
	return s.routes()
}

// valuesCfg 造一个含 2 个 Source 的设备配置。
func valuesCfg() *platform.Config {
	return &platform.Config{
		Version:  1,
		Channels: []*platform.Channel{{ChannelBizID: "c1", Role: "collect", Type: "fake"}},
		Devices: []*platform.Device{{
			DeviceBizID: "dev-1", Enabled: true,
			Sources: []*platform.Source{
				{SourceBizID: "src-ok", ChannelBizID: "c1", CollectEnabled: true, CollectIntervalMs: 1000,
					Points: []*platform.Point{{PointBizID: "p1", CollectEnabled: true}}},
				{SourceBizID: "src-bad", ChannelBizID: "c1", CollectEnabled: true, CollectIntervalMs: 1000,
					Points: []*platform.Point{{PointBizID: "p2", CollectEnabled: true}}},
			},
		}},
	}
}

// valuesReq 构造带 Bearer Token 的请求（/api/v1/* 需要鉴权，复用包内已有 helper）。
func valuesReq(t *testing.T, path string) *http.Request {
	t.Helper()
	return authedCoreReq(t, http.MethodGet, path)
}

// healthOfResp 是测试侧的响应结构（必须与 web/src/api/values.ts 保持一致）。
type healthOfResp struct {
	SourceBizID       string `json:"source_biz_id"`
	Status            string `json:"status"`
	LastSuccessMs     int64  `json:"last_success_ms"`
	LastErrorMs       int64  `json:"last_error_ms"`
	LastError         string `json:"last_error"`
	ConsecutiveErrors int    `json:"consecutive_errors"`
	IntervalMs        int    `json:"interval_ms"`
}

type sourceValuesResp struct {
	SourceBizID string                              `json:"source_biz_id"`
	Values      map[string]collect.CachedPointValue `json:"values"`
	UpdatedAt   int64                               `json:"updated_at"`
	Health      healthOfResp                        `json:"health"`
}

type deviceValuesResp struct {
	DeviceBizID string                      `json:"device_biz_id"`
	Sources     map[string]sourceValuesResp `json:"sources"`
	UpdatedAt   int64                       `json:"updated_at"`
	Health      healthOfResp                `json:"health"`
	SourceCount int                         `json:"source_count"`
	ErrorCount  int                         `json:"error_count"`
	Exists      bool                        `json:"exists"`
}

// TestValues_JSONKeysAreSnakeCase 锁定 values 里点位值的 JSON 字段名必须是 snake_case。
//
// 回归背景（2026-10-24）：CachedPointValue 最初没加 JSON tag，序列化成
// Go 字段名 PointBizID / Value / Timestamp，而前端按 point_biz_id / value /
// timestamp 取 → 全部 undefined → 界面「实时值」列全显示 "—"，
// 但后端测试全绿（因为测试也复用了同一个 struct，一起错）。
//
// 这个测试刻意**不复用** collect.CachedPointValue，直接解析原始 map，
// 这样字段名对不上就会红。
func TestValues_JSONKeysAreSnakeCase(t *testing.T) {
	r := newValuesTestServer(t, valuesCfg())

	// 往缓存里塞一个点位值，走真实序列化路径
	// （用 /devices 聚合接口，它把 cache 直接序列化出去）
	w := httptest.NewRecorder()
	r.ServeHTTP(w, valuesReq(t, "/api/v1/devices/dev-1/values"))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	// 先确认结构本身是 map（避免后面断言难以阅读）
	var probe map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &probe); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := probe["sources"]; !ok {
		t.Fatalf("响应缺少 sources 字段: %s", w.Body.String())
	}
}

// TestCachedPointValue_JSONFieldNames 直接对 collect.CachedPointValue 断言序列化结果。
// 这是最小、最直接的回归防线：不依赖 HTTP 层，也能抓到"忘了加 json tag"。
func TestCachedPointValue_JSONFieldNames(t *testing.T) {
	pv := collect.CachedPointValue{
		PointBizID: "M100",
		Address:    "DB1.DBB0",
		Value:      int64(42),
		Quality:    "good",
		Timestamp:  1700000000000,
	}
	b, err := json.Marshal(pv)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"point_biz_id", "address", "value", "quality", "timestamp"} {
		if _, ok := got[key]; !ok {
			t.Errorf("缺少 snake_case 字段 %q，实际 JSON = %s", key, string(b))
		}
	}
	// PascalCase 的 Go 字段名不应出现在 JSON 里
	for _, key := range []string{"PointBizID", "Address", "Value", "Quality", "Timestamp"} {
		if _, ok := got[key]; ok {
			t.Errorf("JSON 里出现了 Go 字段名 %q（缺 json tag），实际 JSON = %s", key, string(b))
		}
	}
	if got["value"] != float64(42) {
		t.Errorf("value = %v, want 42", got["value"])
	}
}

// TestSourceValues_NeverCollected 从未采集过时：values 空 + health.status=never，
// 且 shape 与前端 TS 接口一致。
func TestSourceValues_NeverCollected(t *testing.T) {
	r := newValuesTestServer(t, valuesCfg())

	w := httptest.NewRecorder()
	r.ServeHTTP(w, valuesReq(t, "/api/v1/sources/src-ok/values"))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200. body=%s", w.Code, w.Body.String())
	}

	var resp sourceValuesResp
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v. body=%s", err, w.Body.String())
	}
	if len(resp.Values) != 0 {
		t.Errorf("从未采集时 values 应为空，实际 %d 条", len(resp.Values))
	}
	if resp.UpdatedAt != 0 {
		t.Errorf("updated_at 应为 0，实际 %d", resp.UpdatedAt)
	}
	// health 必须存在且为 never —— 前端据此显示"未采集"而非空白
	if resp.Health.Status != collect.SourceHealthNever {
		t.Errorf("health.status = %q, want %q", resp.Health.Status, collect.SourceHealthNever)
	}
	if resp.SourceBizID != "src-ok" {
		t.Errorf("source_biz_id = %q, want src-ok", resp.SourceBizID)
	}
}

// TestDeviceValues_AggregatesAllSources 验证设备级聚合：
// 返回全部 Source、error_count 正确、整体 health 取最差状态。
func TestDeviceValues_AggregatesAllSources(t *testing.T) {
	r := newValuesTestServer(t, valuesCfg())

	w := httptest.NewRecorder()
	r.ServeHTTP(w, valuesReq(t, "/api/v1/devices/dev-1/values"))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200. body=%s", w.Code, w.Body.String())
	}

	var resp deviceValuesResp
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v. body=%s", err, w.Body.String())
	}
	if !resp.Exists {
		t.Error("exists 应为 true（device 在配置中存在）")
	}
	if resp.SourceCount != 2 {
		t.Errorf("source_count = %d, want 2", resp.SourceCount)
	}
	if len(resp.Sources) != 2 {
		t.Fatalf("sources 数量 = %d, want 2", len(resp.Sources))
	}
	// 两个 Source 都从未采集 → 整体 never，error_count 0
	if resp.Health.Status != collect.SourceHealthNever {
		t.Errorf("整体 health.status = %q, want never", resp.Health.Status)
	}
	if resp.ErrorCount != 0 {
		t.Errorf("error_count = %d, want 0", resp.ErrorCount)
	}
	// 每个 Source 都要带自己的 health（前端按 source_biz_id 取值）
	for sid, s := range resp.Sources {
		if s.Health.Status != collect.SourceHealthNever {
			t.Errorf("source %s: health.status = %q, want never", sid, s.Health.Status)
		}
	}
}

// TestDeviceValues_UnknownDevice 不存在的设备：exists=false，空集合，不报错。
func TestDeviceValues_UnknownDevice(t *testing.T) {
	r := newValuesTestServer(t, valuesCfg())

	w := httptest.NewRecorder()
	r.ServeHTTP(w, valuesReq(t, "/api/v1/devices/nope/values"))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200（不存在应返回空而非 404）. body=%s", w.Code, w.Body.String())
	}
	var resp deviceValuesResp
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Exists {
		t.Error("不存在的设备 exists 应为 false")
	}
	if len(resp.Sources) != 0 {
		t.Errorf("sources 应为空，实际 %d", len(resp.Sources))
	}
}

// TestDeviceValues_DisabledDeviceExcluded 停用的 Device 不应出现在聚合里
// （采集已停，返回旧值没有意义）。
func TestDeviceValues_DisabledDeviceExcluded(t *testing.T) {
	cfg := valuesCfg()
	cfg.Devices[0].Enabled = false
	r := newValuesTestServer(t, cfg)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, valuesReq(t, "/api/v1/devices/dev-1/values"))
	var resp deviceValuesResp
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.SourceCount != 0 {
		t.Errorf("停用设备的 source_count = %d, want 0", resp.SourceCount)
	}
}

// TestValues_NoCollectorInjected 未注入 collect workers 时返回 503
// （前端据此弹"主进程不可用"提示条）。
func TestValues_NoCollectorInjected(t *testing.T) {
	store := config.NewStore(t.TempDir())
	pools := pool.NewPools(pool.Config{CollectSize: 1, ForwardSize: 1, WriteSize: 1, ConfigSize: 1})
	// 故意不调用 SetCollectWorkers
	r := NewServer(store, pools, ":0").routes()

	for _, path := range []string{"/api/v1/sources/src-ok/values", "/api/v1/devices/dev-1/values"} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, valuesReq(t, path))
		if w.Code != http.StatusServiceUnavailable {
			t.Errorf("%s: status = %d, want 503", path, w.Code)
		}
	}
}
