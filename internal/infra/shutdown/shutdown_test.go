package shutdown

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestNew_DefaultTimeout(t *testing.T) {
	r := New(0) // 0 应 fallback 到 5s
	if r.timeout != 5*time.Second {
		t.Errorf("New(0) timeout: got %v, want 5s", r.timeout)
	}
}

func TestRunner_RegisterAndRun(t *testing.T) {
	r := New(time.Second)
	var called atomic.Int32
	r.Register("a", func(time.Duration) { called.Add(1) })
	r.Register("b", func(time.Duration) { called.Add(1) })

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	ok := r.Run(ctx)
	if !ok {
		t.Error("Run should return true when all steps complete")
	}
	if called.Load() != 2 {
		t.Errorf("expected 2 steps called, got %d", called.Load())
	}
}

func TestRunner_RunOrderPreserved(t *testing.T) {
	var order []string
	r := New(time.Second)
	r.Register("first", func(time.Duration) { order = append(order, "first") })
	r.Register("second", func(time.Duration) { order = append(order, "second") })
	r.Register("third", func(time.Duration) { order = append(order, "third") })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.Run(ctx)

	if len(order) != 3 {
		t.Fatalf("expected 3 steps, got %d", len(order))
	}
	if order[0] != "first" || order[1] != "second" || order[2] != "third" {
		t.Errorf("order wrong: %v", order)
	}
}

func TestRunner_StepTimeout(t *testing.T) {
	// 卡死的 step：永不返回
	r := New(100 * time.Millisecond)
	r.Register("stuck", func(_ time.Duration) {
		time.Sleep(10 * time.Second)
	})
	r.Register("after", func(time.Duration) {})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	ok := r.Run(ctx)
	dur := time.Since(start)

	if ok {
		t.Error("Run should return false when step times out")
	}
	// 整个 Run 应在 100ms timeout + 500ms 余量内完成
	if dur > time.Second {
		t.Errorf("Run took too long (%v), should bail at timeout+500ms", dur)
	}
}

func TestRunner_NilStop(t *testing.T) {
	// 允许 Stop 为 nil（占位）
	r := New(time.Second)
	r.Register("nil", nil)
	r.Register("ok", func(time.Duration) {})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if !r.Run(ctx) {
		t.Error("Run should succeed with nil stop")
	}
}

func TestRunner_EmptySteps(t *testing.T) {
	r := New(time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if !r.Run(ctx) {
		t.Error("empty steps should return true")
	}
}

func TestRunner_ConcurrentRegister(t *testing.T) {
	// 并发注册（不常见但应安全）
	// 注意：必须等所有注册都完成再 Run，否则 Run 复制时可能漏掉部分
	r := New(time.Second)
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.Register("step", func(time.Duration) {})
		}()
	}
	wg.Wait()
	if len(r.steps) != 10 {
		t.Errorf("expected 10 registered, got %d", len(r.steps))
	}
}
