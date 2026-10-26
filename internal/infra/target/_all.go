// Package target 是 PlcBridge 的下游 Target 插件层。
//
// _all.go 是 Target 注册的"事实源"：
//   - 本文件 import 所有内置 Target 子包，触发各 init() 自注册
//   - 加新 Target：1) 新建 Target 子包并实现 TargetPlugin  2) 在本文件加一行 import
//   - 不需要改 main.go / handler / 前端
package target

import (
	// 内置 forward Target（role=forward）
	_ "github.com/plcbridge/plcbridge/internal/infra/target/influxdb" // Register → "influxdb"
	_ "github.com/plcbridge/plcbridge/internal/infra/target/mqtt"     // Register → "mqtt"
)
