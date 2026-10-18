// 单元测试：用真实 modbus 客户端调 mock server，验证 FC03/FC06/FC16。
package bench

import (
	"context"
	"testing"
	"time"

	"github.com/grid-x/modbus"
)

func TestMockPLC_ReadWrite(t *testing.T) {
	plc, err := NewMockPLC("127.0.0.1:0", 1024)
	if err != nil {
		t.Fatal(err)
	}
	defer plc.Close()

	h := modbus.NewTCPClientHandler(plc.addr)
	h.SetSlave(1)
	h.Timeout = 2 * time.Second
	if err := h.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	c := modbus.NewClient(h)

	// FC06 写寄存器 100 = 0x1234
	if _, err := c.WriteSingleRegister(context.Background(), 100, 0x1234); err != nil {
		t.Fatal(err)
	}
	// FC03 读回
	got, err := c.ReadHoldingRegisters(context.Background(), 100, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) < 2 {
		t.Fatalf("got short response: %v", got)
	}
	if val := uint16(got[0])<<8 | uint16(got[1]); val != 0x1234 {
		t.Errorf("got %04x, want 0x1234", val)
	}

	// FC16 写 4 个寄存器 200..203
	if _, err := c.WriteMultipleRegisters(context.Background(), 200, 4,
		[]byte{0x00, 0x10, 0x00, 0x20, 0x00, 0x30, 0x00, 0x40}); err != nil {
		t.Fatal(err)
	}
	got2, err := c.ReadHoldingRegisters(context.Background(), 200, 4)
	if err != nil {
		t.Fatal(err)
	}
	want := []uint16{0x0010, 0x0020, 0x0030, 0x0040}
	for i, w := range want {
		got := uint16(got2[2*i])<<8 | uint16(got2[2*i+1])
		if got != w {
			t.Errorf("reg[%d] got %04x, want %04x", 200+i, got, w)
		}
	}

	r, w, q := plc.Stats()
	t.Logf("stats: reads=%d writes=%d requests=%d", r, w, q)
	if q < 4 {
		t.Errorf("expected >=4 requests, got %d", q)
	}
}

func TestStartMockPool(t *testing.T) {
	plcs, _, err := StartMockPool(55000, 5, 1024)
	if err != nil {
		// 端口冲突时跳过（CI 上可能有遗留）
		t.Skip(err.Error())
	}
	defer func() {
		for _, p := range plcs {
			p.Close()
		}
	}()
	if len(plcs) != 5 {
		t.Errorf("got %d plcs, want 5", len(plcs))
	}
}
