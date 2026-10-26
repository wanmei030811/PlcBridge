//go:build !windows

// supervisor_start_other.go —— 非 Windows 平台下的 supervisor 启动实现。
//
// 背景（2026-10-26 修 CI 阻断项）：
//
//	原来的 startSupervisorIfMissing 写在平台无关文件里，却用了
//	syscall.SysProcAttr{HideWindow: true} —— HideWindow 是 Windows 专属字段，
//	导致 Linux 交叉编译失败：
//
//	unknown field HideWindow in struct literal of type syscall.SysProcAttr
//
//	修法：把"如何拉起 detached 进程"按平台拆开，本文件负责非 Windows。
//
// 非 Windows 下为什么直接返回错误（而不是尝试真的启动）：
//
//	supervisor 是 PowerShell 脚本（scripts/supervisor.ps1），依赖 Windows PowerShell
//	的语义（.NET 运行时、注册表、计划任务等）。在没有 pwsh 的 Linux 机器上，
//	"假装启动成功"只会让 dashboard 显示 supervisor 在线、实际却没有守护进程，
//	比明确报错更难排查。
//
//	如果将来确实要在 Linux 上跑 supervisor，正确做法是：
//	  1. 新增 supervisor.sh（等价语义的 shell 实现）
//	  2. 这里的实现改为 exec.Command("sh", supervisorSh, ...)
//	  3. detectProjectRoot 同时探测 supervisor.sh
//	在那之前，明确报"当前平台不支持"是诚实且可诊断的行为。
package web

import (
	"fmt"
	"runtime"
)

// launchSupervisorDetached 以 detached 方式启动 supervisor 脚本（非 Windows 实现）。
//
// 当前实现直接返回错误：supervisor 依赖 Windows PowerShell，非 Windows 平台
// 无法真正拉起它。调用方（startSupervisorIfMissing）会把这个错误返回给
// dashboard，前端据此提示"当前平台不支持自动启动 supervisor"。
func launchSupervisorDetached(projectRoot, psScript, configDir string) error {
	return fmt.Errorf(
		"当前平台（%s）不支持自动启动 supervisor：supervisor 依赖 Windows PowerShell 脚本 "+
			"(scripts/supervisor.ps1)，请手动启动或改用 Windows 部署",
		runtime.GOOS)
}
