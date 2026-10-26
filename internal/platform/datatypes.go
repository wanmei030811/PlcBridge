// 点位 data_type 词汇表（Plan.md §8.5）。
//
// 数量与定义**内核恒定**，由"协议层与业务层的共同协议"决定，不允许 plugin 扩展：
//   - 新 data_type 必须改这里 + Plan.md §8.5 + Value 解析逻辑
//   - 任何协议 driver 都不应引入私有 data_type
//
// 为什么放 platform 包：
//   - entities.go 已定义 Channel / Device / Source / Point / Target，data_type 是 Point 的字段
//   - 不引用任何 internal/*，可以被所有包（infra/config / infra/protocol / interface/web）依赖
//   - 与 entities.go 同 package，避免额外导入
package platform

// 13 个 data_type 强枚举（Plan.md §8.5）。
// 注释里的「字节」是协议层在 PLC 上的占用，与 JSON 序列化大小无关。
const (
	DataTypeBool    = "bool"    // 1 bit
	DataTypeInt8    = "int8"    // 1 byte
	DataTypeUint8   = "uint8"   // 1 byte
	DataTypeInt16   = "int16"   // 2 bytes
	DataTypeUint16  = "uint16"  // 2 bytes
	DataTypeInt32   = "int32"   // 4 bytes
	DataTypeUint32  = "uint32"  // 4 bytes
	DataTypeInt64   = "int64"   // 8 bytes
	DataTypeUint64  = "uint64"  // 8 bytes
	DataTypeFloat32 = "float32" // 4 bytes (IEEE 754)
	DataTypeFloat64 = "float64" // 8 bytes (IEEE 754)
	DataTypeString  = "string"  // N bytes (S7 STRING header 4B + N chars, max 55 chars)
	DataTypeBytes   = "bytes"   // N bytes (raw, 无字符编码假设；Plan §8.5 词汇表 13 个)
)

// allDataTypes 词汇表（建表一次，O(1) 查询）。
//
// 用 set（map[string]struct{}）而非 slice，避免每次 IsValidDataType 都要遍历。
// 容量 13 = 当前支持的所有 data_type 数。
var allDataTypes = func() map[string]struct{} {
	m := make(map[string]struct{}, 13)
	for _, t := range []string{
		DataTypeBool, DataTypeInt8, DataTypeUint8,
		DataTypeInt16, DataTypeUint16, DataTypeInt32, DataTypeUint32,
		DataTypeInt64, DataTypeUint64,
		DataTypeFloat32, DataTypeFloat64,
		DataTypeString, DataTypeBytes,
	} {
		m[t] = struct{}{}
	}
	return m
}()

// IsValidDataType 判断 data_type 是否在词汇表内。
//
// 用途：handler CRUD 校验 + config 启动校验（单一事实源）。
// 性能：map 查找 ~20-30ns/次，无锁（词汇表只读，init 时构建一次）。
func IsValidDataType(dt string) bool {
	_, ok := allDataTypes[dt]
	return ok
}

// AllDataTypes 返回所有支持的 data_type（按声明顺序）。
//
// 用途：Web 端点位表单下拉框、API 文档生成、配置校验时的错误提示。
// 返回值是 slice 副本，调用方修改不影响内部状态。
func AllDataTypes() []string {
	out := make([]string, 0, len(allDataTypes))
	for _, t := range []string{
		DataTypeBool, DataTypeInt8, DataTypeUint8,
		DataTypeInt16, DataTypeUint16, DataTypeInt32, DataTypeUint32,
		DataTypeInt64, DataTypeUint64,
		DataTypeFloat32, DataTypeFloat64,
		DataTypeString, DataTypeBytes,
	} {
		out = append(out, t)
	}
	return out
}
