// Plan2 主题 A 3.5 收尾：CanonicalizeChannelType 单测。
//
// 目的：
//  1. 锁定 'modbus' → 'modbus_tcp' 映射
//  2. 锁定未知值原样返回（不允许"宽进"或"猜测"）
//  3. 锁定大小写敏感（不允许 'MODBUS' 通过）
package protocol

import "testing"

func TestCanonicalizeChannelType(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		// 已知别名
		{"modbus", "modbus_tcp"},
		// 注册名原样返回
		{"s7", "s7"},
		{"modbus_tcp", "modbus_tcp"},
		{"opcua", "opcua"},
		// 未知值原样返回
		{"", ""},
		{"unknown", "unknown"},
		// 大小写敏感（不允许）
		{"MODBUS", "MODBUS"},
		{"Modbus", "Modbus"},
		// fake 协议
		{"fake", "fake"},
	}
	for _, c := range cases {
		got := CanonicalizeChannelType(c.in)
		if got != c.want {
			t.Errorf("CanonicalizeChannelType(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestCanonicalizeChannelType_ThenGet 验证归一化后能 Get 到 plugin（端到端）。
//
// 注意：本测试依赖插件注册。由于无法 import plugins 包（循环依赖），此处仅测
// 试 CanonicalizeChannelType 本身。subpackage 测试已覆盖 Get 场景。
func TestCanonicalizeChannelType_ThenGet(t *testing.T) {
	infos := AllInfo()
	if len(infos) == 0 {
		t.Skip("protocol registry empty (cannot test Get in this context)")
	}
	canonical := CanonicalizeChannelType("modbus")
	p, err := Get(canonical)
	if err != nil {
		t.Fatalf("Get(%q) 失败（归一化后应能拿到 modbus_tcp）: %v", canonical, err)
	}
	if p.Name() != "modbus_tcp" {
		t.Errorf("plugin.Name() = %q, want \"modbus_tcp\"", p.Name())
	}
}
