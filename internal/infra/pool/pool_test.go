package pool

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// newTestPools 创建一个已启动的测试用 Pools，t.Cleanup 时停止。
func newTestPools(t *testing.T, cfg Config) *Pools {
	t.Helper()
	p := NewPools(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	p.Start(ctx, nil)
	t.Cleanup(func() {
		cancel()
		p.Stop(2 * time.Second)
	})
	return p
}

// TestSubmitRejectsWhenPoolFull 验证：池满时 Submit 立即返回 ErrPoolBusy（不阻塞）。
//
// 这是 worker 池最关键的行为：过载时必须 fail-fast，
// 否则会把上游（PLC 采集循环）一起拖死。
func TestSubmitRejectsWhenPoolFull(t *testing.T) {
	p := newTestPools(t, Config{CollectSize: 1, ForwardSize: 1, WriteSize: 1, ConfigSize: 1})

	// 用一个永不返回的任务占住唯一的 worker。
	block := make(chan struct{})
	release := make(chan struct{})
	entered := make(chan struct{})

	if err := p.SubmitCollect(context.Background(), "blocker", func(context.Context) {
		close(entered)
		<-release
	}); err != nil {
		t.Fatalf("first submit failed: %v", err)
	}

	// 等 blocker 真正开始执行，确保 worker 被占住。
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("blocker task never started")
	}

	// 现在队列已满（tasks channel 容量 = size*4 = 4）。
	// 继续灌任务直到收到 ErrPoolBusy。
	var gotBusy bool
	for i := 0; i < 20; i++ {
		err := p.SubmitCollect(context.Background(), "filler", func(context.Context) {
			<-block
		})
		if errors.Is(err, ErrPoolBusy) {
			gotBusy = true
			break
		}
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}

	close(release)
	close(block)

	if !gotBusy {
		t.Fatal("expected ErrPoolBusy after filling the pool, never got it")
	}
}

// TestSubmitRejectionIsNonBlocking 验证：池满时 Submit 立即返回（不排队等待）。
//
// 若实现错误地阻塞等待，测试会在 2s 超时被 select 捕获。
func TestSubmitRejectionIsNonBlocking(t *testing.T) {
	p := newTestPools(t, Config{CollectSize: 1, ForwardSize: 1, WriteSize: 1, ConfigSize: 1})

	block := make(chan struct{})
	entered := make(chan struct{})

	if err := p.SubmitCollect(context.Background(), "blocker", func(context.Context) {
		close(entered)
		<-block
	}); err != nil {
		t.Fatalf("first submit failed: %v", err)
	}
	<-entered

	// 灌满队列。
	for i := 0; i < 10; i++ {
		_ = p.SubmitCollect(context.Background(), "filler", func(context.Context) { <-block })
	}

	// 下一次 submit 必须在 1s 内返回（成功或 ErrPoolBusy 都行，重点是不阻塞）。
	done := make(chan error, 1)
	go func() {
		done <- p.SubmitCollect(context.Background(), "probe", func(context.Context) {})
	}()

	select {
	case err := <-done:
		if err != nil && !errors.Is(err, ErrPoolBusy) {
			t.Fatalf("unexpected error: %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("Submit blocked when pool was full (should fail-fast)")
	}

	close(block)
}

// TestSubmitBlockingTimesOut 验证：SubmitBlocking 在超时后返回 ErrPoolBusy。
func TestSubmitBlockingTimesOut(t *testing.T) {
	p := newTestPools(t, Config{CollectSize: 1, ForwardSize: 1, WriteSize: 1, ConfigSize: 1})

	block := make(chan struct{})
	entered := make(chan struct{})

	if err := p.SubmitCollect(context.Background(), "blocker", func(context.Context) {
		close(entered)
		<-block
	}); err != nil {
		t.Fatalf("first submit failed: %v", err)
	}
	<-entered

	// 灌满队列，让后续提交必须等待。
	for i := 0; i < 10; i++ {
		_ = p.SubmitCollect(context.Background(), "filler", func(context.Context) { <-block })
	}

	start := time.Now()
	err := p.SubmitCollectBlocking(context.Background(), "slow", func(context.Context) {})
	elapsed := time.Since(start)

	if !errors.Is(err, ErrPoolBusy) {
		t.Fatalf("expected ErrPoolBusy, got %v", err)
	}
	if elapsed < 90*time.Millisecond {
		t.Fatalf("returned too early: %v (expected >= ~100ms)", elapsed)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("blocked far too long: %v", elapsed)
	}

	close(block)
}

// TestWorkerPoolSubmitBlockingTimesOut 验证：WorkerPool.SubmitBlocking 按传入超时返回。
//
// Pools.SubmitCollectBlocking 的超时是写死的 100ms，这里直接测底层
// WorkerPool，验证超时参数真的生效。
func TestWorkerPoolSubmitBlockingTimesOut(t *testing.T) {
	wp := newPool("testPool", 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wp.start(ctx)
	defer wp.stop(2 * time.Second)

	block := make(chan struct{})
	entered := make(chan struct{})
	if err := wp.Submit(context.Background(), "blocker", func(context.Context) {
		close(entered)
		<-block
	}); err != nil {
		t.Fatalf("submit failed: %v", err)
	}
	<-entered

	// 灌满队列（容量 = size*4 = 4）。
	for i := 0; i < 10; i++ {
		_ = wp.Submit(context.Background(), "filler", func(context.Context) { <-block })
	}

	start := time.Now()
	err := wp.SubmitBlocking(context.Background(), "slow", func(context.Context) {}, 200*time.Millisecond)
	elapsed := time.Since(start)

	if !errors.Is(err, ErrPoolBusy) {
		t.Fatalf("expected ErrPoolBusy, got %v", err)
	}
	if elapsed < 180*time.Millisecond {
		t.Fatalf("returned too early: %v (expected >= ~200ms)", elapsed)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("blocked far too long: %v", elapsed)
	}

	close(block)
}

// TestSubmitBlockingRespectsContextCancel 验证：ctx 取消时 SubmitBlocking 返回 ctx.Err()。
func TestSubmitBlockingRespectsContextCancel(t *testing.T) {
	p := newTestPools(t, Config{CollectSize: 1, ForwardSize: 1, WriteSize: 1, ConfigSize: 1})

	block := make(chan struct{})
	entered := make(chan struct{})

	if err := p.SubmitCollect(context.Background(), "blocker", func(context.Context) {
		close(entered)
		<-block
	}); err != nil {
		t.Fatalf("first submit failed: %v", err)
	}
	<-entered

	for i := 0; i < 10; i++ {
		_ = p.SubmitCollect(context.Background(), "filler", func(context.Context) { <-block })
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	err := p.SubmitCollectBlocking(ctx, "cancelled", func(context.Context) {})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}

	close(block)
}

// TestStatsCountSubmittedAndRejected 验证：Stats 正确统计 submitted / rejected。
func TestStatsCountSubmittedAndRejected(t *testing.T) {
	p := newTestPools(t, Config{CollectSize: 1, ForwardSize: 1, WriteSize: 1, ConfigSize: 1})

	block := make(chan struct{})
	entered := make(chan struct{})

	// 1 个成功提交。
	if err := p.SubmitCollect(context.Background(), "ok", func(context.Context) {
		close(entered)
		<-block
	}); err != nil {
		t.Fatalf("submit failed: %v", err)
	}
	<-entered

	// 灌满 → 至少 1 次拒绝。
	rejected := 0
	for i := 0; i < 20; i++ {
		err := p.SubmitCollect(context.Background(), "filler", func(context.Context) { <-block })
		if errors.Is(err, ErrPoolBusy) {
			rejected++
			break
		}
	}

	stats := p.Stats()
	collectStats := stats["collectPool"]

	if collectStats.Submitted < 1 {
		t.Errorf("Submitted = %d, want >= 1", collectStats.Submitted)
	}
	if collectStats.Rejected < int64(rejected) {
		t.Errorf("Rejected = %d, want >= %d", collectStats.Rejected, rejected)
	}
	if collectStats.Size != 1 {
		t.Errorf("Size = %d, want 1", collectStats.Size)
	}

	close(block)
}

// TestPanicInTaskIsRecovered 验证：任务 panic 不会让整个池崩溃。
//
// 这是 worker 池的核心健壮性保证：一个 PLC 点位写坏值导致 driver panic，
// 不能拖垮整个采集进程。
func TestPanicInTaskIsRecovered(t *testing.T) {
	p := newTestPools(t, Config{CollectSize: 1, ForwardSize: 1, WriteSize: 1, ConfigSize: 1})

	var ran atomic.Bool
	err := p.SubmitCollect(context.Background(), "panicker", func(context.Context) {
		panic("boom")
	})
	if err != nil {
		t.Fatalf("submit failed: %v", err)
	}

	// panic 被 recover 后，池应继续能接受新任务。
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if err := p.SubmitCollect(context.Background(), "after-panic", func(context.Context) {
			ran.Store(true)
		}); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// 等任务真正跑完。
	for time.Now().Before(deadline) {
		if ran.Load() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("pool did not recover from task panic")
}

// TestPoolsAreIsolated 验证：4 个池互相独立（一个池满不影响其他池）。
func TestPoolsAreIsolated(t *testing.T) {
	p := newTestPools(t, Config{CollectSize: 1, ForwardSize: 1, WriteSize: 1, ConfigSize: 1})

	block := make(chan struct{})
	entered := make(chan struct{})

	if err := p.SubmitCollect(context.Background(), "blocker", func(context.Context) {
		close(entered)
		<-block
	}); err != nil {
		t.Fatalf("submit failed: %v", err)
	}
	<-entered

	// 灌满 collect 池。
	for i := 0; i < 10; i++ {
		_ = p.SubmitCollect(context.Background(), "filler", func(context.Context) { <-block })
	}

	// 其他池仍应可用。
	if err := p.SubmitForward(context.Background(), "f", func(context.Context) {}); err != nil {
		t.Errorf("forward pool affected by collect pool saturation: %v", err)
	}
	if err := p.SubmitWrite(context.Background(), "w", func(context.Context) {}); err != nil {
		t.Errorf("write pool affected by collect pool saturation: %v", err)
	}
	if err := p.SubmitConfig(context.Background(), "c", func(context.Context) {}); err != nil {
		t.Errorf("config pool affected by collect pool saturation: %v", err)
	}

	close(block)
}

// TestStopWaitsForInFlightTasks 验证：Stop 会等待正在执行的任务完成。
func TestStopWaitsForInFlightTasks(t *testing.T) {
	p := NewPools(Config{CollectSize: 2, ForwardSize: 1, WriteSize: 1, ConfigSize: 1})
	ctx, cancel := context.WithCancel(context.Background())
	p.Start(ctx, nil)

	var done atomic.Bool
	entered := make(chan struct{})
	if err := p.SubmitCollect(context.Background(), "slow", func(context.Context) {
		close(entered)
		time.Sleep(300 * time.Millisecond)
		done.Store(true)
	}); err != nil {
		t.Fatalf("submit failed: %v", err)
	}
	<-entered

	p.Stop(2 * time.Second)
	cancel()

	if !done.Load() {
		t.Error("Stop returned before in-flight task completed")
	}
}

// TestConcurrentSubmitsAreSafe 验证：并发 Submit 不 panic、计数正确。
//
// 用 -race 跑这个测试能捕获 map / 计数器竞争。
func TestConcurrentSubmitsAreSafe(t *testing.T) {
	p := newTestPools(t, Config{CollectSize: 4, ForwardSize: 4, WriteSize: 4, ConfigSize: 4})

	const goroutines = 50
	const perGoroutine = 20

	var wg sync.WaitGroup
	var accepted atomic.Int64
	var rejected atomic.Int64

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				err := p.SubmitCollect(context.Background(), "c", func(context.Context) {})
				switch {
				case err == nil:
					accepted.Add(1)
				case errors.Is(err, ErrPoolBusy):
					rejected.Add(1)
				default:
					t.Errorf("unexpected error: %v", err)
				}
			}
		}()
	}
	wg.Wait()

	stats := p.Stats()["collectPool"]
	if stats.Submitted != accepted.Load() {
		t.Errorf("Stats.Submitted = %d, want %d", stats.Submitted, accepted.Load())
	}
	if stats.Rejected != rejected.Load() {
		t.Errorf("Stats.Rejected = %d, want %d", stats.Rejected, rejected.Load())
	}
}

// TestNewPoolWithZeroSizeUsesOne 验证：size<=0 时退化为 1（而不是 0 造成死锁）。
func TestNewPoolWithZeroSizeUsesOne(t *testing.T) {
	p := newTestPools(t, Config{CollectSize: 0, ForwardSize: 0, WriteSize: 0, ConfigSize: 0})

	stats := p.Stats()
	for name, s := range stats {
		if s.Size != 1 {
			t.Errorf("%s Size = %d, want 1", name, s.Size)
		}
	}

	// size=1 的池必须仍能接受任务。
	if err := p.SubmitCollect(context.Background(), "t", func(context.Context) {}); err != nil {
		t.Errorf("submit failed: %v", err)
	}
}
