// Package charset 提供 PLC 字符串字节 → Go string 的解码。
//
// 背景（2026-10-24）：S7 STRING 读出来的是**原始字节**，之前直接
// `string(bytes)` 丢给前端。多数西门子 PLC 的字符串区用的是 GBK/CP936
// （中文 Windows 环境 + STEP7 默认），不是 UTF-8 —— 于是 GBK 字节被当成
// UTF-8 解释，每个非法字节变成 U+FFFD，界面显示成 "???"。
//
// 为什么是"自动探测"而不是写死 GBK：
//   - PLC 侧编码不统一：有的 PLC 用 GBK，有的用 UTF-8，有的字符串区本来就是 ASCII。
//   - 写死 GBK 会把 UTF-8 的中文再解错一次。
//   - ASCII / UTF-8 / GBK 三者的字节序列有足够区分度，可以可靠探测。
//
// 解码顺序：
//  1. 纯 ASCII                        → 原样（覆盖绝大多数场景，零开销）
//  2. 合法 UTF-8                      → 原样
//  3. 其余（典型 GBK/CP936 双字节）   → 按 GBK 解码
//
// 注意：GBK 解码失败时逐字节回退为 '?'，避免丢数据。
package charset

import (
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"
)

// Decode 把 PLC 返回的原始字节解码成 Go string。
//
// 自动选择编码：ASCII → UTF-8 → GBK。
func Decode(b []byte) string {
	return DecodeWith(b, "")
}

// DecodeWith 按指定编码解码；encoding 为 "" / "auto" 时自动探测。
//
// 支持："utf-8" / "gbk"（别名 "cp936"、"gb18030"）。
// 未知编码名按自动探测处理（不报错，避免一条脏配置搞垮整条采集链路）。
func DecodeWith(b []byte, encoding string) string {
	if len(b) == 0 {
		return ""
	}
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "utf-8", "utf8":
		return decodeUTF8(b)
	case "gbk", "cp936", "gb18030":
		return decodeGBK(b)
	default:
		// "" / "auto" / 未知 → 自动探测
		// 1. 纯 ASCII：最常见，也是唯一 100% 无歧义的情况
		if isASCII(b) {
			return string(b)
		}
		// 2. 合法 UTF-8：含中文但编码就是 UTF-8 的 PLC
		if utf8.Valid(b) {
			return string(b)
		}
		// 3. 兜底：GBK / CP936（西门子 PLC 中文环境最常见）
		return decodeGBK(b)
	}
}

// decodeUTF8 按 UTF-8 解码；非法字节用 U+FFFD 替换（不丢字节位置）。
func decodeUTF8(b []byte) string {
	return strings.ToValidUTF8(string(b), "�")
}

// StartsMultiByte 判断 b 的第一个字节是否属于一个**多字节字符**
// （首字节，或说明"正切在字符中间"的续字节）。
//
// 用途（2026-10-24）：判断 S7 STRING 的 curLen 是否填错了。
//
// 实测（HslCommunicationDemo）：
//   - curLen 正确时，curLen 位置上是 NUL 填充或 ASCII 残留 → StartsMultiByte=false
//   - curLen 填成**字符数**时，curLen 位置上正是一个多字节字符的首字节
//     （例："按说阿斯顿" 15 字节 UTF-8，curLen 填 6 → 第 7 字节是 0xE9）→ true
//
// 纯 ASCII 内容永远返回 false（不会误触发延伸）。
func StartsMultiByte(b []byte, encoding string) bool {
	if len(b) == 0 {
		return false
	}
	c := b[0]
	if c < 0x80 {
		return false // ASCII
	}
	// 续字节：说明切在字符中间
	if c&0xC0 == 0x80 {
		return true
	}
	// 首字节：确认它能起头一个合法字符
	return NextMultiByteLen(b, encoding) > 0
}

// NextMultiByteLen 返回 b 开头那个**多字节**字符的字节长度。
//
// 返回 0 表示：b 开头是 ASCII、不是合法多字节字符、或 b 为空。
func NextMultiByteLen(b []byte, encoding string) int {
	if len(b) == 0 {
		return 0
	}
	c := b[0]
	if c < 0x80 {
		return 0 // ASCII：不是多字节字符
	}
	// 尝试按不同编码确定这个字符的长度
	for _, l := range multiByteCandidateLens(c, encoding) {
		if l > len(b) {
			return 0
		}
		// 该长度必须能解出恰好 1 个字符，且不含替换字符
		dec := DecodeWith(b[:l], encoding)
		runes := []rune(dec)
		if len(runes) != 1 || strings.ContainsRune(dec, '�') {
			continue
		}
		return l
	}
	return 0
}

// multiByteCandidateLens 给定首字节，返回可能的字符长度候选（按优先级）。
//
// UTF-8：首字节决定长度（0xC0-0xDF→2, 0xE0-0xEF→3, 0xF0-0xF7→4）。
// GBK ：双字节，高位字节 0x81-0xFE → 长度 2。
func multiByteCandidateLens(c byte, encoding string) []int {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "utf-8", "utf8":
		switch {
		case c&0xE0 == 0xC0:
			return []int{2}
		case c&0xF0 == 0xE0:
			return []int{3}
		case c&0xF8 == 0xF0:
			return []int{4}
		}
		return nil
	case "gbk", "cp936", "gb18030":
		if c >= 0x81 && c <= 0xFE {
			return []int{2}
		}
		return nil
	default:
		// auto：两种都试，UTF-8 优先（因为 UTF-8 前导字节区间更窄、更明确）
		switch {
		case c&0xE0 == 0xC0:
			return []int{2}
		case c&0xF0 == 0xE0:
			return []int{3}
		case c&0xF8 == 0xF0:
			return []int{4}
		}
		if c >= 0x81 && c <= 0xFE {
			return []int{2}
		}
		return nil
	}
}

// isASCII 判断字节序列是否全部是 ASCII（< 0x80）。
func isASCII(b []byte) bool {
	for _, c := range b {
		if c >= 0x80 {
			return false
		}
	}
	return true
}

// decodeGBK 按 GBK 解码；解码失败的字节回退为 '?'，保证不丢数据、不 panic。
func decodeGBK(b []byte) string {
	dec := simplifiedchinese.GBK.NewDecoder()
	out, err := dec.Bytes(b)
	if err != nil {
		// GBK 解码器对无效序列返回 error 且部分结果为 nil，
		// 这种情况下逐字节尝试，保证至少能给出可读结果。
		return decodeGBKFallback(b)
	}
	return string(out)
}

// decodeGBKFallback 是 decodeGBK 的兜底：逐字节尝试，
// 单字节失败就替换为 '?'，双字节序列失败则整体跳过。
func decodeGBKFallback(b []byte) string {
	var sb []rune
	for i := 0; i < len(b); {
		if b[i] < 0x80 {
			sb = append(sb, rune(b[i]))
			i++
			continue
		}
		// 尝试双字节 GBK
		if i+1 < len(b) {
			r, size := decodeGBKTwo(b[i], b[i+1])
			if size > 0 {
				sb = append(sb, r)
				i += size
				continue
			}
		}
		sb = append(sb, '?')
		i++
	}
	return string(sb)
}

// decodeGBKTwo 尝试把两个字节当作一个 GBK 双字节字符解码。
// 成功返回 (rune, 2)，失败返回 (0, 0)。
func decodeGBKTwo(hi, lo byte) (rune, int) {
	// GBK 双字节范围：首字节 0x81~0xFE，次字节 0x40~0xFE（不含 0x7F）
	if hi < 0x81 || hi > 0xFE || lo < 0x40 || lo > 0xFE || lo == 0x7F {
		return 0, 0
	}
	dec := simplifiedchinese.GBK.NewDecoder()
	out, err := dec.Bytes([]byte{hi, lo})
	if err != nil || len(out) == 0 {
		return 0, 0
	}
	r := []rune(string(out))
	if len(r) != 1 {
		return 0, 0
	}
	return r[0], 2
}

// EncodeWith 按指定编码把 Go string 编码成 PLC 侧字节。
//
// encoding 为 "" / "auto" 时的策略（与 DecodeWith 的自动探测对称）：
//   - 纯 ASCII            → 原样输出（零损耗，与任何 PLC 编码兼容）
//   - 含非 ASCII 字符     → 按 **GBK/CP936** 编码
//
// 为什么 auto 写默认 GBK 而不是 UTF-8：
// 绝大多数西门子 PLC（中文 Windows + STEP7 默认）字符串区是 GBK/CP936。
// 读的时候自动探测能兼容两边，但写必须确定 —— 写 UTF-8 到 GBK 区域
// 会让 PLC 侧中文变成乱码；写 GBK 到 UTF-8 区域则会让 PLC 显示乱码。
// 由于绝大多数现场是 GBK，auto 默认 GBK；确实是 UTF-8 的现场
// 通过 string_encoding="utf-8" 显式指定。
//
// GBK 编码失败的字符（生僻字/emoji）会被替换为 '?'，不返回 error ——
// 写入路径不应该因为一个不可编码的字符而整体失败，替换更符合工业现场预期。
func EncodeWith(s string, encoding string) []byte {
	if s == "" {
		return []byte{}
	}
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "utf-8", "utf8":
		return []byte(s)
	case "gbk", "cp936", "gb18030":
		return encodeGBK(s)
	default:
		// "" / "auto" / 未知
		if isASCII([]byte(s)) {
			return []byte(s)
		}
		return encodeGBK(s)
	}
}

// Encode 是 EncodeWith 的自动探测版本。
func Encode(s string) []byte { return EncodeWith(s, "") }

// encodeGBK 按 GBK 编码；不可编码的字符替换为 '?'。
func encodeGBK(s string) []byte {
	enc := simplifiedchinese.GBK.NewEncoder()
	out, err := enc.Bytes([]byte(s))
	if err == nil {
		return out
	}
	// 兜底：逐字符编码，单个字符失败就替换为 '?'。
	var buf []byte
	for _, r := range s {
		b, err := enc.Bytes([]byte(string(r)))
		if err != nil || len(b) == 0 {
			buf = append(buf, '?')
			continue
		}
		buf = append(buf, b...)
	}
	return buf
}
