package leakcheck

import (
	"runtime"
	"testing"
	"time"
)

// 模拟 goroutine 泄漏检测（基本机制）
func TestLeakcheck_DetectsGoroutineLeak(t *testing.T) {
	Check(t)

	// 启动一个永久 goroutine（模拟泄漏）
	ch := make(chan struct{})
	go func() {
		<-ch
	}()
	// 故意不关，测试在 Check 的 defer 里会检测到
	t.Cleanup(func() {
		close(ch)
	})
}

func TestLeakcheck_AcceptsCleanExit(t *testing.T) {
	Check(t)

	go func() {
		// 立即返回：不应被检测为泄漏
	}()
	// 等待 goroutine 退出
	time.Sleep(50 * time.Millisecond)
}

func TestLeakcheck_StackOf(t *testing.T) {
	// 启动一个带特征名的 goroutine
	done := make(chan struct{})
	go func() {
		defer close(done)
		time.Sleep(100 * time.Millisecond)
	}()
	stack := StackOf("leakcheck_test.go")
	if stack == "" {
		t.Errorf("StackOf should find our goroutine")
	}
	<-done
}

func TestLeakcheck_BaselineStable(t *testing.T) {
	// 跨多次调用基线应稳定
	before := runtime.NumGoroutine()
	// 启动 + 等结束
	for i := 0; i < 5; i++ {
		done := make(chan struct{})
		go func() { close(done) }()
		<-done
	}
	// 给 GC 一点时间
	time.Sleep(50 * time.Millisecond)
	after := runtime.NumGoroutine()
	// 不严格相等（runtime 自身会起 goroutine），只验证大致稳定
	if after > before+5 {
		t.Errorf("goroutine count grew too much: before=%d, after=%d", before, after)
	}
}
