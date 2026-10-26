package influxdb

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/plcbridge/plcbridge/internal/infra/target"
	"github.com/plcbridge/plcbridge/internal/platform"
)

// TestInfluxDBV1Client_HealthCheck_RecoversAfterFailure 验证：
// 探活失败 → 探活成功 → 客户端恢复健康且清空最近错误。
//
// 这是"influxdb 停掉很久后状态才刷新、只有触发诊断才显示异常"的根因：
// 旧代码 HealthCheck 成功路径什么都不做，于是
//   - healthy 一旦为 false 就再也回不来
//   - last_error 永远残留 → 前端状态列一直显示"异常"
//
// 修复后（2026-10-25）：成功路径 markHealthy() + setLastError(nil)。
func TestInfluxDBV1Client_HealthCheck_RecoversAfterFailure(t *testing.T) {
	// 先用一个"总是失败"的 server 让探活出错。
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer failing.Close()

	cfg := InfluxDBConfig{Version: InfluxV1, DB: "test", ConnectionTimeoutMs: 1000}
	c := &influxDBV1Client{addr: failing.URL, cfg: cfg}

	ctx := context.Background()
	if err := c.HealthCheck(ctx); err == nil {
		t.Fatal("对失败 server 的 HealthCheck 应报错")
	}
	if c.IsConnected() {
		t.Error("探活失败后 IsConnected 应为 false")
	}
	if c.LastError() == "" {
		t.Error("探活失败后 LastError 不应为空")
	}

	// 换成正常的 server（模拟 InfluxDB 恢复）。
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ok.Close()
	c.addr = ok.URL

	if err := c.HealthCheck(ctx); err != nil {
		t.Fatalf("恢复后 HealthCheck 应成功: %v", err)
	}
	if !c.IsConnected() {
		t.Error("恢复后 IsConnected 应为 true")
	}
	if c.LastError() != "" {
		t.Errorf("恢复后 LastError 应被清空，实际 = %q", c.LastError())
	}
	if got := c.Stats()["connection_state"]; got != "Connected" {
		t.Errorf("connection_state = %v, want Connected", got)
	}
}

// TestInfluxDBV1Client_HealthCheck_KeepsErrorWhenDown 验证：
// 探活持续失败时，错误信息与 unhealthy 状态被正确保留（供 UI 展示）。
func TestInfluxDBV1Client_HealthCheck_KeepsErrorWhenDown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	cfg := InfluxDBConfig{Version: InfluxV1, DB: "test", ConnectionTimeoutMs: 1000}
	c := &influxDBV1Client{addr: srv.URL, cfg: cfg}

	if err := c.HealthCheck(context.Background()); err == nil {
		t.Fatal("探活应报错")
	}
	if c.IsConnected() {
		t.Error("探活失败后 IsConnected 应为 false")
	}
	if c.LastError() == "" {
		t.Error("LastError 应保留失败原因")
	}
	if got := c.Stats()["connection_state"]; got != "Disconnected" {
		t.Errorf("connection_state = %v, want Disconnected", got)
	}
}

// TestInfluxDBV1Client_Send_MarksUnhealthy 验证：
// 写失败会把客户端标记为不健康（供 UI 显示"异常"）。
func TestInfluxDBV1Client_Send_MarksUnhealthy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	cfg := InfluxDBConfig{Version: InfluxV1, DB: "test", ConnectionTimeoutMs: 1000}
	c := &influxDBV1Client{addr: srv.URL, cfg: cfg,
		target: &platform.Target{TargetBizID: "t", Topic: "m"}}

	err := c.Send(context.Background(), []target.PointValue{
		{DeviceBizID: "d", PointBizID: "p", Value: int64(1)},
	})
	if err == nil {
		t.Fatal("写入失败 server 的 Send 应报错")
	}
	if c.IsConnected() {
		t.Error("写失败后 IsConnected 应为 false")
	}
	if c.LastError() == "" {
		t.Error("写失败后 LastError 不应为空")
	}
}
