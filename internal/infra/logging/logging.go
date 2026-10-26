// Package logging 实现 plcbridge 结构化日志（参考 Plan.md §10）。
//
// 输出：
//   - JSON 行（每行一条 JSON，便于 jq / grep 处理）
//   - 控制台（stdout，容器化部署由 docker logs / journalctl 接管）
//
// 字段（§10.1.1）：
//   - timestamp / level / service / event / trace_id / message 必填
package logging

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
)

var (
	globalLogger zerolog.Logger
	initOnce     sync.Once
	// 注入的额外 writer（测试时通过 SetOutput 替换整体输出）
	extraWriter io.Writer
)

// Logger 是带上下文的日志构建器（基于 zerolog.Event 链式调用）。
type Logger struct {
	ev *zerolog.Event
}

// SetExtraWriter 注册一个额外输出目标（Init 后调用会将 globalLogger 的 Output
// 改为 stdout + extraWriter 双写；Init 前调用则额外 writer 在 Init 时自动加入）。
// 主要用途：单元测试注入 bufio.Writer；正式环境让 Init 自动写日志文件。
func SetExtraWriter(w io.Writer) { extraWriter = w }

// Init 初始化全局 logger（应在 main 启动时调用一次）。
//
// 行为：
//   - 输出到 stdout（stdout 被 supervisor 的 "> file" 重定向时会写入日志文件）
//   - 同时自动写到 DefaultLogPath() 路径的文件（双写，避免 Windows 重定向缓冲问题）
//   - 若文件目录不存在则跳过文件写（不阻塞启动）
//
// level：ERROR / WARN / INFO / DEBUG
func Init(level string) {
	initOnce.Do(func() {
		zerolog.TimeFieldFormat = time.RFC3339Nano
		lvl, err := zerolog.ParseLevel(strings.ToLower(level))
		if err != nil {
			lvl = zerolog.InfoLevel
		}
		zerolog.SetGlobalLevel(lvl)

		w := io.MultiWriter(os.Stdout)

		// 写日志文件（创建目录 + append 模式）
		logPath := DefaultLogPath("plcbridge")
		if dir := filepath.Dir(logPath); dir != "" {
			if err := os.MkdirAll(dir, 0o755); err == nil {
				if f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
					w = io.MultiWriter(os.Stdout, f)
				}
			}
		}

		// SetOutput 注入的额外 writer（测试用）也合并进去
		if extraWriter != nil {
			w = io.MultiWriter(w, extraWriter)
		}

		globalLogger = zerolog.New(w).With().
			Timestamp().
			Logger()
	})
}

// SetOutput 切换输出目标（测试用）。
func SetOutput(w io.Writer) {
	globalLogger = globalLogger.Output(w)
}

// With 返回带上下文的 logger。
func With(kv ...any) *Logger {
	ctx := globalLogger.With()
	for i := 0; i+1 < len(kv); i += 2 {
		if k, ok := kv[i].(string); ok {
			ctx = ctx.Str(k, toString(kv[i+1]))
		}
	}
	l := ctx.Logger()
	return &Logger{ev: l.Info()}
}

// Info 全局 Info 日志。
//
// 使用约定：第一个参数是 event 名（推荐从 Event* 常量中选取），第二个参数是 message 文案。
// 例：logging.Info(EventPollBatchEnd).Str("source", bizID).Msg("采集批次完成")
//
// 向后兼容：单参数用法仍可用（event 字段取 msg），但已不推荐。
func Info(eventOrMsg string) *Logger {
	return &Logger{ev: globalLogger.Info().Str("service", "plcbridge").Str("event", eventOrMsg)}
}

// Error 全局 Error 日志。
func Error(eventOrMsg string) *Logger {
	return &Logger{ev: globalLogger.Error().Str("service", "plcbridge").Str("event", eventOrMsg)}
}

// Warn 全局 Warn 日志。
func Warn(eventOrMsg string) *Logger {
	return &Logger{ev: globalLogger.Warn().Str("service", "plcbridge").Str("event", eventOrMsg)}
}

// Debug 全局 Debug 日志。
func Debug(eventOrMsg string) *Logger {
	return &Logger{ev: globalLogger.Debug().Str("service", "plcbridge").Str("event", eventOrMsg)}
}

// Str 添加 string 字段。
func (l *Logger) Str(key, val string) *Logger {
	l.ev = l.ev.Str(key, val)
	return l
}

// Int 添加 int 字段。
func (l *Logger) Int(key string, val int) *Logger {
	l.ev = l.ev.Int(key, val)
	return l
}

// Int64 添加 int64 字段。
func (l *Logger) Int64(key string, val int64) *Logger {
	l.ev = l.ev.Int64(key, val)
	return l
}

// Bool 添加 bool 字段。
func (l *Logger) Bool(key string, val bool) *Logger {
	l.ev = l.ev.Bool(key, val)
	return l
}

// Dur 添加 duration 字段。
func (l *Logger) Dur(key string, val time.Duration) *Logger {
	l.ev = l.ev.Dur(key, val)
	return l
}

// Err 添加 error 字段。
func (l *Logger) Err(err error) *Logger {
	l.ev = l.ev.Err(err)
	return l
}

// Interface 添加任意类型字段。
func (l *Logger) Interface(key string, val any) *Logger {
	l.ev = l.ev.Interface(key, val)
	return l
}

// Uint8 添加 uint8 字段。
func (l *Logger) Uint8(key string, val uint8) *Logger {
	l.ev = l.ev.Uint8(key, val)
	return l
}

// Msg 发送日志消息。
func (l *Logger) Msg(msg string) {
	l.ev.Msg(msg)
}

// Send 发送日志。
func (l *Logger) Send() {
	l.ev.Send()
}

// DefaultLogPath 跨平台日志路径。
func DefaultLogPath(serviceName string) string {
	switch runtime.GOOS {
	case "windows":
		progdata := os.Getenv("PROGRAMDATA")
		if progdata == "" {
			progdata = `C:\ProgramData`
		}
		return filepath.Join(progdata, "plcbridge", "logs", serviceName+".log")
	default:
		return filepath.Join("/var/log/plcbridge", serviceName+".log")
	}
}

func toString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case int:
		return formatInt(int64(x))
	case int64:
		return formatInt(x)
	case bool:
		if x {
			return "true"
		}
		return "false"
	default:
		return ""
	}
}

func formatInt(n int64) string {
	if n == 0 {
		return "0"
	}
	negative := n < 0
	if negative {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if negative {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
