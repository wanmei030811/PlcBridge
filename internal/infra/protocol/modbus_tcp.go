// ModbusTCP 协议插件实现（Plan §6.3 / 附录A MVP）。
//
// 库依赖：github.com/grid-x/modbus（纯 Go TCP client，无 CGo）。
//
// 地址格式：
//
//	<func><addr>[.<bit>]
//
//	func: 0x=线圈(coil, FC01/FC05) / 1x=离散输入(discrete input, FC02) /
//	      3x=输入寄存器(input register, FC04) / 4x=保持寄存器(holding register, FC03/FC06/FC16)
//
//	addr: 寄存器号 / 线圈号（1-based，Modbus 协议惯例；内部 -1 转 0-based）
//	bit : 可选，仅 bool 类型；写 coil 时表示写该 bit（bit 0..15）
//
//	示例：
//	  "0x100"     → 线圈 100
//	  "1x100"     → 离散输入 100
//	  "3x100"     → 输入寄存器 100
//	  "4x100"     → 保持寄存器 100
//	  "4x100.5"   → 保持寄存器 100 的 bit 5（仅 bool 类型读）
//
// 数据类型映射：
//
//	bool            → 1 线圈 或 1 寄存器的某 bit
//	int16/uint16    → 1 寄存器
//	int32/uint32    → 2 寄存器（BigEndian，高位在前；与 PLC 工程师常用习惯一致）
//	float32         → 2 寄存器（BigEndian IEEE754）
//	int64/uint64    → 4 寄存器（BigEndian）
//	float64         → 4 寄存器（BigEndian IEEE754）
//
// 字节序：BigEndian（Modbus 标准）。如需 LittleEndian/Word-Swap 后续可扩展 cfg。
//
// 批量优化：BatchRead 把同一 fc + 连续地址的点位打包成 1 次 Modbus 请求。
// 寄存器：max 125 寄存器/次。
package protocol

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/grid-x/modbus"

	"github.com/plcbridge/plcbridge/internal/platform"
)

// === 插件注册 ===

type modbusTCPPlugin struct{}

func newModbusTCPPlugin() ProtocolPlugin { return &modbusTCPPlugin{} }

func (p *modbusTCPPlugin) Name() string    { return "modbus_tcp" }
func (p *modbusTCPPlugin) Version() string { return "1.0.0" }

func (p *modbusTCPPlugin) ConfigSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"unit_id":                 {"type": "integer", "default": 1, "minimum": 1, "maximum": 247},
			"connection_timeout_ms":   {"type": "integer", "default": 2000, "minimum": 100, "maximum": 30000},
			"request_timeout_ms":      {"type": "integer", "default": 1000, "minimum": 100, "maximum": 30000}
		},
		"required": ["unit_id"]
	}`)
}

type ModbusTCPConfig struct {
	UnitID              int `json:"unit_id"`
	ConnectionTimeoutMs int `json:"connection_timeout_ms"`
	RequestTimeoutMs    int `json:"request_timeout_ms"`
}

func (p *modbusTCPPlugin) ParseConfig(raw []byte) (any, error) {
	var cfg ModbusTCPConfig
	if len(raw) == 0 {
		cfg = ModbusTCPConfig{UnitID: 1, ConnectionTimeoutMs: 2000, RequestTimeoutMs: 1000}
		return cfg, nil
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("ModbusTCP 配置解析失败: %w", err)
	}
	if cfg.UnitID <= 0 || cfg.UnitID > 247 {
		return nil, fmt.Errorf("ModbusTCP unit_id 非法: %d（1..247）", cfg.UnitID)
	}
	if cfg.ConnectionTimeoutMs == 0 {
		cfg.ConnectionTimeoutMs = 2000
	}
	if cfg.RequestTimeoutMs == 0 {
		cfg.RequestTimeoutMs = 1000
	}
	return cfg, nil
}

func (p *modbusTCPPlugin) Init(_ context.Context) error { return nil }
func (p *modbusTCPPlugin) Close() error                { return nil }

func (p *modbusTCPPlugin) CreateChannel(ch *platform.Channel) (ProtocolChannel, error) {
	if ch.Role != "collect" {
		return nil, fmt.Errorf("ModbusTCP MVP 仅支持 role=collect（got %s）", ch.Role)
	}
	// ParseConfig 已在 Registry.Acquire 之前由 collect workers 调过；
	// 这里再调一次防御性解析（plugin interface 允许分离）
	cfgRaw, err := p.ParseConfig(ch.Config)
	if err != nil {
		return nil, err
	}
	cfg := cfgRaw.(ModbusTCPConfig)
	return &modbusTCPChannel{
		addr: ch.Address,
		cfg:  cfg,
	}, nil
}

// === Channel 实现 ===

type modbusTCPChannel struct {
	addr string
	cfg  ModbusTCPConfig

	mu     sync.Mutex
	client modbus.Client
	// handler 保留 TCPClientHandler 引用，确保 Disconnect 时真正关闭 TCP 连接；
	// 否则 grid-x/modbus 库不会主动关闭 socket，资源会持续占用到下次 Connect 替换。
	handler *modbus.TCPClientHandler

	closed    atomic.Bool
	lastUsed  atomic.Int64
	lastErr   atomic.Pointer[string]
	lastErrAt atomic.Int64

	// PR2：详细统计
	readCount   atomic.Uint64
	writeCount  atomic.Uint64
	errorCount  atomic.Uint64
	lastReadOkMs  atomic.Int64
	lastWriteOkMs atomic.Int64
	lastErrorMs   atomic.Int64
}

func (c *modbusTCPChannel) Connect(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.client != nil {
		return nil
	}
	return c.connectLocked(ctx)
}

// Reconnect 强制重建连接。
// 与 Connect 的区别：即使 client != nil，也会关闭旧 handler 再重连。
// 用于 registry 检测到 LastError 持续非空时的恢复路径。
func (c *modbusTCPChannel) Reconnect(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.handler != nil {
		_ = c.handler.Close()
		c.handler = nil
	}
	c.client = nil
	if err := c.connectLocked(ctx); err != nil {
		c.errorCount.Add(1)
		c.lastErrorMs.Store(time.Now().UnixMilli())
		return err
	}
	return nil
}

func (c *modbusTCPChannel) connectLocked(ctx context.Context) error {
	h := modbus.NewTCPClientHandler(c.addr)
	h.SetSlave(byte(c.cfg.UnitID))
	h.Timeout = durationFromMs(c.cfg.ConnectionTimeoutMs)
	if err := h.Connect(ctx); err != nil {
		c.setErr(err)
		return fmt.Errorf("ModbusTCP Connect %s 失败: %w", c.addr, err)
	}
	c.handler = h
	c.client = modbus.NewClient(h)
	c.lastUsed.Store(time.Now().UnixNano())
	c.setErr(nil)
	return nil
}

func (c *modbusTCPChannel) Disconnect() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.handler == nil {
		return nil
	}
	h := c.handler
	c.handler = nil
	c.client = nil
	h.Close()
	return nil
}

func (c *modbusTCPChannel) HealthCheck(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.client == nil {
		return errors.New("ModbusTCP channel 未连接")
	}
	c.lastUsed.Store(time.Now().UnixNano())
	c.setErr(nil)
	return nil
}

// IsConnected 本地视图：client 非空且未关闭。
func (c *modbusTCPChannel) IsConnected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.client != nil && !c.closed.Load()
}

// LastActiveAt 返回最近一次成功操作的本地时间。
func (c *modbusTCPChannel) LastActiveAt() time.Time {
	return time.Unix(0, c.lastUsed.Load())
}

// LastError 返回最近一次错误（写操作失败或 Connect 失败时设置）。
func (c *modbusTCPChannel) LastError() string {
	if p := c.lastErr.Load(); p != nil {
		return *p
	}
	return ""
}

// LastErrorAt 返回最近一次 lastErr 被设置为非空的时间戳。
func (c *modbusTCPChannel) LastErrorAt() time.Time {
	ns := c.lastErrAt.Load()
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns)
}

// Stats 返回 Modbus TCP 协议特定运行时数据。
//
// 字段约定：
//   - "connection_state": "Connected" / "Disconnected"（本地视图）
//   - "address" / "unit_id": 静态配置
//   - "last_probe_ok_ms": 最近一次 ProbeActive 成功 unix ms
//   - "read_count" / "write_count" / "error_count": 累计计数（PR2）
//   - "last_read_ok_ms" / "last_write_ok_ms" / "last_error_ms": 时间戳（PR2）
func (c *modbusTCPChannel) Stats() map[string]any {
	c.mu.Lock()
	connected := c.client != nil && !c.closed.Load()
	addr := c.addr
	unitID := c.cfg.UnitID
	c.mu.Unlock()
	state := "Disconnected"
	if connected {
		state = "Connected"
	}
	probeMs := int64(0)
	if lastUsedNs := c.lastUsed.Load(); lastUsedNs != 0 {
		probeMs = time.Unix(0, lastUsedNs).UnixMilli()
	}
	return map[string]any{
		"connection_state": state,
		"address":          addr,
		"unit_id":          unitID,
		"read_count":       c.readCount.Load(),
		"write_count":      c.writeCount.Load(),
		"error_count":      c.errorCount.Load(),
		"last_read_ok_ms":  c.lastReadOkMs.Load(),
		"last_write_ok_ms": c.lastWriteOkMs.Load(),
		"last_error_ms":    c.lastErrorMs.Load(),
		"last_probe_ok_ms": probeMs,
	}
}

// ProbeActive 主动探活：发送 Modbus "Read Device Identification" (FC43/14)。
// 这是 modbus 标准探活命令，不读用户数据区，不影响 PLC 业务。
//   - 成功：touch + 清空 lastErr
//   - 失败：setErr
func (c *modbusTCPChannel) ProbeActive(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.client == nil {
		err := errors.New("ModbusTCP channel 未连接")
		c.setErr(err)
		c.errorCount.Add(1)
		c.lastErrorMs.Store(time.Now().UnixMilli())
		return err
	}
	// FC43/14 探活：即使服务器不支持也只会返回 Illegal Function (0x01)，
	// 仍代表"链路通"。但我们目前只看是否拿到非 transport-level 错误。
	if _, err := c.client.ReadDeviceIdentificationSpecificObject(ctx, 0x00); err != nil {
		c.setErr(err)
		c.errorCount.Add(1)
		c.lastErrorMs.Store(time.Now().UnixMilli())
		return err
	}
	c.lastUsed.Store(time.Now().UnixNano())
	c.setErr(nil)
	return nil
}

// setErr 原子地记录最近一次错误。
func (c *modbusTCPChannel) setErr(err error) {
	if err == nil {
		c.lastErr.Store(nil)
		c.lastErrAt.Store(0)
		return
	}
	msg := err.Error()
	c.lastErr.Store(&msg)
	c.lastErrAt.Store(time.Now().UnixNano())
}

// markBrokenRead 读失败时调用：仅记录错误。
//
// 历史版本：曾"关闭 socket + 置 client=nil"以让 IsConnected() 立即返回 false。
// 但实测发现主动 Close 会破坏 modbus 客户端内部状态，且 registry 缓存的 cli 句柄
// 被多个 worker 复用，关闭会误伤其他在跑的读。
// 现在改为：仅记 lastErr，由 Snapshot() 的 `cli.LastError() == ""` 检查决定是否显示 OFF。
// 这同时满足"快速 OFF"诉求，且不会破坏连接生命周期。
//
// 必须在 c.mu 持有时调用。
func (c *modbusTCPChannel) markBrokenRead(err error) error {
	c.setErr(err)
	c.errorCount.Add(1)
	c.lastErrorMs.Store(time.Now().UnixMilli())
	return err
}

// === 单点 Read ===

func (c *modbusTCPChannel) Read(ctx context.Context, address string, dataType string) (Value, error) {
	loc, err := parseModbusAddress(address)
	if err != nil {
		return Value{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.client == nil {
		return Value{}, errors.New("ModbusTCP channel 未连接")
	}
	switch dataType {
	case "bool":
		v, err := readBool(ctx, c.client, loc)
		if err != nil {
			return Value{DataType: dataType, Quality: "bad"}, c.markBrokenRead(err)
		}
		c.setErr(nil)
		c.readCount.Add(1)
		c.lastReadOkMs.Store(time.Now().UnixMilli())
		return v, nil
	case "int16", "uint16":
		buf, err := readRegs(ctx, c.client, loc, 1)
		if err != nil {
			return Value{DataType: dataType, Quality: "bad"}, c.markBrokenRead(err)
		}
		v, err := decodeUint16N(buf, dataType)
		if err == nil {
			c.setErr(nil)
			c.readCount.Add(1)
			c.lastReadOkMs.Store(time.Now().UnixMilli())
		}
		return v, err
	case "int32", "uint32", "float32":
		buf, err := readRegs(ctx, c.client, loc, 2)
		if err != nil {
			return Value{DataType: dataType, Quality: "bad"}, c.markBrokenRead(err)
		}
		v, err := decodeUint16N(buf, dataType)
		if err == nil {
			c.setErr(nil)
			c.readCount.Add(1)
			c.lastReadOkMs.Store(time.Now().UnixMilli())
		}
		return v, err
	case "int64", "uint64", "float64":
		buf, err := readRegs(ctx, c.client, loc, 4)
		if err != nil {
			return Value{DataType: dataType, Quality: "bad"}, c.markBrokenRead(err)
		}
		v, err := decodeUint16N(buf, dataType)
		if err == nil {
			c.setErr(nil)
			c.readCount.Add(1)
			c.lastReadOkMs.Store(time.Now().UnixMilli())
		}
		return v, err
	case "string":
		// Modbus TCP 无标准 STRING layout；让上游/前端禁用 Modbus 上的 string 点位。
		return Value{}, fmt.Errorf("ModbusTCP data_type=string 不支持（无标准 layout）")
	default:
		return Value{}, fmt.Errorf("ModbusTCP data_type 不支持: %s", dataType)
	}
}

func readBool(ctx context.Context, cli modbus.Client, loc modbusLocation) (Value, error) {
	switch loc.funcCode {
	case "0x":
		b, err := cli.ReadCoils(ctx, loc.addr, 1)
		if err != nil {
			return Value{DataType: "bool", Quality: "bad"}, err
		}
		return Value{DataType: "bool", Bool: len(b) > 0 && b[0] == 1, Quality: "good"}, nil
	case "1x":
		b, err := cli.ReadDiscreteInputs(ctx, loc.addr, 1)
		if err != nil {
			return Value{DataType: "bool", Quality: "bad"}, err
		}
		return Value{DataType: "bool", Bool: len(b) > 0 && b[0] == 1, Quality: "good"}, nil
	case "4x":
		buf, err := cli.ReadHoldingRegisters(ctx, loc.addr, 1)
		if err != nil {
			return Value{DataType: "bool", Quality: "bad"}, err
		}
		return Value{DataType: "bool", Bool: bytesToRegs(buf)[0]>>uint(loc.bit)&1 == 1, Quality: "good"}, nil
	case "3x":
		buf, err := cli.ReadInputRegisters(ctx, loc.addr, 1)
		if err != nil {
			return Value{DataType: "bool", Quality: "bad"}, err
		}
		return Value{DataType: "bool", Bool: bytesToRegs(buf)[0]>>uint(loc.bit)&1 == 1, Quality: "good"}, nil
	}
	return Value{}, fmt.Errorf("bool 读不支持 func %q", loc.funcCode)
}

func readRegs(ctx context.Context, cli modbus.Client, loc modbusLocation, n uint16) ([]byte, error) {
	switch loc.funcCode {
	case "3x":
		return cli.ReadInputRegisters(ctx, loc.addr, n)
	case "4x":
		return cli.ReadHoldingRegisters(ctx, loc.addr, n)
	default:
		return nil, fmt.Errorf("func %q 不支持寄存器读", loc.funcCode)
	}
}

// === BatchRead ===

func (c *modbusTCPChannel) BatchRead(ctx context.Context, points []*platform.Point) (map[string]Value, error) {
	out := make(map[string]Value, len(points))
	if len(points) == 0 {
		return out, nil
	}

	type bucket struct {
		funcCode string
		dataType string
		regCount uint16
		points   []*platform.Point
	}
	buckets := map[string]*bucket{}
	var bucketOrder []string
	for _, pt := range points {
		loc, err := parseModbusAddress(pt.Address)
		if err != nil {
			out[pt.PointBizID] = Value{DataType: pt.DataType, Quality: "bad"}
			continue
		}
		if pt.DataType == "bool" {
			// bool 单点读（不打包）
			c.mu.Lock()
			v, err := readBool(ctx, c.client, loc)
			if err != nil {
				// 读失败 → 仅记 lastErr。不断开 client。
				c.markBrokenRead(err)
				c.mu.Unlock()
				out[pt.PointBizID] = Value{DataType: pt.DataType, Quality: "bad"}
				continue
			}
			c.setErr(nil)
			c.mu.Unlock()
			out[pt.PointBizID] = v
			continue
		}
		if pt.DataType == "string" {
			// Modbus TCP 无标准 STRING layout；记录为 bad 让上层报警并配置层拒绝。
			out[pt.PointBizID] = Value{DataType: pt.DataType, Quality: "bad"}
			continue
		}
		key := loc.funcCode + "|" + pt.DataType
		if _, ok := buckets[key]; !ok {
			buckets[key] = &bucket{funcCode: loc.funcCode, dataType: pt.DataType, regCount: registerCount(pt.DataType)}
			bucketOrder = append(bucketOrder, key)
		}
		buckets[key].points = append(buckets[key].points, pt)
	}

	for _, key := range bucketOrder {
		b := buckets[key]
		sortedPoints := sortPointsByAddr(b.points)
		c.batchReadBucket(ctx, sortedPoints, b.funcCode, b.regCount, out)
	}
	// 成功：BatchRead 完成（按 batch 计数）
	c.readCount.Add(1)
	c.lastReadOkMs.Store(time.Now().UnixMilli())
	return out, nil
}

func (c *modbusTCPChannel) batchReadBucket(ctx context.Context, points []*platform.Point, funcCode string, n uint16, out map[string]Value) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.client == nil {
		for _, pt := range points {
			out[pt.PointBizID] = Value{DataType: pt.DataType, Quality: "bad"}
		}
		return
	}
	i := 0
	for i < len(points) {
		startLoc, _ := parseModbusAddress(points[i].Address)
		j := i + 1
		for j < len(points) {
			nextLoc, _ := parseModbusAddress(points[j].Address)
			if nextLoc.funcCode != funcCode {
				break
			}
			if nextLoc.addr != startLoc.addr+uint16((j-i)*int(n)) {
				break
			}
			j++
		}
		count := uint16(j-i) * n
		if count > 125 {
			count = 125
			j = i + int(count/n)
		}
		buf, err := batchReadRegisters(ctx, c.client, funcCode, startLoc.addr, count)
		if err != nil {
			// 读失败 → 仅记 lastErr。不断开 client。
			c.markBrokenRead(err)
			for k := i; k < j; k++ {
				out[points[k].PointBizID] = Value{DataType: points[k].DataType, Quality: "bad"}
			}
			i = j
			continue
		}
		// 读成功 → 清 lastErr，让状态立刻恢复 ON
		c.setErr(nil)
		regs := bytesToRegs(buf)
		for k := i; k < j; k++ {
			pt := points[k]
			off := uint16(k-i) * n
			v, err := decodeUint16NFromRegs(regs[off:off+n], pt.DataType)
			if err != nil {
				out[pt.PointBizID] = Value{DataType: pt.DataType, Quality: "bad"}
				continue
			}
			out[pt.PointBizID] = v
		}
		i = j
	}
}

func batchReadRegisters(ctx context.Context, cli modbus.Client, funcCode string, addr, count uint16) ([]byte, error) {
	switch funcCode {
	case "3x":
		return cli.ReadInputRegisters(ctx, addr, count)
	case "4x":
		return cli.ReadHoldingRegisters(ctx, addr, count)
	default:
		return nil, fmt.Errorf("batch 寄存器读不支持 func %q", funcCode)
	}
}

// === 单点 Write ===

func (c *modbusTCPChannel) Write(ctx context.Context, address string, value any, dataType string) error {
	loc, err := parseModbusAddress(address)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.client == nil {
		return errors.New("ModbusTCP channel 未连接")
	}
	switch dataType {
	case "bool":
		v, ok := value.(bool)
		if !ok {
			n, nok := toInt64Modbus(value)
			if nok != nil {
				return fmt.Errorf("ModbusTCP bool 值类型非法: %T", value)
			}
			v = n != 0
		}
		switch loc.funcCode {
		case "0x":
			var bv uint16
			if v {
				bv = 0xFF00
			}
			_, err := c.client.WriteSingleCoil(ctx, loc.addr, bv)
			if err != nil {
				return c.markBrokenRead(err)
			}
			c.setErr(nil)
			c.writeSingleOK()
			return nil
		case "4x":
			buf, rerr := c.client.ReadHoldingRegisters(ctx, loc.addr, 1)
			if rerr != nil {
				return c.markBrokenRead(rerr)
			}
			regs := bytesToRegs(buf)
			mask := uint16(1) << uint(loc.bit)
			if v {
				regs[0] |= mask
			} else {
				regs[0] &^= mask
			}
			_, werr := c.client.WriteSingleRegister(ctx, loc.addr, regs[0])
			if werr != nil {
				return c.markBrokenRead(werr)
			}
			c.setErr(nil)
			c.writeSingleOK()
			return nil
		default:
			return fmt.Errorf("ModbusTCP bool 写不支持 func %q", loc.funcCode)
		}
	case "int16", "uint16":
		n, err := toInt64Modbus(value)
		if err != nil {
			return fmt.Errorf("ModbusTCP int16 值转换失败: %w", err)
		}
		if loc.funcCode != "4x" {
			return fmt.Errorf("ModbusTCP 寄存器写必须用 4x（got %s）", loc.funcCode)
		}
		_, err = c.client.WriteSingleRegister(ctx, loc.addr, uint16(n))
		if err != nil {
			return c.markBrokenRead(err)
		}
		c.setErr(nil)
		c.writeSingleOK()
		return nil
	case "int32", "uint32", "float32":
		regs, err := encodeUint16N(value, dataType)
		if err != nil {
			return err
		}
		if loc.funcCode != "4x" {
			return fmt.Errorf("ModbusTCP 多寄存器写必须用 4x（got %s）", loc.funcCode)
		}
		_, err = c.client.WriteMultipleRegisters(ctx, loc.addr, uint16(len(regs)), regsToBytes(regs))
		if err != nil {
			return c.markBrokenRead(err)
		}
		c.setErr(nil)
		c.writeSingleOK()
		return nil
	case "int64", "uint64", "float64":
		regs, err := encodeUint16N(value, dataType)
		if err != nil {
			return err
		}
		if loc.funcCode != "4x" {
			return fmt.Errorf("ModbusTCP 多寄存器写必须用 4x（got %s）", loc.funcCode)
		}
		_, err = c.client.WriteMultipleRegisters(ctx, loc.addr, uint16(len(regs)), regsToBytes(regs))
		if err != nil {
			return c.markBrokenRead(err)
		}
		c.setErr(nil)
		c.writeSingleOK()
		return nil
	}
	return fmt.Errorf("ModbusTCP data_type 不支持: %s", dataType)
}

func (c *modbusTCPChannel) BatchWrite(ctx context.Context, writes []WriteOp) error {
	for _, w := range writes {
		if err := c.Write(ctx, w.Address, w.Value, w.DataType); err != nil {
			return err
		}
	}
	// 成功：BatchWrite 完成
	c.writeCount.Add(1)
	c.lastWriteOkMs.Store(time.Now().UnixMilli())
	return nil
}

// === 工具 ===
// 必须在 c.mu 持有时调用。原子地记录一次成功操作（按调用层级：single vs batch）。
// writeSingleOK 用于 Write() 内部成功路径；BatchWrite 单独计数（PR2 保持 batch 语义）。
func (c *modbusTCPChannel) writeSingleOK() {
	c.writeCount.Add(1)
	c.lastWriteOkMs.Store(time.Now().UnixMilli())
}

type modbusLocation struct {
	funcCode string
	addr     uint16 // 0-based
	bit      int    // 0..15
}

func parseModbusAddress(s string) (modbusLocation, error) {
	s = strings.TrimSpace(s)
	if len(s) < 3 {
		return modbusLocation{}, fmt.Errorf("ModbusTCP 地址过短: %q", s)
	}
	fc := s[:2]
	rest := s[2:]
	if fc != "0x" && fc != "1x" && fc != "3x" && fc != "4x" {
		return modbusLocation{}, fmt.Errorf("ModbusTCP 地址 func 非法: %q（期望 0x/1x/3x/4x）", fc)
	}
	bit := 0
	if idx := strings.Index(rest, "."); idx >= 0 {
		bitStr := rest[idx+1:]
		rest = rest[:idx]
		n, err := strconv.Atoi(bitStr)
		if err != nil || n < 0 || n > 15 {
			return modbusLocation{}, fmt.Errorf("ModbusTCP 地址 bit 非法: %q", bitStr)
		}
		bit = n
	}
	n, err := strconv.Atoi(rest)
	if err != nil || n < 1 || n > 65535 {
		return modbusLocation{}, fmt.Errorf("ModbusTCP 地址号非法: %q", rest)
	}
	return modbusLocation{funcCode: fc, addr: uint16(n - 1), bit: bit}, nil
}

func registerCount(dataType string) uint16 {
	switch dataType {
	case "int16", "uint16":
		return 1
	case "int32", "uint32", "float32":
		return 2
	case "int64", "uint64", "float64":
		return 4
	case "string":
		// Modbus TCP 无标准 STRING layout；BatchRead 不打包，遇到 string 直接跳过。
		return 0
	default:
		return 1
	}
}

// bytesToRegs / regsToBytes 把 modbus 库的 []byte 形式转 []uint16（BigEndian）。
func bytesToRegs(buf []byte) []uint16 {
	n := len(buf) / 2
	out := make([]uint16, n)
	for i := 0; i < n; i++ {
		out[i] = binary.BigEndian.Uint16(buf[i*2 : i*2+2])
	}
	return out
}

func regsToBytes(regs []uint16) []byte {
	out := make([]byte, len(regs)*2)
	for i, r := range regs {
		binary.BigEndian.PutUint16(out[i*2:i*2+2], r)
	}
	return out
}

func decodeUint16N(buf []byte, dataType string) (Value, error) {
	return decodeUint16NFromRegs(bytesToRegs(buf), dataType)
}

func decodeUint16NFromRegs(regs []uint16, dataType string) (Value, error) {
	switch dataType {
	case "int16":
		return Value{DataType: dataType, Int64: int64(int16(regs[0])), Quality: "good"}, nil
	case "uint16":
		return Value{DataType: dataType, Uint64: uint64(regs[0]), Quality: "good"}, nil
	case "int32", "uint32", "float32":
		if len(regs) < 2 {
			return Value{}, fmt.Errorf("decode %s 需要 2 寄存器", dataType)
		}
		buf := make([]byte, 4)
		binary.BigEndian.PutUint16(buf[0:2], regs[0])
		binary.BigEndian.PutUint16(buf[2:4], regs[1])
		switch dataType {
		case "int32":
			return Value{DataType: dataType, Int64: int64(int32(binary.BigEndian.Uint32(buf))), Quality: "good"}, nil
		case "uint32":
			return Value{DataType: dataType, Uint64: uint64(binary.BigEndian.Uint32(buf)), Quality: "good"}, nil
		case "float32":
			return Value{DataType: dataType, Float64: float64(math.Float32frombits(binary.BigEndian.Uint32(buf))), Quality: "good"}, nil
		}
	case "int64", "uint64", "float64":
		if len(regs) < 4 {
			return Value{}, fmt.Errorf("decode %s 需要 4 寄存器", dataType)
		}
		buf := make([]byte, 8)
		for i := 0; i < 4; i++ {
			binary.BigEndian.PutUint16(buf[i*2:i*2+2], regs[i])
		}
		switch dataType {
		case "int64":
			return Value{DataType: dataType, Int64: int64(binary.BigEndian.Uint64(buf)), Quality: "good"}, nil
		case "uint64":
			return Value{DataType: dataType, Uint64: binary.BigEndian.Uint64(buf), Quality: "good"}, nil
		case "float64":
			return Value{DataType: dataType, Float64: math.Float64frombits(binary.BigEndian.Uint64(buf)), Quality: "good"}, nil
		}
	}
	return Value{}, fmt.Errorf("decode 未知 dataType: %s", dataType)
}

func encodeUint16N(value any, dataType string) ([]uint16, error) {
	switch dataType {
	case "int16":
		n, err := toInt64Modbus(value)
		if err != nil {
			return nil, err
		}
		return []uint16{uint16(int16(n))}, nil
	case "uint16":
		n, err := toInt64Modbus(value)
		if err != nil {
			return nil, err
		}
		return []uint16{uint16(n)}, nil
	case "int32", "uint32", "float32":
		var u32 uint32
		switch dataType {
		case "int32":
			n, err := toInt64Modbus(value)
			if err != nil {
				return nil, err
			}
			u32 = uint32(int32(n))
		case "uint32":
			n, err := toInt64Modbus(value)
			if err != nil {
				return nil, err
			}
			u32 = uint32(n)
		case "float32":
			f, ok := value.(float64)
			if !ok {
				if f32, ok2 := value.(float32); ok2 {
					f = float64(f32)
				} else {
					return nil, fmt.Errorf("ModbusTCP float32 值类型非法: %T", value)
				}
			}
			u32 = math.Float32bits(float32(f))
		}
		buf := make([]byte, 4)
		binary.BigEndian.PutUint32(buf, u32)
		return []uint16{binary.BigEndian.Uint16(buf[0:2]), binary.BigEndian.Uint16(buf[2:4])}, nil
	case "int64", "uint64", "float64":
		var u64 uint64
		switch dataType {
		case "int64":
			n, err := toInt64Modbus(value)
			if err != nil {
				return nil, err
			}
			u64 = uint64(int64(n))
		case "uint64":
			n, err := toInt64Modbus(value)
			if err != nil {
				return nil, err
			}
			u64 = uint64(n)
		case "float64":
			f, ok := value.(float64)
			if !ok {
				return nil, fmt.Errorf("ModbusTCP float64 值类型非法: %T", value)
			}
			u64 = math.Float64bits(f)
		}
		buf := make([]byte, 8)
		binary.BigEndian.PutUint64(buf, u64)
		return []uint16{
			binary.BigEndian.Uint16(buf[0:2]),
			binary.BigEndian.Uint16(buf[2:4]),
			binary.BigEndian.Uint16(buf[4:6]),
			binary.BigEndian.Uint16(buf[6:8]),
		}, nil
	}
	return nil, fmt.Errorf("encode 未知 dataType: %s", dataType)
}

// toInt64Modbus 命名前缀避免与 s7.go 的同名函数冲突（同包内不允许重复声明）。
func toInt64Modbus(v any) (int64, error) {
	switch x := v.(type) {
	case int:
		return int64(x), nil
	case int8:
		return int64(x), nil
	case int16:
		return int64(x), nil
	case int32:
		return int64(x), nil
	case int64:
		return x, nil
	case uint:
		return int64(x), nil
	case uint8:
		return int64(x), nil
	case uint16:
		return int64(x), nil
	case uint32:
		return int64(x), nil
	case uint64:
		if x > math.MaxInt64 {
			return 0, errors.New("uint64 超过 int64 范围")
		}
		return int64(x), nil
	case float32:
		return int64(x), nil
	case float64:
		return int64(x), nil
	case bool:
		if x {
			return 1, nil
		}
		return 0, nil
	}
	return 0, fmt.Errorf("无法转 int64: %T", v)
}

func sortPointsByAddr(points []*platform.Point) []*platform.Point {
	out := make([]*platform.Point, len(points))
	copy(out, points)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0; j-- {
			aAddr, _ := parseModbusAddress(out[j-1].Address)
			bAddr, _ := parseModbusAddress(out[j].Address)
			if aAddr.addr > bAddr.addr {
				out[j-1], out[j] = out[j], out[j-1]
			} else {
				break
			}
		}
	}
	return out
}

func durationFromMs(ms int) time.Duration {
	if ms <= 0 {
		return 2 * time.Second
	}
	return time.Duration(ms) * time.Millisecond
}
