// ModbusTCP 协议插件单元测试。
//
// 重点验证：
//  1. parseModbusAddress 各种格式解析
//  2. encode/decode 字节序 + 大小端正确
//  3. bool coil / register bit 解析
//
// 不连真实 broker（modbus 服务器启动 + Client 集成在 cmd/mbprobe 验证）。
package modbus_tcp

import (
	"testing"

	"github.com/plcbridge/plcbridge/internal/infra/protocol"
)

func TestParseModbusAddress(t *testing.T) {
	cases := []struct {
		in     string
		want   modbusLocation
		errSub string
	}{
		{"0x100", modbusLocation{funcCode: "0x", addr: 99, bit: 0}, ""},
		{"1x00100", modbusLocation{funcCode: "1x", addr: 99, bit: 0}, ""},
		{"3x100", modbusLocation{funcCode: "3x", addr: 99, bit: 0}, ""},
		{"4x100", modbusLocation{funcCode: "4x", addr: 99, bit: 0}, ""},
		{"4x100.5", modbusLocation{funcCode: "4x", addr: 99, bit: 5}, ""},
		{"4x65535", modbusLocation{funcCode: "4x", addr: 65534, bit: 0}, ""},
		// 错误用例
		{"", modbusLocation{}, "地址过短"},
		{"2x100", modbusLocation{}, "func 非法"},
		{"0x0", modbusLocation{}, "地址号非法"},
		{"4x65536", modbusLocation{}, "地址号非法"},
		{"4x100.16", modbusLocation{}, "bit 非法"},
		{"4xabc", modbusLocation{}, "地址号非法"},
		{"4x100.abc", modbusLocation{}, "bit 非法"},
	}
	for _, c := range cases {
		got, err := parseModbusAddress(c.in)
		if c.errSub != "" {
			if err == nil {
				t.Errorf("parseModbusAddress(%q): want error containing %q, got nil", c.in, c.errSub)
				continue
			}
			if !contains(err.Error(), c.errSub) {
				t.Errorf("parseModbusAddress(%q): error = %q, want containing %q", c.in, err.Error(), c.errSub)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseModbusAddress(%q): unexpected error %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("parseModbusAddress(%q) = %+v, want %+v", c.in, got, c.want)
		}
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	cases := []struct {
		dataType string
		value    any
	}{
		{"int16", int16(-12345)},
		{"uint16", uint16(54321)},
		{"int32", int32(-123456789)},
		{"uint32", uint32(3456789012)},
		{"float32", float32(3.14159)},
		{"int64", int64(-1234567890123)},
		{"uint64", uint64(1234567890123456)},
		{"float64", 3.14159265358979},
	}
	for _, c := range cases {
		regs, err := encodeUint16N(c.value, c.dataType)
		if err != nil {
			t.Errorf("encodeUint16N(%v, %s) err = %v", c.value, c.dataType, err)
			continue
		}
		v, err := decodeUint16NFromRegs(regs, c.dataType)
		if err != nil {
			t.Errorf("decodeUint16NFromRegs err = %v", err)
			continue
		}
		if !valueEqual(v, c.value, c.dataType) {
			t.Errorf("RoundTrip %s: encoded %v → got %+v", c.dataType, c.value, v)
		}
	}
}

// valueEqual 比对 Value 反解后的字段与原始值是否一致（按 dataType 选字段）。
func valueEqual(v protocol.Value, orig any, dataType string) bool {
	switch dataType {
	case "int16":
		if v.DataType != "int16" {
			return false
		}
		return v.Int64 == int64(orig.(int16))
	case "uint16":
		if v.DataType != "uint16" {
			return false
		}
		return v.Uint64 == uint64(orig.(uint16))
	case "int32":
		if v.DataType != "int32" {
			return false
		}
		return v.Int64 == int64(orig.(int32))
	case "uint32":
		if v.DataType != "uint32" {
			return false
		}
		return v.Uint64 == uint64(orig.(uint32))
	case "float32":
		if v.DataType != "float32" {
			return false
		}
		return v.Float64 == float64(orig.(float32))
	case "int64":
		if v.DataType != "int64" {
			return false
		}
		return v.Int64 == orig.(int64)
	case "uint64":
		if v.DataType != "uint64" {
			return false
		}
		return v.Uint64 == orig.(uint64)
	case "float64":
		if v.DataType != "float64" {
			return false
		}
		return v.Float64 == orig.(float64)
	}
	return false
}

func TestModbusConfigParse(t *testing.T) {
	p := newPlugin()
	// 正常
	cfg, err := p.ParseConfig([]byte(`{"unit_id": 5, "connection_timeout_ms": 3000}`))
	if err != nil {
		t.Fatal(err)
	}
	mc := cfg.(ModbusTCPConfig)
	if mc.UnitID != 5 || mc.ConnectionTimeoutMs != 3000 {
		t.Errorf("got %+v", mc)
	}
	if mc.RequestTimeoutMs != 1000 {
		t.Errorf("default request_timeout_ms = %d, want 1000", mc.RequestTimeoutMs)
	}
	// 非法 unit_id
	if _, err := p.ParseConfig([]byte(`{"unit_id": 248}`)); err == nil {
		t.Error("unit_id=248 应报错")
	}
	if _, err := p.ParseConfig([]byte(`{"unit_id": 0}`)); err == nil {
		t.Error("unit_id=0 应报错")
	}
	// 空：默认值
	cfg, err = p.ParseConfig(nil)
	if err != nil {
		t.Fatal(err)
	}
	mc = cfg.(ModbusTCPConfig)
	if mc.UnitID != 1 {
		t.Errorf("默认 unit_id = %d, want 1", mc.UnitID)
	}
}

func TestRegisterCount(t *testing.T) {
	cases := map[string]uint16{
		"int16": 1, "uint16": 1,
		"int32": 2, "uint32": 2, "float32": 2,
		"int64": 4, "uint64": 4, "float64": 4,
		"unknown": 1,
	}
	for dt, want := range cases {
		if got := registerCount(dt); got != want {
			t.Errorf("registerCount(%s) = %d, want %d", dt, got, want)
		}
	}
}

// TestModbusTCPChannel_Disconnect_Idempotent 验证未连接时 Disconnect 幂等返回 nil。
func TestModbusTCPChannel_Disconnect_Idempotent(t *testing.T) {
	ch := &modbusTCPChannel{}
	if err := ch.Disconnect(); err != nil {
		t.Errorf("未连接 Disconnect 应返回 nil, got %v", err)
	}
	if err := ch.Disconnect(); err != nil {
		t.Errorf("第二次 Disconnect 应返回 nil, got %v", err)
	}
}

// TestModbusTCPChannel_Stats_PR2Fields 验证 PR2 详细统计字段都存在。
func TestModbusTCPChannel_Stats_PR2Fields(t *testing.T) {
	ch := &modbusTCPChannel{
		addr: "127.0.0.1:502",
		cfg:  ModbusTCPConfig{UnitID: 1, ConnectionTimeoutMs: 2000},
	}
	stats := ch.Stats()
	if stats["connection_state"] != "Disconnected" {
		t.Errorf("connection_state = %v, want Disconnected", stats["connection_state"])
	}
	if stats["unit_id"] != 1 {
		t.Errorf("unit_id = %v, want 1", stats["unit_id"])
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

// contains 简单字符串包含（避免 import strings 编译告警）
func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
