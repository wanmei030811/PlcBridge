// S7 协议插件（基于 github.com/robinson/gos7，ISO-on-TCP / port 102）。
//
// 实现要点（Plan.md §6）：
//   - 单 Channel 单连接；由 protocol.ChannelRegistry 复用给多个 Source / Heartbeat
//   - Read / BatchRead：地址解析 → 区域（DB/M/I/Q/T/C）+ 偏移 → 打包批量读
//   - Write / BatchWrite：地址解析 → 批量写（DB 区按字节、位用 RMW）
//   - 心跳：复用同一连接翻转 DB99.DBX0.0（§6.3.3）
//   - 协议层硬约束（§6.2.1）：单 PLC 并发 1~16，由 ChannelRegistry 引用计数 + 并发上限保证
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

	"github.com/plcbridge/plcbridge/internal/infra/logging"
	"github.com/plcbridge/plcbridge/internal/platform"
	gos7 "github.com/robinson/gos7"
)

// s7Plugin 是 S7 协议的 ProtocolPlugin 实现。
type s7Plugin struct{}

func newS7Plugin() ProtocolPlugin { return &s7Plugin{} }

func (p *s7Plugin) Name() string    { return "s7" }
func (p *s7Plugin) Version() string { return "1.0.0" }

// ConfigSchema S7 协议 JSON Schema（驱动 Web UI）。
func (p *s7Plugin) ConfigSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"rack":  {"type": "integer", "default": 0, "minimum": 0, "maximum": 7},
			"slot":  {"type": "integer", "default": 1, "minimum": 0, "maximum": 31},
			"connection_timeout_ms": {"type": "integer", "default": 2000, "minimum": 100, "maximum": 10000},
			"connect_type": {"type": "integer", "default": 1, "minimum": 1, "maximum": 3,
				"description": "1=PG 2=OP 3=Basic"},
			"string_layout": {"type": "integer", "default": 4, "enum": [0, 1, 2, 4],
				"description": "STRING 字节头长度: 4=标准 S7 [2B maxLen][2B curLen][N chars], 2=短头 [2B curLen][N chars], 1=Hsl 等异型 [1B curLen][N chars], 0=无头 N 字节字符"}
		},
		"required": ["rack", "slot"]
	}`)
}

type S7Config struct {
	Rack                int `json:"rack"`
	Slot                int `json:"slot"`
	ConnectionTimeoutMs int `json:"connection_timeout_ms"`
	ConnectType         int `json:"connect_type"`
	// StringLayoutPtr STRING 头字节数指针（区分"未配置" vs "显式 0"）。
	//   nil         → 未配置（sentinel -1，stringHdrBytes 降级为 4 = 标准 S7 STRING，兼容老 config）
	//   *4 (或4)    → 标准 S7 STRING: [2B maxLen][2B curLen][N chars]
	//   *2 (或2)    → 短头:           [2B curLen][N chars]
	//   *1 (或1)    → Hsl 异型:       [1B curLen][N chars]
	//   *0 (或0)    → 无头:           [N chars]
	// 写入路径当前未实现 string（见 writeBytes），所以此字段仅影响读取。
	// 暴露 Pointer 是为了 web 层能区分"未设置"和"显式 0"，前端 el-select placeholder 残留值也走 -1 sentinel。
	StringLayoutPtr *int `json:"string_layout,omitempty"`
}

func (p *s7Plugin) ParseConfig(raw []byte) (any, error) {
	if len(raw) == 0 {
		return &S7Config{Rack: 0, Slot: 1, ConnectionTimeoutMs: 2000, ConnectType: 1, StringLayoutPtr: nil}, nil
	}
	var cfg S7Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("S7 配置解析失败: %w", err)
	}
	if cfg.Rack < 0 || cfg.Rack > 7 {
		return nil, fmt.Errorf("S7 rack 越界: %d", cfg.Rack)
	}
	if cfg.Slot < 0 || cfg.Slot > 31 {
		return nil, fmt.Errorf("S7 slot 越界: %d", cfg.Slot)
	}
	if cfg.ConnectionTimeoutMs == 0 {
		cfg.ConnectionTimeoutMs = 2000
	}
	if cfg.ConnectType == 0 {
		cfg.ConnectType = 1 // PG
	}
	// StringLayoutPtr：nil（未配置）= 默认 4；非 nil 必须 ∈ {0,1,2,4}；-1 也视作未配置（前端 placeholder 残留）
	if cfg.StringLayoutPtr != nil {
		switch *cfg.StringLayoutPtr {
		case 0, 1, 2, 4:
			// ok
		case -1:
			cfg.StringLayoutPtr = nil
		default:
			return nil, fmt.Errorf("S7 string_layout 非法: %d（仅允许 0/1/2/4）", *cfg.StringLayoutPtr)
		}
	}
	return &cfg, nil
}

func (p *s7Plugin) Init(_ context.Context) error { return nil }
func (p *s7Plugin) Close() error                { return nil }

func (p *s7Plugin) CreateChannel(ch *platform.Channel) (ProtocolChannel, error) {
	parsed, err := p.ParseConfig(ch.Config)
	if err != nil {
		return nil, err
	}
	cfg, ok := parsed.(*S7Config)
	if !ok {
		return nil, fmt.Errorf("S7 CreateChannel: ParseConfig 返回类型 %T", parsed)
	}
	return newS7Channel(ch.Address, ch.ChannelBizID, cfg), nil
}

// === s7Channel ===

type s7Channel struct {
	addr      string
	channelID string
	cfg       *S7Config

	client   gos7.Client
	handler  *gos7.TCPClientHandler
	mu       sync.Mutex
	closed   atomic.Bool
	lastUsed atomic.Int64
	lastErr  atomic.Pointer[string]
	// lastErrAt 记录 lastErr 最近一次非空的时间戳（unix ns）。
	// 用途：registry 探活用 lastErrAt 而非 lastUsed，因为 lastUsed 即使在读失败
	// 时也会被 touch() 更新（readSpanBytes 入口处先 touch），无法区分"刚成功"和"刚失败"。
	lastErrAt atomic.Int64

	// PR2：详细统计（web 仪表盘用）
	readCount   atomic.Uint64 // 累计 BatchRead/Read 成功次数（按 batch 计数，非按 span）
	writeCount  atomic.Uint64 // 累计 BatchWrite/Write 成功次数
	errorCount  atomic.Uint64 // 累计 Read/Write/ProbeActive/Reconnect 失败次数
	lastReadOkMs  atomic.Int64 // 最近一次读成功 unix ms
	lastWriteOkMs atomic.Int64 // 最近一次写成功 unix ms
	lastErrorMs   atomic.Int64 // 最近一次失败 unix ms（与 lastErrAt 不同：lastErrAt = nanos, this = ms）
}

func newS7Channel(addr, channelID string, cfg *S7Config) *s7Channel {
	return &s7Channel{
		addr:      addr,
		channelID: channelID,
		cfg:       cfg,
	}
}

// Connect 建立 S7 连接（TCP + ISO + S7 握手）。
// 幂等：若已有 client，直接返回 nil。
func (c *s7Channel) Connect(ctx context.Context) error {
	if c.closed.Load() {
		return fmt.Errorf("S7 channel %s 已关闭", c.channelID)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.client != nil {
		return nil // 幂等
	}
	return c.connectLocked(ctx)
}

// Reconnect 强制重建 S7 连接：关闭现有 handler 后从头建立。
// 用于：registry 探测到 LastError 持续非空，怀疑底层 socket 已死。
// 与 Connect 不同：本方法会无脑替换 handler（即使 client != nil）。
func (c *s7Channel) Reconnect(ctx context.Context) error {
	if c.closed.Load() {
		return fmt.Errorf("S7 channel %s 已关闭", c.channelID)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// 关键：不要提前把 c.handler / c.client 置 nil。
	// 因为 connectLocked 在失败时不会恢复这两个字段，
	// 提前置 nil 会导致 connectLocked 失败后 cli 处于"client=nil / handler=nil"的
	// 永久死状态，后续 ProbeActive 只能返回误导性的"S7 未连接"，而无法分辨是
	// "PLC 关停"还是"重连中"。保留旧 client/handler，让失败时仍能：
	//   1. 用 setErr 记录准确的 dial 错误（不是"未连接"）
	//   2. 下次 read/probe 直接用旧 handler（虽会失败但错误更明确）
	// 成功后再统一关闭旧 handler。
	oldHandler := c.handler
	oldClient := c.client
	err := c.connectLocked(ctx)
	if err != nil {
		// 重连失败：c.handler/c.client 仍为旧值（connectLocked 在失败时未修改），
		// 或若旧值为 nil 则仍为 nil（这是 acquire 后的初始 Reconnect，无旧可保）。
		// 旧 handler 也不在这里关闭（已经被 PLC 主动 RST，无须再 Close）。
		_ = oldClient
		c.errorCount.Add(1)
		c.lastErrorMs.Store(time.Now().UnixMilli())
		return err
	}
	// 重连成功：connectLocked 已写入新的 c.handler/c.client。关闭旧的。
	if oldHandler != nil {
		_ = oldHandler.Close()
	}
	return nil
}

// connectLocked 在 c.mu 持有时执行实际连接建立。
// 成功返回 nil，失败返回错误（同时 setErr 让 Snapshot 可见）。
func (c *s7Channel) connectLocked(ctx context.Context) error {
	timeout := time.Duration(c.cfg.ConnectionTimeoutMs) * time.Millisecond
	handler := gos7.NewTCPClientHandlerWithConnectType(c.addr, c.cfg.Rack, c.cfg.Slot, c.cfg.ConnectType)
	handler.Timeout = timeout
	handler.IdleTimeout = 2 * timeout

	if err := handler.ConnectContext(ctx); err != nil {
		c.setErr(err)
		return fmt.Errorf("S7 %s 连接失败: %w", c.addr, err)
	}

	c.handler = handler
	c.client = gos7.NewClient(handler)
	c.touch()
	c.setErr(nil) // 连接成功，清空最近错误
	return nil
}

func (c *s7Channel) Disconnect() error {
	if c.closed.Load() {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.handler == nil {
		return nil
	}
	err := c.handler.Close()
	c.client = nil
	c.handler = nil
	return err
}

func (c *s7Channel) HealthCheck(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.client == nil {
		return fmt.Errorf("S7 channel %s 未连接", c.addr)
	}
	c.touch()
	return nil
}

func (c *s7Channel) Read(ctx context.Context, address string, dataType string) (Value, error) {
	area, db, byteOff, bitOff, err := parseS7Address(address)
	if err != nil {
		return Value{}, err
	}
	bytes, _, err := c.readBytes(ctx, area, db, byteOff, 1, bitOff)
	if err != nil {
		c.errorCount.Add(1)
		c.lastErrorMs.Store(time.Now().UnixMilli())
		return Value{}, err
	}
	c.readCount.Add(1)
	c.lastReadOkMs.Store(time.Now().UnixMilli())
	v, err := decodeS7Value(dataType, bytes, bitOff)
	if err != nil {
		c.errorCount.Add(1)
		c.lastErrorMs.Store(time.Now().UnixMilli())
	}
	return v, err
}

func (c *s7Channel) BatchRead(ctx context.Context, points []*platform.Point) (map[string]Value, error) {
	if len(points) == 0 {
		return map[string]Value{}, nil
	}

	result := make(map[string]Value, len(points))

	// §6.1：按 [area + db + 数据类型 + 连续性] 分组打包。
	parsed := make([]batchItem, 0, len(points))
	for _, p := range points {
		if !p.CollectEnabled {
			continue
		}
		a, db, off, bit, err := parseS7Address(p.Address)
		if err != nil {
			return nil, fmt.Errorf("点位 %s 地址解析失败: %w", p.PointBizID, err)
		}
		parsed = append(parsed, batchItem{point: p, area: a, db: db, byteOff: off, bitOff: bit})
	}
	if len(parsed) == 0 {
		return result, nil
	}

	// 分组：[area][db][dataType][stringLength][stringLayout] → []batchItem
	// string 必须按 StringLength 拆组：byteSize = hdrBytes+len，混了不同长度会让 span 大小计算错。
	// stringLayout 也要拆组：异型 header 字节数不同，合并后 decode 时无法用同一 hdr。
	groups := map[string][]batchItem{}
	for _, it := range parsed {
		k := string(rune(byte(it.area))) + "|" + strconv.Itoa(it.db) + "|" + it.point.DataType
		if it.point.DataType == "string" {
			n := it.point.StringLength
			if n <= 0 {
				n = 64
			}
			k += "|s" + strconv.Itoa(n) + "|h" + strconv.Itoa(stringHdrBytes(c.cfg.StringLayoutPtr))
		}
		groups[k] = append(groups[k], it)
	}

	for _, items := range groups {
		if err := c.batchReadGroup(ctx, items, result); err != nil {
			c.errorCount.Add(1)
			c.lastErrorMs.Store(time.Now().UnixMilli())
			return nil, err
		}
	}
	// 成功：BatchRead 完成（按 batch 计数，与 BatchRead 入口对齐）
	c.readCount.Add(1)
	c.lastReadOkMs.Store(time.Now().UnixMilli())
	return result, nil
}

// batchReadGroup 处理一组同 area+db+dataType+stringLayout 的点位，按字节连续性合并为 span。
func (c *s7Channel) batchReadGroup(ctx context.Context, items []batchItem, result map[string]Value) error {
	if len(items) == 0 {
		return nil
	}
	area, db, dataType := items[0].area, items[0].db, items[0].point.DataType
	hdrBytes := stringHdrBytes(c.cfg.StringLayoutPtr)

	// 按 byteOff 排序
	sorted := make([]batchItem, len(items))
	copy(sorted, items)
	sortItemsByOffset(sorted)

	// 打包：连续字节合一次读
	type span struct {
		start   int
		size    int
		members []int // 索引到 sorted
	}
	var spans []span
	for i, it := range sorted {
		nChars := 0
		if it.point != nil && it.point.DataType == "string" {
			nChars = it.point.StringLength
		}
		byteSize := dataTypeByteSize(dataType, nChars, hdrBytes)
		if byteSize <= 0 {
			byteSize = 1
		}
		if dataType == "bool" {
			byteSize = 1
		}

		if n := len(spans); n > 0 {
			last := &spans[n-1]
			if dataType == "bool" {
				// 同字节多 bit 任意合并
				if last.start == it.byteOff {
					last.members = append(last.members, i)
					continue
				}
				// 跨字节仅当上一个 span 末尾 byte 的"下一字节"是当前 byteOff，且当前 bitOff=0
				if it.bitOff == 0 && it.byteOff == last.start+last.size {
					last.size++ // 区间扩张一字节
					last.members = append(last.members, i)
					continue
				}
			} else {
				if last.start+last.size >= it.byteOff && last.start+last.size <= it.byteOff+byteSize {
					newEnd := it.byteOff + byteSize
					if newEnd > last.start+last.size {
						last.size = newEnd - last.start
					}
					last.members = append(last.members, i)
					continue
				}
			}
		}
		spans = append(spans, span{start: it.byteOff, size: byteSize, members: []int{i}})
	}

	for _, sp := range spans {
		buf, err := c.readSpanBytes(ctx, area, db, sp.start, sp.size)
		if err != nil {
			return fmt.Errorf("S7 读失败 [area=%d db=%d start=%d size=%d]: %w", area, db, sp.start, sp.size, err)
		}
		for _, idx := range sp.members {
			it := sorted[idx]
			v, err := decodeS7ValueSpan(dataType, buf, sp.start, it.byteOff, it.bitOff, hdrBytes)
			if err != nil {
				return fmt.Errorf("点位 %s 解码失败: %w", it.point.PointBizID, err)
			}
			result[it.point.PointBizID] = v
		}
	}
	return nil
}

// readSpanBytes 读取 [start, start+size) 区间的字节。
func (c *s7Channel) readSpanBytes(ctx context.Context, area s7Area, db, start, size int) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.checkCtx(ctx); err != nil {
		return nil, err
	}
	if c.client == nil {
		return nil, errors.New("S7 未连接")
	}
	c.touch()
	buf := make([]byte, size)
	var rerr error
	switch area {
	case areaDB:
		rerr = c.client.AGReadDB(db, start, size, buf)
	case areaMB:
		rerr = c.client.AGReadMB(start, size, buf)
	case areaIB:
		rerr = c.client.AGReadEB(start, size, buf)
	case areaQB:
		rerr = c.client.AGReadAB(start, size, buf)
	default:
		return nil, fmt.Errorf("批量读不支持 area=%d", area)
	}
	if rerr != nil {
		// 读失败（PLC 关闭 / 网络断 / 帧错误）→ 记录 lastErr 让 Snapshot 立即看到 OFF。
		// 注意：这里**不断开 socket**——socket 关闭会破坏 gos7 内部状态，
		// 而且下次 collect 周期（10s breaker）再来时，同一个 cli 仍可用。
		// 连不上的根本问题由 registry.Acquire 在新一次 connect 失败时处理。
		c.setErr(rerr)
		return nil, rerr
	}
	// 读成功 → 清 lastErr，让状态立刻恢复 ON（无需等 ProbeActive）
	c.setErr(nil)
	return buf, nil
}

// readBytes 读 1 字节（用于 Read 单点）。
func (c *s7Channel) readBytes(ctx context.Context, area s7Area, db, start, size int, bitOff int) ([]byte, int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.checkCtx(ctx); err != nil {
		return nil, 0, err
	}
	if c.client == nil {
		return nil, 0, errors.New("S7 未连接")
	}
	c.touch()
	buf := make([]byte, 1)
	var rerr error
	switch area {
	case areaDB:
		rerr = c.client.AGReadDB(db, start, 1, buf)
	case areaMB:
		rerr = c.client.AGReadMB(start, 1, buf)
	case areaIB:
		rerr = c.client.AGReadEB(start, 1, buf)
	case areaQB:
		rerr = c.client.AGReadAB(start, 1, buf)
	default:
		return nil, 0, fmt.Errorf("S7 Read 不支持 area=%d", area)
	}
	if rerr != nil {
		c.setErr(rerr)
		return nil, 0, rerr
	}
	c.setErr(nil)
	return buf, bitOff, nil
}

func (c *s7Channel) Write(ctx context.Context, address string, value any, dataType string) error {
	area, db, byteOff, bitOff, err := parseS7Address(address)
	if err != nil {
		return err
	}
	return c.writeBytes(ctx, area, db, byteOff, bitOff, dataType, value)
}

func (c *s7Channel) BatchWrite(ctx context.Context, writes []WriteOp) error {
	for _, w := range writes {
		if err := c.Write(ctx, w.Address, w.Value, w.DataType); err != nil {
			return err
		}
	}
	// 成功：BatchWrite 完成（按 batch 计数）
	c.writeCount.Add(1)
	c.lastWriteOkMs.Store(time.Now().UnixMilli())
	return nil
}

func (c *s7Channel) writeBytes(ctx context.Context, area s7Area, db, byteOff, bitOff int, dataType string, value any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.checkCtx(ctx); err != nil {
		return err
	}
	if c.client == nil {
		return errors.New("S7 未连接")
	}
	c.touch()
	if area != areaDB {
		return fmt.Errorf("S7 Write 暂仅支持 DB 区")
	}

	switch dataType {
	case "bool":
		v, ok := value.(bool)
		if !ok {
			if b, ok2 := toBool(value); ok2 {
				v = b
			} else {
				return fmt.Errorf("bool 写入值类型错误: %T", value)
			}
		}
		// gos7 不直接提供位写，走 Read-Modify-Write 字节
		buf := make([]byte, 1)
		if err := c.client.AGReadDB(db, byteOff, 1, buf); err != nil {
			return c.markBrokenRead(err)
		}
		mask := byte(1 << uint(bitOff))
		if v {
			buf[0] |= mask
		} else {
			buf[0] &^= mask
		}
		if err := c.client.AGWriteDB(db, byteOff, 1, buf); err != nil {
			return c.markBrokenRead(err)
		}
		return nil
	case "int16", "uint16":
		v, err := toInt64(value)
		if err != nil {
			return fmt.Errorf("int16/uint16 写入值类型错误: %w", err)
		}
		buf := make([]byte, 2)
		if dataType == "uint16" {
			binary.BigEndian.PutUint16(buf, uint16(v))
		} else {
			binary.BigEndian.PutUint16(buf, uint16(int16(v)))
		}
		if err := c.client.AGWriteDB(db, byteOff, 2, buf); err != nil {
			return c.markBrokenRead(err)
		}
		return nil
	case "int32", "uint32", "float32":
		buf := make([]byte, 4)
		switch dataType {
		case "int32":
			v, err := toInt64(value)
			if err != nil {
				return err
			}
			binary.BigEndian.PutUint32(buf, uint32(int32(v)))
		case "uint32":
			v, err := toInt64(value)
			if err != nil {
				return err
			}
			binary.BigEndian.PutUint32(buf, uint32(v))
		case "float32":
			f, ok := value.(float64)
			if !ok {
				if f32, ok2 := value.(float32); ok2 {
					f = float64(f32)
				} else {
					return fmt.Errorf("float32 写入值类型错误: %T", value)
				}
			}
			binary.BigEndian.PutUint32(buf, math.Float32bits(float32(f)))
		}
		if err := c.client.AGWriteDB(db, byteOff, 4, buf); err != nil {
			return c.markBrokenRead(err)
		}
		return nil
	case "int64", "uint64", "float64":
		buf := make([]byte, 8)
		switch dataType {
		case "int64":
			v, err := toInt64(value)
			if err != nil {
				return err
			}
			binary.BigEndian.PutUint64(buf, uint64(v))
		case "uint64":
			v, err := toInt64(value)
			if err != nil {
				return err
			}
			binary.BigEndian.PutUint64(buf, uint64(v))
		case "float64":
			f, ok := value.(float64)
			if !ok {
				return fmt.Errorf("float64 写入值类型错误: %T", value)
			}
			binary.BigEndian.PutUint64(buf, math.Float64bits(f))
		}
		if err := c.client.AGWriteDB(db, byteOff, 8, buf); err != nil {
			return c.markBrokenRead(err)
		}
		return nil
	default:
		return fmt.Errorf("S7 Write 不支持的数据类型: %s", dataType)
	}
}

func (c *s7Channel) checkCtx(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

func (c *s7Channel) touch() {
	c.lastUsed.Store(time.Now().UnixNano())
}

func (c *s7Channel) LastUsedAt() time.Time {
	return time.Unix(0, c.lastUsed.Load())
}

// IsConnected 返回本地视图的连接状态（基于是否持有 client）。
// 真实链路健康需调用 HealthCheck，但 IsConnected 用于快速判断"是否被 connect 过 / 还没被 disconnect"。
func (c *s7Channel) IsConnected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.client != nil && !c.closed.Load()
}

// LastActiveAt 等价 LastUsedAt（命名对齐 ProtocolChannel 接口）。
func (c *s7Channel) LastActiveAt() time.Time { return c.LastUsedAt() }

// LastError 返回最近一次失败的错误信息。
func (c *s7Channel) LastError() string {
	if p := c.lastErr.Load(); p != nil {
		return *p
	}
	return ""
}

// ProbeActive 主动探活：读 1 字节 MB0（几乎所有 S7 PLC 都支持）。
//   - 成功：touch + 清空 lastErr
//   - 失败：setErr
// 注意：ProbeActive 不抢 mu 长时间，仅作短暂探活；调用方应节流。
func (c *s7Channel) ProbeActive(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.client == nil {
		err := fmt.Errorf("S7 channel %s 未连接", c.channelID)
		c.setErr(err)
		c.errorCount.Add(1)
		c.lastErrorMs.Store(time.Now().UnixMilli())
		return err
	}
	buf := make([]byte, 1)
	if err := c.client.AGReadMB(0, 1, buf); err != nil {
		c.setErr(err)
		c.errorCount.Add(1)
		c.lastErrorMs.Store(time.Now().UnixMilli())
		return err
	}
	c.touch()
	c.setErr(nil)
	return nil
}

// setErr 原子地记录最近一次错误（写操作失败时调用）。
func (c *s7Channel) setErr(err error) {
	if err == nil {
		c.lastErr.Store(nil)
		c.lastErrAt.Store(0)
		return
	}
	msg := err.Error()
	c.lastErr.Store(&msg)
	c.lastErrAt.Store(time.Now().UnixNano())
}

// LastErrorAt 返回最近一次错误（非空）的时间戳；从未出错返回 time.Time{}。
func (c *s7Channel) LastErrorAt() time.Time {
	ns := c.lastErrAt.Load()
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns)
}

// Stats 返回 S7 协议特定运行时数据（ProtocolChannel.Stats 文档约束）。
//
// 字段约定：
//   - "connection_state": "Connected" / "Disconnected"（基于本地视图；底层 handler 死时为 "Disconnected"）
//   - "address" / "rack" / "slot" / "connect_type" / "string_layout": 静态配置
//   - "read_count" / "write_count" / "error_count": 累计操作计数（PR2）
//   - "last_read_ok_ms" / "last_write_ok_ms" / "last_error_ms": 最近操作时间（PR2）
//   - "last_probe_ok_ms": 最近一次 ProbeActive 成功 unix ms
func (c *s7Channel) Stats() map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	connected := c.client != nil && !c.closed.Load()
	state := "Disconnected"
	if connected {
		state = "Connected"
	}
	probeMs := int64(0)
	if lastUsedNs := c.lastUsed.Load(); lastUsedNs != 0 {
		probeMs = time.Unix(0, lastUsedNs).UnixMilli()
	}
	return map[string]any{
		"connection_state":  state,
		"address":           c.addr,
		"rack":              c.cfg.Rack,
		"slot":              c.cfg.Slot,
		"connect_type":      c.cfg.ConnectType,
		"string_layout":     stringHdrBytes(c.cfg.StringLayoutPtr),
		"read_count":        c.readCount.Load(),
		"write_count":       c.writeCount.Load(),
		"error_count":       c.errorCount.Load(),
		"last_read_ok_ms":   c.lastReadOkMs.Load(),
		"last_write_ok_ms":  c.lastWriteOkMs.Load(),
		"last_error_ms":     c.lastErrorMs.Load(),
		"last_probe_ok_ms":  probeMs,
	}
}

// markBrokenRead 读失败时调用：记录错误。
//
// 历史版本：曾经"关闭 socket + 置 client=nil"以让 IsConnected() 立即返回 false。
// 但实测发现：
//   1. 主动 Close 破坏 gos7 内部状态，导致后续 reconnect 报 "is null" 错误
//   2. registry 缓存的 cli 句柄被多个 worker 复用，关闭会误伤其他在跑的读
//   3. 即使 cli 已 broken，下次 BatchRead 仍会失败 → 旧的"自然"链路就足够诊断
// 现在改为：仅记 lastErr，由 Snapshot() 的 `cli.LastError() == ""` 检查决定是否显示 OFF。
// 这同时满足"快速 OFF"诉求，且不会破坏连接生命周期。
func (c *s7Channel) markBrokenRead(err error) error {
	c.setErr(err)
	c.errorCount.Add(1)
	c.lastErrorMs.Store(time.Now().UnixMilli())
	return err
}

// tryReconnectLockHeld 在 c.mu 持有时调用：若 client==nil，尝试重连。
// 重连成功返回 nil；失败返回错误（不调 markBrokenRead，避免覆盖当前 lastErr）。
func (c *s7Channel) tryReconnectLockHeld(ctx context.Context) error {
	if c.client != nil {
		return nil
	}
	if c.closed.Load() {
		return fmt.Errorf("S7 channel %s 已关闭", c.channelID)
	}
	// 用与 Connect 相同的方式重建（直接走 Connect 的同款路径）
	timeout := time.Duration(c.cfg.ConnectionTimeoutMs) * time.Millisecond
	handler := gos7.NewTCPClientHandlerWithConnectType(c.addr, c.cfg.Rack, c.cfg.Slot, c.cfg.ConnectType)
	handler.Timeout = timeout
	handler.IdleTimeout = 2 * timeout
	if err := handler.ConnectContext(ctx); err != nil {
		return fmt.Errorf("S7 重连失败: %w", err)
	}
	c.handler = handler
	c.client = gos7.NewClient(handler)
	c.touch()
	c.setErr(nil)
	logging.Info(logging.EventChannelEstablished).
		Str("channel", c.channelID).
		Msg("S7 channel 已重连")
	return nil
}

// === 地址解析与解码辅助 ===

type s7Area byte

type batchItem struct {
	point   *platform.Point
	area    s7Area
	db      int
	byteOff int
	bitOff  int
}

const (
	areaDB  s7Area = 1
	areaIB  s7Area = 2
	areaQB  s7Area = 3
	areaMB  s7Area = 4
	areaTM  s7Area = 5
	areaCT  s7Area = 6
)

// parseS7Address 解析 S7 地址 → (area, db, byteOff, bitOff)
//
// 支持格式：
//   DB1.DBB0 / DB1.DBW10 / DB1.DBD100 / DB1.DBX0.0 / DB1.DBX0.7
//   MB0 / MW10 / MD100 / MX0.0
//   IB0 / IW10 / ID100 / IX0.0
//   QB0 / QW10 / QD100 / QX0.0
//   M100  -> MB100 (西门子常用简写)
func parseS7Address(addr string) (s7Area, int, int, int, error) {
	a := strings.ToUpper(strings.TrimSpace(addr))
	if a == "" {
		return 0, 0, 0, 0, fmt.Errorf("空地址")
	}

	// DB 区
	if strings.HasPrefix(a, "DB") {
		// 形如 DB1.DBX0.0 / DB1.DBD100
		parts := strings.SplitN(a, ".", 3)
		if len(parts) < 2 {
			return 0, 0, 0, 0, fmt.Errorf("DB 地址格式错误: %s", addr)
		}
		dbStr := strings.TrimPrefix(parts[0], "DB")
		db, err := strconv.Atoi(dbStr)
		if err != nil {
			return 0, 0, 0, 0, fmt.Errorf("DB 号解析失败: %s", addr)
		}
		rest := parts[1]
		if len(parts) == 3 {
			rest = parts[1] + "." + parts[2]
		}
		return parseS7AreaRest(areaDB, db, rest)
	}

	// M / I / Q 区
	prefix := a[:1]
	rest := a[1:]
	db := 0
	switch prefix {
	case "M":
		return parseS7AreaRest(areaMB, db, rest)
	case "I", "E":
		return parseS7AreaRest(areaIB, db, rest)
	case "Q", "A":
		return parseS7AreaRest(areaQB, db, rest)
	}

	// 西门子简写：M100 / MW100 / M100.0
	return 0, 0, 0, 0, fmt.Errorf("无法识别地址: %s", addr)
}

func parseS7AreaRest(area s7Area, db int, rest string) (s7Area, int, int, int, error) {
	rest = strings.TrimSpace(rest)
	switch {
	case strings.HasPrefix(rest, "DBX"):
		num, bit, err := parseBitAddr(strings.TrimPrefix(rest, "DBX"))
		if err != nil {
			return 0, 0, 0, 0, err
		}
		return area, db, num, bit, nil
	case strings.HasPrefix(rest, "DBB"):
		off, err := strconv.Atoi(strings.TrimPrefix(rest, "DBB"))
		if err != nil {
			return 0, 0, 0, 0, err
		}
		return area, db, off, 0, nil
	case strings.HasPrefix(rest, "DBW"):
		off, err := strconv.Atoi(strings.TrimPrefix(rest, "DBW"))
		if err != nil {
			return 0, 0, 0, 0, err
		}
		return area, db, off, 0, nil
	case strings.HasPrefix(rest, "DBD"):
		off, err := strconv.Atoi(strings.TrimPrefix(rest, "DBD"))
		if err != nil {
			return 0, 0, 0, 0, err
		}
		return area, db, off, 0, nil
	case strings.HasPrefix(rest, "X"):
		num, bit, err := parseBitAddr(strings.TrimPrefix(rest, "X"))
		if err != nil {
			return 0, 0, 0, 0, err
		}
		return area, db, num, bit, nil
	case strings.HasPrefix(rest, "B"):
		off, err := strconv.Atoi(strings.TrimPrefix(rest, "B"))
		if err != nil {
			return 0, 0, 0, 0, err
		}
		return area, db, off, 0, nil
	case strings.HasPrefix(rest, "W"):
		off, err := strconv.Atoi(strings.TrimPrefix(rest, "W"))
		if err != nil {
			return 0, 0, 0, 0, err
		}
		return area, db, off, 0, nil
	case strings.HasPrefix(rest, "D"):
		off, err := strconv.Atoi(strings.TrimPrefix(rest, "D"))
		if err != nil {
			return 0, 0, 0, 0, err
		}
		return area, db, off, 0, nil
	}

	// 简写 M100 -> MB100
	if off, err := strconv.Atoi(rest); err == nil {
		return area, db, off, 0, nil
	}
	return 0, 0, 0, 0, fmt.Errorf("地址段解析失败: %s", rest)
}

func parseBitAddr(s string) (int, int, error) {
	parts := strings.SplitN(s, ".", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("位地址格式错误: %s", s)
	}
	num, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, err
	}
	bit, err := strconv.Atoi(parts[1])
	if err != nil || bit < 0 || bit > 7 {
		return 0, 0, fmt.Errorf("位偏移非法: %s", s)
	}
	return num, bit, nil
}

// stringHdrBytes 把 S7Config.StringLayoutPtr 规整为 {0,1,2,4}。
// nil（未配置）降级为 4（标准 S7 STRING，向后兼容老 config）。
// *int 值为 -1（前端 placeholder 残留）也降级为 4。
// 非法值（>4）降级为 4。
// 用户显式配置 *0（无头裸字节）走 case 0 分支，不受影响。
func stringHdrBytes(layout *int) int {
	if layout == nil {
		return 4
	}
	switch *layout {
	case -1:
		return 4
	case 0, 1, 2, 4:
		return *layout
	default:
		return 4
	}
}

// dataTypeByteSize 返回数据类型字节数（bool 计 1 字节；string = hdrBytes 字节头 + nChars 字节）。
//
// S7 标准 STRING layout：[2字节 maxLen][2字节 curLen][N 字节字符] → 总长 4+N。
// 异型 STRING layout 见 S7Config.StringLayout 说明（hdrBytes ∈ {0,1,2,4}）。
// string 走 nChars>0；nChars<=0 视为"未配置"，由调用方打点"未配置 string_length"并回退到 64 字节。
func dataTypeByteSize(dt string, nChars int, hdrBytes int) int {
	switch dt {
	case "bool":
		return 1
	case "int16", "uint16":
		return 2
	case "int32", "uint32", "float32":
		return 4
	case "int64", "uint64", "float64":
		return 8
	case "string":
		n := nChars
		if n <= 0 {
			n = 64 // 回退值；调用方会 warn 一次
		}
		h := hdrBytes
		if h < 0 {
			h = 4
		}
		if h > 4 {
			h = 4 // 防御性 cap
		}
		return h + n
	default:
		return 1
	}
}

// decodeS7Value 从 1 字节 buf 解码单值。
// hdrBytes 仅对 string 类型生效；其他类型忽略。
// s7Channel.Read 当前不会被 string 调用（采集走 BatchRead，写入未实现 string），所以默认 4 即可。
func decodeS7Value(dataType string, buf []byte, bitOff int) (Value, error) {
	return decodeS7ValueSpan(dataType, buf, 0, 0, bitOff, 4)
}

// boolToF 把 bool 规范成 0/1。保证 Value.Float64 对所有数值型都可用。
func boolToF(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// decodeS7ValueSpan 从跨 span 读取的 buf 解码 point（point 在 span 内偏移 spanStart）。
// hdrBytes：string 类型的字节头长度（0/1/2/4，默认 4 = 标准 S7 STRING），其他类型忽略。
func decodeS7ValueSpan(dataType string, buf []byte, spanStart, pointStart, bitOff, hdrBytes int) (Value, error) {
	if len(buf) == 0 {
		return Value{}, errors.New("读缓冲为空")
	}
	off := pointStart - spanStart
	if off < 0 || off >= len(buf) {
		return Value{}, fmt.Errorf("点位偏移 %d 超出 span [%d,%d)", pointStart, spanStart, spanStart+len(buf))
	}
	switch dataType {
	case "bool":
		v := (buf[off]>>uint(bitOff))&1 == 1
		return Value{DataType: dataType, Bool: v, Float64: boolToF(v), Quality: "good"}, nil
	case "int8":
		v := int64(int8(buf[off]))
		return Value{DataType: dataType, Int64: v, Float64: float64(v), Quality: "good"}, nil
	case "uint8":
		v := uint64(buf[off])
		return Value{DataType: dataType, Uint64: v, Float64: float64(v), Quality: "good"}, nil
	case "int16":
		u := binary.BigEndian.Uint16(buf[off : off+2])
		v := int64(int16(u))
		return Value{DataType: dataType, Int64: v, Float64: float64(v), Quality: "good"}, nil
	case "uint16":
		u := binary.BigEndian.Uint16(buf[off : off+2])
		v := uint64(u)
		return Value{DataType: dataType, Uint64: v, Float64: float64(v), Quality: "good"}, nil
	case "int32":
		u := binary.BigEndian.Uint32(buf[off : off+4])
		v := int64(int32(u))
		return Value{DataType: dataType, Int64: v, Float64: float64(v), Quality: "good"}, nil
	case "uint32":
		u := binary.BigEndian.Uint32(buf[off : off+4])
		v := uint64(u)
		return Value{DataType: dataType, Uint64: v, Float64: float64(v), Quality: "good"}, nil
	case "float32":
		u := binary.BigEndian.Uint32(buf[off : off+4])
		return Value{DataType: dataType, Float64: float64(math.Float32frombits(u)), Quality: "good"}, nil
	case "int64":
		u := binary.BigEndian.Uint64(buf[off : off+8])
		v := int64(u)
		return Value{DataType: dataType, Int64: v, Float64: float64(v), Quality: "good"}, nil
	case "uint64":
		u := binary.BigEndian.Uint64(buf[off : off+8])
		v := u
		return Value{DataType: dataType, Uint64: v, Float64: float64(v), Quality: "good"}, nil
	case "float64":
		u := binary.BigEndian.Uint64(buf[off : off+8])
		return Value{DataType: dataType, Float64: math.Float64frombits(u), Quality: "good"}, nil
	case "string":
		// STRING 字节布局由 hdrBytes 决定（默认 4 = 标准 S7 STRING）：
		//   hdr=4: [2B maxLen][2B curLen][N chars]
		//   hdr=2: [2B curLen][N chars]
		//   hdr=1: [1B curLen][N chars]   (HslCommunication 等异型 PLC 模拟器)
		//   hdr=0: [N chars]              (无头裸字节)
		// buf 必须包含 hdrBytes+N 字节（由 dataTypeByteSize 保证）；off 已经是字符起点
		// （pointStart - spanStart 调整过）。
		h := hdrBytes
		if h < 0 {
			h = 4
		}
		if h > 4 {
			h = 4
		}
		if off+h > len(buf) {
			return Value{}, fmt.Errorf("string 头越界: off=%d hdr=%d len=%d", off, h, len(buf))
		}
		var curLen int
		switch h {
		case 4:
			// [2B maxLen][2B curLen][N chars]
			_ = int(binary.BigEndian.Uint16(buf[off : off+2])) // maxLen 暂不强制
			curLen = int(binary.BigEndian.Uint16(buf[off+2 : off+4]))
		case 2:
			// [2B curLen][N chars]
			curLen = int(binary.BigEndian.Uint16(buf[off : off+2]))
		case 1:
			// [1B curLen][N chars]（Hsl 等异型）
			curLen = int(buf[off])
		case 0:
			// 无头：curLen 取"point 配置的 nChars"（N），由调用方保证数据足够长
			curLen = len(buf) - (off + h)
		}
		if curLen > len(buf)-(off+h) {
			curLen = len(buf) - (off + h)
		}
		if curLen < 0 {
			curLen = 0
		}
		chars := buf[off+h : off+h+curLen]
		// 去掉尾部 NUL，避免 PLC 端未写满时的脏字符污染
		for len(chars) > 0 && chars[len(chars)-1] == 0 {
			chars = chars[:len(chars)-1]
		}
		return Value{DataType: dataType, String: string(chars), Quality: "good"}, nil
	default:
		v := uint64(buf[off])
		return Value{DataType: dataType, Uint64: v, Float64: float64(v), Quality: "good"}, nil
	}
}

func sortItemsByOffset(items []batchItem) {
	// 简单插入排序：点数一般 < 100，可接受
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && items[j].byteOff < items[j-1].byteOff; j-- {
			items[j], items[j-1] = items[j-1], items[j]
		}
	}
}

func toInt64(v any) (int64, error) {
	switch x := v.(type) {
	case int:
		return int64(x), nil
	case int32:
		return int64(x), nil
	case int64:
		return x, nil
	case uint:
		return int64(x), nil
	case uint32:
		return int64(x), nil
	case uint64:
		return int64(x), nil
	case float32:
		return int64(x), nil
	case float64:
		return int64(x), nil
	default:
		return 0, fmt.Errorf("无法转换为整数: %T", v)
	}
}

func toBool(v any) (bool, bool) {
	switch x := v.(type) {
	case bool:
		return x, true
	case int:
		return x != 0, true
	case int64:
		return x != 0, true
	case float64:
		return x != 0, true
	}
	return false, false
}
