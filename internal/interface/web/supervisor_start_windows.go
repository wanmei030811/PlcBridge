//go:build windows

// supervisor_start_windows.go —— Windows 下拉起 supervisor 的具体实现。
//
// 背景（2026-10-26 修 CI 阻断项）：
//
//	原来的 startSupervisorIfMissing 写在平台无关的 process_control.go 里，
//	却用了 syscall.SysProcAttr{HideWindow: true} —— HideWindow 是 Windows 专属字段，
//	导致 Linux 交叉编译直接失败：
//
//	unknown field HideWindow in struct literal of type syscall.SysProcAttr
//
//	这个阻断项在本地 Windows 开发机上永远看不到，只有 CI 的 Linux 构建才会暴露。
//
// 为什么用 cmd.exe /c start /B 而不是直接 Start powershell：
//
//	"start /B" 让 powershell 在**当前控制台**的后台启动，不弹新窗口；
//	直接 exec powershell 会继承父进程的控制台句柄，在某些部署方式下 web 退出时
//	会连带拖死 supervisor。
//
// 为什么必须 HideWindow：
//
//	没有它，cmd.exe 启动时会短暂闪现一个黑框，scanner / 无人值守部署下很碍眼。
package web

import (
	"os/exec"
	"syscall"
)

// launchSupervisorDetached 以 detached 方式启动 supervisor 脚本（Windows 实现）。
//
// 参数：
//   - projectRoot：项目根目录，传给 supervisor 的 -Root
//   - psScript：supervisor.ps1 的绝对路径
//   - configDir：配置目录，传给 supervisor 的 -ConfigDir（state 落盘位置）
//
// 调用方（startSupervisorIfMissing）已确认：
//   - supervisor 当前未在运行
//   - psScript 文件存在
//
// 返回值：仅表示"进程是否成功 spawn"，不表示 supervisor 业务是否启动成功
// （supervisor 自己会异步写 state 文件，由 isSupervisorRunning 轮询确认）。
func launchSupervisorDetached(projectRoot, psScript, configDir string) error {
	cmd := exec.Command("cmd.exe", "/c", "start", "/B",
		"powershell.exe", "-NoProfile", "-ExecutionPolicy", "Bypass",
		"-File", psScript, "-Root", projectRoot, "-ConfigDir", configDir)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return cmd.Start()
}
