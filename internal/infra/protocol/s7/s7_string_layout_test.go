package s7

import (
	"testing"
)

// 以下测试数据全部来自对 HslCommunicationDemo 的实测（2026-10-24），
// 用 gos7 直读 DB1.DBB100 得到的原始字节。
//
// 关键事实（实测 + Hsl 官方库交叉验证）：
//   - Hsl 的 STRING 是 [1B maxLen][1B curLen]，curLen 单位是**字节**
//   - Hsl 官方 Write(addr, value, encoding) 写 "HELLO" → FE 05 "HELLO" ...
//     （maxLen=0xFE, curLen=5，5 字节 ASCII）
//   - Hsl 官方 Write(addr, value, encoding) 写 "程序号"(GBK) → FE 06 B3 CC D0 F2 BA C5
//     （curLen=6，6 字节 GBK）
//   - 但 Hsl 官方 Write **不清零**尾部 → 旧数据残留在内容区后面
//   - 而 PLC 侧有 bug 时会把 curLen 填成**字符数**而非字节数

// TestDecodeS7ValueSpan_HslHeaderCorrectBytes Hsl 正确写入的 ASCII 字符串。
func TestDecodeS7ValueSpan_HslHeaderCorrectBytes(t *testing.T) {
	// [maxLen=0xFE][curLen=5]"HELLO" + 残留 'A'*10 + "ZZ"
	buf := []byte{
		0xFE, 0x05,
		'H', 'E', 'L', 'L', 'O',
		'A', 'A', 'A', 'A', 'A', 'A', 'A', 'A', 'A', 'A',
		'Z', 'Z', 0x00, 0x00,
	}
	v, err := decodeS7ValueSpan("string", buf, 0, 0, 0, 2, "")
	if err != nil {
		t.Fatalf("decodeS7ValueSpan: %v", err)
	}
	if v.String != "HELLO" {
		t.Errorf("String = %q, want %q（curLen 应按字节数截断，不该带出残留）", v.String, "HELLO")
	}
}

// TestDecodeS7ValueSpan_HslHeaderGBK Hsl 正确写入的 GBK 中文。
func TestDecodeS7ValueSpan_HslHeaderGBK(t *testing.T) {
	buf := []byte{
		0xFE, 0x06, // maxLen=254, curLen=6 (字节)
		0xB3, 0xCC, 0xD0, 0xF2, 0xBA, 0xC5, // "程序号" GBK
		'A', 'A', 'A', 'A', 'A', 'A', 'A', 'A', 'A', 'A',
		'Z', 'Z', 0x00, 0x00,
	}
	v, err := decodeS7ValueSpan("string", buf, 0, 0, 0, 2, "")
	if err != nil {
		t.Fatalf("decodeS7ValueSpan: %v", err)
	}
	if v.String != "程序号" {
		t.Errorf("String = %q, want %q", v.String, "程序号")
	}
}

// TestDecodeS7ValueSpan_CurLenIsCharCount 回归：PLC 把 curLen 填成**字符数**。
//
// 实测 Hsl 的行为：写 5 个汉字（UTF-8 15 字节）后，头里 curLen=6。
// 按 curLen 截断会切在多字节字符中间 → 半个字 + 残留。
// 期望：能自动延伸到真正的字符串结束，读出完整的 "按说阿斯顿"。
func TestDecodeS7ValueSpan_CurLenIsCharCount(t *testing.T) {
	buf := []byte{
		0xFE, 0x06, // maxLen=254, curLen=6 ← 错的！应该是 15 字节
		0xE6, 0x8C, 0x89, 0xE8, 0xAF, 0xB4, 0xE9, 0x98, 0xBF, // "按说阿"
		0xE6, 0x96, 0xAF, 0xE9, 0xA1, 0xBF, // "斯顿" → 合计 15 字节
		'7', 0x00, 0x00, 0x00, // 残留的旧数据
	}
	v, err := decodeS7ValueSpan("string", buf, 0, 0, 0, 2, "")
	if err != nil {
		t.Fatalf("decodeS7ValueSpan: %v", err)
	}
	// 不应含替换字符
	if containsReplacement(v.String) {
		t.Errorf("String = %q 含 U+FFFD 替换字符（截断在多字节字符中间）", v.String)
	}
	// 应至少包含完整的 "按说阿斯顿"
	if !contains(v.String, "按说阿斯顿") {
		t.Errorf("String = %q, 期望包含 %q", v.String, "按说阿斯顿")
	}
}

// TestDecodeS7ValueSpan_CurLenTooBigCurLenIsBigEndian 回归：
// 旧代码把 2 字节头按**大端 uint16** 读 → curLen=0xFE06=65030 → 被缓冲区截断。
//
// 期望：新代码正确读成 curLen=6（1B maxLen + 1B curLen）。
func TestDecodeS7ValueSpan_CurLenNotBigEndian(t *testing.T) {
	buf := []byte{
		0xFE, 0x06,
		'H', 'E', 'L', 'L', 'O', 'X',
		'A', 'A', 'A', 'A', 'A', 'A', 'A', 'A', 'A', 'A',
		'Z', 'Z', 0x00, 0x00,
	}
	v, err := decodeS7ValueSpan("string", buf, 0, 0, 0, 2, "")
	if err != nil {
		t.Fatalf("decodeS7ValueSpan: %v", err)
	}
	// 大端读会得到 0xFE06=65030 → 截断到整个 buf → "HELLOX" + 残留
	// 正确读法：curLen=6 → "HELLOX"
	if v.String != "HELLOX" {
		t.Errorf("String = %q, want %q（curLen 应为 6，不是大端的 65030）", v.String, "HELLOX")
	}
}

// TestDecodeS7ValueSpan_StandardS7String 标准 4 字节头仍然正确。
func TestDecodeS7ValueSpan_StandardS7String(t *testing.T) {
	buf := []byte{
		0x00, 0x10, // maxLen = 16
		0x00, 0x05, // curLen = 5
		'H', 'E', 'L', 'L', 'O',
		'A', 'A', 'A', 'A', 'A', 'A', 'A', 'A', 'A', 'A',
		'Z', 'Z', 0x00, 0x00,
	}
	v, err := decodeS7ValueSpan("string", buf, 0, 0, 0, 4, "")
	if err != nil {
		t.Fatalf("decodeS7ValueSpan: %v", err)
	}
	if v.String != "HELLO" {
		t.Errorf("String = %q, want %q", v.String, "HELLO")
	}
}

// TestDecodeS7ValueSpan_CurLenCorrectButTailNotCleared 记录一个**预期行为**：
//
// PLC 写的 curLen 是正确的（=实际写入字节数），但 PLC 自己**没有清零**尾部，
// 于是旧数据的尾巴被算进字符串里。
//
// 现场数据（2026-10-24 实测）：PLC 字节 = FE 08 | "按说"(6B) | "21"(2B)
// → curLen=8 正确 → 读出 "按说21"，其中 "21" 是 PLC 侧未清零的残留。
//
// 这**不是**读取侧的 bug：curLen 说 8 字节，读取就该拿 8 字节。
// 根治要靠写入时清零整段（见 writeBytes 的 string 分支）。
func TestDecodeS7ValueSpan_CurLenCorrectButTailNotCleared(t *testing.T) {
	buf := []byte{
		0xFE, 0x08, // maxLen=254, curLen=8（PLC 写的字节数，正确）
		0xE6, 0x8C, 0x89, 0xE8, 0xAF, 0xB4, // "按说" (6B UTF-8)
		'2', '1', // PLC 未清零的残留 (2B)
		0x00, 0x00,
	}
	v, err := decodeS7ValueSpan("string", buf, 0, 0, 0, 2, "")
	if err != nil {
		t.Fatalf("decodeS7ValueSpan: %v", err)
	}
	// curLen 正确 → 严格按 8 字节取，不多不少
	if v.String != "按说21" {
		t.Errorf("String = %q, want %q（curLen=8 正确时应严格取 8 字节）", v.String, "按说21")
	}
}

// TestDecodeS7ValueSpan_SingleByteHeader 1 字节头 [1B curLen]。
func TestDecodeS7ValueSpan_SingleByteHeader(t *testing.T) {
	buf := []byte{
		0x05,
		'H', 'E', 'L', 'L', 'O',
		'A', 'A', 'A', 'A', 'A', 'A', 'A', 'A', 'A', 'A',
		'Z', 'Z', 0x00, 0x00,
	}
	v, err := decodeS7ValueSpan("string", buf, 0, 0, 0, 1, "")
	if err != nil {
		t.Fatalf("decodeS7ValueSpan: %v", err)
	}
	if v.String != "HELLO" {
		t.Errorf("String = %q, want %q", v.String, "HELLO")
	}
}

// TestDecodeS7ValueSpan_NoHeader 无头：整段都是内容，去掉尾部 NUL。
func TestDecodeS7ValueSpan_NoHeader(t *testing.T) {
	buf := []byte{'H', 'E', 'L', 'L', 'O', 0x00, 0x00, 0x00}
	v, err := decodeS7ValueSpan("string", buf, 0, 0, 0, 0, "")
	if err != nil {
		t.Fatalf("decodeS7ValueSpan: %v", err)
	}
	if v.String != "HELLO" {
		t.Errorf("String = %q, want %q", v.String, "HELLO")
	}
}

// TestDecodeS7ValueSpan_NoResidueLeak 关键回归：
// 内容写短了，尾部有旧数据，但 curLen 正确 → 绝不能带出旧数据。
//
// 这是"没有释放以前的数据"问题的正解：只要 curLen 正确，
// 就能精确截断，不依赖尾部是否清零。
func TestDecodeS7ValueSpan_NoResidueLeak(t *testing.T) {
	buf := []byte{
		0xFE, 0x05, // curLen = 5
		'H', 'E', 'L', 'L', 'O',
		'X', 'X', 'X', 'X', 'X', 'X', 'X', 'X', 'X', 'X', // 旧数据
		'Z', 'Z', 0x00, 0x00,
	}
	v, err := decodeS7ValueSpan("string", buf, 0, 0, 0, 2, "")
	if err != nil {
		t.Fatalf("decodeS7ValueSpan: %v", err)
	}
	if v.String != "HELLO" {
		t.Errorf("String = %q, want %q（不该带出尾部残留）", v.String, "HELLO")
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

func containsReplacement(s string) bool {
	for _, r := range s {
		if r == '�' {
			return true
		}
	}
	return false
}
