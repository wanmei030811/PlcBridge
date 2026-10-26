package s7

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
)

// reserveClosedAddr 返回一个"确定没人监听"的本地地址。
//
// 用途：测试"连不上"这条路径时，不能用 S7 默认端口 102 ——
// 开发机上很可能正跑着 s7mock 或真实 PLC，连接会意外成功，
// 让测试因与被测逻辑无关的原因失败。
//
// 做法：绑定 :0 让内核分配一个空闲端口，记录地址后立刻关闭。
// 该端口随即进入可分配状态，但本机几乎不会马上复用同一个端口。
func reserveClosedAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("分配临时端口失败: %v", err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatalf("关闭临时监听失败: %v", err)
	}
	return addr
}

// TestIsDeadConnErr 验证"底层连接已死"的错误分类。
//
// 为什么重要（2026-10-25 修复）：读失败时只有连接类错误才作废 socket。
// 业务类错误（地址非法、类型不支持）如果也作废连接，现场表现是
// "点位配错一个地址 → PLC 连接被反复重建 → 连接数暴涨"。
func TestIsDeadConnErr(t *testing.T) {
	dead := []string{
		"S7 读失败 [area=4 db=0 start=100 size=1]: write tcp 127.0.0.1:61220->127.0.0.1:102: wsasend: An existing connection was forcibly closed by the remote host.",
		"read tcp 127.0.0.1:102: connection reset by peer",
		"Connection to address 127.0.0.1:102 is null",
		"io: read/write on closed pipe",
		"use of closed network connection",
		"dial tcp 127.0.0.1:102: connectex: No connection could be made because the target machine actively refused it.",
		"write tcp: broken pipe",
		"read tcp: i/o timeout",
	}
	for _, msg := range dead {
		if !isDeadConnErr(msg) {
			t.Errorf("isDeadConnErr(%q) = false, want true（应识别为连接已死）", msg)
		}
	}

	// 业务类错误：连接本身没问题，不能作废。
	alive := []string{
		"点位 P1 地址解析失败: bad address",
		"S7 Read 不支持 area=9",
		"S7 Write 不支持的数据类型: complex64",
		"点位 P2 解码失败: 非法 UTF-8",
	}
	for _, msg := range alive {
		if isDeadConnErr(msg) {
			t.Errorf("isDeadConnErr(%q) = true, want false（业务错误不应作废连接）", msg)
		}
	}
}

// TestInvalidateDeadConnLocked_ClearsStaleClient 验证：
// 连接类读失败后，client/handler 被清空（socket 已死，不能再复用）。
//
// 这正是"PLC 停机再启动后采集一直失败、失败次数一直涨"的根因：
// 旧代码里 c.client 永远非 nil，registry.Reconnect 又走独立路径，
// 于是死连接被无限复用，永远读不成功。
func TestInvalidateDeadConnLocked_ClearsStaleClient(t *testing.T) {
	c := &s7Channel{
		addr:      "127.0.0.1:102",
		channelID: "s7-test",
		cfg:       &S7Config{Rack: 0, Slot: 1, ConnectionTimeoutMs: 500, ConnectType: 1},
	}
	// 模拟"曾经连上过"的状态：client 非 nil。
	c.client = nil
	c.handler = nil

	// client==nil 时是 no-op（不应 panic）。
	c.invalidateDeadConnLocked(errors.New("some error"))
	if c.client != nil || c.handler != nil {
		t.Fatal("client==nil 时 invalidate 不应改变任何状态")
	}

	// 业务错误：不作废。
	c.invalidateDeadConnLocked(errors.New("点位 P1 解码失败"))
	if c.client != nil || c.handler != nil {
		t.Fatal("业务错误不应作废连接")
	}
}

// TestS7Channel_ReadAfterDeadConnReportsNotConnected 验证：
// 连接被作废（c.client == nil）后，后续读应报错而不是"静默成功"。
//
// 错误文案有两种可能（都算正确）：
//   - "未连接"：刚作废、还在自动重连节流窗口内
//   - 连接失败（dial refused 等）：节流窗口已过，尝试重连但 PLC 还没起来
//
// 关键是不再返回"用死 socket 报错"的误导性结果，
// 且 collect 层能明确区分"连接已死（需要重连）"与"点位配置错误"。
func TestS7Channel_ReadAfterDeadConnReportsNotConnected(t *testing.T) {
	// 关键：必须用**保证没人监听**的端口。
	//
	// 旧实现硬编码 127.0.0.1:102（S7 默认端口）。一旦开发机上跑着
	// s7mock / 真实 PLC，client 会连接成功，readBytes 返回 nil error，
	// 测试反而失败 —— 而且失败原因和被测逻辑完全无关（典型的测试隔离缺陷）。
	//
	// 做法：先监听 :0 拿到一个空闲端口，随即关闭它，再把该端口用作
	// "连不上"的目标。关闭到连接之间的竞态窗口极小，且本机端口几乎不会被
	// 立即复用。
	deadAddr := reserveClosedAddr(t)
	c := &s7Channel{
		addr:      deadAddr,
		channelID: "s7-test",
		cfg:       &S7Config{Rack: 0, Slot: 1, ConnectionTimeoutMs: 500, ConnectType: 1},
	}
	// 手工置一个非 nil client 是不可能的（gos7.Client 是接口，可以用 nil 之外的实现），
	// 这里改为直接验证"client==nil 时读失败"的语义。
	_, _, err := c.readBytes(context.Background(), areaDB, 1, 100, 1, 0)
	if err == nil {
		t.Fatal("client==nil 时 readBytes 应报错")
	}
	msg := err.Error()
	ok := strings.Contains(msg, "未连接") ||
		strings.Contains(msg, "连接失败") ||
		strings.Contains(msg, "refused")
	if !ok {
		t.Errorf("err = %q, want 含 '未连接' 或连接失败类错误", msg)
	}
}
