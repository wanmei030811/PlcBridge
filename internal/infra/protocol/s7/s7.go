// S7 协议插件（基于 github.com/robinson/gos7，ISO-on/TCP / port 102）。
//
// 实现要点（Plan.md §6）：
//   - 单 Channel 单连接；由 protocol.ChannelRegistry 复用给多个 Source / Heartbeat
//   - Read / BatchRead：地址解析 → 区域（DB/M/I/Q/T/C）+ 偏移 → 打包批量读
//   - Write / BatchWrite：地址解析 → 批量写（DB 区按字节、位用 RMW）
//   - 心跳：复用同一连接翻转 DB99.DBX0.0（§6.3.3）
//   - 协议层硬约束（§6.2.1）：单 PLC 并发 1~16，由 ChannelRegistry 引用计数 + 并发上限保证
package s7

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
	"github.com/plcbridge/plcbridge/internal/infra/protocol"
	"github.com/plcbridge/plcbridge/internal/infra/protocol/charset"
	"github.com/plcbridge/plcbridge/internal/platform"
	gos7 "github.com/robinson/gos7"
)

// s7Plugin 是 S7 协议的 ProtocolPlugin 实现。
type s7Plugin struct{}

func newS7Plugin() protocol.ProtocolPlugin { return &s7Plugin{} }

func (p *s7Plugin) Name() string    { return "s7" }
func (p *s7Plugin) Version() string { return "1.0.0" }

// Capabilities S7 协议：必填 rack/slot；支持批量读 / 心跳（s7conn.go 内置 connect_type=3 Basic 周期性翻转）。
func (p *s7Plugin) Capabilities() protocol.Capability {
	return protocol.Capability{
		SupportsBatchRead: true,
		SupportsHeartbeat: true,
		RequiresRackSlot:  true,
	}
}

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
				"description": "STRING 字节头长度: 4=标准 S7 [2B maxLen][2B curLen][N chars], 2=短头 [2B curLen][N chars], 1=Hsl 等异型 [1B curLen][N chars], 0=无头 N 字节字符"},
			"string_encoding": {"type": "string", "default": "auto", "enum": ["auto", "utf-8", "gbk"],
				"description": "字符串解码编码: auto=自动探测(ASCII→UTF-8→GBK，推荐), utf-8=强制 UTF-8, gbk=强制 GBK/CP936(西门子中文环境常用)。PLC 侧中文若显示为乱码/问号，多半是这里需要显式指定 gbk"}
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
	//   *4 (或4)    → 标准 S7 STRING: [2B maxLen][2B curLen][N bytes]
	//   *2 (或2)    → 短头:           [1B maxLen][1B curLen][N bytes]  ← 西门子/Hsl 实际格式
	//   *1 (或1)    → 单字节头:       [1B curLen][N bytes]
	//   *0 (或0)    → 无头:           [N bytes]
	//
	// curLen 的单位一律是**字节数**（西门子 STRING 语义）。
	//
	// 读取与写入路径共用此布局定义（decodeS7ValueSpan / writeStringLocked），
	// 两侧严格对称 —— 这是"写进去读出来是乱码/半截"的主要根因所在。
	// 暴露 Pointer 是为了 web 层能区分"未设置"和"显式 0"，前端 el-select placeholder 残留值也走 -1 sentinel。
	StringLayoutPtr *int `json:"string_layout,omitempty"`
	// StringEncoding 字符串编码："" / "auto" = 自动探测（ASCII→UTF-8→GBK），
	// "utf-8" / "gbk" = 强制指定。
	//
	// 默认自动探测，因为 PLC 侧编码不统一（西门子中文环境多为 GBK/CP936，
	// 但也有 UTF-8 的）。自动探测对 ASCII/UTF-8 无歧义；
	// 只有"GBK 字节恰好是合法 UTF-8"这种极端情况会误判，此时用本字段强制指定。
	StringEncoding string `json:"string_encoding,omitempty"`
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
	// StringEncoding：空 = 自动探测；只接受已知编码名。
	// 未知值不报错（回退到自动探测）—— 一条脏配置不该让整条采集链路起不来。
	switch strings.ToLower(strings.TrimSpace(cfg.StringEncoding)) {
	case "", "auto", "utf-8", "utf8", "gbk", "cp936", "gb18030":
		// ok
	default:
		cfg.StringEncoding = "auto"
	}
	return &cfg, nil
}

func (p *s7Plugin) Init(_ context.Context) error { return nil }
func (p *s7Plugin) Close() error                 { return nil }

func (p *s7Plugin) CreateChannel(ch *platform.Channel) (protocol.ProtocolChannel, error) {
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

	// lastReconnectAt 记录最近一次**自动重连**尝试的时间（c.mu 保护）。
	//
	// 为什么需要（2026-10-25 修复）：
	// PLC 停机 → 读失败 → 连接被作废（invalidateDeadConnLocked）→ c.client == nil。
	// 此时若没有自动重连，采集会一直失败到"用户手动触发诊断"为止。
	// 而每次读失败都立刻重连会导致重连风暴（PLC 停机期间 1Hz 采集 = 1Hz 重连），
	// 所以用这个时间戳做节流。
	lastReconnectAt time.Time

	// PR2：详细统计（web 仪表盘用）
	readCount     atomic.Uint64 // 累计 BatchRead/Read 成功次数（按 batch 计数，非按 span）
	writeCount    atomic.Uint64 // 累计 BatchWrite/Write 成功次数
	errorCount    atomic.Uint64 // 累计 Read/Write/ProbeActive/Reconnect 失败次数
	lastReadOkMs  atomic.Int64  // 最近一次读成功 unix ms
	lastWriteOkMs atomic.Int64  // 最近一次写成功 unix ms
	lastErrorMs   atomic.Int64  // 最近一次失败 unix ms（与 lastErrAt 不同：lastErrAt = nanos, this = ms）
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
	oldHandler := c.handler
	oldClient := c.client
	err := c.connectLocked(ctx)
	if err != nil {
		_ = oldClient
		c.errorCount.Add(1)
		c.lastErrorMs.Store(time.Now().UnixMilli())
		return err
	}
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

func (c *s7Channel) Read(ctx context.Context, address string, dataType string) (protocol.Value, error) {
	area, db, byteOff, bitOff, err := parseS7Address(address)
	if err != nil {
		return protocol.Value{}, err
	}
	bytes, _, err := c.readBytes(ctx, area, db, byteOff, 1, bitOff)
	if err != nil {
		c.errorCount.Add(1)
		c.lastErrorMs.Store(time.Now().UnixMilli())
		return protocol.Value{}, err
	}
	c.readCount.Add(1)
	c.lastReadOkMs.Store(time.Now().UnixMilli())
	v, err := decodeS7Value(dataType, bytes, bitOff, c.cfg.StringEncoding)
	if err != nil {
		c.errorCount.Add(1)
		c.lastErrorMs.Store(time.Now().UnixMilli())
	}
	return v, err
}

func (c *s7Channel) BatchRead(ctx context.Context, points []*platform.Point) (map[string]protocol.Value, error) {
	if len(points) == 0 {
		return map[string]protocol.Value{}, nil
	}

	result := make(map[string]protocol.Value, len(points))

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
	c.readCount.Add(1)
	c.lastReadOkMs.Store(time.Now().UnixMilli())
	return result, nil
}

// batchReadGroup 处理一组同 area+db+dataType+stringLayout 的点位，按字节连续性合并为 span。
func (c *s7Channel) batchReadGroup(ctx context.Context, items []batchItem, result map[string]protocol.Value) error {
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
				if it.bitOff == 0 && it.byteOff == last.start+last.size {
					last.size++
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
			v, err := decodeS7ValueSpan(dataType, buf, sp.start, it.byteOff, it.bitOff, hdrBytes, c.cfg.StringEncoding)
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
	// 连接可能已被作废（对端断开）→ 先尝试惰性重连，再读。
	if err := c.ensureConnectedLocked(ctx); err != nil {
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
		// 关键：底层 socket 可能已被 PLC 关闭（RST / forcibly closed）。
		// 此时 c.client 仍非 nil，但已经永远发不出去了 —— 每次读都失败，
		// 而 registry 的 Reconnect 走的是"重建 handler"的路径，所以需要先把
		// 死掉的 handler/client 清掉（否则 Reconnect 里 connectLocked 成功后
		// 旧 handler 被 Close，新旧句柄状态混乱）。
		//
		// 不这么做的话：PLC 停机再启动后，采集永远失败、ConsecutiveErrors 一直涨，
		// 直到用户手动触发诊断（走 registry.Snapshot → ProbeActive）才可能恢复。
		c.invalidateDeadConnLocked(rerr)
		c.setErr(rerr)
		return nil, rerr
	}
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
	// 连接可能已被作废（对端断开）→ 先尝试惰性重连，再读。
	if err := c.ensureConnectedLocked(ctx); err != nil {
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
		c.invalidateDeadConnLocked(rerr)
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
	return c.writeBytes(ctx, area, db, byteOff, bitOff, dataType, value, 0)
}

// WriteStringLen 实现 protocol.StringLengthWriter。
//
// length 是内容区**字节数**上限（来自点位配置 platform.Point.StringLength）。
//
// 为什么必须有这个：
//   - S7 无头布局（string_layout=0）没有长度字段，读侧只能靠"尾部 NUL"判断结束。
//     写入时如果不知道内容区有多长，就无法正确填充 NUL，
//     PLC 侧会读到上一次的残留数据。
//   - 有头布局（1/2/4）虽然能从 PLC 侧读回 maxLen，但那只在 PLC 侧
//     **已经初始化过**该 STRING 时才有效（PLC 刚上电时头可能是垃圾值）。
//
// 所以优先用调用方给的 length（点位配置，最权威），
// 读回的 maxLen 只作为 length 缺失时的兜底。
func (c *s7Channel) WriteStringLen(ctx context.Context, address string, value any, dataType string, length int) error {
	area, db, byteOff, bitOff, err := parseS7Address(address)
	if err != nil {
		return err
	}
	if dataType != "string" {
		return fmt.Errorf("S7 WriteStringLen 仅支持 string 类型，收到 %s", dataType)
	}
	return c.writeBytes(ctx, area, db, byteOff, bitOff, dataType, value, length)
}

func (c *s7Channel) BatchWrite(ctx context.Context, writes []protocol.WriteOp) error {
	for _, w := range writes {
		if err := c.Write(ctx, w.Address, w.Value, w.DataType); err != nil {
			return err
		}
	}
	c.writeCount.Add(1)
	c.lastWriteOkMs.Store(time.Now().UnixMilli())
	return nil
}

func (c *s7Channel) writeBytes(ctx context.Context, area s7Area, db, byteOff, bitOff int, dataType string, value any, stringLen int) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.checkCtx(ctx); err != nil {
		return err
	}
	// 连接可能已被作废（对端断开）→ 先尝试惰性重连，再写。
	if err := c.ensureConnectedLocked(ctx); err != nil {
		return err
	}
	if c.client == nil {
		return errors.New("S7 未连接")
	}
	c.touch()

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
		if err := c.areaRead1(area, db, byteOff, buf); err != nil {
			return c.markBrokenRead(err)
		}
		mask := byte(1 << uint(bitOff))
		if v {
			buf[0] |= mask
		} else {
			buf[0] &^= mask
		}
		if err := c.areaWrite(area, db, byteOff, buf); err != nil {
			return c.markBrokenRead(err)
		}
		return nil
	case "int8", "uint8":
		v, err := toInt64(value)
		if err != nil {
			return fmt.Errorf("int8/uint8 写入值类型错误: %w", err)
		}
		buf := make([]byte, 1)
		if dataType == "uint8" {
			buf[0] = byte(v)
		} else {
			buf[0] = byte(int8(v))
		}
		if err := c.areaWrite(area, db, byteOff, buf); err != nil {
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
		if err := c.areaWrite(area, db, byteOff, buf); err != nil {
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
		if err := c.areaWrite(area, db, byteOff, buf); err != nil {
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
		if err := c.areaWrite(area, db, byteOff, buf); err != nil {
			return c.markBrokenRead(err)
		}
		return nil
	case "string":
		return c.writeStringLocked(area, db, byteOff, value, stringLen)
	default:
		return fmt.Errorf("S7 Write 不支持的数据类型: %s", dataType)
	}
}

// areaWrite 按存储区分发写请求（调用方已持有 c.mu）。
//
// 现场 PLC 的回写点并不都在 DB 区：M 位（中间标志）、Q 输出字节、
// I/E 输入字节都是常见回写目标。早期实现只支持 DB 区，
// 写 M/Q/I 直接报"S7 Write 暂仅支持 DB 区"——
// 现场表现为"回写接口能通、但一半的点位写不进去"。
//
// DB 区带 db 号，其余区（M/I/E/Q/A）db 恒为 0，由底层库忽略。
func (c *s7Channel) areaWrite(area s7Area, db, byteOff int, buf []byte) error {
	switch area {
	case areaDB:
		return c.client.AGWriteDB(db, byteOff, len(buf), buf)
	case areaMB:
		return c.client.AGWriteMB(byteOff, len(buf), buf)
	case areaIB:
		return c.client.AGWriteEB(byteOff, len(buf), buf)
	case areaQB:
		return c.client.AGWriteAB(byteOff, len(buf), buf)
	default:
		return fmt.Errorf("S7 Write 不支持 area=%d", area)
	}
}

// areaRead1 读 1 字节（位写 RMW 的读半边，调用方已持有 c.mu）。
func (c *s7Channel) areaRead1(area s7Area, db, byteOff int, buf []byte) error {
	return c.areaReadN(area, db, byteOff, buf)
}

// areaReadN 读 N 字节（调用方已持有 c.mu）。
func (c *s7Channel) areaReadN(area s7Area, db, byteOff int, buf []byte) error {
	switch area {
	case areaDB:
		return c.client.AGReadDB(db, byteOff, len(buf), buf)
	case areaMB:
		return c.client.AGReadMB(byteOff, len(buf), buf)
	case areaIB:
		return c.client.AGReadEB(byteOff, len(buf), buf)
	case areaQB:
		return c.client.AGReadAB(byteOff, len(buf), buf)
	default:
		return fmt.Errorf("S7 Read 不支持 area=%d", area)
	}
}

// writeStringLocked 写入 S7 STRING（调用方已持有 c.mu）。
//
// 布局（由 channel 配置 string_layout 决定，与读取路径严格对称）：
//
//	hdr=4: [2B maxLen][2B curLen][content...]   标准 S7 STRING
//	hdr=2: [1B maxLen][1B curLen][content...]   短头（西门子/Hsl 实际格式）
//	hdr=1: [1B curLen][content...]              单字节头
//	hdr=0: [content...]                         无头
//
// curLen 的单位是**字节数**（西门子 STRING 语义）。
//
// 写入策略（生产环境的三个硬约束）：
//
//  1. **RMW 而非盲写**：先把目标区读出来，再只改「头部 + 内容」这两段，
//     尾部未用区域保留 PLC 侧原值。直接全量覆盖会把 PLC 侧预留的填充字节
//     清成 0，某些 PLC 的 STRING 尾部有校验位/填充语义，会导致 PLC 报异常。
//
//  2. **长度单位是字节不是字符**：GBK 下 1 个汉字占 2 字节，UTF-8 下占 3 字节。
//     写 header 里的 curLen 必须用 len(encodedBytes)，用 rune 数会让 PLC
//     只读到一半（这就是"写进去是乱码/半截"的根因）。
//
//  3. **超长截断而不是报错**：工业现场字符串长度常与 PLC 侧 maxLen 不一致，
//     截断比整条写失败更符合预期（写入失败会阻塞回写队列）。但截断会记录 WARN。
func (c *s7Channel) writeStringLocked(area s7Area, db, byteOff int, value any, stringLen int) error {
	s, ok := value.(string)
	if !ok {
		return fmt.Errorf("string 写入值类型错误: %T", value)
	}

	hdr := stringHdrBytes(c.cfg.StringLayoutPtr)
	// 编码：auto 时纯 ASCII 原样，含中文按 GBK（见 charset.EncodeWith 注释）。
	encoded := charset.EncodeWith(s, c.cfg.StringEncoding)

	// 读回目标区头部，用于拿到 PLC 侧真实的 maxLen（决定可写上限）。
	// 读失败不阻断：退化成"用配置长度当作上限"。
	var maxLen int
	if hdr > 0 {
		head := make([]byte, hdr)
		if err := c.areaReadN(area, db, byteOff, head); err != nil {
			// 读头失败：记录但继续写（PLC 可能刚上电，头还没初始化）。
			// 这里不返回错误 —— 写失败对业务影响更大。
			c.lastErrorMs.Store(time.Now().UnixMilli())
		} else {
			maxLen = parseStringMaxLen(head, hdr)
		}
	}

	// 计算内容区上限（字节）。
	//
	// 优先级：点位声明的 StringLength > PLC 侧 header 的 maxLen > 默认 64。
	//
	// 为什么点位声明优先：
	//   - 它是用户显式配置的"这个 STRING 点位有多长"，最权威。
	//   - PLC 侧 header 的 maxLen 只有在 PLC **已经初始化过**该 STRING 时才可信；
	//     PLC 刚上电 / DB 区未初始化时那里是垃圾值（可能是 0 或 65535）。
	//   - 无头布局（hdr=0）根本没有 maxLen，只能靠这个。
	capacity := stringLen
	if capacity <= 0 {
		capacity = maxLen
	}
	if capacity <= 0 {
		capacity = 64
	}
	// 头部里写 maxLen 时用"本次实际容量"，避免写入一个 PLC 侧没声明过的 maxLen。
	if maxLen <= 0 {
		maxLen = capacity
	}

	content := encoded
	truncated := false
	if len(content) > capacity {
		content = content[:capacity]
		truncated = true
	}

	// 组装写入缓冲：头部 + 内容。
	buf := buildStringWriteBuf(hdr, maxLen, capacity, content)

	if err := c.areaWrite(area, db, byteOff, buf); err != nil {
		return c.markBrokenRead(err)
	}

	if truncated {
		logging.Warn(logging.EventWriteFail).
			Str("event", "s7_string_truncated").
			Int("db", db).
			Int("offset", byteOff).
			Int("want_bytes", len(encoded)).
			Int("capacity", capacity).
			Msg("S7 字符串超长已截断写入")
	}
	return nil
}

// buildStringWriteBuf 组装 S7 STRING 的写入字节：头部 + 内容。
//
// 与读取路径（decodeS7ValueSpan）严格对称：
//
//	hdr=4 → [2B maxLen 大端][2B curLen 大端][content]
//	hdr=2 → [1B maxLen][1B curLen][content]
//	hdr=1 → [1B curLen][content]
//	hdr=0 → [content][NUL 填充到 capacity]
//
// maxLen：PLC 侧声明的最大长度（0 = PLC 未声明/未读到）。
// capacity：本次写入的内容区实际上限（字节）。
// content：已编码、已截断到 capacity 以内的内容字节。
//
// curLen 写的是 len(content)（**字节数**）—— 这是最容易出错的地方：
// GBK 下 1 个汉字 2 字节、UTF-8 下 3 字节，用字符数会让 PLC 只读到半截。
//
// **hdr=0 的特殊性**：无头布局没有长度字段，读取路径只能"读到缓冲区末尾再去掉尾部 NUL"。
// 所以写入侧必须把内容之后的剩余空间**用 NUL 填满**（补到 capacity），
// 否则 PLC 侧会读到上一次写入的残留数据。
// hdr>0 的布局有 curLen 兜底，读侧会按 curLen 精确截断，所以不需要填尾部
// —— 保留 PLC 侧原值更安全（某些 PLC 的 STRING 尾部有填充/校验语义）。
func buildStringWriteBuf(hdr, maxLen, capacity int, content []byte) []byte {
	curLen := len(content)

	if hdr == 0 {
		// 无头：内容 + NUL 填充到 capacity，让读侧能靠尾部 NUL 判断结束。
		buf := make([]byte, 0, capacity)
		buf = append(buf, content...)
		for i := curLen; i < capacity; i++ {
			buf = append(buf, 0x00)
		}
		return buf
	}

	buf := make([]byte, 0, hdr+curLen)

	switch hdr {
	case 4:
		m := maxLen
		if m <= 0 || m > 0xFFFF {
			m = capacity
		}
		b := make([]byte, 4)
		binary.BigEndian.PutUint16(b[0:2], uint16(m))
		binary.BigEndian.PutUint16(b[2:4], uint16(curLen))
		buf = append(buf, b...)
	case 2:
		m := maxLen
		if m <= 0 || m > 0xFF {
			m = capacity
		}
		b := make([]byte, 2)
		b[0] = byte(m)
		b[1] = byte(curLen)
		buf = append(buf, b...)
	case 1:
		buf = append(buf, byte(curLen))
	}
	return append(buf, content...)
}

// parseStringMaxLen 从 STRING 头部解析 PLC 侧声明的最大长度。
//
// 布局与读取路径（decodeS7ValueSpan 的 case "string"）严格对称：
//
//	hdr=4 → 前 2B 是 maxLen（大端 uint16）
//	hdr=2 → 第 1B 是 maxLen
//	hdr=1 → 无 maxLen（只有 curLen），返回 0
//	hdr=0 → 无头，返回 0
func parseStringMaxLen(head []byte, hdr int) int {
	switch hdr {
	case 4:
		if len(head) < 4 {
			return 0
		}
		return int(binary.BigEndian.Uint16(head[0:2]))
	case 2:
		if len(head) < 2 {
			return 0
		}
		return int(head[0])
	default:
		// hdr=1 / hdr=0：没有 maxLen 字段
		return 0
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
func (c *s7Channel) Stats() map[string]any {
	c.mu.Lock()
	connected := c.client != nil && !c.closed.Load()
	state := "Disconnected"
	if connected {
		state = "Connected"
	}
	probeMs := int64(0)
	if lastUsedNs := c.lastUsed.Load(); lastUsedNs != 0 {
		probeMs = time.Unix(0, lastUsedNs).UnixMilli()
	}
	c.mu.Unlock()
	return map[string]any{
		"connection_state": state,
		"address":          c.addr,
		"rack":             c.cfg.Rack,
		"slot":             c.cfg.Slot,
		"connect_type":     c.cfg.ConnectType,
		"string_layout":    stringHdrBytes(c.cfg.StringLayoutPtr),
		"read_count":       c.readCount.Load(),
		"write_count":      c.writeCount.Load(),
		"error_count":      c.errorCount.Load(),
		"last_read_ok_ms":  c.lastReadOkMs.Load(),
		"last_write_ok_ms": c.lastWriteOkMs.Load(),
		"last_error_ms":    c.lastErrorMs.Load(),
		"last_probe_ok_ms": probeMs,
	}
}

// markBrokenRead 读失败时调用：记录错误。
func (c *s7Channel) markBrokenRead(err error) error {
	c.setErr(err)
	// 写路径同样可能撞上"对端已断开"—— 同样要作废死连接，
	// 否则后续写会一直用死 socket 失败（读路径已由 invalidateDeadConnLocked 处理）。
	c.invalidateDeadConnLocked(err)
	c.errorCount.Add(1)
	c.lastErrorMs.Store(time.Now().UnixMilli())
	return err
}

// invalidateDeadConnLocked 在 c.mu 持有时调用：判定底层连接是否已死，
// 若是则把 client/handler 清空，让下一次 Reconnect / Connect 能真正重建。
//
// 为什么需要（2026-10-25 修复）：
// PLC 停机再启动后，TCP 连接已被对端 RST（"An existing connection was forcibly
// closed by the remote host"）。此时 c.client 仍非 nil，但 socket 已死：
//   - 每次 BatchRead 都失败 → Source 一直显示"采集异常"，ConsecutiveErrors 无限累加
//   - registry.Snapshot 的 ProbeActive 也用同一个死 client，必然失败
//   - 只有 registry 的 Reconnect（重建 handler）能恢复，但它走的是独立路径
//
// 所以在读失败时必须主动"作废"死连接，让后续 Reconnect 能建出新 socket。
//
// 判定：只对"连接类"错误作废；业务类错误（如地址越界、类型不支持）保持连接，
// 否则一次点位配置错误会导致连接被反复重建（现场表现为 PLC 连接数暴涨）。
func (c *s7Channel) invalidateDeadConnLocked(err error) {
	if err == nil {
		return
	}
	if !isDeadConnErr(err.Error()) {
		return
	}
	if c.client == nil {
		return
	}
	old := c.handler
	// 先清句柄再关旧 handler：避免并发读者拿到"半死不活"的 client。
	c.client = nil
	c.handler = nil
	if old != nil {
		_ = old.Close()
	}
	c.errorCount.Add(1)
	c.lastErrorMs.Store(time.Now().UnixMilli())
	logging.Warn(logging.EventChannelClosed).
		Str("channel", c.channelID).
		Err(err).
		Msg("S7 连接已失效（对端断开），标记为待重连")
}

// autoReconnectInterval 自动重连的最小间隔。
//
// 为什么需要节流：PLC 停机期间采集是 1Hz，如果每次读失败都立刻重连，
// 就是 1Hz 的 TCP 握手风暴 —— 对 PLC 的连接数压力很大，
// 而且每次握手要 100~500ms，会把采集线程拖慢。
//
// 1s 的取值：与 collect 的采集周期一致，最坏情况下"PLC 恢复后 ~1s 内重连成功"。
const autoReconnectInterval = 1 * time.Second

// ensureConnectedLocked 在 c.mu 持有时调用：
// 若连接已作废（c.client == nil）且距上次自动重连超过节流窗口，则尝试重建连接。
//
// 返回值：
//   - nil：连接可用（原本就可用，或本次重连成功）
//   - err：连接仍不可用（本次没到重连时机，或重连失败）
//
// 为什么需要（2026-10-25 修复）：
// invalidateDeadConnLocked 把死连接清掉之后，如果没有人在下一次读之前重建它，
// 采集会一直返回 "S7 未连接" —— 正是用户反馈的
// "S7 停掉再恢复后，还显示采集异常、还在累计采集失败次数"。
//
// 这里做的是**惰性自愈**：读失败 → 作废连接 → 下一次读时顺手重连。
// 不需要用户打开页面、也不需要点诊断。
func (c *s7Channel) ensureConnectedLocked(ctx context.Context) error {
	if c.client != nil {
		return nil
	}
	if c.closed.Load() {
		return fmt.Errorf("S7 channel %s 已关闭", c.channelID)
	}
	now := time.Now()
	if !c.lastReconnectAt.IsZero() && now.Sub(c.lastReconnectAt) < autoReconnectInterval {
		// 刚重连过（大概率失败）→ 节流，等待下一次读再试。
		return fmt.Errorf("S7 未连接")
	}
	c.lastReconnectAt = now
	timeout := time.Duration(c.cfg.ConnectionTimeoutMs) * time.Millisecond
	connectCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := c.connectLocked(connectCtx); err != nil {
		return err
	}
	logging.Info(logging.EventChannelEstablished).
		Str("channel", c.channelID).
		Str("addr", c.addr).
		Msg("S7 连接已自动重建")
	return nil
}

// isDeadConnErr 判断错误是否表示"底层 TCP 连接已死，需要重建"。
//
// 覆盖现场常见的几类：
//   - 对端 RST："forcibly closed by the remote host" / "connection reset"
//   - gos7 handler 已销毁："is null"
//   - Go net 包关闭："use of closed network connection" / "closed pipe"
//   - 管道写超时："broken pipe" / "i/o timeout"
//
// 刻意不匹配"地址解析失败""类型不支持"等业务错误 —— 那些错误跟连接健康无关。
func isDeadConnErr(msg string) bool {
	low := strings.ToLower(msg)
	deadSubstrings := []string{
		"forcibly closed",
		"connection reset",
		"broken pipe",
		"closed pipe",
		"closed network connection",
		"use of closed",
		"is null",
		"i/o timeout",
		"connection refused",
		"no connection could be made",
	}
	for _, sub := range deadSubstrings {
		if strings.Contains(low, sub) {
			return true
		}
	}
	return false
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
	areaDB s7Area = 1
	areaIB s7Area = 2
	areaQB s7Area = 3
	areaMB s7Area = 4
	areaTM s7Area = 5
	areaCT s7Area = 6
)

// parseS7Address 解析 S7 地址 → (area, db, byteOff, bitOff)
//
// 支持格式：
//
//	DB1.DBB0 / DB1.DBW10 / DB1.DBD100 / DB1.DBX0.0 / DB1.DBX0.7
//	MB0 / MW10 / MD100 / MX0.0
//	IB0 / IW10 / ID100 / IX0.0
//	QB0 / QW10 / QD100 / QX0.0
//	M100  -> MB100 (西门子常用简写)
func parseS7Address(addr string) (s7Area, int, int, int, error) {
	a := strings.ToUpper(strings.TrimSpace(addr))
	if a == "" {
		return 0, 0, 0, 0, fmt.Errorf("空地址")
	}

	// DB 区
	if strings.HasPrefix(a, "DB") {
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
	// 简写位地址 M0.1 / Q0.1 / I0.7 -> 字节 0 的第 1/7 位。
	//
	// 现场常见写法（尤其博途/汇川/西门子部分 HMI 导出）就是 "M0.1" 而不是
	// "MX0.1"。早期实现只认 "MX0.1"，导致这类点位在**读取时**能解析，
	// 但用户手工填入 "M0.1" 时直接报"地址段解析失败"，
	// 现场表现为"同样的点换个写法就不能用了"。
	if strings.Contains(rest, ".") {
		num, bit, err := parseBitAddr(rest)
		if err != nil {
			return 0, 0, 0, 0, err
		}
		return area, db, num, bit, nil
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
			n = 64
		}
		h := hdrBytes
		if h < 0 {
			h = 4
		}
		if h > 4 {
			h = 4
		}
		return h + n
	default:
		return 1
	}
}

// decodeS7Value 从 1 字节 buf 解码单值。
func decodeS7Value(dataType string, buf []byte, bitOff int, encoding string) (protocol.Value, error) {
	return decodeS7ValueSpan(dataType, buf, 0, 0, bitOff, 4, encoding)
}

// boolToF 把 bool 规范成 0/1。保证 Value.Float64 对所有数值型都可用。
func boolToF(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// decodeS7ValueSpan 从跨 span 读取的 buf 解码 point（point 在 span 内偏移 spanStart）。
//
// encoding："" / "auto" = 自动探测；"utf-8" / "gbk" = 强制指定。
func decodeS7ValueSpan(dataType string, buf []byte, spanStart, pointStart, bitOff, hdrBytes int, encoding string) (protocol.Value, error) {
	if len(buf) == 0 {
		return protocol.Value{}, errors.New("读缓冲为空")
	}
	off := pointStart - spanStart
	if off < 0 || off >= len(buf) {
		return protocol.Value{}, fmt.Errorf("点位偏移 %d 超出 span [%d,%d)", pointStart, spanStart, spanStart+len(buf))
	}
	switch dataType {
	case "bool":
		v := (buf[off]>>uint(bitOff))&1 == 1
		return protocol.Value{DataType: dataType, Bool: v, Float64: boolToF(v), Quality: "good"}, nil
	case "int8":
		v := int64(int8(buf[off]))
		return protocol.Value{DataType: dataType, Int64: v, Float64: float64(v), Quality: "good"}, nil
	case "uint8":
		v := uint64(buf[off])
		return protocol.Value{DataType: dataType, Uint64: v, Float64: float64(v), Quality: "good"}, nil
	case "int16":
		u := binary.BigEndian.Uint16(buf[off : off+2])
		v := int64(int16(u))
		return protocol.Value{DataType: dataType, Int64: v, Float64: float64(v), Quality: "good"}, nil
	case "uint16":
		u := binary.BigEndian.Uint16(buf[off : off+2])
		v := uint64(u)
		return protocol.Value{DataType: dataType, Uint64: v, Float64: float64(v), Quality: "good"}, nil
	case "int32":
		u := binary.BigEndian.Uint32(buf[off : off+4])
		v := int64(int32(u))
		return protocol.Value{DataType: dataType, Int64: v, Float64: float64(v), Quality: "good"}, nil
	case "uint32":
		u := binary.BigEndian.Uint32(buf[off : off+4])
		v := uint64(u)
		return protocol.Value{DataType: dataType, Uint64: v, Float64: float64(v), Quality: "good"}, nil
	case "float32":
		u := binary.BigEndian.Uint32(buf[off : off+4])
		return protocol.Value{DataType: dataType, Float64: float64(math.Float32frombits(u)), Quality: "good"}, nil
	case "int64":
		u := binary.BigEndian.Uint64(buf[off : off+8])
		v := int64(u)
		return protocol.Value{DataType: dataType, Int64: v, Float64: float64(v), Quality: "good"}, nil
	case "uint64":
		u := binary.BigEndian.Uint64(buf[off : off+8])
		v := u
		return protocol.Value{DataType: dataType, Uint64: v, Float64: float64(v), Quality: "good"}, nil
	case "float64":
		u := binary.BigEndian.Uint64(buf[off : off+8])
		return protocol.Value{DataType: dataType, Float64: math.Float64frombits(u), Quality: "good"}, nil
	case "string":
		h := hdrBytes
		if h < 0 {
			h = 4
		}
		if h > 4 {
			h = 4
		}
		if off+h > len(buf) {
			return protocol.Value{}, fmt.Errorf("string 头越界: off=%d hdr=%d len=%d", off, h, len(buf))
		}
		var curLen int
		switch h {
		case 4:
			// 标准 S7 STRING: [2B maxLen][2B curLen]，两个都是大端 uint16。
			// curLen 的单位是**字节**。
			_ = int(binary.BigEndian.Uint16(buf[off : off+2]))
			curLen = int(binary.BigEndian.Uint16(buf[off+2 : off+4]))
		case 2:
			// 短头: [1B maxLen][1B curLen]（西门子/Hsl 实际使用的格式）。
			//
			// 历史 bug（2026-10-24）：这里原来按**大端 uint16** 读，
			// 于是 Hsl 的 [0xFE, 0x06] 被读成 curLen=0xFE06=65030，
			// 再被缓冲区长度截断 → 恰好读满整段 → 看起来"对"，
			// 实则完全忽略了头里的 curLen 语义。
			// 实际是 1 字节 maxLen + 1 字节 curLen，curLen 单位是字节。
			_ = int(buf[off])
			curLen = int(buf[off+1])
		case 1:
			// [1B curLen]（无 maxLen）
			curLen = int(buf[off])
		case 0:
			// 无头：整段都是内容
			curLen = len(buf) - (off + h)
		}
		if curLen > len(buf)-(off+h) {
			curLen = len(buf) - (off + h)
		}
		if curLen < 0 {
			curLen = 0
		}
		raw := buf[off+h : off+h+curLen]
		// 去掉尾部 NUL 填充
		for len(raw) > 0 && raw[len(raw)-1] == 0 {
			raw = raw[:len(raw)-1]
		}
		chars := raw
		// curLen 填的是**字符数**而不是字节数时（某些 PLC / 模拟器的 bug），
		// 截断会切在多字节字符中间，或（更常见的）正好落在某个字符边界上
		// 但内容还没写完 —— 两种情况都会导致尾部残留旧数据混进来。
		//
		// 判据（实测推导）：curLen 位置上的字节若是**多字节字符的首字节**，
		// 说明真正的内容还在继续 → 往后延伸直到 NUL / ASCII 残留。
		if rest := buf[off+h+curLen:]; charset.StartsMultiByte(rest, encoding) {
			if ext, ok := extendString(buf[off+h:], curLen, encoding); ok {
				chars = ext
			} else {
				// 延伸失败：至少别把半个字符带出去
				chars = trimToCompleteRune(raw)
			}
		}
		return protocol.Value{DataType: dataType, String: charset.DecodeWith(chars, encoding), Quality: "good"}, nil
	default:
		v := uint64(buf[off])
		return protocol.Value{DataType: dataType, Uint64: v, Float64: float64(v), Quality: "good"}, nil
	}
}

func sortItemsByOffset(items []batchItem) {
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && items[j].byteOff < items[j-1].byteOff; j-- {
			items[j], items[j-1] = items[j-1], items[j]
		}
	}
}

// trimToCompleteRune 切掉末尾不完整的多字节字符（GBK 双字节 / UTF-8 3-4 字节）。
//
// 背景（2026-10-24）：某些 PLC 写字符串时 curLen 填的是**字符数**而不是
// **字节数**（HslCommunicationDemo 就是这样：5 个汉字填 6，实际 15 字节）。
// 于是按 curLen 截断会切在多字节字符中间 → 解码出 U+FFFD 乱码。
//
// 这里只在"字节流确实不完整"时切尾，不会动完整字符。
// 注意：GBK 是变长编码（1 字节或 2 字节），无法从字节流单独判断合法性，
// 所以这里只处理**明确的 UTF-8 前导字节**被截断的情况；
// GBK 的半截字符由 charset 包解码时回退成 '?'，不会产生乱码。
func trimToCompleteRune(b []byte) []byte {
	// 找最后一个可能的 UTF-8 序列起点
	for i := len(b) - 1; i >= 0 && i >= len(b)-4; i-- {
		c := b[i]
		switch {
		case c < 0x80:
			// ASCII 结尾，一定完整
			return b
		case c&0xE0 == 0xC0:
			// 2 字节前导：需要 1 个后续字节
			if len(b)-i < 2 {
				return b[:i]
			}
			return b
		case c&0xF0 == 0xE0:
			// 3 字节前导：需要 2 个后续字节
			if len(b)-i < 3 {
				return b[:i]
			}
			return b
		case c&0xF8 == 0xF0:
			// 4 字节前导：需要 3 个后续字节
			if len(b)-i < 4 {
				return b[:i]
			}
			return b
		default:
			// 续字节或 GBK 高位字节，继续往前找
		}
	}
	return b
}

// extendString 在 curLen 截断位置之后寻找真正的字符串结束位置。
//
// 规则（实测推导，2026-10-24）：
//
//  1. curLen 正确时，字符串区后面是 **NUL 填充**或 ASCII 残留 → 立即停止，不延伸 ✓
//  2. curLen 填成字符数时，curLen 处正好是一个多字节字符的首字节。
//     从那里继续往后，逐个吃掉多字节字符，直到遇到 NUL / ASCII ✓
//  3. 残留的旧数据通常是 ASCII → 停在 ASCII 前 ✓
//
// 返回值 ok=false 表示没找到更好的边界，调用方应保留原内容。
func extendString(buf []byte, curLen int, encoding string) ([]byte, bool) {
	if curLen < 0 || curLen >= len(buf) {
		return nil, false
	}
	// 从 curLen 起向后扩展，每步吃掉一个多字节字符
	end := curLen
	for steps := 0; steps < 32; steps++ {
		rest := buf[end:]
		if len(rest) == 0 {
			break
		}
		// NUL 结束
		if rest[0] == 0 {
			break
		}
		// ASCII：说明后面是旧数据/填充，不是新内容
		if rest[0] < 0x80 {
			break
		}
		n := charset.NextMultiByteLen(rest, encoding)
		if n == 0 {
			break
		}
		end += n
	}
	if end <= curLen {
		return nil, false
	}
	cand := buf[:end]
	// 去掉尾部 NUL
	for len(cand) > 0 && cand[len(cand)-1] == 0 {
		cand = cand[:len(cand)-1]
	}
	// 校验：解码后不能有替换字符
	if strings.ContainsRune(charset.DecodeWith(cand, encoding), '�') {
		return nil, false
	}
	return cand, true
}

func toInt64(v any) (int64, error) {
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
	case int8:
		return x != 0, true
	case int16:
		return x != 0, true
	case int32:
		return x != 0, true
	case int64:
		return x != 0, true
	case uint:
		return x != 0, true
	case uint8:
		return x != 0, true
	case uint16:
		return x != 0, true
	case uint32:
		return x != 0, true
	case uint64:
		return x != 0, true
	case float32:
		return x != 0, true
	case float64:
		return x != 0, true
	}
	return false, false
}

// init S7 协议自注册（_all.go 触发）。
func init() {
	if err := protocol.Register(newS7Plugin()); err != nil {
		panic(err)
	}
}
