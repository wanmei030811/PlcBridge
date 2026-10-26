// Plan2 主题 A 3.1 词汇表单元测试。
//
// 目的：
//  1. 保证 13 个 Plan §8.5 数据类型全部通过（防止漏列）
//  2. 保证非法值不通过（防止误放宽）
//  3. 锁定词汇表大小为 13（防止新增/删除时静默漂移）
//
// 测试方法：纯函数，零依赖，go test ./internal/platform/ 即可。
package platform

import "testing"

// TestIsValidDataType_AllKnownTypes 锁定 13 个合法值。
//
// 若 Plan §8.5 词汇表扩容/缩容，必须同步改这里 + 改 datatypes.go。
func TestIsValidDataType_AllKnownTypes(t *testing.T) {
	known := []string{
		"bool", "int8", "uint8",
		"int16", "uint16", "int32", "uint32",
		"int64", "uint64",
		"float32", "float64",
		"string", "bytes",
	}
	for _, dt := range known {
		if !IsValidDataType(dt) {
			t.Errorf("IsValidDataType(%q) = false, want true (词汇表漏列?)", dt)
		}
	}
}

// TestIsValidDataType_UnknownTypes 锁定非法值全部拒绝。
func TestIsValidDataType_UnknownTypes(t *testing.T) {
	unknown := []string{
		"",         // 空字符串
		"Bool",     // 大小写错
		"int",      // 缩写
		"int128",   // 超过 Plan §8.5 范围
		"double",   // 别名
		"string32", // 带长度后缀
		"bcd16",    // 私有扩展
		"datetime", // 未声明
	}
	for _, dt := range unknown {
		if IsValidDataType(dt) {
			t.Errorf("IsValidDataType(%q) = true, want false (词汇表过宽?)", dt)
		}
	}
}

// TestAllDataTypes 锁定返回列表数量 = 13。
//
// 若 IsValidDataType / AllDataTypes 数量不一致，立即爆掉。
func TestAllDataTypes(t *testing.T) {
	all := AllDataTypes()
	if len(all) != 13 {
		t.Errorf("AllDataTypes() len = %d, want 13 (词汇表数量漂移)", len(all))
	}
	// 每个返回值都应通过 IsValidDataType
	for _, dt := range all {
		if !IsValidDataType(dt) {
			t.Errorf("AllDataTypes() 含非法值 %q", dt)
		}
	}
}

// TestIsValidDataType_ConsistentWithConstants 锁定常量字符串 = 词汇表键。
//
// 防止有人改了常量（如 DataTypeFloat32 = "f32"）但忘了更新 allDataTypes。
func TestIsValidDataType_ConsistentWithConstants(t *testing.T) {
	constants := []string{
		DataTypeBool, DataTypeInt8, DataTypeUint8,
		DataTypeInt16, DataTypeUint16, DataTypeInt32, DataTypeUint32,
		DataTypeInt64, DataTypeUint64,
		DataTypeFloat32, DataTypeFloat64,
		DataTypeString, DataTypeBytes,
	}
	for _, c := range constants {
		if !IsValidDataType(c) {
			t.Errorf("IsValidDataType(%q) = false, want true (常量与词汇表不一致)", c)
		}
	}
}
