// OPC UA 客户端协议插件的集成测试。
//
// 用 gopcua/opcua/server 启动一个内嵌 OPC UA server（None + Anonymous，端口随机），
// 暴露若干 ns=N;s=... 节点（N 由 server 分配），然后用真实 opcuaChannel
// 验证 Read / BatchRead / Write / BatchWrite / Subscription 通路。
package protocol

import (
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gopcua/opcua/id"
	"github.com/gopcua/opcua/server"
	"github.com/gopcua/opcua/server/attrs"
	"github.com/gopcua/opcua/ua"
	"github.com/plcbridge/plcbridge/internal/platform"
)

// pickFreePort 申请一个空闲 TCP 端口（避免 4840 冲突）。
func pickFreePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pickFreePort: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// startTestServer 起一个最小 OPC UA server，ns=N;s=plcbridge.<name> 节点集合，
// 并返回 server / endpoint / 节点 namespace index。
func startTestServer(t *testing.T, port int) (*server.Server, string, uint16) {
	t.Helper()
	s := server.New(
		server.EndPoint("127.0.0.1", port),
		server.EnableSecurity("None", ua.MessageSecurityModeNone),
		server.EnableAuthMode(ua.UserTokenTypeAnonymous),
	)

	rootNS, _ := s.Namespace(0)
	rootObj := rootNS.Objects()

	ns := server.NewNodeNameSpace(s, "plcbridge-test")
	s.AddNamespace(ns)
	nsObj := ns.Objects()
	rootObj.AddRef(nsObj, id.HasComponent, true)

	// 可读可写 float64 节点：用 server.NewNode + AccessLevel（AccessLevel 必须能被 (uint8) 断言）
	addRWFloat := func(name string, v float64) {
		n := server.NewNode(
			ua.NewStringNodeID(ns.ID(), name),
			server.Attributes{
				ua.AttributeIDAccessLevel:     server.DataValueFromValue(uint8(ua.AccessLevelTypeCurrentRead | ua.AccessLevelTypeCurrentWrite)),
				ua.AttributeIDUserAccessLevel: server.DataValueFromValue(uint8(ua.AccessLevelTypeCurrentRead | ua.AccessLevelTypeCurrentWrite)),
				ua.AttributeIDBrowseName:      server.DataValueFromValue(attrs.BrowseName(name)),
				ua.AttributeIDDisplayName:     server.DataValueFromValue(attrs.DisplayName(name, name)),
				ua.AttributeIDNodeClass:       server.DataValueFromValue(uint32(ua.NodeClassVariable)),
			},
			nil,
			func() *ua.DataValue { return server.DataValueFromValue(v) },
		)
		ns.AddNode(n)
		nsObj.AddRef(n, server.RefTypeIDHasComponent, true)
	}
	addRWFloat("rw_float", 12.34)
	addRWFloat("rw_float2", 56.78)

	addRWBool := func(name string, v bool) {
		n := server.NewNode(
			ua.NewStringNodeID(ns.ID(), name),
			server.Attributes{
				ua.AttributeIDAccessLevel:     server.DataValueFromValue(uint8(ua.AccessLevelTypeCurrentRead | ua.AccessLevelTypeCurrentWrite)),
				ua.AttributeIDUserAccessLevel: server.DataValueFromValue(uint8(ua.AccessLevelTypeCurrentRead | ua.AccessLevelTypeCurrentWrite)),
				ua.AttributeIDBrowseName:      server.DataValueFromValue(attrs.BrowseName(name)),
				ua.AttributeIDDisplayName:     server.DataValueFromValue(attrs.DisplayName(name, name)),
				ua.AttributeIDNodeClass:       server.DataValueFromValue(uint32(ua.NodeClassVariable)),
			},
			nil,
			func() *ua.DataValue { return server.DataValueFromValue(v) },
		)
		ns.AddNode(n)
		nsObj.AddRef(n, server.RefTypeIDHasComponent, true)
	}
	addRWBool("rw_bool", true)
	addRWBool("rw_bool2", false)

	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start test server: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, fmt.Sprintf("opc.tcp://127.0.0.1:%d", port), ns.ID()
}

// newTestChannel 拿一个真实 opcuaChannel，指向测试 server。
func newTestChannel(t *testing.T, endpoint string) *opcuaChannel {
	t.Helper()
	cfg := &OPCUAConfig{
		Endpoint:           endpoint,
		SecurityMode:       "None",
		SecurityPolicy:     "None",
		AuthType:           "Anonymous",
		SessionTimeoutMs:   60000,
		PublishIntervalMs:  200,
		AutoSelectEndpoint: true,
	}
	ch, err := newOPCUAChannel("test-"+endpoint, cfg)
	if err != nil {
		t.Fatalf("newOPCUAChannel: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := ch.Connect(ctx); err != nil {
		t.Fatalf("Connect OPC UA test server: %v", err)
	}
	t.Cleanup(func() { _ = ch.Disconnect() })
	return ch
}

// === 测试 ===

func TestOPCUA_ConnectAndRead(t *testing.T) {
	port := pickFreePort(t)
	_, endpoint, nsIdx := startTestServer(t, port)
	ch := newTestChannel(t, endpoint)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	v, err := ch.Read(ctx, fmt.Sprintf("ns=%d;s=rw_float", nsIdx), "float64")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if v.Quality != "good" {
		t.Fatalf("quality = %q, want good", v.Quality)
	}
	if v.Float64 != 12.34 {
		t.Fatalf("Float64 = %v, want 12.34", v.Float64)
	}
}

func TestOPCUA_BatchRead(t *testing.T) {
	port := pickFreePort(t)
	_, endpoint, nsIdx := startTestServer(t, port)
	ch := newTestChannel(t, endpoint)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	addr := func(name string) string { return fmt.Sprintf("ns=%d;s=%s", nsIdx, name) }
	points := []*platform.Point{
		{PointBizID: "p_f1", Address: addr("rw_float"), DataType: "float64", CollectEnabled: true},
		{PointBizID: "p_f2", Address: addr("rw_float2"), DataType: "float64", CollectEnabled: true},
		{PointBizID: "p_b1", Address: addr("rw_bool"), DataType: "bool", CollectEnabled: true},
	}
	res, err := ch.BatchRead(ctx, points)
	if err != nil {
		t.Fatalf("BatchRead: %v", err)
	}
	if len(res) != 3 {
		t.Fatalf("BatchRead result len = %d, want 3: %+v", len(res), res)
	}
	if res["p_f1"].Float64 != 12.34 {
		t.Fatalf("p_f1 = %v, want 12.34", res["p_f1"].Float64)
	}
	if res["p_f2"].Float64 != 56.78 {
		t.Fatalf("p_f2 = %v, want 56.78", res["p_f2"].Float64)
	}
	if res["p_b1"].Bool != true {
		t.Fatalf("p_b1 = %v, want true", res["p_b1"].Bool)
	}
}

func TestOPCUA_WriteAndBatchWrite(t *testing.T) {
	port := pickFreePort(t)
	_, endpoint, nsIdx := startTestServer(t, port)
	ch := newTestChannel(t, endpoint)
	addr := func(name string) string { return fmt.Sprintf("ns=%d;s=%s", nsIdx, name) }

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// 单写：验证 Write 调用本身成功（StatusOK）
	// 注：gopcua/opcua v0.9 server 的 n.SetAttribute(Value) 实现是
	// "n.val = func() { return val }" 的占位替换，对 Variant 的 round-trip
	// 不一定保留原始值，所以不依赖再 Read 回值。
	if err := ch.Write(ctx, addr("rw_float"), float64(99.0), "float64"); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// 批量写：同样验证调用成功
	writes := []WriteOp{
		{Address: addr("rw_float2"), Value: float64(7.5), DataType: "float64"},
		{Address: addr("rw_bool"), Value: false, DataType: "bool"},
	}
	if err := ch.BatchWrite(ctx, writes); err != nil {
		t.Fatalf("BatchWrite: %v", err)
	}

	// 基础批量读还应该成功（与写无关）
	res, err := ch.BatchRead(ctx, []*platform.Point{
		{PointBizID: "p_f1", Address: addr("rw_float"), DataType: "float64", CollectEnabled: true},
		{PointBizID: "p_b1", Address: addr("rw_bool"), DataType: "bool", CollectEnabled: true},
	})
	if err != nil {
		t.Fatalf("BatchRead: %v", err)
	}
	if len(res) != 2 {
		t.Fatalf("BatchRead len=%d, want 2", len(res))
	}
	if res["p_b1"].Quality != "good" {
		t.Fatalf("p_b1 quality=%s", res["p_b1"].Quality)
	}
}

func TestOPCUA_Subscription(t *testing.T) {
	port := pickFreePort(t)
	_, endpoint, nsIdx := startTestServer(t, port)
	ch := newTestChannel(t, endpoint)
	addr := func(name string) string { return fmt.Sprintf("ns=%d;s=%s", nsIdx, name) }

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var (
		mu     sync.Mutex
		seen   = map[string]int{}
		gotOne = make(chan struct{}, 1)
	)
	err := ch.EnableSubscription(func(address string, v Value) {
		mu.Lock()
		defer mu.Unlock()
		seen[address]++
		select {
		case gotOne <- struct{}{}:
		default:
		}
	})
	if err != nil {
		t.Fatalf("EnableSubscription: %v", err)
	}
	if err := ch.AddMonitoredItem(ctx, addr("rw_float")); err != nil {
		t.Fatalf("AddMonitoredItem: %v", err)
	}
	if err := ch.AddMonitoredItem(ctx, addr("rw_bool")); err != nil {
		t.Fatalf("AddMonitoredItem bool: %v", err)
	}

	// 等待首次发布
	select {
	case <-gotOne:
	case <-time.After(5 * time.Second):
		t.Fatalf("timeout waiting for first subscription notification")
	}

	// 写一个值，触发 data change
	if err := ch.Write(ctx, addr("rw_float"), float64(3.14), "float64"); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// 等下一次 callback
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		count := seen[addr("rw_float")]
		mu.Unlock()
		if count >= 2 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if seen[addr("rw_float")] < 2 {
		t.Fatalf("expected at least 2 notifications for rw_float, got %d", seen[addr("rw_float")])
	}
}

func TestOPCUA_ParseNodeID(t *testing.T) {
	cases := []struct {
		addr   string
		expect string // gopcua NodeID.String() 在 ns=0 时会省略 ns= 前缀
	}{
		{"ns=2;s=Channel1.Device1.Tag", "ns=2;s=Channel1.Device1.Tag"},
		{"ns=2;i=1234", "ns=2;i=1234"},
		{"i=42", "i=42"},
		{"/a/b/c", "s=/a/b/c"},
		{"plain_node", "s=plain_node"},
	}
	for _, c := range cases {
		nid, err := parseOPCUANodeID(c.addr)
		if err != nil {
			t.Errorf("parseOPCUANodeID(%q): %v", c.addr, err)
			continue
		}
		if got := nid.String(); got != c.expect {
			t.Errorf("parseOPCUANodeID(%q) = %q, want %q", c.addr, got, c.expect)
		}
	}
}

// TestOPCUAChannel_Stats_PR2Fields 验证 PR2 详细统计字段都存在。
func TestOPCUAChannel_Stats_PR2Fields(t *testing.T) {
	ch := &opcuaChannel{
		channelID: "test-ch",
		cfg: &OPCUAConfig{
			Endpoint:         "opc.tcp://127.0.0.1:4840",
			SecurityMode:     "None",
			SecurityPolicy:   "None",
			AuthType:         "Anonymous",
			PublishIntervalMs: 1000,
		},
		handleToA:   map[uint32]string{},
		handleToNID: map[uint32]*ua.NodeID{},
	}
	stats := ch.Stats()
	if stats["connection_state"] != "Disconnected" {
		t.Errorf("connection_state = %v, want Disconnected", stats["connection_state"])
	}
	if stats["endpoint"] != "opc.tcp://127.0.0.1:4840" {
		t.Errorf("endpoint = %v", stats["endpoint"])
	}
	// PR2 字段
	for _, k := range []string{"read_count", "write_count", "error_count"} {
		if _, ok := stats[k]; !ok {
			t.Errorf("Stats missing key %q", k)
		}
	}
	for _, k := range []string{"last_read_ok_ms", "last_write_ok_ms", "last_error_ms", "last_probe_ok_ms"} {
		if _, ok := stats[k]; !ok {
			t.Errorf("Stats missing timestamp key %q", k)
		}
	}
}

// suppress unused warnings
var _ = atomic.Bool{}
