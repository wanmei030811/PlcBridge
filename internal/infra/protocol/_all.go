// Package protocol 是 PlcBridge 的协议插件层。
//
// _all.go 是协议注册的"事实源"：
//   - 本文件 import 所有内置 plugin 子包，触发各 init() 自注册
//   - 加新协议：1) 新建 plugin 子目录并实现 ProtocolPlugin  2) 在本文件加一行 import
//   - 不需要改 main.go / handler / 前端
//
// 顺序：s7 → modbus → opcua → fake（fake 仅测试用，生产环境不导入）
package protocol

import (
	// 内置 collect 协议（role=collect）
	_ "github.com/plcbridge/plcbridge/internal/infra/protocol/modbus_tcp"
	_ "github.com/plcbridge/plcbridge/internal/infra/protocol/opcua"
	_ "github.com/plcbridge/plcbridge/internal/infra/protocol/s7"
)
