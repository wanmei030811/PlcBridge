// Plan2 主题 C 5.1：Value.AsGoNative 单元测试。
package protocol

import (
	"reflect"
	"testing"
)

// TestValue_AsGoNative_TypeMapping 覆盖 platform.AllDataTypes() 全部 13 个 data_type。
//
// 契约：AsGoNative() 严格按 DataType 返回对应 Go 原生类型，unknown 返回 nil。
// 此测试是"加新 data_type 不破坏应用层"的契约测试。
func TestValue_AsGoNative_TypeMapping(t *testing.T) {
	cases := []struct {
		dataType string
		set      func(v *Value)
		want     any
	}{
		{"bool", func(v *Value) { v.Bool = true }, true},
		{"int8", func(v *Value) { v.Int64 = 1 }, int64(1)},
		{"int16", func(v *Value) { v.Int64 = 2 }, int64(2)},
		{"int32", func(v *Value) { v.Int64 = 3 }, int64(3)},
		{"int64", func(v *Value) { v.Int64 = 4 }, int64(4)},
		{"uint8", func(v *Value) { v.Uint64 = 5 }, uint64(5)},
		{"uint16", func(v *Value) { v.Uint64 = 6 }, uint64(6)},
		{"uint32", func(v *Value) { v.Uint64 = 7 }, uint64(7)},
		{"uint64", func(v *Value) { v.Uint64 = 8 }, uint64(8)},
		{"float32", func(v *Value) { v.Float64 = 1.5 }, 1.5},
		{"float64", func(v *Value) { v.Float64 = 2.5 }, 2.5},
		{"string", func(v *Value) { v.String = "hi" }, "hi"},
		{"bytes", func(v *Value) { v.Bytes = []byte{0x01, 0x02} }, []byte{0x01, 0x02}},
		// 未知类型 → nil（应用层降级）
		{"unknown_type", func(v *Value) {}, nil},
		{"", func(v *Value) {}, nil},
	}
	for _, c := range cases {
		t.Run(c.dataType, func(t *testing.T) {
			v := Value{DataType: c.dataType}
			c.set(&v)
			got := v.AsGoNative()
			// 不可比较类型用 reflect.DeepEqual
			switch want := c.want.(type) {
			case []byte:
				if !reflect.DeepEqual(got, want) {
					t.Errorf("AsGoNative() = %v, want %v", got, want)
				}
			default:
				if got != want {
					t.Errorf("AsGoNative() = %v (%T), want %v (%T)", got, got, want, want)
				}
			}
		})
	}
}
