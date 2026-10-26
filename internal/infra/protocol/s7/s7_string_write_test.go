package s7

import (
	"encoding/binary"
	"testing"

	"github.com/plcbridge/plcbridge/internal/infra/protocol/charset"
)

// 以下测试验证 **S7 STRING 写入路径**（writeStringLocked / buildStringWriteBuf）。
//
// 核心不变式：**写入的字节能被读取路径原样读回**。
// 读取路径见 s7_string_layout_test.go（decodeS7ValueSpan）。
//
// 为什么这个不变式是生产关键：
// 读取侧已经修好了 GBK/UTF-8/curLen 语义的各种坑，但如果写入侧的头部格式
// 或 curLen 单位与读取侧不一致，就会出现"写进去是乱码 / 半截 / 残留旧数据"
// —— 而且**读测试全绿、写测试全绿，组合起来才炸**。所以这里做端到端往返验证。

// TestBuildStringWriteBuf_HeaderLayout 验证 4 种布局的头部字节。
func TestBuildStringWriteBuf_HeaderLayout(t *testing.T) {
	content := []byte("HELLO")

	t.Run("hdr4_标准S7", func(t *testing.T) {
		got := buildStringWriteBuf(4, 16, 16, content)
		want := []byte{0x00, 0x10, 0x00, 0x05, 'H', 'E', 'L', 'L', 'O'}
		if !bytesEqual(got, want) {
			t.Errorf("buf = %v, want %v", got, want)
		}
	})

	t.Run("hdr2_Hsl短头", func(t *testing.T) {
		got := buildStringWriteBuf(2, 254, 254, content)
		want := []byte{0xFE, 0x05, 'H', 'E', 'L', 'L', 'O'}
		if !bytesEqual(got, want) {
			t.Errorf("buf = %v, want %v", got, want)
		}
	})

	t.Run("hdr1_单字节头", func(t *testing.T) {
		got := buildStringWriteBuf(1, 0, 64, content)
		want := []byte{0x05, 'H', 'E', 'L', 'L', 'O'}
		if !bytesEqual(got, want) {
			t.Errorf("buf = %v, want %v", got, want)
		}
	})

	t.Run("hdr0_无头_带NUL填充", func(t *testing.T) {
		// 无头布局没有长度字段，读侧靠"尾部 NUL"判断字符串结束，
		// 所以写入侧必须把剩余空间填 NUL，否则会读出上次的残留数据。
		got := buildStringWriteBuf(0, 0, 8, content)
		want := []byte{'H', 'E', 'L', 'L', 'O', 0x00, 0x00, 0x00}
		if !bytesEqual(got, want) {
			t.Errorf("buf = %v, want %v（无头布局必须 NUL 填充到 capacity）", got, want)
		}
	})
}

// TestBuildStringWriteBuf_CurLenIsBytesNotRunes 关键回归：
// curLen 必须是**字节数**，不是字符数。
//
// GBK 下 1 个汉字 2 字节，UTF-8 下 3 字节。
// 如果写字符数，PLC 会只读到半截汉字 —— 这是"写进去乱码/半截"的经典根因。
func TestBuildStringWriteBuf_CurLenIsBytesNotRunes(t *testing.T) {
	t.Run("GBK_3汉字_6字节", func(t *testing.T) {
		encoded := charset.EncodeWith("程序号", "gbk")
		if len(encoded) != 6 {
			t.Fatalf("GBK 编码长度 = %d, want 6（3 个汉字 × 2 字节）", len(encoded))
		}
		buf := buildStringWriteBuf(2, 254, 254, encoded)
		// curLen 必须是 6（字节），不是 3（字符）
		if buf[1] != 6 {
			t.Errorf("curLen = %d, want 6（字节数，不是字符数）", buf[1])
		}
	})

	t.Run("UTF8_3汉字_9字节", func(t *testing.T) {
		encoded := charset.EncodeWith("程序号", "utf-8")
		if len(encoded) != 9 {
			t.Fatalf("UTF-8 编码长度 = %d, want 9（3 个汉字 × 3 字节）", len(encoded))
		}
		buf := buildStringWriteBuf(2, 254, 254, encoded)
		if buf[1] != 9 {
			t.Errorf("curLen = %d, want 9（字节数，不是字符数）", buf[1])
		}
	})
}

// TestStringWriteReadRoundTrip 端到端往返：写入的字节 → 读取路径读回 → 必须完全相等。
//
// 这是最重要的一条：它保证写进去什么，PLC 侧（用同样的头部语义）读出来就是什么。
func TestStringWriteReadRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		hdr  int
		// maxLen 是 PLC 侧 header 声明的 maxLen（0 = 未声明）。
		maxLen int
		// readChars 是点位配置的 StringLength（字符数），
		// 决定读侧实际读多少字节（dataTypeByteSize）。
		readChars int
		encoding  string
		value     string
	}{
		{"ASCII_hdr4", 4, 16, 16, "", "HELLO"},
		{"ASCII_hdr2", 2, 254, 254, "", "HELLO"},
		{"ASCII_hdr1", 1, 0, 64, "", "HELLO"},
		{"ASCII_hdr0", 0, 0, 64, "", "HELLO"},
		{"GBK_hdr2", 2, 254, 254, "gbk", "程序号"},
		{"GBK_hdr4", 4, 16, 16, "gbk", "程序号"},
		{"UTF8_hdr2", 2, 254, 254, "utf-8", "程序号"},
		{"Empty_hdr2", 2, 254, 254, "", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			encoded := charset.EncodeWith(tc.value, tc.encoding)
			capacity := tc.maxLen
			if capacity <= 0 {
				capacity = 64
			}
			content := encoded
			if len(content) > capacity {
				content = content[:capacity]
			}

			buf := buildStringWriteBuf(tc.hdr, tc.maxLen, capacity, content)

			// 模拟真实读取：读侧按 dataTypeByteSize("string", nChars, hdr)
			// 精确读 hdr+nChars 字节 —— 这才是 PLC 侧真实会发生的事。
			//
			// 关键：nChars 来自点位配置 StringLength（字符数），
			// 但读回来的是**字节**。GBK 下 nChars 个汉字占 2*nChars 字节，
			// 所以读侧会多读一倍的字节 —— 这是现有读取路径的既有语义，
			// 写入侧必须与之兼容（curLen 用字节数，内容按字节截断）。
			readSize := dataTypeByteSize("string", tc.readChars, tc.hdr)
			padded := make([]byte, readSize)
			copy(padded, buf)
			// PLC 侧不清零：未写到的区域保留旧数据
			for i := len(buf); i < len(padded); i++ {
				padded[i] = 'X'
			}

			got, err := decodeS7ValueSpan("string", padded, 0, 0, 0, tc.hdr, tc.encoding)
			if err != nil {
				t.Fatalf("decodeS7ValueSpan: %v", err)
			}
			if got.String != tc.value {
				t.Errorf("往返后 = %q, want %q（写入字节 = %v）", got.String, tc.value, buf)
			}
		})
	}
}

// TestStringWriteRoundTrip_NoResidueLeak 写短内容时，尾部旧数据不能被读出来。
//
// 场景：PLC 侧 STRING 区是 "OLDOLD..."，我们写入 "HI"（curLen=2）。
// 期望：读回来是 "HI"，不是 "HIOLDOLD"。
func TestStringWriteRoundTrip_NoResidueLeak(t *testing.T) {
	content := []byte("HI")
	buf := buildStringWriteBuf(2, 254, 254, content)

	padded := make([]byte, 64)
	copy(padded, buf)
	for i := len(buf); i < len(padded); i++ {
		padded[i] = 'X'
	}

	got, err := decodeS7ValueSpan("string", padded, 0, 0, 0, 2, "")
	if err != nil {
		t.Fatalf("decodeS7ValueSpan: %v", err)
	}
	if got.String != "HI" {
		t.Errorf("往返后 = %q, want %q（不该带出尾部旧数据）", got.String, "HI")
	}
}

// TestParseStringMaxLen 验证头部 maxLen 解析与读取路径对称。
func TestParseStringMaxLen(t *testing.T) {
	t.Run("hdr4_大端uint16", func(t *testing.T) {
		head := []byte{0x00, 0x10, 0x00, 0x05}
		if got := parseStringMaxLen(head, 4); got != 16 {
			t.Errorf("maxLen = %d, want 16", got)
		}
	})

	t.Run("hdr2_单字节", func(t *testing.T) {
		head := []byte{0xFE, 0x05}
		if got := parseStringMaxLen(head, 2); got != 254 {
			t.Errorf("maxLen = %d, want 254", got)
		}
	})

	t.Run("hdr1_无maxLen字段", func(t *testing.T) {
		if got := parseStringMaxLen([]byte{0x05}, 1); got != 0 {
			t.Errorf("maxLen = %d, want 0（hdr=1 没有 maxLen 字段）", got)
		}
	})

	t.Run("hdr0_无头", func(t *testing.T) {
		if got := parseStringMaxLen([]byte{}, 0); got != 0 {
			t.Errorf("maxLen = %d, want 0", got)
		}
	})

	t.Run("缓冲不足_返回0", func(t *testing.T) {
		if got := parseStringMaxLen([]byte{0x00}, 4); got != 0 {
			t.Errorf("maxLen = %d, want 0（缓冲不足）", got)
		}
	})
}

// TestBuildStringWriteBuf_MaxLenFallback 验证 maxLen 缺失/越界时的降级行为。
//
// PLC 侧没写 maxLen（头未初始化）时，用本次 capacity 兜底，
// 保证写出的头部至少是自洽的（maxLen >= curLen）。
func TestBuildStringWriteBuf_MaxLenFallback(t *testing.T) {
	content := []byte("HELLO")

	t.Run("hdr4_maxLen为0_退回capacity", func(t *testing.T) {
		got := buildStringWriteBuf(4, 0, 64, content)
		if binary.BigEndian.Uint16(got[0:2]) != 64 {
			t.Errorf("maxLen = %d, want 64（退回 capacity）", binary.BigEndian.Uint16(got[0:2]))
		}
		if binary.BigEndian.Uint16(got[2:4]) != 5 {
			t.Errorf("curLen = %d, want 5", binary.BigEndian.Uint16(got[2:4]))
		}
	})

	t.Run("hdr2_maxLen超255_退回capacity", func(t *testing.T) {
		got := buildStringWriteBuf(2, 1000, 64, content)
		if got[0] != 64 {
			t.Errorf("maxLen = %d, want 64（超出 1 字节范围，退回 capacity）", got[0])
		}
	})
}

// TestCharsetEncodeWith 验证编码器行为。
func TestCharsetEncodeWith(t *testing.T) {
	t.Run("ASCII_原样", func(t *testing.T) {
		got := charset.EncodeWith("HELLO", "")
		if string(got) != "HELLO" {
			t.Errorf("got %q, want %q", got, "HELLO")
		}
	})

	t.Run("auto_中文走GBK", func(t *testing.T) {
		got := charset.EncodeWith("程序号", "")
		if len(got) != 6 {
			t.Errorf("长度 = %d, want 6（auto 下中文按 GBK 编码）", len(got))
		}
	})

	t.Run("utf8_强制UTF8", func(t *testing.T) {
		got := charset.EncodeWith("程序号", "utf-8")
		if len(got) != 9 {
			t.Errorf("长度 = %d, want 9", len(got))
		}
	})

	t.Run("空串", func(t *testing.T) {
		if got := charset.EncodeWith("", ""); len(got) != 0 {
			t.Errorf("长度 = %d, want 0", len(got))
		}
	})

	t.Run("GBK往返可还原", func(t *testing.T) {
		orig := "程序号ABC"
		encoded := charset.EncodeWith(orig, "gbk")
		decoded := charset.DecodeWith(encoded, "gbk")
		if decoded != orig {
			t.Errorf("往返 = %q, want %q", decoded, orig)
		}
	})
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
