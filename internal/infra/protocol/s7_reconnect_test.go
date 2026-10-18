package protocol

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/plcbridge/plcbridge/internal/platform"
)

// TestS7Channel_ReconnectFailureDoesNotLeaveDeadHandler 验证：
//   - 初始状态 c.client=nil, c.handler=nil
//   - Reconnect 失败后这两个字段应仍为 nil（没有悬空）
//   - 多次 Reconnect 失败状态保持稳定（不死锁、不 panic）
//   - lastErr 是真实的 dial/connect 错误，不是误导性的 "S7 未连接"
//
// 这覆盖了"OFF→ON"路径上的关键回归：之前的版本 Reconnect 失败后会
// 把 c.client 置 nil + 旧 handler 已 close → 后续 ProbeActive 只能
// 返回 "S7 未连接"，与实际网络错误信息不一致，且看似已死锁。
func TestS7Channel_ReconnectFailureDoesNotLeaveDeadHandler(t *testing.T) {
	MustRegisterDefaults() // 测试自身注册 plugin
	plugin, err := Get("s7")
	if err != nil {
		t.Skip("s7 plugin not registered:", err)
	}
	cfg := &platform.Channel{
		ChannelBizID: "S7",
		Type:         "s7",
		Address:      "127.0.0.1:1", // 故意指向不可用端口
	}
	cli, err := plugin.CreateChannel(cfg)
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	c, ok := cli.(*s7Channel)
	if !ok {
		t.Fatalf("expected *s7Channel, got %T", cli)
	}
	defer func() { _ = c.Disconnect() }()

	// 第一次 Reconnect：必失败
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := c.Reconnect(ctx); err == nil {
		t.Fatal("expected first Reconnect to fail (127.0.0.1:1 unreachable)")
	}

	// 失败后 c.client/c.handler 应为 nil（初始本来就是 nil）
	if c.client != nil {
		t.Fatal("c.client should be nil after first failed Reconnect (never connected)")
	}
	if c.handler != nil {
		t.Fatal("c.handler should be nil after first failed Reconnect (never connected)")
	}

	// 第二次 Reconnect：再失败一次（不能 hang / panic）
	ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()
	if err := c.Reconnect(ctx2); err == nil {
		t.Fatal("second Reconnect should also fail")
	}
	if c.client != nil {
		t.Fatal("c.client should still be nil after second failed Reconnect")
	}
	if c.handler != nil {
		t.Fatal("c.handler should still be nil after second failed Reconnect")
	}

	// lastErr 应是真实的 dial 错误（不要是误导性的 "S7 未连接"）
	lastErr := c.LastError()
	if lastErr == "" {
		t.Fatal("lastErr should be set after failed Reconnect")
	}
	// 不能是 "S7 channel X 未连接" 错误（这是 client==nil 的误导）
	if strings.Contains(lastErr, "未连接") {
		t.Fatalf("lastErr is misleading '未连接' (should be actual dial error): %q", lastErr)
	}
	// 应包含 dial/connect 相关关键字
	if !strings.Contains(lastErr, "dial") && !strings.Contains(lastErr, "connect") && !strings.Contains(lastErr, "refused") {
		t.Logf("warning: lastErr = %q, expected dial/connect/refused in error", lastErr)
	}
}

// TestS7Channel_Stats_PR2Fields 验证 PR2 详细统计字段都存在。
func TestS7Channel_Stats_PR2Fields(t *testing.T) {
	ch := &s7Channel{
		addr:      "127.0.0.1:102",
		channelID: "test-ch",
		cfg:       &S7Config{Rack: 0, Slot: 1, ConnectionTimeoutMs: 2000, ConnectType: 1, StringLayoutPtr: nil},
	}
	stats := ch.Stats()
	if stats["connection_state"] != "Disconnected" {
		t.Errorf("connection_state = %v, want Disconnected", stats["connection_state"])
	}
	if stats["rack"] != 0 {
		t.Errorf("rack = %v, want 0", stats["rack"])
	}
	if stats["slot"] != 1 {
		t.Errorf("slot = %v, want 1", stats["slot"])
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
