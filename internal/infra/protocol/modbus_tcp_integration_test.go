// ModbusTCP 协议插件集成测试（需真实 broker 或 mock）。
//
// 用 mock 实现 grid-x/modbus.Client，模拟 FC03/FC06/FC16 等应答。
// 验证：
//   1. Read + BatchRead 数据解码正确
//   2. Write 数据编码正确
//   3. 错误传递
package protocol

import (
	"context"
	"encoding/binary"
	"errors"
	"math"
	"testing"

	"github.com/grid-x/modbus"

	"github.com/plcbridge/plcbridge/internal/platform"
)

// fakeModbusClient 实现 modbus.Client，按预设寄存器数据应答。
type fakeModbusClient struct {
	holding []uint16 // 4x 寄存器
	input   []uint16 // 3x 输入寄存器
	coils   []byte   // 0x 线圈
	discr   []byte   // 1x 离散输入

	// 注入错误（任意方法）
	err error
}

func (f *fakeModbusClient) ReadCoils(_ context.Context, addr, quantity uint16) ([]byte, error) {
	if f.err != nil {
		return nil, f.err
	}
	if int(addr)+int(quantity) > len(f.coils) {
		return nil, errors.New("addr out of range")
	}
	out := make([]byte, quantity)
	for i := uint16(0); i < quantity; i++ {
		// coil state 是单字节 0/1（不是位打包）
		if f.coils[addr+i] != 0 {
			out[i] = 1
		}
	}
	return out, nil
}
func (f *fakeModbusClient) ReadDiscreteInputs(_ context.Context, addr, quantity uint16) ([]byte, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := make([]byte, quantity)
	for i := uint16(0); i < quantity; i++ {
		if f.discr[addr+i] != 0 {
			out[i] = 1
		}
	}
	return out, nil
}
func (f *fakeModbusClient) ReadHoldingRegisters(_ context.Context, addr, quantity uint16) ([]byte, error) {
	if f.err != nil {
		return nil, f.err
	}
	if int(addr)+int(quantity) > len(f.holding) {
		return nil, errors.New("addr out of range")
	}
	regs := f.holding[addr : addr+quantity]
	return regsToBytes(regs), nil
}
func (f *fakeModbusClient) ReadInputRegisters(_ context.Context, addr, quantity uint16) ([]byte, error) {
	if f.err != nil {
		return nil, f.err
	}
	if int(addr)+int(quantity) > len(f.input) {
		return nil, errors.New("addr out of range")
	}
	regs := f.input[addr : addr+quantity]
	return regsToBytes(regs), nil
}
func (f *fakeModbusClient) WriteSingleCoil(_ context.Context, addr, value uint16) ([]byte, error) {
	if f.err != nil {
		return nil, f.err
	}
	if int(addr) >= len(f.coils) {
		return nil, errors.New("addr out of range")
	}
	if value == 0 {
		f.coils[addr] = 0
	} else {
		f.coils[addr] = 1
	}
	return []byte{byte(addr >> 8), byte(addr), 0xFF, 0x00}, nil
}
func (f *fakeModbusClient) WriteMultipleCoils(_ context.Context, addr, quantity uint16, value []byte) ([]byte, error) {
	if f.err != nil {
		return nil, f.err
	}
	copy(f.coils[addr:addr+quantity], value)
	return nil, nil
}
func (f *fakeModbusClient) WriteSingleRegister(_ context.Context, addr, value uint16) ([]byte, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.holding[addr] = value
	return regsToBytes([]uint16{value}), nil
}
func (f *fakeModbusClient) WriteMultipleRegisters(_ context.Context, addr, quantity uint16, value []byte) ([]byte, error) {
	if f.err != nil {
		return nil, f.err
	}
	regs := bytesToRegs(value)
	for i := uint16(0); i < quantity; i++ {
		f.holding[addr+i] = regs[i]
	}
	return nil, nil
}
func (f *fakeModbusClient) ReadWriteMultipleRegisters(_ context.Context, ra, rq, _, _ uint16, _ []byte) ([]byte, error) {
	return f.ReadHoldingRegisters(context.Background(), ra, rq)
}
func (f *fakeModbusClient) MaskWriteRegister(_ context.Context, _ uint16, andMask, orMask uint16) ([]byte, error) {
	return regsToBytes([]uint16{andMask, orMask}), nil
}
func (f *fakeModbusClient) ReadFIFOQueue(_ context.Context, _ uint16) ([]byte, error) { return nil, nil }
func (f *fakeModbusClient) ReadDeviceIdentification(_ context.Context, _ modbus.ReadDeviceIDCode) (map[byte][]byte, error) {
	return nil, nil
}
func (f *fakeModbusClient) ReadDeviceIdentificationSpecificObject(_ context.Context, _ byte) (map[byte][]byte, error) {
	return nil, nil
}

// newChannelWithFake 创建带 fake client 的 modbusTCPChannel（绕过 TCP 连接）。
func newChannelWithFake(fake *fakeModbusClient) *modbusTCPChannel {
	return &modbusTCPChannel{
		addr:   "127.0.0.1:5020",
		cfg:    ModbusTCPConfig{UnitID: 1, ConnectionTimeoutMs: 1000, RequestTimeoutMs: 1000},
		client: fake,
	}
}

func TestReadHoldingRegister_Uint16(t *testing.T) {
	fake := &fakeModbusClient{
		holding: []uint16{0, 100, 200, 300, 400, 500, 600, 700, 800, 900},
	}
	c := newChannelWithFake(fake)
	// 4x2 → 1-based 2 → 0-based 1 → holding[1] = 100
	v, err := c.Read(context.Background(), "4x2", "uint16")
	if err != nil {
		t.Fatal(err)
	}
	if v.Uint64 != 100 {
		t.Errorf("got %d, want 100", v.Uint64)
	}
}

func TestReadHoldingRegister_Float32(t *testing.T) {
	// 把 3.14 编为 2 寄存器 BigEndian 放到 holding
	f := float32(3.14)
	buf := make([]byte, 4)
	binary.BigEndian.PutUint32(buf, math.Float32bits(f))
	regs := []uint16{binary.BigEndian.Uint16(buf[0:2]), binary.BigEndian.Uint16(buf[2:4])}
	fake := &fakeModbusClient{holding: []uint16{0, 0, regs[0], regs[1]}}
	c := newChannelWithFake(fake)
	v, err := c.Read(context.Background(), "4x3", "float32")
	if err != nil {
		t.Fatal(err)
	}
	if v.Float64 < 3.13 || v.Float64 > 3.15 {
		t.Errorf("got %v, want ≈ 3.14", v.Float64)
	}
}

func TestReadCoil_Bool(t *testing.T) {
	fake := &fakeModbusClient{
		coils: []byte{1, 0}, // coil 1 = 1, coil 2 = 0（0-based addr 0 和 1）
	}
	c := newChannelWithFake(fake)
	v, err := c.Read(context.Background(), "0x1", "bool")
	if err != nil {
		t.Fatal(err)
	}
	if !v.Bool {
		t.Error("0x1 expected true")
	}
	v, err = c.Read(context.Background(), "0x2", "bool")
	if err != nil {
		t.Fatal(err)
	}
	if v.Bool {
		t.Error("0x2 expected false")
	}
}

func TestReadRegisterBit_Bool(t *testing.T) {
	// holding[0] = 0b00000101 → bit 0 = 1, bit 2 = 1
	fake := &fakeModbusClient{holding: []uint16{0x0005}}
	c := newChannelWithFake(fake)
	v, err := c.Read(context.Background(), "4x1.0", "bool")
	if err != nil {
		t.Fatal(err)
	}
	if !v.Bool {
		t.Error("4x1.0 expected true")
	}
	v, err = c.Read(context.Background(), "4x1.2", "bool")
	if err != nil {
		t.Fatal(err)
	}
	if !v.Bool {
		t.Error("4x1.2 expected true")
	}
	v, err = c.Read(context.Background(), "4x1.1", "bool")
	if err != nil {
		t.Fatal(err)
	}
	if v.Bool {
		t.Error("4x1.1 expected false")
	}
}

func TestBatchRead_Continuous(t *testing.T) {
	fake := &fakeModbusClient{
		holding: []uint16{0, 100, 200, 300, 400},
	}
	c := newChannelWithFake(fake)
	points := []*platform.Point{
		{PointBizID: "p1", Address: "4x2", DataType: "uint16"},
		{PointBizID: "p2", Address: "4x3", DataType: "uint16"},
		{PointBizID: "p3", Address: "4x4", DataType: "uint16"},
	}
	out, err := c.BatchRead(context.Background(), points)
	if err != nil {
		t.Fatal(err)
	}
	// 4x2 → addr 1 → 100；4x3 → addr 2 → 200；4x4 → addr 3 → 300
	if out["p1"].Uint64 != 100 || out["p2"].Uint64 != 200 || out["p3"].Uint64 != 300 {
		t.Errorf("got %+v", out)
	}
}

func TestWriteSingleRegister(t *testing.T) {
	fake := &fakeModbusClient{holding: []uint16{0, 0, 0}}
	c := newChannelWithFake(fake)
	if err := c.Write(context.Background(), "4x2", 0xABCD, "uint16"); err != nil {
		t.Fatal(err)
	}
	if fake.holding[1] != 0xABCD {
		t.Errorf("holding[1] = %#x, want 0xABCD", fake.holding[1])
	}
}

func TestWriteMultipleRegisters_Float32(t *testing.T) {
	fake := &fakeModbusClient{holding: []uint16{0, 0, 0, 0}}
	c := newChannelWithFake(fake)
	if err := c.Write(context.Background(), "4x2", 3.14, "float32"); err != nil {
		t.Fatal(err)
	}
	// 读回验证
	v, err := c.Read(context.Background(), "4x2", "float32")
	if err != nil {
		t.Fatal(err)
	}
	if v.Float64 < 3.13 || v.Float64 > 3.15 {
		t.Errorf("Round trip float32 = %v, want ≈ 3.14", v.Float64)
	}
}

func TestReadError_Propagated(t *testing.T) {
	want := errors.New("simulated broker disconnect")
	fake := &fakeModbusClient{err: want}
	c := newChannelWithFake(fake)
	if _, err := c.Read(context.Background(), "4x1", "uint16"); !errors.Is(err, want) {
		t.Errorf("got %v, want %v", err, want)
	}
}
