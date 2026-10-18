// Package timefmt 定义全局统一的"北京时间"格式化与序列化约定。
//
// 单一事实源：所有对外暴露的时间字段（REST API、MQTT payload、log 等）都按北京时间
// （CST / Asia/Shanghai / UTC+8）展示给用户，不混用 UTC / 容器时区。
//
// 设计要点：
//   - 用 time.FixedZone 固定 +08:00，不依赖外部 tzdata
//   - BJTLayout 是面向人读的固定格式：yyyy-MM-dd HH:mm:ss.SSS +0800
//   - BJT 序列化为 int64 unix 毫秒（值稳定），JSON 渲染走 MarshalJSON → 北京时间字符串
package timefmt

import (
	"strconv"
	"time"
)

// BJT 固定时区：UTC+8（北京时间）。
var BJT = time.FixedZone("CST", 8*60*60)

// BJTLayout 北京时间人读格式（含毫秒和时区后缀）。
const BJTLayout = "2006-01-02 15:04:05.000 -0700"

// FormatTime 把 time.Time 格式化为北京时间字符串。
// 输入为 0 值时返回空串。
func FormatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.In(BJT).Format(BJTLayout)
}

// FormatMs 把 epoch 毫秒格式化为北京时间字符串；ms=0 返回空串。
func FormatMs(ms int64) string {
	if ms == 0 {
		return ""
	}
	return FormatTime(time.UnixMilli(ms))
}

// BJTMillis 是一个 int64 epoch ms，但 JSON 渲染时输出北京时间字符串。
//
// 用法：
//
//	type X struct {
//	    CreatedAt BJTMillis `json:"created_at"`
//	}
//
// 0 值序列化为 null（保持 Go 零值语义）；非 0 序列化为字符串。
type BJTMillis int64

// MarshalJSON 实现自定义 JSON 序列化。
func (m BJTMillis) MarshalJSON() ([]byte, error) {
	if m == 0 {
		return []byte("null"), nil
	}
	return strconv.AppendQuote(nil, FormatTime(time.UnixMilli(int64(m)))), nil
}

// UnmarshalJSON 接受数字（ms）或北京时间字符串，反序列化为 ms。
//
// 支持：
//   - 数字（与原 int64 兼容）
//   - 北京时间字符串（与 MarshalJSON 输出对齐）
//   - RFC3339（兼容历史数据）
func (m *BJTMillis) UnmarshalJSON(b []byte) error {
	if len(b) == 0 || string(b) == "null" {
		*m = 0
		return nil
	}
	if b[0] == '"' {
		// 字符串：先试 BJTLayout，再 fallback RFC3339
		s := string(b[1 : len(b)-1])
		if t, err := time.ParseInLocation(BJTLayout, s, BJT); err == nil {
			*m = BJTMillis(t.UnixMilli())
			return nil
		}
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			*m = BJTMillis(t.UnixMilli())
			return nil
		}
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			*m = BJTMillis(t.UnixMilli())
			return nil
		}
		// 全部解析失败：保持 0
		*m = 0
		return nil
	}
	// 数字
	v, err := strconv.ParseInt(string(b), 10, 64)
	if err != nil {
		return err
	}
	*m = BJTMillis(v)
	return nil
}

// Time 返回底层 time.Time（已转 BJT 时区）。
func (m BJTMillis) Time() time.Time {
	return time.UnixMilli(int64(m)).In(BJT)
}

// Int64 返回底层 epoch ms。
func (m BJTMillis) Int64() int64 {
	return int64(m)
}
