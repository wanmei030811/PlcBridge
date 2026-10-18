// Package pool 实现 plcbridge 单进程内的 4 个 worker 池。
//
// 设计原则（参考 Plan.md §6.2.2）：
//   1. 池间物理隔离：4 个独立 goroutine 池，跨池通信只走 channel
//   2. 每个池独立 context，互不传染超时
//   3. 任务级非阻塞：池满时调用方立即收到 ErrPoolBusy，不排队阻塞
package pool

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/plcbridge/plcbridge/internal/infra/logging"
)

// ErrPoolBusy 池满时返回，调用方应立即放弃（fail-fast）。
var ErrPoolBusy = errors.New("worker pool busy")

// Task 是 worker 池接收的任务。
//
// 每个任务自带 context（推荐复用调用方 ctx），确保超时可传递。
type Task struct {
	Name string
	Ctx  context.Context
	Run  func(ctx context.Context)
}

// Config 4 个池的容量配置。
//
// 推荐基线（Plan.md §6.1.2）：
//   - CollectSize 200  goroutine
//   - ForwardSize 100  goroutine
//   - WriteSize   50   goroutine
//   - ConfigSize  4    goroutine
type Config struct {
	CollectSize int
	ForwardSize int
	WriteSize   int
	ConfigSize  int
}

// Pools 持有 4 个 worker 池。
type Pools struct {
	collect *WorkerPool
	forward *WorkerPool
	write   *WorkerPool
	config  *WorkerPool
}

// NewPools 创建 4 个 worker 池。
func NewPools(cfg Config) *Pools {
	return &Pools{
		collect: newPool("collectPool", cfg.CollectSize),
		forward: newPool("forwardPool", cfg.ForwardSize),
		write:   newPool("writePool", cfg.WriteSize),
		config:  newPool("configPool", cfg.ConfigSize),
	}
}

// SubmitCollect 向 collectPool 提交任务。池满时立即返回 ErrPoolBusy（不阻塞）。
func (p *Pools) SubmitCollect(ctx context.Context, name string, run func(context.Context)) error {
	return p.collect.Submit(ctx, name, run)
}

// SubmitForward 向 forwardPool 提交任务。
func (p *Pools) SubmitForward(ctx context.Context, name string, run func(context.Context)) error {
	return p.forward.Submit(ctx, name, run)
}

// SubmitForwardBlocking 阻塞 100ms 后返回 ErrPoolBusy。
func (p *Pools) SubmitForwardBlocking(ctx context.Context, name string, run func(context.Context), timeout time.Duration) error {
	return p.forward.SubmitBlocking(ctx, name, run, timeout)
}

// SubmitWrite 向 writePool 提交任务。
func (p *Pools) SubmitWrite(ctx context.Context, name string, run func(context.Context)) error {
	return p.write.Submit(ctx, name, run)
}

// SubmitWriteBlocking 阻塞 100ms 后返回 ErrPoolBusy。
func (p *Pools) SubmitWriteBlocking(ctx context.Context, name string, run func(context.Context), timeout time.Duration) error {
	return p.write.SubmitBlocking(ctx, name, run, timeout)
}

// SubmitConfig 向 configPool 提交任务。
func (p *Pools) SubmitConfig(ctx context.Context, name string, run func(context.Context)) error {
	return p.config.Submit(ctx, name, run)
}

// SubmitCollectBlocking 向 collectPool 提交任务，池满时阻塞 100ms 后返回 ErrPoolBusy。
//
// 仅用于必须执行的关键路径（如 PLC 采集周期主流程）。
func (p *Pools) SubmitCollectBlocking(ctx context.Context, name string, run func(context.Context)) error {
	return p.collect.SubmitBlocking(ctx, name, run, 100*time.Millisecond)
}

// Start 启动 4 个池（在根 ctx 取消时停止）。
func (p *Pools) Start(ctx context.Context, _ interface{}) {
	p.collect.start(ctx)
	p.forward.start(ctx)
	p.write.start(ctx)
	p.config.start(ctx)
}

// Stop 优雅停止（最多等待 timeout）。
func (p *Pools) Stop(timeout time.Duration) {
	p.collect.stop(timeout)
	p.forward.stop(timeout)
	p.write.stop(timeout)
	p.config.stop(timeout)
}

// Stats 返回所有池的运行时统计。
func (p *Pools) Stats() map[string]PoolStats {
	return map[string]PoolStats{
		"collectPool": p.collect.Stats(),
		"forwardPool": p.forward.Stats(),
		"writePool":   p.write.Stats(),
		"configPool":  p.config.Stats(),
	}
}

// === 单个 WorkerPool 实现 ===

// WorkerPool 是一个有固定容量的 goroutine 池。
//
// 实现说明：
//   - 不使用第三方库（如 ants），手写 30 行实现更可控
//   - 使用 semaphore（chan struct{}）+ 池化 goroutine
//   - 任务分发走 buffered channel，pool full 时 Submit 返回 ErrPoolBusy
type WorkerPool struct {
	name    string
	size    int
	sem     chan struct{}
	tasks   chan Task
	wg      sync.WaitGroup
	ctx     context.Context
	cancel  context.CancelFunc
	stopped chan struct{}

	// 监控指标
	submitted atomic.Int64
	rejected  atomic.Int64
	running   atomic.Int64
}

func newPool(name string, size int) *WorkerPool {
	if size <= 0 {
		size = 1
	}
	return &WorkerPool{
		name:    name,
		size:    size,
		sem:     make(chan struct{}, size),
		tasks:   make(chan Task, size*4),
		stopped: make(chan struct{}),
	}
}

// PoolStats 是 Prometheus 上报的指标对象。
type PoolStats struct {
	Size      int   `json:"size"`
	Submitted int64 `json:"submitted"`
	Rejected  int64 `json:"rejected"`
	Running   int64 `json:"running"`
}

func (p *WorkerPool) Stats() PoolStats {
	return PoolStats{
		Size:      p.size,
		Submitted: p.submitted.Load(),
		Rejected:  p.rejected.Load(),
		Running:   p.running.Load(),
	}
}

// Submit 非阻塞提交，池满/队列满立即返回 ErrPoolBusy。
func (p *WorkerPool) Submit(ctx context.Context, name string, run func(context.Context)) error {
	task := Task{Name: name, Ctx: ctx, Run: run}
	select {
	case p.tasks <- task:
		p.submitted.Add(1)
		return nil
	default:
		p.rejected.Add(1)
		logging.Warn(logging.EventServiceStart).
			Str("pool", p.name).
			Str("task", name).
			Msg("worker pool busy, task rejected")
		return ErrPoolBusy
	}
}

// SubmitBlocking 阻塞提交，最多等待 timeout；超时返回 ErrPoolBusy。
func (p *WorkerPool) SubmitBlocking(ctx context.Context, name string, run func(context.Context), timeout time.Duration) error {
	task := Task{Name: name, Ctx: ctx, Run: run}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case p.tasks <- task:
		p.submitted.Add(1)
		return nil
	case <-timer.C:
		p.rejected.Add(1)
		logging.Warn(logging.EventServiceStart).
			Str("pool", p.name).
			Str("task", name).
			Dur("timeout", timeout).
			Msg("worker pool submit timeout")
		return ErrPoolBusy
	case <-ctx.Done():
		return ctx.Err()
	}
}

// start 启动池（一次性启动 size 个 dispatch goroutine）。
func (p *WorkerPool) start(parent context.Context) {
	p.ctx, p.cancel = context.WithCancel(parent)
	for i := 0; i < p.size; i++ {
		p.wg.Add(1)
		go p.dispatch(i)
	}
}

func (p *WorkerPool) dispatch(id int) {
	defer p.wg.Done()
	for {
		select {
		case <-p.ctx.Done():
			return
		case task := <-p.tasks:
			p.sem <- struct{}{}
			p.running.Add(1)
			p.run(task, id)
			<-p.sem
			p.running.Add(-1)
		}
	}
}

// run 在独立 goroutine 中执行 task，确保 panic 不影响池主循环。
func (p *WorkerPool) run(task Task, id int) {
	defer func() {
		if r := recover(); r != nil {
			stack := debug.Stack()
			errMsg := fmt.Sprintf("%v", r)
			if e, ok := r.(error); ok {
				errMsg = e.Error()
			}
			logging.Error(logging.EventStatsPanic).
				Str("pool", p.name).
				Str("task", task.Name).
				Int("worker_id", id).
				Str("panic", errMsg).
				Str("stack", string(stack)).
				Msg("worker panic recovered")
		}
	}()
	task.Run(task.Ctx)
}

// stop 优雅停止：取消 ctx → 等所有 in-flight 任务完成（最多 timeout）。
func (p *WorkerPool) stop(timeout time.Duration) {
	p.cancel()

	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		close(p.stopped)
	case <-time.After(timeout):
		logging.Warn(logging.EventServiceStop).
			Str("pool", p.name).
			Dur("timeout", timeout).
			Int("running", int(p.running.Load())).
			Msg("worker pool stop timeout, forcing exit")
	}
}
