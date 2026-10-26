// Package plugins 是协议插件的"注册事实源"。
//
// 为什么是独立包而不是 protocol 包内的 _all.go：
//
//  1. Go 工具链**忽略以 _ 开头的文件**（如 _all.go），放在 protocol 包内根本不会被编译，
//     导致插件的 init() 从未执行 → protocol.Get("s7") 永远失败。
//  2. protocol/s7 等子包 import 了 protocol 父包（实现 ProtocolPlugin 接口），
//     父包再 import 子包会形成**导入循环**，Go 编译器直接报错。
//
// 因此把"聚合所有内置插件"的职责放到独立的 plugins 包：
//   - plugins → protocol/s7 → protocol （单向，无循环）
//   - 谁需要协议被注册，就 import _ ".../protocol/plugins"
//
// 加新协议：1) 新建 protocol/<name>/ 子包并实现 ProtocolPlugin
//
//  2. 在本文件加一行 import
//
// 不需要改 main.go / handler / 前端（Plan2 主题 A 3.2 零改动契约）。
package plugins

import (
	// 内置 collect 协议（role=collect）
	_ "github.com/plcbridge/plcbridge/internal/infra/protocol/modbus_tcp"
	_ "github.com/plcbridge/plcbridge/internal/infra/protocol/opcua"
	_ "github.com/plcbridge/plcbridge/internal/infra/protocol/s7"
)
