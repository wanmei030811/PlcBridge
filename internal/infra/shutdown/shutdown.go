// Package shutdown 提供统一的优雅关闭工具（Phase 1.6）。
//
// 用法：
//   runner := shutdown.New(time.Second*10)
//   runner.Register("collect", collect.Stop)
//   runner.Register("api", api.Stop)
//   runner.Run(ctx) // 阻塞直到 ctx 取消或全部完成
//
// 特性：
//   - 步骤注册：按注册顺序串行执行
//   - 每步独立超时：单个卡住不影响后续
//   - 总时长日志：方便排障 "为什么退出这么久"
package shutdown

import (
	"context"
	"sync"
	"time"

	"github.com/plcbridge/plcbridge/internal/infra/logging"
)

// Step 关闭步骤。
//
// name 用于日志，stop 是具体关闭函数（参数 timeout）。
type Step struct {
	Name string
	Stop func(timeout time.Duration)
}

// Runner 关闭编排器。
type Runner struct {
	timeout    time.Duration // 每步默认超时
	mu         sync.Mutex
	steps      []Step
}

// New 创建 Runner。
func New(defaultTimeout time.Duration) *Runner {
	if defaultTimeout <= 0 {
		defaultTimeout = 5 * time.Second
	}
	return &Runner{timeout: defaultTimeout}
}

// Register 注册一个关闭步骤（线程安全）。
func (r *Runner) Register(name string, stop func(time.Duration)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.steps = append(r.steps, Step{Name: name, Stop: stop})
}

// Run 等待 ctx 取消后，按注册顺序执行所有步骤。
//
// 返回：是否所有步骤在超时内完成（true）/ 至少一步超时（false）。
func (r *Runner) Run(ctx context.Context) bool {
	<-ctx.Done()

	r.mu.Lock()
	steps := make([]Step, len(r.steps))
	copy(steps, r.steps)
	r.mu.Unlock()

	if len(steps) == 0 {
		return true
	}

	start := time.Now()
	allOK := true
	for _, s := range steps {
		stepStart := time.Now()
		// 单步用 goroutine + done channel 防止 stop 内部不响应 ctx
		done := make(chan struct{})
		go func() {
			defer close(done)
			if s.Stop != nil {
				s.Stop(r.timeout)
			}
		}()
		select {
		case <-done:
			logging.Info(logging.EventServiceStop).
				Str("step", s.Name).
				Dur("dur_ms", time.Since(stepStart)).
				Msg("shutdown 步骤完成")
		case <-time.After(r.timeout + 500*time.Millisecond):
			// 留 500ms 余量（防止 stop 内部不返回）
			logging.Warn(logging.EventServiceStop).
				Str("step", s.Name).
				Dur("timeout_ms", r.timeout).
				Msg("shutdown 步骤超时，强制继续下一步")
			allOK = false
		}
	}

	logging.Info(logging.EventServiceStop).
		Dur("total_ms", time.Since(start)).
		Bool("all_ok", allOK).
		Int("steps", len(steps)).
		Msg("shutdown 完成")
	return allOK
}
