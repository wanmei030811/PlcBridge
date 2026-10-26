package s7

import (
	"testing"
)

// TestDecodeS7ValueSpan_StringGBK 验证 S7 STRING 读取路径能正确解码 GBK 中文。
//
// 回归背景（2026-10-24）：S7 读出的字符串之前直接 `string(bytes)`，
// 而西门子 PLC 中文环境的 STRING 区是 GBK/CP936 字节 → 界面显示 "???"。
//
// 这里构造标准 S7 STRING[16] 布局（4 字节头：[2B maxLen][2B curLen]）
// 的缓冲区，内容是 "程序号" 的 GBK 字节。
func TestDecodeS7ValueSpan_StringGBK(t *testing.T) {
	// [2B maxLen=16][2B curLen=6][6 bytes GBK]
	buf := []byte{
		0x00, 0x10, // maxLen = 16
		0x00, 0x06, // curLen = 6
		0xB3, 0xCC, 0xD0, 0xF2, 0xBA, 0xC5, // "程序号" 的 GBK
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // 尾部填充
	}
	v, err := decodeS7ValueSpan("string", buf, 0, 0, 0, 4, "")
	if err != nil {
		t.Fatalf("decodeS7ValueSpan: %v", err)
	}
	if v.String != "程序号" {
		t.Errorf("String = %q, want %q", v.String, "程序号")
	}
	if v.Quality != "good" {
		t.Errorf("Quality = %q, want good", v.Quality)
	}
}

// TestDecodeS7ValueSpan_StringGBK_ShortHeader 2 字节头布局（HslCommunicationDemo 用的就是这种）。
func TestDecodeS7ValueSpan_StringGBK_ShortHeader(t *testing.T) {
	// Hsl 短头：[1B maxLen=0xFE][1B curLen=0x06] + 内容
	buf := []byte{
		0xFE, 0x06, // maxLen / curLen
		0xB3, 0xCC, 0xD0, 0xF2, 0xBA, 0xC5, // "程序号" 的 GBK
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	}
	v, err := decodeS7ValueSpan("string", buf, 0, 0, 0, 2, "")
	if err != nil {
		t.Fatalf("decodeS7ValueSpan: %v", err)
	}
	if v.String != "程序号" {
		t.Errorf("String = %q, want %q", v.String, "程序号")
	}
}

// TestDecodeS7ValueSpan_StringUTF8 UTF-8 编码的字符串也必须原样返回（不能被当 GBK 再解一次）。
//
// 注意 curLen 的单位是**字节**（S7 STRING 语义），不是字符数：
// "程序号" 的 UTF-8 是 9 字节 / 3 字符，所以 curLen 必须是 9。
// GBK 下同样是 3 字符但只有 6 字节。
func TestDecodeS7ValueSpan_StringUTF8(t *testing.T) {
	utf8Bytes := []byte("程序号")
	buf := make([]byte, 4+16)
	copy(buf[0:2], []byte{0x00, 0x10}) // maxLen
	copy(buf[2:4], []byte{0x00, 0x09}) // curLen = 9 字节
	copy(buf[4:], utf8Bytes)
	v, err := decodeS7ValueSpan("string", buf, 0, 0, 0, 4, "")
	if err != nil {
		t.Fatalf("decodeS7ValueSpan: %v", err)
	}
	if v.String != "程序号" {
		t.Errorf("String = %q, want %q", v.String, "程序号")
	}
}

// TestDecodeS7ValueSpan_StringForceEncoding 强制 string_encoding=gbk / utf-8。
func TestDecodeS7ValueSpan_StringForceEncoding(t *testing.T) {
	gbk := []byte{0xB3, 0xCC, 0xD0, 0xF2, 0xBA, 0xC5}
	buf := make([]byte, 4+16)
	copy(buf[0:2], []byte{0x00, 0x10})
	copy(buf[2:4], []byte{0x00, 0x06})
	copy(buf[4:], gbk)

	// 强制 GBK → 中文
	v, err := decodeS7ValueSpan("string", buf, 0, 0, 0, 4, "gbk")
	if err != nil {
		t.Fatalf("decodeS7ValueSpan: %v", err)
	}
	if v.String != "程序号" {
		t.Errorf("force gbk: String = %q, want %q", v.String, "程序号")
	}

	// 强制 UTF-8 → 同样的字节是无效 UTF-8，应产生替换字符（而不是中文）
	v2, err := decodeS7ValueSpan("string", buf, 0, 0, 0, 4, "utf-8")
	if err != nil {
		t.Fatalf("decodeS7ValueSpan: %v", err)
	}
	if v2.String == "程序号" {
		t.Error("force utf-8 不应把 GBK 字节解成中文")
	}
}

// TestParseConfig_StringEncoding 校验 string_encoding 的处理。
func TestParseConfig_StringEncoding(t *testing.T) {
	p := &s7Plugin{}

	// 未配置 → 空串（自动探测）
	cfg, err := p.ParseConfig([]byte(`{"rack":0,"slot":1}`))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	sc := cfg.(*S7Config)
	if sc.StringEncoding != "" {
		t.Errorf("未配置时 StringEncoding = %q, want 空串", sc.StringEncoding)
	}

	// 显式 gbk
	cfg2, err := p.ParseConfig([]byte(`{"rack":0,"slot":1,"string_encoding":"gbk"}`))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if got := cfg2.(*S7Config).StringEncoding; got != "gbk" {
		t.Errorf("StringEncoding = %q, want gbk", got)
	}

	// 未知编码名 → 回退 auto（不报错）
	cfg3, err := p.ParseConfig([]byte(`{"rack":0,"slot":1,"string_encoding":"bogus"}`))
	if err != nil {
		t.Fatalf("ParseConfig 不应因未知编码报错: %v", err)
	}
	if got := cfg3.(*S7Config).StringEncoding; got != "auto" {
		t.Errorf("未知编码应回退 auto, got %q", got)
	}
}
