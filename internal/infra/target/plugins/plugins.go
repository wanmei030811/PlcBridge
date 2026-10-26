// Package plugins 是 Target 插件的"注册事实源"。
//
// 为什么是独立包而不是 target 包内的 _all.go：
//
//  1. Go 工具链**忽略以 _ 开头的文件**（如 _all.go），放在 target 包内根本不会被编译。
//  2. target/mqtt 等子包 import 了 target 父包，父包再 import 子包会形成**导入循环**。
//
// 因此把"聚合所有内置 Target"的职责放到独立的 plugins 包：
//   - plugins → target/mqtt → target （单向，无循环）
//   - 谁需要 Target 被注册，就 import _ ".../target/plugins"
//
// 加新 Target：1) 新建 target/<name>/ 子包并实现 TargetPlugin
//
//  2. 在本文件加一行 import
//
// 不需要改 main.go / handler / 前端（Plan2 主题 A 3.2 零改动契约）。
package plugins

import (
	// 内置 forward Target（role=forward）
	_ "github.com/plcbridge/plcbridge/internal/infra/target/influxdb" // Register → "influxdb"
	_ "github.com/plcbridge/plcbridge/internal/infra/target/mqtt"     // Register → "mqtt"
)
