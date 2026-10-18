// InfluxDB Target 插件测试（V1 / V1.8+ / V2 / V3 全版本覆盖）。
//
// 单元：buildV1LineProtocol 转义、字段值转换、V1 URL 构造、ParseConfig 校验
// 集成：httptest server 模拟 InfluxDB，验证 Send 真发请求 + 内容
package target

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/plcbridge/plcbridge/internal/platform"
)

// === 单元：V1 Line Protocol 转义 ===

func TestBuildV1LineProtocol_Basic(t *testing.T) {
	ts := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
	pv := PointValue{
		DeviceBizID: "dev-1", SourceBizID: "src-1", PointBizID: "pt-temp",
		Value: 25.5, DataType: "float64",
		Timestamp: ts,
	}
	lp, err := buildV1LineProtocol(pv, &platform.Target{Topic: "plc"})
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("plc,device=dev-1,source=src-1 pt-temp=25.5 %d", ts.UnixNano())
	if lp != want {
		t.Errorf("line protocol:\n got: %s\nwant: %s", lp, want)
	}
}

func TestBuildV1LineProtocol_AllTypes(t *testing.T) {
	ts := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name     string
		pv       PointValue
		wantFrag string // 子串检查（时间戳独立测）
	}{
		{"int64", PointValue{Value: int64(42), DataType: "int64", Timestamp: ts}, " p=42i "},
		{"bool-true", PointValue{Value: true, DataType: "bool", Timestamp: ts}, " p=t "},
		{"bool-false", PointValue{Value: false, DataType: "bool", Timestamp: ts}, " p=f "},
		{"string", PointValue{Value: "hello", DataType: "string", Timestamp: ts}, ` p="hello" `},
		{"string-with-quote", PointValue{Value: `say"hi`, DataType: "string", Timestamp: ts}, ` p="say\"hi" `},
	}
	for _, c := range cases {
		c.pv.DeviceBizID = "d"
		c.pv.SourceBizID = "s"
		c.pv.PointBizID = "p"
		lp, err := buildV1LineProtocol(c.pv, &platform.Target{Topic: "plc"})
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if !strings.Contains(lp, c.wantFrag) {
			t.Errorf("%s: %q 不包含 %q", c.name, lp, c.wantFrag)
		}
	}
}

func TestBuildV1LineProtocol_EscapeTag(t *testing.T) {
	// 修订后 tags 只有 device/source，point 转 tag 路径已删；
	// 此测试现在只覆盖 device/source 的转义。
	pv := PointValue{
		DeviceBizID: "dev with space", SourceBizID: "src,1", PointBizID: "pt=1",
		Value: 1, DataType: "int32", Timestamp: time.Now(),
	}
	lp, err := buildV1LineProtocol(pv, &platform.Target{Topic: "plc"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(lp, `device=dev\ with\ space`) {
		t.Errorf("空格未转义: %s", lp)
	}
	if !strings.Contains(lp, `source=src\,1`) {
		t.Errorf("逗号未转义: %s", lp)
	}
	// point tag 已移除；这里改断言"等号不再作为 tag 出现"
	if strings.Contains(lp, `,point=`) {
		t.Errorf("point tag 不应再出现: %s", lp)
	}
	// 但 point_biz_id 现在作为 field key，需要 escape
	if !strings.Contains(lp, `pt\=1=1i`) {
		t.Errorf("field key 'pt=1' 应被 escape 为 'pt\\=1=1i', got: %s", lp)
	}
}

func TestFormatTimestamp(t *testing.T) {
	ts := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		precision string
		want      string
	}{
		{"ns", "1767225600000000000"},
		{"us", "1767225600000000"},
		{"ms", "1767225600000"},
		{"s", "1767225600"},
	}
	for _, c := range cases {
		got := formatTimestamp(ts, c.precision)
		if got != c.want {
			t.Errorf("precision=%s: got %s, want %s", c.precision, got, c.want)
		}
	}
}

// === 单元：V1 URL 构造 ===

func TestBuildV1WriteURL(t *testing.T) {
	c := &influxDBV1Client{
		addr: "http://localhost:8086",
		cfg:  InfluxDBConfig{Version: InfluxV1, DB: "mydb", Precision: "ns"},
	}
	got, err := c.buildV1WriteURL()
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(got)
	if u.Scheme != "http" || u.Host != "localhost:8086" || u.Path != "/write" {
		t.Errorf("URL 路径错: %s", got)
	}
	if u.Query().Get("db") != "mydb" || u.Query().Get("precision") != "ns" {
		t.Errorf("query 错: %s", got)
	}
}

func TestBuildV1WriteURL_BadAddr(t *testing.T) {
	c := &influxDBV1Client{addr: "://bad", cfg: InfluxDBConfig{DB: "d"}}
	if _, err := c.buildV1WriteURL(); err == nil {
		t.Error("坏地址应报错")
	}
}

// === 单元：ParseConfig 校验（通过 CreateTarget 入口间接测试） ===

func TestCreateTarget_VersionDefaultsTo2(t *testing.T) {
	// 不指定 version → 应走 V2 默认路径
	cfgJSON, _ := json.Marshal(InfluxDBConfig{Org: "o", Bucket: "b", Token: "t"})
	_, err := newInfluxDBPlugin().CreateTarget(
		&platform.Target{TargetBizID: "t", Topic: "plc"},
		&platform.Channel{ChannelBizID: "c", Type: "influxdb", Address: "http://x", Config: cfgJSON},
	)
	if err != nil {
		t.Errorf("默认 V2 应成功: %v", err)
	}
}

func TestCreateTarget_InvalidVersion(t *testing.T) {
	cfgJSON, _ := json.Marshal(InfluxDBConfig{Version: InfluxDBVersion("99.x"), Org: "o", Bucket: "b", Token: "t"})
	_, err := newInfluxDBPlugin().CreateTarget(
		&platform.Target{TargetBizID: "t", Topic: "plc"},
		&platform.Channel{ChannelBizID: "c", Type: "influxdb", Address: "http://x", Config: cfgJSON},
	)
	if err == nil {
		t.Error("非法 version 应报错")
	}
}

func TestCreateTarget_V1RequiresDB(t *testing.T) {
	cfgJSON, _ := json.Marshal(InfluxDBConfig{Version: InfluxV1})
	_, err := newInfluxDBPlugin().CreateTarget(
		&platform.Target{TargetBizID: "t", Topic: "plc"},
		&platform.Channel{ChannelBizID: "c", Type: "influxdb", Address: "http://x", Config: cfgJSON},
	)
	if err == nil {
		t.Error("V1 缺 db 应报错")
	}
}

func TestCreateTarget_V2RequiresOrg(t *testing.T) {
	cfgJSON, _ := json.Marshal(InfluxDBConfig{Version: InfluxV2, Bucket: "b", Token: "t"})
	_, err := newInfluxDBPlugin().CreateTarget(
		&platform.Target{TargetBizID: "t", Topic: "plc"},
		&platform.Channel{ChannelBizID: "c", Type: "influxdb", Address: "http://x", Config: cfgJSON},
	)
	if err == nil {
		t.Error("V2 缺 org 应报错")
	}
}

// === 单元：CreateTarget 拒空 topic（用于 InfluxDB measurement） ===

func TestCreateTarget_RequiresTopic(t *testing.T) {
	cfgJSON, _ := json.Marshal(InfluxDBConfig{Version: InfluxV1, DB: "d"})
	_, err := newInfluxDBPlugin().CreateTarget(
		&platform.Target{TargetBizID: "t"}, // topic 空
		&platform.Channel{ChannelBizID: "c", Type: "influxdb", Address: "http://x", Config: cfgJSON},
	)
	if err == nil {
		t.Error("InfluxDB Target topic 为空应报错（用作 InfluxDB measurement 名）")
	}
}

func TestCreateTarget_NilTarget(t *testing.T) {
	cfgJSON, _ := json.Marshal(InfluxDBConfig{Version: InfluxV1, DB: "d"})
	_, err := newInfluxDBPlugin().CreateTarget(
		nil, // target 为 nil
		&platform.Channel{ChannelBizID: "c", Type: "influxdb", Address: "http://x", Config: cfgJSON},
	)
	if err == nil {
		t.Error("InfluxDB Target 为 nil 应报错")
	}
}

// === 单元：resolveMeasurement 4 种场景 ===

func TestResolveMeasurement(t *testing.T) {
	cases := []struct {
		name   string
		target *platform.Target
		want   string
	}{
		{"nil target fallback", nil, "plc"},
		{"empty topic fallback", &platform.Target{Topic: ""}, "plc"},
		{"custom topic", &platform.Target{Topic: "device01"}, "device01"},
		{"topic with special char（注意：web 端应有校验，运行时兜底不逃逸）", &platform.Target{Topic: "opc data"}, "opc data"},
	}
	for _, c := range cases {
		got := resolveMeasurement(c.target)
		if got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}// === 集成：V1 端到端 ===

type v1FakeServer struct {
	mu          sync.Mutex
	lines       []string
	queryParams url.Values
	callCount   int
	statusCode  int
}

func (s *v1FakeServer) handler(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.callCount++
	if s.statusCode > 0 {
		s.statusCode--
		http.Error(w, "boom", http.StatusInternalServerError)
		return
	}
	if r.URL.Path == "/ping" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.URL.Path != "/write" {
		http.NotFound(w, r)
		return
	}
	body, _ := io.ReadAll(r.Body)
	s.queryParams = r.URL.Query()
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		if line != "" {
			s.lines = append(s.lines, line)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func TestInfluxDBV1_Send(t *testing.T) {
	srv := &v1FakeServer{}
	ts := httptest.NewServer(http.HandlerFunc(srv.handler))
	defer ts.Close()

	cfgJSON, _ := json.Marshal(InfluxDBConfig{Version: InfluxV1, DB: "mydb", Precision: "ns"})
	channel := &platform.Channel{ChannelBizID: "c", Type: "influxdb", Address: ts.URL, Config: cfgJSON}
	tgt := &platform.Target{TargetBizID: "t", ForwardChannelBizID: "c", Topic: "plc"}
	cli, err := newInfluxDBPlugin().CreateTarget(tgt, channel)
	if err != nil {
		t.Fatal(err)
	}

	batch := []PointValue{
		{DeviceBizID: "dev-1", SourceBizID: "src-1", PointBizID: "pt-temp", Value: 25.5, DataType: "float64", Timestamp: time.Now()},
		{DeviceBizID: "dev-1", SourceBizID: "src-1", PointBizID: "pt-press", Value: int64(101325), DataType: "int64", Timestamp: time.Now()},
	}
	if err := cli.Send(context.Background(), batch); err != nil {
		t.Fatal(err)
	}

	srv.mu.Lock()
	defer srv.mu.Unlock()
	if len(srv.lines) != 2 {
		t.Fatalf("V1 收到 %d 行, want 2", len(srv.lines))
	}
	if srv.queryParams.Get("db") != "mydb" {
		t.Errorf("query db = %q, want mydb", srv.queryParams.Get("db"))
	}
	if srv.queryParams.Get("precision") != "ns" {
		t.Errorf("query precision = %q, want ns", srv.queryParams.Get("precision"))
	}
	if !strings.Contains(srv.lines[0], "plc,device=dev-1,source=src-1 pt-temp=25.5") {
		t.Errorf("line 0 格式错: %s", srv.lines[0])
	}
	if !strings.Contains(srv.lines[1], "pt-press=101325i") {
		t.Errorf("int64 应写为 101325i, got: %s", srv.lines[1])
	}
}

func TestInfluxDBV1_BasicAuth(t *testing.T) {
	srv := &v1FakeServer{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "alice" || pass != "secret" {
			http.Error(w, "unauth", http.StatusUnauthorized)
			return
		}
		srv.handler(w, r)
	}))
	defer ts.Close()

	cfgJSON, _ := json.Marshal(InfluxDBConfig{
		Version: InfluxV1, DB: "mydb", Precision: "ns",
		Username: "alice", Password: "secret",
	})
	channel := &platform.Channel{ChannelBizID: "c", Type: "influxdb", Address: ts.URL, Config: cfgJSON}
	tgt := &platform.Target{TargetBizID: "t", ForwardChannelBizID: "c", Topic: "plc"}
	cli, err := newInfluxDBPlugin().CreateTarget(tgt, channel)
	if err != nil {
		t.Fatal(err)
	}
	if err := cli.Send(context.Background(), []PointValue{{
		DeviceBizID: "d", SourceBizID: "s", PointBizID: "p",
		Value: 1, DataType: "int32", Timestamp: time.Now(),
	}}); err != nil {
		t.Errorf("Basic Auth 应通过: %v", err)
	}
}

func TestInfluxDBV1_ServerError_Propagates(t *testing.T) {
	srv := &v1FakeServer{statusCode: 1}
	ts := httptest.NewServer(http.HandlerFunc(srv.handler))
	defer ts.Close()
	cfgJSON, _ := json.Marshal(InfluxDBConfig{Version: InfluxV1, DB: "d"})
	channel := &platform.Channel{ChannelBizID: "c", Type: "influxdb", Address: ts.URL, Config: cfgJSON}
	tgt := &platform.Target{TargetBizID: "t", ForwardChannelBizID: "c", Topic: "plc"}
	cli, _ := newInfluxDBPlugin().CreateTarget(tgt, channel)
	if err := cli.Send(context.Background(), []PointValue{{
		DeviceBizID: "d", SourceBizID: "s", PointBizID: "p",
		Value: 1, DataType: "int32", Timestamp: time.Now(),
	}}); err == nil {
		t.Error("5xx 应返回 error")
	}
}

func TestInfluxDBV1_HealthCheck(t *testing.T) {
	srv := &v1FakeServer{}
	ts := httptest.NewServer(http.HandlerFunc(srv.handler))
	defer ts.Close()
	cfgJSON, _ := json.Marshal(InfluxDBConfig{Version: InfluxV1, DB: "d"})
	channel := &platform.Channel{ChannelBizID: "c", Type: "influxdb", Address: ts.URL, Config: cfgJSON}
	tgt := &platform.Target{TargetBizID: "t", ForwardChannelBizID: "c", Topic: "plc"}
	cli, _ := newInfluxDBPlugin().CreateTarget(tgt, channel)
	if err := cli.HealthCheck(context.Background()); err != nil {
		t.Errorf("HealthCheck 应通过: %v", err)
	}
}

// === 集成：V2 端到端（保留旧测试，确认 V2 client 仍工作） ===

type v2FakeServer struct {
	mu      sync.Mutex
	lines   []string
	headers http.Header
	status  int
	count   int
}

func (s *v2FakeServer) handler(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.count++
	if s.status > 0 {
		s.status--
		http.Error(w, "boom", http.StatusInternalServerError)
		return
	}
	body, _ := io.ReadAll(r.Body)
	s.headers = r.Header.Clone()
	for _, l := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		if l != "" {
			s.lines = append(s.lines, l)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func TestInfluxDBV2_Send(t *testing.T) {
	srv := &v2FakeServer{}
	ts := httptest.NewServer(http.HandlerFunc(srv.handler))
	defer ts.Close()

	cfgJSON, _ := json.Marshal(InfluxDBConfig{
		Version: InfluxV2, Org: "my-org", Bucket: "my-bucket", Token: "secret-token",
	})
	channel := &platform.Channel{ChannelBizID: "c", Type: "influxdb", Address: ts.URL, Config: cfgJSON}
	tgt := &platform.Target{TargetBizID: "t", ForwardChannelBizID: "c", Topic: "plc"}
	cli, err := newInfluxDBPlugin().CreateTarget(tgt, channel)
	if err != nil {
		t.Fatal(err)
	}
	batch := []PointValue{
		{DeviceBizID: "d", SourceBizID: "s", PointBizID: "p",
			Value: true, DataType: "bool", Timestamp: time.Now()},
	}
	if err := cli.Send(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if len(srv.lines) != 1 {
		t.Fatalf("V2 收到 %d 行, want 1", len(srv.lines))
	}
	auth := srv.headers.Get("Authorization")
	if auth != "Token secret-token" {
		t.Errorf("V2 鉴权头 = %q, want %q", auth, "Token secret-token")
	}
}

// === 路由分发：版本 → 客户端类型 ===

func TestCreateTarget_VersionRouting(t *testing.T) {
	cfgJSON, _ := json.Marshal(InfluxDBConfig{Version: InfluxV1, DB: "d"})
	v1Cli, err := newInfluxDBPlugin().CreateTarget(
		&platform.Target{TargetBizID: "t", Topic: "plc"},
		&platform.Channel{ChannelBizID: "c", Type: "influxdb", Address: "http://x", Config: cfgJSON},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := v1Cli.(*influxDBV1Client); !ok {
		t.Error("V1.x 应路由到 influxDBV1Client")
	}

	cfgJSON, _ = json.Marshal(InfluxDBConfig{Version: InfluxV2, Org: "o", Bucket: "b", Token: "t"})
	v2Cli, err := newInfluxDBPlugin().CreateTarget(
		&platform.Target{TargetBizID: "t", Topic: "plc"},
		&platform.Channel{ChannelBizID: "c", Type: "influxdb", Address: "http://x", Config: cfgJSON},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := v2Cli.(*influxDBV2Client); !ok {
		t.Error("V2.0 应路由到 influxDBV2Client")
	}

	cfgJSON, _ = json.Marshal(InfluxDBConfig{Version: InfluxV3, Org: "o", Bucket: "b", Token: "t"})
	v3Cli, err := newInfluxDBPlugin().CreateTarget(
		&platform.Target{TargetBizID: "t", Topic: "plc"},
		&platform.Channel{ChannelBizID: "c", Type: "influxdb", Address: "http://x", Config: cfgJSON},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := v3Cli.(*influxDBV2Client); !ok {
		t.Error("V3.x 也应路由到 influxDBV2Client（V3 server 默认 V2 兼容）")
	}
}

// 编译期接口断言
var (
	_ TargetClient   = (*influxDBV1Client)(nil)
	_ TargetClient   = (*influxDBV2Client)(nil)
	_ TargetCloser   = (*influxDBV2Client)(nil) // 注入 close 释放 conn pool
)

// TestInfluxDBV2Client_Close_Idempotent 验证 Close 幂等且不 panic。
func TestInfluxDBV2Client_Close_Idempotent(t *testing.T) {
	cfg := InfluxDBConfig{Version: InfluxV2, Org: "o", Bucket: "b", Token: "t", ConnectionTimeoutMs: 200}
	c := &influxDBV2Client{addr: "http://127.0.0.1:1", cfg: cfg}
	if err := c.Close(); err != nil {
		t.Errorf("第一次 Close（cli=nil）应返回 nil, got %v", err)
	}
	if err := c.Close(); err != nil {
		t.Errorf("第二次 Close 应返回 nil（幂等）, got %v", err)
	}
}

// TestInfluxDBV2Client_Stats_PR2Fields 验证 PR2 字段都存在（即使为 0）。
func TestInfluxDBV2Client_Stats_PR2Fields(t *testing.T) {
	cfg := InfluxDBConfig{Version: InfluxV2, Org: "o", Bucket: "b", Token: "t", ConnectionTimeoutMs: 200}
	c := &influxDBV2Client{addr: "http://127.0.0.1:1", cfg: cfg, target: &platform.Target{TargetBizID: "t"}}
	stats := c.Stats()
	// PR1 字段
	if stats["connection_state"] != "Disconnected" {
		t.Errorf("connection_state = %v, want Disconnected", stats["connection_state"])
	}
	if stats["protocol"] != "influxdb" {
		t.Errorf("protocol = %v, want influxdb", stats["protocol"])
	}
	// PR2 字段
	for _, k := range []string{"sent_count", "sent_points", "error_count", "connect_count", "health_check_count"} {
		if _, ok := stats[k]; !ok {
			t.Errorf("Stats missing key %q", k)
		}
	}
}

// TestInfluxDBV1Client_Stats_PR2Fields 同上 V1 路径。
func TestInfluxDBV1Client_Stats_PR2Fields(t *testing.T) {
	cfg := InfluxDBConfig{Version: InfluxV1, DB: "test", ConnectionTimeoutMs: 200}
	c := &influxDBV1Client{addr: "http://127.0.0.1:1", cfg: cfg, target: &platform.Target{TargetBizID: "t"}}
	stats := c.Stats()
	if stats["connection_state"] != "Disconnected" {
		t.Errorf("connection_state = %v, want Disconnected", stats["connection_state"])
	}
	if stats["protocol"] != "influxdb" {
		t.Errorf("protocol = %v, want influxdb", stats["protocol"])
	}
	if stats["version"] != string(InfluxV1) {
		t.Errorf("version = %v, want %v", stats["version"], InfluxV1)
	}
	for _, k := range []string{"sent_count", "sent_points", "error_count", "connect_count", "health_check_count"} {
		if _, ok := stats[k]; !ok {
			t.Errorf("Stats missing key %q", k)
		}
	}
}
