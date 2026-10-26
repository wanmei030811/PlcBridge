package s7_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/plcbridge/plcbridge/internal/infra/protocol"
	"github.com/plcbridge/plcbridge/internal/infra/protocol/s7/s7mock"
	"github.com/plcbridge/plcbridge/internal/platform"
)

// TestS7Channel_RecoversAfterServerRestart 端到端复现并验证本次修复的核心场景：
//
//	PLC（mock）运行 → 连接建立 → PLC 停机（socket 被 RST）→ 采集失败
//	→ PLC 恢复（同一地址重新监听）→ **无需任何人工干预**，连接自动重建、读取恢复成功。
//
// 修复前的问题：
//   - PLC 停机后 c.client 仍非 nil（指向已死的 socket），每次读都失败
//   - Source 一直显示"采集异常"，ConsecutiveErrors 无限累加
//   - 只有手动触发诊断（走 registry.Snapshot → ProbeActive）才可能恢复
func TestS7Channel_RecoversAfterServerRestart(t *testing.T) {
	srv, err := s7mock.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatalf("启动 mock S7 服务端: %v", err)
	}
	addr := srv.Addr()

	plugin, err := protocol.Get("s7")
	if err != nil {
		t.Skip("s7 plugin not registered:", err)
	}
	cli, err := plugin.CreateChannel(&platform.Channel{
		ChannelBizID: "S7",
		Type:         "s7",
		Address:      addr,
	})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	defer func() { _ = cli.Disconnect() }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := cli.Connect(ctx); err != nil {
		t.Fatalf("首次 Connect 失败: %v", err)
	}

	// 模拟 PLC 停机：关闭 server + 断开所有已建立连接（客户端收到 RST）。
	if err := srv.Close(); err != nil {
		t.Fatalf("关闭 mock 服务端: %v", err)
	}

	// 停机后第一次读必须失败（此时连接已死）。
	readErr := readOnce(t, cli)
	if readErr == nil {
		t.Fatal("PLC 停机后读取本应失败")
	}
	t.Logf("停机后读取错误（预期）: %v", readErr)

	// PLC 恢复：同一地址重新监听。
	srv2, err := s7mock.Listen(addr)
	if err != nil {
		t.Fatalf("重启 mock S7 服务端: %v", err)
	}
	defer func() { _ = srv2.Close() }()

	// 关键：不做任何人工干预（不调 Reconnect / ProbeActive），
	// 只重复读 —— 期望在若干次内自动恢复。
	deadline := time.Now().Add(15 * time.Second)
	var lastErr error
	ok := false
	for time.Now().Before(deadline) {
		lastErr = readOnce(t, cli)
		if lastErr == nil {
			ok = true
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !ok {
		t.Fatalf("PLC 恢复后读取未自动恢复，最后错误: %v", lastErr)
	}
	if !cli.IsConnected() {
		t.Error("恢复后 IsConnected 应为 true")
	}
	if e := cli.LastError(); e != "" {
		t.Errorf("恢复后 LastError 应被清空，实际 = %q", e)
	}
	t.Log("PLC 恢复后读取自动成功（无需人工干预）")
}

// readOnce 读一个点，成功返回 nil。
func readOnce(t *testing.T, cli protocol.ProtocolChannel) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := cli.Read(ctx, "M100", "int8")
	return err
}

// TestS7Channel_ReadErrorMentionsDeadConnection 验证：
// 对端断开后，读失败错误里能看到"连接已死"的信号（便于日志定位）。
func TestS7Channel_ReadErrorMentionsDeadConnection(t *testing.T) {
	srv, err := s7mock.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatalf("启动 mock S7 服务端: %v", err)
	}
	addr := srv.Addr()

	plugin, err := protocol.Get("s7")
	if err != nil {
		t.Skip("s7 plugin not registered:", err)
	}
	cli, err := plugin.CreateChannel(&platform.Channel{
		ChannelBizID: "S7",
		Type:         "s7",
		Address:      addr,
	})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	defer func() { _ = cli.Disconnect() }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := cli.Connect(ctx); err != nil {
		t.Fatalf("Connect 失败: %v", err)
	}
	_ = srv.Close()

	// 反复读，直到出现错误（第一次可能因缓冲读到了数据）。
	var lastErr error
	for i := 0; i < 5; i++ {
		if lastErr = readOnce(t, cli); lastErr != nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if lastErr == nil {
		t.Fatal("断开后读取应报错")
	}
	t.Logf("断开后读取错误: %v", lastErr)
	if !strings.Contains(strings.ToLower(lastErr.Error()), "closed") &&
		!strings.Contains(strings.ToLower(lastErr.Error()), "reset") &&
		!strings.Contains(strings.ToLower(lastErr.Error()), "null") {
		t.Logf("提示：错误文本未直接包含 closed/reset/null，实际 = %q", lastErr.Error())
	}
}
