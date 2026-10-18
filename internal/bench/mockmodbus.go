// Package bench —— mockmodbus：一个进程内启动 N 个 Modbus TCP server，
// 模拟"每台 PLC 占用一个端口"。
//
// 支持的功能码：
//   - FC03 (0x03) Read Holding Registers
//   - FC06 (0x06) Write Single Register
//   - FC16 (0x10) Write Multiple Registers
//
// 数据全部存在内存里（每个 PLC 单独一片），初始为 0，每次写入都可读回。
// 故意做的"快"：单次响应延迟 < 1ms，避免压测变网络压测。
package bench

import (
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"sync/atomic"
)

// MockPLC 是一台虚拟 PLC，对应一个 TCP 端口。
type MockPLC struct {
	listener net.Listener
	regs     []uint16 // 保持寄存器
	mu       sync.RWMutex

	// 统计
	reads    atomic.Uint64
	writes   atomic.Uint64
	requests atomic.Uint64
	addr     string
}

// NewMockPLC 在 addr 上启动一个 Modbus TCP server。
//
// size 是寄存器数量（实际可寻址 0..size-1），内存约 size*2 byte。
func NewMockPLC(addr string, size int) (*MockPLC, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", addr, err)
	}
	m := &MockPLC{
		listener: ln,
		regs:     make([]uint16, size),
		addr:     ln.Addr().String(),
	}
	go m.acceptLoop()
	return m, nil
}

// Addr 返回实际监听地址（用于客户端连）。
func (m *MockPLC) Addr() string { return m.addr }

// Stats 返回读寄存器数 / 写寄存器数 / 总请求数。
func (m *MockPLC) Stats() (reads, writes, total uint64) {
	return m.reads.Load(), m.writes.Load(), m.requests.Load()
}

// Close 关闭 server。
func (m *MockPLC) Close() error { return m.listener.Close() }

func (m *MockPLC) acceptLoop() {
	for {
		conn, err := m.listener.Accept()
		if err != nil {
			return
		}
		go m.handle(conn)
	}
}

// handle 处理一个 Modbus TCP 连接。
//
// MBAP header:
//   [0..1] Transaction ID
//   [2..3] Protocol ID (0x0000)
//   [4..5] Length (uint16 big endian) = 1(unit) + 1(fc) + N(data)
//   [6]   Unit ID
//   [7]   Function Code
//   [8..] Data
func (m *MockPLC) handle(conn net.Conn) {
	defer conn.Close()
	for {
		hdr := make([]byte, 8)
		if _, err := io.ReadFull(conn, hdr); err != nil {
			return
		}
		bodyLen := int(binary.BigEndian.Uint16(hdr[4:6]))
		if bodyLen < 2 || bodyLen > 260 {
			return
		}
		unit := hdr[6]
		fc := hdr[7]
		dataLen := bodyLen - 2
		var data []byte
		if dataLen > 0 {
			data = make([]byte, dataLen)
			if _, err := io.ReadFull(conn, data); err != nil {
				return
			}
		}

		m.requests.Add(1)
		resp := m.dispatch(unit, fc, data)

		// 完整响应帧：TID(2) + PID(2) + Length(2) + 负载(unit+fc+body)
		// Length = len(resp)（resp 已包含 unit+fc+body）
		out := make([]byte, 6+len(resp))
		copy(out[0:2], hdr[0:2]) // echo TID
		out[2] = 0; out[3] = 0   // PID=0
		binary.BigEndian.PutUint16(out[4:6], uint16(len(resp)))
		copy(out[6:], resp)
		if _, err := conn.Write(out); err != nil {
			return
		}
	}
}

// dispatch 分发请求到对应 fc handler。
//
// handler 返回的 byte 流 = unit + fc + 数据区（含错误响应）。
func (m *MockPLC) dispatch(unit, fc byte, data []byte) []byte {
	var body []byte
	switch fc {
	case 0x03:
		body = m.f03ReadHoldingRegisters(data)
	case 0x06:
		body = m.f06WriteSingleRegister(data)
	case 0x10:
		body = m.f16WriteMultipleRegisters(data)
	default:
		body = []byte{fc | 0x80, 0x01} // illegal function
	}
	resp := make([]byte, 2+len(body))
	resp[0] = unit
	resp[1] = fc
	copy(resp[2:], body)
	return resp
}

// f03ReadHoldingRegisters 请求: start_addr(2) + qty(2)
func (m *MockPLC) f03ReadHoldingRegisters(data []byte) []byte {
	if len(data) < 4 {
		return []byte{0x03 | 0x80, 0x03}
	}
	start := binary.BigEndian.Uint16(data[0:2])
	qty := binary.BigEndian.Uint16(data[2:4])
	if qty == 0 || qty > 125 {
		return []byte{0x03 | 0x80, 0x03}
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if int(start)+int(qty) > len(m.regs) {
		return []byte{0x03 | 0x80, 0x02}
	}
	out := make([]byte, 1+2*int(qty)) // byte_count + values
	out[0] = byte(2 * qty)
	for i := 0; i < int(qty); i++ {
		binary.BigEndian.PutUint16(out[1+2*i:3+2*i], m.regs[int(start)+i])
	}
	m.reads.Add(uint64(qty))
	return out
}

// f06WriteSingleRegister 请求: addr(2) + value(2)，响应 echo 请求
func (m *MockPLC) f06WriteSingleRegister(data []byte) []byte {
	if len(data) < 4 {
		return []byte{0x06 | 0x80, 0x03}
	}
	addr := binary.BigEndian.Uint16(data[0:2])
	val := binary.BigEndian.Uint16(data[2:4])
	m.mu.Lock()
	if int(addr) >= len(m.regs) {
		m.mu.Unlock()
		return []byte{0x06 | 0x80, 0x02}
	}
	m.regs[addr] = val
	m.mu.Unlock()
	m.writes.Add(1)
	// echo request as response body
	out := make([]byte, 4)
	copy(out, data)
	return out
}

// f16WriteMultipleRegisters 请求: start_addr(2) + qty(2) + byte_count(1) + values(qty*2)
func (m *MockPLC) f16WriteMultipleRegisters(data []byte) []byte {
	if len(data) < 5 {
		return []byte{0x10 | 0x80, 0x03}
	}
	start := binary.BigEndian.Uint16(data[0:2])
	qty := binary.BigEndian.Uint16(data[2:4])
	bc := int(data[4])
	if bc != int(2*qty) || qty > 123 {
		return []byte{0x10 | 0x80, 0x03}
	}
	if 5+bc > len(data) {
		return []byte{0x10 | 0x80, 0x03}
	}
	m.mu.Lock()
	if int(start)+int(qty) > len(m.regs) {
		m.mu.Unlock()
		return []byte{0x10 | 0x80, 0x02}
	}
	for i := 0; i < int(qty); i++ {
		m.regs[int(start)+i] = binary.BigEndian.Uint16(data[5+2*i : 7+2*i])
	}
	m.mu.Unlock()
	m.writes.Add(uint64(qty))
	// 响应: addr(2) + qty(2)
	out := make([]byte, 4)
	binary.BigEndian.PutUint16(out[0:2], start)
	binary.BigEndian.PutUint16(out[2:4], qty)
	return out
}

// StartMockPool 在 127.0.0.1 上启动 n 个 MockPLC，端口从 basePort 起。
//
// 返回 (list of PLC, basePort used, error). 任一失败 → 回滚已起的。
func StartMockPool(basePort int, count, regsPerPLC int) ([]*MockPLC, int, error) {
	plcs := make([]*MockPLC, 0, count)
	for i := 0; i < count; i++ {
		addr := fmt.Sprintf("127.0.0.1:%d", basePort+i)
		m, err := NewMockPLC(addr, regsPerPLC)
		if err != nil {
			for _, p := range plcs {
				_ = p.Close()
			}
			return nil, 0, fmt.Errorf("start %s: %w", addr, err)
		}
		plcs = append(plcs, m)
	}
	if len(plcs) > 0 {
		log.Printf("[mockmodbus] 启动了 %d 个 PLC，端口 %d..%d", count, basePort, basePort+count-1)
	}
	return plcs, basePort, nil
}
