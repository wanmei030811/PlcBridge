// Package web 进程级 health + ready 端点。
//
// 路由：
//
//	GET /api/v1/health         : liveness（进程是否还活着），永远 200
//	GET /api/v1/health/ready   : readiness（依赖是否就绪），可 503（Phase 1.5）
//
// 设计（Phase 1.5）：
//   - /health 给前端仪表盘 3s 心跳用，不能因为 DB 抖动就误报
//   - /health/ready 给 K8s/LB 用，必须 503 才能触发流量切换
package web

import (
	"bufio"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
)

// healthHandler: 返回 plcbridge-web 进程 + 数据库健康信息，供前端仪表盘 3s 心跳使用。
//
// 字段：
//
//	{"status":"ok", "db": "ok", "version": "dev"}
//	"process": {
//	  "pid":             int,     // 当前进程 PID
//	  "started_at_ms":   int64,   // 进程启动 unix ms
//	  "uptime_ms":       int64,   // 当前 - started_at
//	  "rss_bytes":        uint64,  // VmRSS 常驻内存字节（Linux）；其他平台 fallback 到 Alloc
//	  "num_goroutine":   int,     // runtime.NumGoroutine
//	  "go_version":      string,  // "go1.22.3"
//	  "num_cpu":         int      // runtime.NumCPU
//	}
//
// 失败时仍返回 200 + status="degraded"，方便前端区分"完全不可达" vs "进程在线但某字段缺失"。
// 路由由 web.go RegisterRoutes 注册为公开（不需要登录）。
func healthHandler(db interface {
	Ping() error
}) gin.HandlerFunc {
	return func(c *gin.Context) {
		resp := gin.H{
			"status":  "ok",
			"db":      "ok",
			"version": Version,
			"process": gin.H{
				"pid":           os.Getpid(),
				"started_at_ms": webStartTime.UnixMilli(),
				"uptime_ms":     time.Since(webStartTime).Milliseconds(),
				"rss_bytes":     residentMemoryBytes(),
				"num_goroutine": runtime.NumGoroutine(),
				"go_version":    runtime.Version(),
				"num_cpu":       runtime.NumCPU(),
			},
		}
		// 数据库 ping（db 可为 nil，仅在 RegisterRoutes 注入）
		if db != nil {
			if err := db.Ping(); err != nil {
				resp["status"] = "degraded"
				resp["db"] = err.Error()
			}
		}
		c.JSON(http.StatusOK, resp)
	}
}

// readyHandler 返回深度健康检查（Phase 1.5）。
//
// 用途：Kubernetes readinessProbe / 外部 LB / 监控告警。
// 语义：检查所有依赖项；任一失败 → 503 + status="unready"。
//
// 检查项：
//   - db: SQLite ping（必须）
//   - config: 配置文件是否已加载（由调用方通过 SetConfigHealthProvider 注入）
//
// 200 OK：status="ready"
// 503 Service Unavailable：status="unready" + 失败项
//
// 重要区别于 /api/v1/health（liveness）：
//   - /health: 进程是否还活着（always 200 if process up）
//   - /health/ready: 进程是否**准备好服务请求**（受依赖影响）
func readyHandler(db interface {
	Ping() error
}) gin.HandlerFunc {
	return func(c *gin.Context) {
		checks := gin.H{}
		allOK := true

		// 1. DB ping
		if db != nil {
			if err := db.Ping(); err != nil {
				checks["db"] = gin.H{"status": "fail", "error": err.Error()}
				allOK = false
			} else {
				checks["db"] = gin.H{"status": "ok"}
			}
		} else {
			checks["db"] = gin.H{"status": "fail", "error": "db 未注入"}
			allOK = false
		}

		// 2. Config provider（如有）
		if provider := configHealthProvider.Load(); provider != nil {
			cfgCheck := (*provider).Check()
			checks["config"] = cfgCheck
			if s, ok := cfgCheck["status"].(string); !ok || s != "ok" {
				allOK = false
			}
		} else {
			// 未注入 config provider：跳过（健康检查不强制依赖）
			checks["config"] = gin.H{"status": "skip", "reason": "未注入 config provider"}
		}

		status := http.StatusOK
		overall := "ready"
		if !allOK {
			status = http.StatusServiceUnavailable
			overall = "unready"
		}
		c.JSON(status, gin.H{
			"status":        overall,
			"version":       Version,
			"checks":        checks,
			"checked_at_ms": time.Now().UnixMilli(),
		})
	}
}

// ConfigHealthProvider 提供 config 健康状态（用于 readyHandler）。
//
// 实现方：调用方在 main 中通过 SetConfigHealthProvider 注入。
// 默认无 provider，/health/ready 不查 config。
type ConfigHealthProvider interface {
	// Check 返回 config 状态（status: "ok" | "fail" + 可选 error）。
	Check() gin.H
}

// configHealthProvider 存储当前注入的 provider（atomic 保证并发安全）。
var configHealthProvider atomic.Pointer[ConfigHealthProvider]

// SetConfigHealthProvider 注入 config 健康 provider（通常 main 启动后调一次）。
func SetConfigHealthProvider(p ConfigHealthProvider) {
	configHealthProvider.Store(&p)
}

// Version 暴露服务版本（编译期通过 -ldflags 注入，否则显示 "dev"）。
var Version = "dev"

// SetVersion 设置版本（main 启动时通过编译期 -ldflags 注入）。
func SetVersion(v string) {
	if v != "" {
		Version = v
	}
}

// residentMemoryBytes 返回当前进程常驻内存字节数。
// Windows：fallback 到 runtime.MemStats.Alloc（堆分配，不完全等价 VmRSS，但能反映压力）。
// Linux：解析 /proc/self/status 的 VmRSS。
// 其他平台：返回 0，前端显示 "—"。
func residentMemoryBytes() uint64 {
	if rss := readRSSFromProc(); rss > 0 {
		return rss
	}
	if runtime.GOOS == "windows" {
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		return m.Alloc
	}
	return 0
}

// readRSSFromProc Linux 下读 /proc/self/status 拿 VmRSS。
// 失败（OS 不支持 / 文件读取失败 / 字段不存在）返回 0，调用方 fallback。
func readRSSFromProc() uint64 {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "VmRSS:") {
			continue
		}
		// 形如 "VmRSS:    12345 kB"
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0
		}
		kb, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return 0
		}
		return kb * 1024
	}
	return 0
}
