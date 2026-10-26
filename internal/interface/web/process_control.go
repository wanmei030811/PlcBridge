// process_control.go：仪表盘"启动/停止/重启" 控制面板。
//
// 设计前提：plcbridge-web 是被 scripts/supervisor.ps1 守护的。
// 仪表盘点按钮 → 这里写一条控制消息到 runtime/control.pending.json（原子 rename 写入）
//
//	→ supervisor 每 3s 扫描该文件 → 执行对应动作 → 归档。
//
// 为什么不在 web 端直接 Stop-Process？
//  1. web 自己停止自己后再启动别人需要外部进程介入；
//  2. 进程崩溃后自动重启也是 supervisor 的活；
//  3. 解耦：web 只发指令，不持有任何 PID 句柄。
//
// 关于"重载配置"：
//
//	plcbridge 进程通过 fsnotify 监听 configDir/config.json 变更并自动 reload。
//	因此"重载"= 重新写一次 config.json（哪怕内容不变），plcbridge 会重新解析全量。
//	这条逻辑 web 端直接做即可，不走 supervisor。
//
// 关于"重启 plcbridge-web"：
//
//	web 写一条 {action: "restart_plcbridge-web"}，再异步 graceful shutdown 自己。
//	supervisor 看到 web 退出 → 自动拉新 web。
//	若 supervisor 不在 → web 退出后需用户手动拉起，dashboard 会有 503 提示。
package web

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/plcbridge/plcbridge/internal/infra/logging"
)

// processSelfShutdownCh 全局信号，dashboard 点"重启 web"时向 main loop 发信号。
// 在 main.go 中 select 它做 srv.Shutdown。
var processSelfShutdownCh = make(chan struct{}, 1)

// ProcessSelfShutdownCh 暴露给 main.go 监听（dashboard 重启 web 时使用）。
func ProcessSelfShutdownCh() <-chan struct{} { return processSelfShutdownCh }

// flagsFile —— supervisor 用的 pause 标记文件。web 端写时直接 atomic rename。
// 字段: pause_core=true 表示 dashboard 已让 plcbridge 停止，supervisor 不再拉起。
const flagsFile = "supervisor.flags.json"

type supervisorFlags struct {
	PauseCore bool   `json:"pause_core"`
	PauseWeb  bool   `json:"pause_web"`
	UpdatedAt string `json:"updated_at"`
}

// writeFlags 原子写 supervisor flags 文件（merge 已有字段）。
func writeFlags(runtimeDir string, patch func(*supervisorFlags)) error {
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		return fmt.Errorf("create runtime dir: %w", err)
	}
	flags := supervisorFlags{}
	if b, err := os.ReadFile(filepath.Join(runtimeDir, flagsFile)); err == nil {
		_ = json.Unmarshal(b, &flags)
	}
	patch(&flags)
	flags.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	tmp, err := os.CreateTemp(runtimeDir, "supervisor.flags.tmp.*")
	if err != nil {
		return err
	}
	enc := json.NewEncoder(tmp)
	if err := enc.Encode(&flags); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(runtimeDir, flagsFile))
}

// startSupervisorIfMissing —— dashboard 点"启动 supervisor" 时：若已在线则忽略；
// 否则在 detached 进程里启动 scripts\supervisor.ps1（避免 web 端阻塞）。
//
// 注意：Windows 下 supervisor 用 PowerShell，必须新窗口启动（不能 Wait，否则 web 卡死）。
//
// 参数说明：
//   - configDir：plcbridge 配置目录（含 runtime/ 子目录）
//   - projectRoot：plcbridge 项目根目录（含 scripts/supervisor.ps1、bin/ 等）
//
// 历史 BUG：之前把 configDir 当作 projectRoot 拼路径，导致 dashboard"启动 supervisor"按钮
// 始终找不到 scripts/supervisor.ps1，从未真正工作过 —— 用户点按钮其实是 no-op。
// 修完后：脚本路径用 projectRoot，runtime/state 文件路径仍用 configDir/runtime。
//
// 平台差异（2026-10-26 修）：
//
//	  supervisor 本身是 PowerShell 脚本（scripts/supervisor.ps1），只在 Windows 上有意义。
//	  但 syscall.SysProcAttr.HideWindow 是 **Windows 专属字段**，
//	  写在平台无关文件里会让 Linux 交叉编译直接失败：
//
//		process_control.go:116:41: unknown field HideWindow in struct literal of type syscall.SysProcAttr
//
//	  （这个阻断项在本地 Windows 开发机上永远看不到，只有 CI 的 Linux 构建才会暴露。）
//
//	  修法：公共逻辑留在本文件（跨平台），"如何拉起 detached 进程"下沉到
//	  supervisor_start_windows.go / supervisor_start_other.go，由构建标签选择。
func startSupervisorIfMissing(configDir string) error {
	rt := filepath.Join(configDir, "runtime")
	if isSupervisorRunning(rt) {
		return nil
	}
	projectRoot := detectProjectRoot(configDir)
	psScript := filepath.Join(projectRoot, "scripts", "supervisor.ps1")
	if _, err := os.Stat(psScript); err != nil {
		return fmt.Errorf("supervisor script not found: %s", err)
	}
	// 显式传 -ConfigDir 让 supervisor 把 state 写到 web 端 runtime 所在目录，
	// 否则 supervisor 默认 <Root>/config/runtim（生产部署 configDir 在别处时会找不到）。
	return launchSupervisorDetached(projectRoot, psScript, configDir)
}

// detectProjectRoot 从 configDir 反推项目根目录。
// 解析规则（按顺序命中即返回）：
//  1. 环境变量 PLCBRIDGE_PROJECT_ROOT（手动指定，最优先）
//  2. 父目录存在 scripts/supervisor.ps1 → 父目录
//  3. 父目录的父目录存在 scripts/supervisor.ps1 → 祖父目录（兼容两级嵌套）
//  4. 兜底：直接取 configDir（即便脚本不在，至少能启动 powershell 给用户清晰错误）
func detectProjectRoot(configDir string) string {
	if env := os.Getenv("PLCBRIDGE_PROJECT_ROOT"); env != "" {
		if _, err := os.Stat(filepath.Join(env, "scripts", "supervisor.ps1")); err == nil {
			return env
		}
	}
	parent := filepath.Dir(configDir)
	for _, candidate := range []string{parent, filepath.Dir(parent)} {
		if _, err := os.Stat(filepath.Join(candidate, "scripts", "supervisor.ps1")); err == nil {
			return candidate
		}
	}
	return parent
}

// AutoStartSupervisorIfMissing —— web 启动后兜底：若 supervisor 死了则拉起。
//
// 设计动机：supervisor 自己不能保活自己（它管 web+core，但没人管它），
// 如果 supervisor 异常退出 / stop_supervisor 后，core 就不会被人拉起。
// 由 web（dashboard 控制台）做"supervisor 自身的保活"是闭环内最合适的：web 必须常驻，
// 一旦 web 起来了，就尝试把 supervisor 也拉起来，让 supervisor 接管 core 的保活。
//
// 注意：无竞争风险——supervisor 起来后会"保留 web"，不会重启 web 进程（已修）。
func AutoStartSupervisorIfMissing(configDir string) error {
	return startSupervisorIfMissing(configDir)
}

// runtimeDirFromConfigDir 控制目录 = <configDir>/runtime
func runtimeDirFromConfigDir(configDir string) string {
	return filepath.Join(configDir, "runtime")
}

// supervisorStateFile supervisor 写的状态文件，前端 GET 可知 plcbridge 是否在跑。
const supervisorStateFile = "supervisor.state.json"

type processControlRequest struct {
	Action string `json:"action"`
	Reason string `json:"reason"`
}

type processControlResponse struct {
	OK        bool   `json:"ok"`
	Action    string `json:"action"`
	Message   string `json:"message"`
	Triggered bool   `json:"triggered"` // supervisor 是否在跑（false = 指令可能不会被执行）
}

// isSupervisorRunning 检查 supervisor 是否在跑。
//
// 判定规则:
//  1. state 文件不存在 → 一定没在跑
//  2. status 字段为 "stopped" / "stopping" → 一定没在跑（即使 mtime 很近）
//  3. 否则要求 mtime < 10s —— supervisor 心跳每 3s 写一次 state，超过 10s 没写就是僵死
func isSupervisorRunning(runtimeDir string) bool {
	p := filepath.Join(runtimeDir, supervisorStateFile)
	fi, err := os.Stat(p)
	if err != nil {
		return false
	}
	// 读 status 字段（如果有）
	if b, err := os.ReadFile(p); err == nil {
		var st struct {
			Status string `json:"status"`
		}
		if json.Unmarshal(b, &st) == nil {
			if st.Status == "stopped" || st.Status == "stopping" {
				return false
			}
		}
	}
	return time.Since(fi.ModTime()) < 10*time.Second
}

// writeControlCommand 原子写控制命令：写到 .tmp → os.Rename 覆盖 .pending。
func writeControlCommand(runtimeDir, action string) error {
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		return fmt.Errorf("create runtime dir: %w", err)
	}
	cmd := struct {
		Action string `json:"action"`
		TS     int64  `json:"ts"`
	}{
		Action: action,
		TS:     time.Now().UnixMilli(),
	}
	tmp, err := os.CreateTemp(runtimeDir, "control.pending.tmp.*")
	if err != nil {
		return err
	}
	enc := json.NewEncoder(tmp)
	if err := enc.Encode(cmd); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	pending := filepath.Join(runtimeDir, "control.pending.json")
	return os.Rename(tmp.Name(), pending)
}

// processControlHandler POST /api/v1/process/control
//
// Body: { "action": "reload_config" | "stop_plcbridge" | "start_plcbridge" | "restart_plcbridge-web" | "stop_supervisor" }
func processControlHandler(db *sql.DB, configDir string) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req processControlRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			WriteError(c, ErrInvalidParam.With("invalid json body: "+err.Error()))
			return
		}
		if req.Action == "" {
			WriteError(c, ErrInvalidParam.With("action is required"))
			return
		}

		allowed := map[string]bool{
			"reload_config":         true,
			"export_config":         true,
			"stop_plcbridge":        true,
			"start_plcbridge":       true,
			"restart_plcbridge-web": true,
			"start_supervisor":      true,
			"stop_supervisor":       true,
		}
		if !allowed[req.Action] {
			WriteError(c, ErrInvalidParam.With("unknown action: "+req.Action))
			return
		}

		rt := runtimeDirFromConfigDir(configDir)
		supOK := isSupervisorRunning(rt)

		switch req.Action {
		case "reload_config":
			// 直接复用 exportSnapshot 的内部 buildExport 流程：
			// buildExport → tmp file → rename → fsnotify 触发 plcbridge reload。
			if err := reloadConfigViaExport(c.Request.Context(), db, configDir); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{
					"ok":      false,
					"action":  req.Action,
					"message": err.Error(),
				})
				return
			}
			logging.Info(logging.EventConfigReload).Str("action", req.Action).Msg("process control: 配置已重载")
			c.JSON(http.StatusOK, processControlResponse{
				OK:        true,
				Action:    req.Action,
				Message:   "config.json 已重新导出，plcbridge 将在 1s 内自动热重载",
				Triggered: true,
			})
			return

		case "export_config":
			// 仅导出快照到 configDir/config.json，plcbridge 会通过 fsnotify 自动热重载。
			if err := reloadConfigViaExport(c.Request.Context(), db, configDir); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{
					"ok":      false,
					"action":  req.Action,
					"message": err.Error(),
				})
				return
			}
			logging.Info(logging.EventConfigExport).Str("action", req.Action).Msg("process control: 配置已导出")
			c.JSON(http.StatusOK, processControlResponse{
				OK:        true,
				Action:    req.Action,
				Message:   "config.json 已导出，plcbridge 1s 内自动热重载",
				Triggered: true,
			})
			return

		case "start_supervisor":
			// web 不知道自己是否在 supervisor 监管下：直接尝试 detached 启动。
			// startSupervisorIfMissing 内部会判 isSupervisorRunning。
			if err := startSupervisorIfMissing(configDir); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{
					"ok":      false,
					"action":  req.Action,
					"message": "启动 supervisor 失败: " + err.Error(),
				})
				return
			}
			c.JSON(http.StatusAccepted, processControlResponse{
				OK:        true,
				Action:    req.Action,
				Message:   "已触发启动 supervisor（3s 内会启动 plcbridge-web + plcbridge）",
				Triggered: true,
			})
			return

		case "stop_plcbridge":
			// 同步写 pause flag，否则 supervisor watchdog 会立刻拉起。
			if err := writeFlags(rt, func(f *supervisorFlags) { f.PauseCore = true }); err != nil {
				logging.Warn(logging.EventConfigReloadFail).Str("err", err.Error()).Msg("write pause flag failed")
			}
		case "start_plcbridge":
			// 取消 pause，然后写 control；supervisor 看到 pause=false 会拉起。
			if err := writeFlags(rt, func(f *supervisorFlags) { f.PauseCore = false }); err != nil {
				logging.Warn(logging.EventConfigReloadFail).Str("err", err.Error()).Msg("write pause flag failed")
			}
		}

		// 以下 action 写 control 文件
		if err := writeControlCommand(rt, req.Action); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{
				"ok":      false,
				"action":  req.Action,
				"message": "写控制文件失败: " + err.Error(),
			})
			return
		}

		logging.Info(logging.EventConfigReload).
			Str("action", req.Action).
			Bool("supervisor_active", supOK).
			Str("reason", req.Reason).
			Msg("process control: 已发指令")

		if req.Action == "restart_plcbridge-web" {
			// 写完指令后异步让 web 自己退出。supervisor 巡检 3s 一次，等 500ms 让其下一轮 tick 读到。
			go func() {
				time.Sleep(500 * time.Millisecond)
				select {
				case processSelfShutdownCh <- struct{}{}:
				default:
				}
			}()
		}

		msg := "指令已写入 runtime/control.pending.json"
		if !supOK {
			msg += "（⚠️ 未检测到 supervisor 在运行，指令可能不会被执行）"
		}
		c.JSON(http.StatusAccepted, processControlResponse{
			OK:        true,
			Action:    req.Action,
			Message:   msg,
			Triggered: supOK,
		})
	}
}

// supervisorStatusHandler GET /api/v1/process/supervisor
//
// 返回 supervisor 状态信息（用于仪表盘按钮的 enable/disable + 显示）。
func supervisorStatusHandler(configDir string) gin.HandlerFunc {
	return func(c *gin.Context) {
		rt := runtimeDirFromConfigDir(configDir)
		statePath := filepath.Join(rt, supervisorStateFile)
		out := gin.H{
			"supervisor_running": isSupervisorRunning(rt),
			"runtime_dir":        rt,
		}
		if b, err := os.ReadFile(statePath); err == nil {
			var st map[string]any
			if json.Unmarshal(b, &st) == nil {
				out["state"] = st
			}
		}
		c.JSON(http.StatusOK, out)
	}
}

// reloadConfigViaExport 把 SQLite 状态导出到 configDir/config.json。
//
// 与 exportSnapshot 共享 buildExport + 原子写逻辑。
//
// ctx 由调用方传入：手动导出用请求 ctx（客户端断开则取消），
// 自动导出用自己的超时 ctx（不能绑在 HTTP 请求上 —— 请求早结束了，导出还要继续）。
func reloadConfigViaExport(ctx context.Context, db *sql.DB, configDir string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cfg, err := buildExport(ctx, db)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(configDir, "config.json")
	tmp, err := os.CreateTemp(configDir, "config.json.tmp.*")
	if err != nil {
		return err
	}
	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(cfg); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	logging.Info(logging.EventConfigExport).
		Str("service", "web").
		Str("path", path).
		Int64("version", cfg.Version).
		Msg("config 快照已导出（via process control reload）")
	return nil
}
