package charset

import "testing"

// 以下用例全部来自对 HslCommunicationDemo 的实测（2026-10-24）。

// TestStartsMultiByte_CurLenCorrect 实测场景 A：curLen 正确（=字节数 15）。
//
//	FE 0F | "按说阿斯顿"(15B UTF-8) | 37 00 00
//
// curLen=15 位置上正是残留的 ASCII '7' → 不是多字节字符 → 不该延伸。
func TestStartsMultiByte_CurLenCorrect(t *testing.T) {
	buf := []byte{0x37, 0x00, 0x00}
	if StartsMultiByte(buf, "") {
		t.Error("StartsMultiByte = true, want false（curLen 正确时位置上应是 ASCII 残留）")
	}
	if StartsMultiByte(nil, "") {
		t.Error("StartsMultiByte(nil) = true, want false")
	}
}

// TestStartsMultiByte_CurLenIsCharCount 实测场景 B：curLen 填成字符数(=6)。
//
//	FE 06 | "按说阿斯顿"(15B UTF-8) | 37 00 00
//
// curLen=6 位置上正好是第 7 个字节 0xE9（"东"的首字节）→ 多字节字符 → 该延伸。
func TestStartsMultiByte_CurLenIsCharCount(t *testing.T) {
	// "按说阿斯顿" 的 UTF-8
	full := []byte{
		0xE6, 0x8C, 0x89, 0xE8, 0xAF, 0xB4, 0xE9, 0x98, 0xBF,
		0xE6, 0x96, 0xAF, 0xE9, 0xA1, 0xBF,
	}
	rest := full[6:] // curLen=6 之后的部分
	if !StartsMultiByte(rest, "") {
		t.Errorf("StartsMultiByte(%X) = false, want true（应是多字节字符首字节）", rest)
	}
}

// TestStartsMultiByte_GBK GBK 双字节字符。
func TestStartsMultiByte_GBK(t *testing.T) {
	// "程序号" GBK = B3 CC D0 F2 BA C5
	full := []byte{0xB3, 0xCC, 0xD0, 0xF2, 0xBA, 0xC5}
	if !StartsMultiByte(full, "gbk") {
		t.Error("StartsMultiByte(GBK) = false, want true")
	}
	if !StartsMultiByte(full, "") {
		t.Error("StartsMultiByte(GBK, auto) = false, want true")
	}
}

// TestNextMultiByteLen 各编码下的字符长度。
func TestNextMultiByteLen(t *testing.T) {
	cases := []struct {
		name     string
		b        []byte
		encoding string
		want     int
	}{
		{"ASCII", []byte{'A', 'B'}, "", 0},
		{"empty", nil, "", 0},
		{"UTF8 2字节", []byte{0xC3, 0xA9}, "utf-8", 2},       // é
		{"UTF8 3字节", []byte{0xE8, 0xAF, 0xB4}, "utf-8", 3}, // 说
		{"UTF8 4字节", []byte{0xF0, 0x9F, 0x98, 0x80}, "utf-8", 4},
		{"GBK 双字节", []byte{0xB3, 0xCC}, "gbk", 2},
		{"GBK 单字节ASCII", []byte{'A'}, "gbk", 0},
		{"auto UTF8 3字节", []byte{0xE8, 0xAF, 0xB4}, "", 3},
		{"auto GBK 2字节", []byte{0xB3, 0xCC}, "", 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := NextMultiByteLen(c.b, c.encoding); got != c.want {
				t.Errorf("NextMultiByteLen(%X, %q) = %d, want %d", c.b, c.encoding, got, c.want)
			}
		})
	}
}

// TestNextMultiByteLen 各编码下的字符长度。
