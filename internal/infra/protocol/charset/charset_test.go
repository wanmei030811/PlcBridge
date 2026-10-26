package charset

import "testing"

// TestDecode_ASCII 纯 ASCII 必须原样返回（零开销路径）。
func TestDecode_ASCII(t *testing.T) {
	cases := []string{"", "M100", "hello world", "0123456789", "line1\nline2"}
	for _, in := range cases {
		if got := Decode([]byte(in)); got != in {
			t.Errorf("Decode(%q) = %q, want %q", in, got, in)
		}
	}
}

// TestDecode_UTF8 合法 UTF-8 必须原样返回（不能被当 GBK 再解一次）。
func TestDecode_UTF8(t *testing.T) {
	cases := []string{"程序号", "堆叠", "温度", "café", "温度-25.3℃"}
	for _, in := range cases {
		got := Decode([]byte(in))
		if got != in {
			t.Errorf("Decode(%q) = %q, want %q", in, got, in)
		}
	}
}

// TestDecode_GBK 是本次修复的核心用例（2026-10-24 回归）：
//
// S7 STRING 区是 GBK/CP936 字节，之前 `string(bytes)` 直接产出 U+FFFD，
// 界面显示 "???"。这里断言 GBK 字节能正确还原成中文。
func TestDecode_GBK(t *testing.T) {
	cases := []struct {
		raw  []byte
		want string
	}{
		// "程序号" 的 GBK 编码
		{[]byte{0xB3, 0xCC, 0xD0, 0xF2, 0xBA, 0xC5}, "程序号"},
		// "堆叠" 的 GBK 编码
		{[]byte{0xB6, 0xD1, 0xB5, 0xFE}, "堆叠"},
		// "温度" 的 GBK 编码
		{[]byte{0xCE, 0xC2, 0xB6, 0xC8}, "温度"},
		// 混合：ASCII + GBK
		{[]byte{'A', 0xB3, 0xCC, 0xD0, 0xF2, 0xBA, 0xC5, 'B'}, "A程序号B"},
		// 单个 GBK 双字节
		{[]byte{0xC4, 0xE3}, "你"},
		// 6 个汉字
		{[]byte{0xC9, 0xE8, 0xB1, 0xB8, 0xD4, 0xCB, 0xD0, 0xD0, 0xD7, 0xB4, 0xCC, 0xAC}, "设备运行状态"},
	}
	for _, c := range cases {
		got := Decode(c.raw)
		if got != c.want {
			t.Errorf("Decode(% x) = %q (% x), want %q", c.raw, got, []byte(got), c.want)
		}
	}
}

// TestDecode_GBK_NoReplacementChar 关键断言：
// 解码结果里**不能**出现 U+FFFD（替换字符）——
// 那正是用户看到 "???" 的原因。
func TestDecode_GBK_NoReplacementChar(t *testing.T) {
	raw := []byte{0xB3, 0xCC, 0xD0, 0xF2, 0xBA, 0xC5}
	got := Decode(raw)
	for i, r := range got {
		if r == '\uFFFD' {
			t.Errorf("Decode(% x) 含 U+FFFD 替换字符: %q (位置 %d)", raw, got, i)
		}
	}
}

// TestDecode_InvalidBytes 无效字节不能 panic，且要有可读输出（不丢数据）。
func TestDecode_InvalidBytes(t *testing.T) {
	cases := [][]byte{
		{0xFF, 0xFE, 0xFD},
		{0x80},
		{0xC4},
		{'a', 0xFF, 'b'},
	}
	for _, raw := range cases {
		got := Decode(raw)
		if got == "" && len(raw) > 0 {
			t.Errorf("Decode(% x) 返回空串，丢数据了", raw)
		}
	}
}

// TestDecodeWith_ForceGBK 强制 GBK：即使字节恰好是合法 UTF-8 也按 GBK 解。
func TestDecodeWith_ForceGBK(t *testing.T) {
	// "程序号" 的 GBK 编码
	raw := []byte{0xB3, 0xCC, 0xD0, 0xF2, 0xBA, 0xC5}
	if got := DecodeWith(raw, "gbk"); got != "程序号" {
		t.Errorf("DecodeWith(gbk) = %q, want 程序号", got)
	}
	// 别名
	if got := DecodeWith(raw, "CP936"); got != "程序号" {
		t.Errorf("DecodeWith(CP936) = %q, want 程序号", got)
	}
}

// TestDecodeWith_ForceUTF8 强制 UTF-8。
func TestDecodeWith_ForceUTF8(t *testing.T) {
	raw := []byte("程序号")
	if got := DecodeWith(raw, "utf-8"); got != "程序号" {
		t.Errorf("DecodeWith(utf-8) = %q, want 程序号", got)
	}
	if got := DecodeWith(raw, "UTF8"); got != "程序号" {
		t.Errorf("DecodeWith(UTF8) = %q, want 程序号", got)
	}
}

// TestDecodeWith_AutoAndUnknown 未知编码名回退到自动探测（不报错、不 panic）。
func TestDecodeWith_AutoAndUnknown(t *testing.T) {
	raw := []byte{0xB3, 0xCC, 0xD0, 0xF2, 0xBA, 0xC5}
	for _, enc := range []string{"", "auto", "AUTO", "  auto  ", "unknown-enc", "latin1"} {
		got := DecodeWith(raw, enc)
		if got != "程序号" {
			t.Errorf("DecodeWith(%q) = %q, want 程序号（未知编码应回退自动探测）", enc, got)
		}
	}
}

// TestDecode_Empty 空输入返回空串。
func TestDecode_Empty(t *testing.T) {
	if got := Decode(nil); got != "" {
		t.Errorf("Decode(nil) = %q, want 空串", got)
	}
	if got := Decode([]byte{}); got != "" {
		t.Errorf("Decode([]) = %q, want 空串", got)
	}
}

// TestDecode_TrailingNullS7 模拟 S7 STRING 区的尾部 NUL 填充：
// 读取层已经剥掉尾部 0，这里确认即使没剥干净也不会产生噪声。
func TestDecode_TrailingNullS7(t *testing.T) {
	raw := []byte{0xB3, 0xCC, 0xD0, 0xF2, 0xBA, 0xC5, 0x00, 0x00, 0x00, 0x00}
	got := Decode(raw)
	if got != "程序号\x00\x00\x00\x00" {
		t.Errorf("Decode = %q, want %q", got, "程序号\x00\x00\x00\x00")
	}
}
