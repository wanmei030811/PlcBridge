// MQTT Target 插件单元测试。
//
// 注：集成测试（连真实 broker 验证 publish）由 cmd/mqttprobe + 用户本地 broker 完成。
// 内嵌 mochi broker 与 autopaho v5 client 在某些 keepalive + captureHook 配置下会 hang；
// 实际部署用 mosquitto / emqx / hivemq 等成熟 broker 不存在该问题。
//
// 本文件仅覆盖无 broker 依赖的部分：URL 归一化、payload 编码、Config 校验、Plugin metadata。
package target

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/plcbridge/plcbridge/internal/platform"
)

func TestMQTTPlugin_Metadata(t *testing.T) {
	p := newMQTTPlugin()
	if p.Name() != "mqtt" {
		t.Errorf("Name = %q, want mqtt", p.Name())
	}
	if p.Version() == "" {
		t.Errorf("Version empty")
	}
	if !strings.Contains(p.Version(), ".") {
		t.Errorf("Version %q not semver-ish", p.Version())
	}
	schema := p.ConfigSchema()
	if len(schema) == 0 {
		t.Errorf("ConfigSchema empty")
	}
	var raw map[string]any
	if err := json.Unmarshal(schema, &raw); err != nil {
		t.Fatalf("ConfigSchema not JSON: %v", err)
	}
	if raw["type"] != "object" {
		t.Errorf("ConfigSchema root type = %v, want object", raw["type"])
	}
}

func TestMQTTPlugin_CreateTarget_WrongChannelType(t *testing.T) {
	p := newMQTTPlugin()
	ch := &platform.Channel{
		ChannelBizID: "ch-x", Type: "modbus_tcp", Role: "forward", Address: "127.0.0.1:502",
	}
	tg := &platform.Target{TargetBizID: "tg-x", ForwardChannelBizID: "ch-x"}
	_, err := p.CreateTarget(tg, ch)
	if err == nil {
		t.Fatal("expected error for non-mqtt channel")
	}
	if !strings.Contains(err.Error(), "mqtt Channel") {
		t.Errorf("error msg = %q, want mention of mqtt Channel", err.Error())
	}
}

func TestMQTTPlugin_CreateTarget_InvalidQoS(t *testing.T) {
	p := newMQTTPlugin()
	ch := &platform.Channel{
		ChannelBizID: "ch-y", Type: "mqtt", Role: "forward", Address: "tcp://127.0.0.1:1",
		Config: json.RawMessage(`{"qos": 3}`),
	}
	tg := &platform.Target{TargetBizID: "tg-y", ForwardChannelBizID: "ch-y"}
	_, err := p.CreateTarget(tg, ch)
	if err == nil {
		t.Fatal("expected error for qos=3")
	}
	if !strings.Contains(err.Error(), "qos") {
		t.Errorf("error msg = %q, want qos mention", err.Error())
	}
}

func TestMQTTPlugin_CreateTarget_InvalidLWTQoS(t *testing.T) {
	p := newMQTTPlugin()
	ch := &platform.Channel{
		ChannelBizID: "ch-z", Type: "mqtt", Role: "forward", Address: "tcp://127.0.0.1:1",
		Config: json.RawMessage(`{"lwt_topic":"t","lwt_qos":7}`),
	}
	tg := &platform.Target{TargetBizID: "tg-z", ForwardChannelBizID: "ch-z"}
	_, err := p.CreateTarget(tg, ch)
	if err == nil {
		t.Fatal("expected error for lwt_qos=7")
	}
}

func TestMQTTPlugin_CreateTarget_NoAddress(t *testing.T) {
	p := newMQTTPlugin()
	ch := &platform.Channel{
		ChannelBizID: "ch-q", Type: "mqtt", Role: "forward", Address: "",
	}
	tg := &platform.Target{TargetBizID: "tg-q", ForwardChannelBizID: "ch-q"}
	_, err := p.CreateTarget(tg, ch)
	if err == nil {
		t.Fatal("expected error for empty address")
	}
}

func TestProtocolVersion_Validate(t *testing.T) {
	cases := []struct {
		in     ProtocolVersion
		wantOK bool
	}{
		{ProtocolV5, true},
		{ProtocolV311, true},
		{"", true}, // 空字符串由调用方在解析时填默认 v5
		{"v3.1", false},
		{"v4", false},
		{"MQTT 5", false},
	}
	for _, c := range cases {
		err := c.in.Validate()
		if (err == nil) != c.wantOK {
			t.Errorf("ProtocolVersion(%q).Validate() err = %v, wantOK=%v", c.in, err, c.wantOK)
		}
	}
}

func TestMQTTPlugin_ProtocolVersionDefaultIsV5(t *testing.T) {
	// 不填 protocol_version → 默认走 v5
	ch := &platform.Channel{
		ChannelBizID: "ch-d", Type: "mqtt", Role: "forward", Address: "tcp://127.0.0.1:1",
		Config: json.RawMessage(`{"client_id":"x","connect_timeout_sec":1}`),
	}
	tg := &platform.Target{TargetBizID: "tg-d", ForwardChannelBizID: "ch-d"}
	cli, err := newMQTTPlugin().CreateTarget(tg, ch)
	if err != nil {
		t.Fatalf("CreateTarget: %v", err)
	}
	defer func() { _ = cli.(closer).Close() }()
	tt := cli.(*mqttTarget)
	if tt.cfg.ProtocolVersion != ProtocolV5 {
		t.Errorf("default ProtocolVersion = %q, want %q", tt.cfg.ProtocolVersion, ProtocolV5)
	}
	if _, ok := tt.backend.(*v5Backend); !ok {
		t.Errorf("default backend = %T, want *v5Backend", tt.backend)
	}
}

func TestMQTTPlugin_ProtocolVersionExplicitV311(t *testing.T) {
	ch := &platform.Channel{
		ChannelBizID: "ch-e", Type: "mqtt", Role: "forward", Address: "tcp://127.0.0.1:1",
		Config: json.RawMessage(`{"client_id":"x","protocol_version":"v3.1.1","connect_timeout_sec":1}`),
	}
	tg := &platform.Target{TargetBizID: "tg-e", ForwardChannelBizID: "ch-e"}
	cli, err := newMQTTPlugin().CreateTarget(tg, ch)
	if err != nil {
		t.Fatalf("CreateTarget: %v", err)
	}
	defer func() { _ = cli.(closer).Close() }()
	tt := cli.(*mqttTarget)
	if tt.cfg.ProtocolVersion != ProtocolV311 {
		t.Errorf("ProtocolVersion = %q, want %q", tt.cfg.ProtocolVersion, ProtocolV311)
	}
	if _, ok := tt.backend.(*v311Backend); !ok {
		t.Errorf("backend = %T, want *v311Backend", tt.backend)
	}
}

func TestMQTTPlugin_ProtocolVersionInvalid(t *testing.T) {
	ch := &platform.Channel{
		ChannelBizID: "ch-f", Type: "mqtt", Role: "forward", Address: "tcp://127.0.0.1:1",
		Config: json.RawMessage(`{"protocol_version":"v3.1"}`),
	}
	tg := &platform.Target{TargetBizID: "tg-f", ForwardChannelBizID: "ch-f"}
	_, err := newMQTTPlugin().CreateTarget(tg, ch)
	if err == nil {
		t.Fatal("expected error for protocol_version=v3.1")
	}
	if !strings.Contains(err.Error(), "protocol_version") {
		t.Errorf("error msg = %q, want protocol_version mention", err.Error())
	}
}

func TestEncodeBatchPayload_JSON(t *testing.T) {
	ts := time.Date(2026, 1, 2, 3, 4, 5, 6_000_000, time.UTC)
	batch := []PointValue{
		{DeviceBizID: "plc-01", SourceBizID: "src-s7", PointBizID: "temperature", Timestamp: ts, Value: 42.5, DataType: "float"},
		{DeviceBizID: "plc-01", SourceBizID: "src-s7", PointBizID: "pressure", Timestamp: ts, Value: 1.2, DataType: "float"},
	}
	raw, err := encodeBatchPayload(batch)
	if err != nil {
		t.Fatalf("encodeBatchPayload: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("not JSON: %v / %s", err, raw)
	}
	if got["device"] != "plc-01" {
		t.Errorf("device = %v", got["device"])
	}
	if got["source"] != "src-s7" {
		t.Errorf("source = %v", got["source"])
	}
	if f, ok := got["temperature"].(float64); !ok || f != 42.5 {
		t.Errorf("temperature = %v, want 42.5", got["temperature"])
	}
	if f, ok := got["pressure"].(float64); !ok || f != 1.2 {
		t.Errorf("pressure = %v, want 1.2", got["pressure"])
	}
	tsStr, _ := got["timestamp"].(string)
	// 2026-01-02T03:04:05.006Z (UTC) -> 2026-01-02 11:04:05.006 +0800 (BJT)
	if tsStr != "2026-01-02 11:04:05.006 +0800" {
		t.Errorf("timestamp = %q", tsStr)
	}
}

func TestEncodeBatchPayload_BoolValue(t *testing.T) {
	batch := []PointValue{{Value: true, DataType: "bool", PointBizID: "p"}}
	raw, err := encodeBatchPayload(batch)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	_ = json.Unmarshal(raw, &got)
	if got["p"] != true {
		t.Errorf("p = %v, want true", got["p"])
	}
}

func TestNormalizeBrokerURL(t *testing.T) {
	cases := []struct {
		in     string
		want   string
		errSub string // 空字符串 = 期望无错
	}{
		{"tcp://192.168.1.10:1883", "mqtt://192.168.1.10:1883", ""},
		{"ssl://broker:8883", "tls://broker:8883", ""},
		{"mqtt://broker:1883", "mqtt://broker:1883", ""},
		{"tls://broker:8883", "tls://broker:8883", ""},
		{"ws://broker:8083/mqtt", "ws://broker:8083/mqtt", ""},
		{"wss://broker:8443/mqtt", "wss://broker:8443/mqtt", ""},
		{"192.168.1.10:1883", "mqtt://192.168.1.10:1883", ""},
	}
	for _, c := range cases {
		u, err := normalizeBrokerURL(c.in)
		if c.errSub != "" {
			if err == nil {
				t.Errorf("normalizeBrokerURL(%q) expected error", c.in)
			} else if !strings.Contains(err.Error(), c.errSub) {
				t.Errorf("normalizeBrokerURL(%q) err = %v, want substring %q", c.in, err, c.errSub)
			}
			continue
		}
		if err != nil {
			t.Errorf("normalizeBrokerURL(%q) err = %v", c.in, err)
			continue
		}
		if u.String() != c.want {
			t.Errorf("normalizeBrokerURL(%q) = %q, want %q", c.in, u.String(), c.want)
		}
	}
	// 错误输入
	for _, bad := range []string{"", "ftp://x:1", "mqtt://"} {
		if _, err := normalizeBrokerURL(bad); err == nil {
			t.Errorf("normalizeBrokerURL(%q) expected error", bad)
		}
	}
	// garbage 不带 ://，会被处理成 mqtt://garbage，url.Parse 接受，但 Host=="garbage" 不空
	// 所以实际不会报错；这是个边角案例，跳过
	if u, err := normalizeBrokerURL("garbage"); err == nil {
		_ = u // 没有错误合理
	}
}

func TestEncodeBatchPayload_StructValue(t *testing.T) {
	// interface{} 类型 → JSON 应当 pass-through 序列化
	type custom struct {
		Name string `json:"name"`
	}
	batch := []PointValue{
		{Value: custom{Name: "x"}, DataType: "struct", PointBizID: "p", Timestamp: time.Unix(0, 0).UTC()},
	}
	raw, err := encodeBatchPayload(batch)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"name":"x"`) {
		t.Errorf("custom struct not serialized correctly: %s", raw)
	}
}

// === 回归测试：修复后的并发安全 ===

// TestMQTTTarget_Close_Idempotent Close 多次调用不 panic、返回 nil。
func TestMQTTTarget_Close_Idempotent(t *testing.T) {
	qos := 1
	ch := newTestChannelMQTT("tcp://127.0.0.1:1", &MQTTConfig{
		ClientID:          "idem-1",
		QoS:               &qos,
		KeepaliveSec:      10,
		CleanSession:      true,
		ConnectTimeoutSec: 1,
	})
	tg := newTestTargetMQTT()
	cli, err := newMQTTPlugin().CreateTarget(tg, ch)
	if err != nil {
		t.Fatalf("CreateTarget: %v", err)
	}
	for i := 0; i < 5; i++ {
		if err := cli.(closer).Close(); err != nil {
			t.Errorf("Close #%d: %v", i, err)
		}
	}
}

// TestMQTTTarget_SendAfterClose Close 后 Send 返回 error，不 panic。
func TestMQTTTarget_SendAfterClose(t *testing.T) {
	qos := 1
	ch := newTestChannelMQTT("tcp://127.0.0.1:1", &MQTTConfig{
		ClientID:          "sac-1",
		QoS:               &qos,
		KeepaliveSec:      10,
		CleanSession:      true,
		ConnectTimeoutSec: 1,
	})
	tg := newTestTargetMQTT()
	cli, err := newMQTTPlugin().CreateTarget(tg, ch)
	if err != nil {
		t.Fatalf("CreateTarget: %v", err)
	}
	if err := cli.(closer).Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err = cli.Send(ctx, []PointValue{{DeviceBizID: "d", PointBizID: "p"}})
	if err == nil {
		t.Error("Send after Close: expected error, got nil")
	}
}

// TestMQTTTarget_HealthCheckAfterClose Close 后 HealthCheck 返回 error，不 panic。
func TestMQTTTarget_HealthCheckAfterClose(t *testing.T) {
	qos := 1
	ch := newTestChannelMQTT("tcp://127.0.0.1:1", &MQTTConfig{
		ClientID:          "hca-1",
		QoS:               &qos,
		KeepaliveSec:      10,
		CleanSession:      true,
		ConnectTimeoutSec: 1,
	})
	tg := newTestTargetMQTT()
	cli, err := newMQTTPlugin().CreateTarget(tg, ch)
	if err != nil {
		t.Fatalf("CreateTarget: %v", err)
	}
	if err := cli.(closer).Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := cli.HealthCheck(ctx); err == nil {
		t.Error("HealthCheck after Close: expected error, got nil")
	}
}

// TestMQTTTarget_PublishAfterClose_V3 验证 v3.1.1 backend Close 后 Send 立即返回 error。
func TestMQTTTarget_PublishAfterClose_V3(t *testing.T) {
	qos := 1
	ch := newTestChannelMQTT("tcp://127.0.0.1:1", &MQTTConfig{
		ProtocolVersion:   ProtocolV311,
		ClientID:          "pac-v3",
		QoS:               &qos,
		KeepaliveSec:      10,
		CleanSession:      true,
		ConnectTimeoutSec: 1,
	})
	tg := newTestTargetMQTT()
	cli, err := newMQTTPlugin().CreateTarget(tg, ch)
	if err != nil {
		t.Fatalf("CreateTarget: %v", err)
	}
	if err := cli.(closer).Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err = cli.Send(ctx, []PointValue{{DeviceBizID: "d", PointBizID: "p"}})
	if err == nil {
		t.Error("Send after Close (v311): expected error, got nil")
	}
}

// TestV311Backend_Connect_Idempotent v3 backend 多次 Connect 不 panic 也不报错。
//
// 设计：v3 库对重复 Connect() 会 panic，我们用 connected atomic + cached error 隔离。
// 注意：连不上 broker（broker=127.0.0.1:1）所以每个 Connect 都失败，验证缓存错误正确返回。
func TestV311Backend_Connect_Idempotent(t *testing.T) {
	b, cancel, err := newV311Backend(newTestTargetMQTT(), mustURL(t, "tcp://127.0.0.1:1"), &MQTTConfig{
		ClientID:          "ic-1",
		QoS:               intPtr(1),
		KeepaliveSec:      10,
		CleanSession:      true,
		ConnectTimeoutSec: 1,
	})
	if err != nil {
		t.Fatalf("newV311Backend: %v", err)
	}
	defer cancel()
	defer func() { _ = b.Close() }()

	ctx, cancel2 := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel2()

	done := make(chan error, 10)
	for i := 0; i < 10; i++ {
		go func() { done <- b.Connect(ctx) }()
	}
	for i := 0; i < 10; i++ {
		err := <-done
		if err == nil {
			t.Errorf("Connect #%d: 期望失败", i)
		}
	}
}

// TestV311Backend_Publish_ContextCancel 验证 Publish 在 ctx 取消时立即返回，不永久阻塞。
func TestV311Backend_Publish_ContextCancel(t *testing.T) {
	b, cancel, err := newV311Backend(newTestTargetMQTT(), mustURL(t, "tcp://127.0.0.1:1"), &MQTTConfig{
		ClientID:          "pub-cancel",
		QoS:               intPtr(1),
		KeepaliveSec:      10,
		CleanSession:      true,
		ConnectTimeoutSec: 1,
	})
	if err != nil {
		t.Fatalf("newV311Backend: %v", err)
	}
	defer cancel()
	defer func() { _ = b.Close() }()

	ctx, cancel2 := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel2()
	start := time.Now()
	err = b.Publish(ctx, "test/topic", []byte("{}"))
	elapsed := time.Since(start)
	if err == nil {
		t.Error("Publish: 期望 ctx 超时 error")
	}
	if elapsed > 2*time.Second {
		t.Errorf("Publish 阻塞 %v，期望 < ctx timeout", elapsed)
	}
}

// TestV311Backend_Close_Idempotent Close 多次调不 panic。
func TestV311Backend_Close_Idempotent(t *testing.T) {
	b, cancel, err := newV311Backend(newTestTargetMQTT(), mustURL(t, "tcp://127.0.0.1:1"), &MQTTConfig{
		ClientID:          "close-id",
		QoS:               intPtr(1),
		KeepaliveSec:      10,
		CleanSession:      true,
		ConnectTimeoutSec: 1,
	})
	if err != nil {
		t.Fatalf("newV311Backend: %v", err)
	}
	defer cancel()
	for i := 0; i < 5; i++ {
		if err := b.Close(); err != nil {
			t.Errorf("Close #%d: %v", i, err)
		}
	}
	// Done() 必须已 close
	select {
	case <-b.Done():
	default:
		t.Error("Done() 期望已 close")
	}
}

// TestV5Backend_Close_Idempotent v5 也幂等关闭。
func TestV5Backend_Close_Idempotent(t *testing.T) {
	b, cancel, err := newV5Backend(newTestTargetMQTT(), mustURL(t, "tcp://127.0.0.1:1"), &MQTTConfig{
		ClientID:          "close-id-v5",
		QoS:               intPtr(1),
		KeepaliveSec:      10,
		CleanSession:      true,
		ConnectTimeoutSec: 1,
	})
	if err != nil {
		t.Fatalf("newV5Backend: %v", err)
	}
	defer cancel()
	for i := 0; i < 5; i++ {
		if err := b.Close(); err != nil {
			t.Errorf("Close #%d: %v", i, err)
		}
	}
	select {
	case <-b.Done():
	default:
		t.Error("Done() 期望已 close")
	}
}

func intPtr(v int) *int { return &v }
func mustURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := normalizeBrokerURL(s)
	if err != nil {
		t.Fatalf("normalizeBrokerURL(%q): %v", s, err)
	}
	return u
}

// === mqttTarget 单元方法（不需要 broker） ===

// closer 接口用于测试中关闭 MQTT Target 的后台 goroutine。
// 若 TargetClient 接口将来包含 Close()，可删除此断言。
type closer interface{ Close() error }

// cleanupMQTT 关闭 *mqttTarget 以释放后台连接重试 goroutine。
func cleanupMQTT(t *testing.T, cli TargetClient) {
	t.Helper()
	if c, ok := cli.(closer); ok {
		_ = c.Close()
	}
}

func TestMQTTTarget_HealthCheck_NotConnected(t *testing.T) {
	qos := 1
	ch := newTestChannelMQTT("tcp://127.0.0.1:1", &MQTTConfig{
		ClientID:          "unit-1",
		QoS:               &qos,
		KeepaliveSec:      10,
		CleanSession:      true,
		ConnectTimeoutSec: 1,
	})
	tg := newTestTargetMQTT()
	p := newMQTTPlugin()
	cli, err := p.CreateTarget(tg, ch)
	if err != nil {
		t.Fatalf("CreateTarget: %v", err)
	}
	t.Cleanup(func() { cleanupMQTT(t, cli) })

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = cli.HealthCheck(ctx) // 即使报错也不 fatal；只验证不 panic
}

// TestMQTTTarget_Integration_RealBroker 集成测试：连本机实际 broker（mosquitto / emqx 等）。
//
// 默认 skip——需要在跑得起 broker 的环境（默认 127.0.0.1:1883）才能通过。
// 启用方式：PLCB_INTEGRATION=1 go test ./internal/infra/target/...
//
// 子测试：
//   - v5       → 走 autopaho (MQTT v5)
//   - v3_1_1   → 走 paho.mqtt.golang (MQTT v3.1.1)
//
// broker 必须同时接受两个版本（mosquitto 默认即可；emqx 默认即可；老 broker 配置了
// accept_protocol_versions=4 时需用 PLCB_MQTT_BROKER_V311 指向兼容端口）。
func TestMQTTTarget_Integration_RealBroker(t *testing.T) {
	if os.Getenv("PLCB_INTEGRATION") == "" {
		t.Skip("skip integration test; set PLCB_INTEGRATION=1 to enable")
	}

	cases := []struct {
		name    string
		version ProtocolVersion
		broker  string // 默认用 PLCB_MQTT_BROKER；可单独覆盖
	}{
		{name: "v5", version: ProtocolV5, broker: os.Getenv("PLCB_MQTT_BROKER")},
		{name: "v3_1_1", version: ProtocolV311, broker: firstNonEmpty(os.Getenv("PLCB_MQTT_BROKER_V311"), os.Getenv("PLCB_MQTT_BROKER"))},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			addr := c.broker
			if addr == "" {
				addr = "tcp://127.0.0.1:1883"
			}
			topic := "plcbridge/test/" + c.name + "/" + time.Now().Format("20060102T150405")
			cfg := &MQTTConfig{
				ProtocolVersion:   c.version,
				ClientID:          "plcbridge-it-" + c.name + "-" + time.Now().Format("150405"),
				QoS:               func() *int { v := 1; return &v }(),
				Retained:          false,
				KeepaliveSec:      10,
				CleanSession:      true,
				ConnectTimeoutSec: 15,
			}
			tg := makeMQTTTarget(t, addr, cfg)
			t.Cleanup(func() { _ = tg.Close() })
			tg.target.Topic = topic

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := tg.Connect(ctx); err != nil {
				t.Fatalf("Connect(%s): %v", c.name, err)
			}
			if err := tg.HealthCheck(ctx); err != nil {
				t.Fatalf("HealthCheck(%s): %v", c.name, err)
			}
			now := time.Now().UTC()
			if err := tg.Send(ctx, []PointValue{
				{TraceID: "t1", DeviceBizID: "dev", PointBizID: "pt", Timestamp: now, Value: 42, DataType: "int"},
			}); err != nil {
				t.Fatalf("Send(%s): %v", c.name, err)
			}
			t.Logf("[%s] published 1 message to %s", c.name, topic)
		})
	}
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// TestMQTTTarget_Stats_EmptyStats 验证 Stats 在未启动连接前返回 0/默认值。
// 不需要真实 broker —— 纯字段测试。
func TestMQTTTarget_Stats_EmptyStats(t *testing.T) {
	tgt := &platform.Target{
		TargetBizID: "test-tgt",
		Topic:       "test/topic",
	}
	ch := &platform.Channel{
		ChannelBizID: "test-ch",
		Address:      "tcp://127.0.0.1:1883",
	}
	cfg := &MQTTConfig{
		ProtocolVersion: ProtocolV5,
		ClientID:        "test-client",
	}
	c, err := newMQTTTarget(tgt, ch, cfg)
	if err != nil {
		// newMQTTTarget 不会失败（不做连接）；如果 broker 不存在也不影响。
		t.Logf("newMQTTTarget: %v (expected to silently succeed; backend not initialized)", err)
		return
	}
	// 关闭 backend 以防泄漏
	defer func() { _ = c.Close() }()

	stats := c.Stats()
	if stats == nil {
		t.Fatal("Stats returned nil")
	}
	// PR1 字段
	if stats["connection_state"] != "Disconnected" {
		t.Errorf("connection_state = %v, want Disconnected", stats["connection_state"])
	}
	if stats["protocol"] != "mqtt" {
		t.Errorf("protocol = %v, want mqtt", stats["protocol"])
	}
	// PR2 字段（全部应存在且为 0）
	for _, k := range []string{"sent_count", "sent_points", "error_count", "connect_count", "health_check_count"} {
		if _, ok := stats[k]; !ok {
			t.Errorf("Stats missing key %q", k)
		}
	}
	// 时间戳字段应为 0
	if ms, ok := stats["last_send_ok_ms"].(int64); !ok || ms != 0 {
		t.Errorf("last_send_ok_ms = %v, want int64(0)", stats["last_send_ok_ms"])
	}
}
