package s7_test

import (
	"context"
	"testing"
	"time"

	"github.com/plcbridge/plcbridge/internal/infra/protocol"
	"github.com/plcbridge/plcbridge/internal/infra/protocol/s7/s7mock"
	"github.com/plcbridge/plcbridge/internal/platform"
)

// TestS7Write_NonDBAreas 验证 M / Q / I(E) 区也能回写。
//
// 历史行为：writeBytes 直接 `if area != areaDB { return "S7 Write 暂仅支持 DB 区" }`，
// 于是 M100 / Q10 / IB0 这类回写点全部写不进去 ——
// 现场表现为"回写接口能通，但 M 位就是写不进去"。
//
// 现在这些区都走各自的底层调用（AGWriteMB / AGWriteAB / AGWriteEB），
// 位写仍是 Read-Modify-Write。
func TestS7Write_NonDBAreas(t *testing.T) {
	srv, err := s7mock.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatalf("启动 mock S7 服务端: %v", err)
	}
	defer func() { _ = srv.Close() }()

	plugin, err := protocol.Get("s7")
	if err != nil {
		t.Skip("s7 plugin not registered:", err)
	}
	cli, err := plugin.CreateChannel(&platform.Channel{
		ChannelBizID: "S7",
		Type:         "s7",
		Address:      srv.Addr(),
	})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	defer func() { _ = cli.Disconnect() }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := cli.Connect(ctx); err != nil {
		t.Fatalf("Connect 失败: %v", err)
	}

	cases := []struct {
		name     string
		address  string
		value    any
		dataType string
	}{
		{"M区字节", "MB10", uint8(0x5A), "uint8"},
		{"M区字", "MW10", uint16(1234), "uint16"},
		{"M区双字", "MD10", uint32(70000), "uint32"},
		{"M区位", "M100", true, "bool"},
		{"Q区字节", "QB10", uint8(0x11), "uint8"},
		{"Q区位", "Q0.1", false, "bool"},
		{"I(E)区字节", "IB10", uint8(0x22), "uint8"},
		{"DB区字节", "DB1.DBB10", uint8(0x33), "uint8"},
		{"DB区位", "DB1.DBX0.3", true, "bool"},
		{"DB区字符串", "DB1.DBB100", "HELLO", "string"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wctx, wcancel := context.WithTimeout(ctx, 5*time.Second)
			defer wcancel()
			if err := cli.Write(wctx, tc.address, tc.value, tc.dataType); err != nil {
				t.Fatalf("Write(%s) 失败: %v", tc.address, err)
			}
		})
	}
}

// TestS7Write_MAreaRejectsUnsupportedTypes 验证非 DB 区对不支持的数据类型
// 仍然返回明确错误（而不是静默成功或 panic）。
func TestS7Write_MAreaRejectsUnsupportedTypes(t *testing.T) {
	srv, err := s7mock.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatalf("启动 mock S7 服务端: %v", err)
	}
	defer func() { _ = srv.Close() }()

	plugin, err := protocol.Get("s7")
	if err != nil {
		t.Skip("s7 plugin not registered:", err)
	}
	cli, err := plugin.CreateChannel(&platform.Channel{
		ChannelBizID: "S7",
		Type:         "s7",
		Address:      srv.Addr(),
	})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	defer func() { _ = cli.Disconnect() }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := cli.Connect(ctx); err != nil {
		t.Fatalf("Connect 失败: %v", err)
	}

	// float64 在 M 区是合法的（8 字节）。
	if err := cli.Write(ctx, "MD0", 1.5, "float64"); err != nil {
		t.Fatalf("Write(MD0, float64) 失败: %v", err)
	}

	// 未支持的数据类型必须报错。
	if err := cli.Write(ctx, "MD0", "x", "blob"); err == nil {
		t.Fatal("Write(blob) 本应失败，却返回 nil")
	}
}
