// Package leakcheck 提供 goroutine 泄漏检测（Phase 1.8）。
//
// 用法：
//
//	func TestSomething(t *testing.T) {
//	    defer leakcheck.Check(t)  // 必须在 defer 中，函数退出时检查
//	    // ... 业务代码
//	}
//
// 原理：defer 时对比当前 goroutine 数与函数开始时基线，
//
//	若差值 > 容忍阈值（默认 0），则报失败并打印堆栈。
//
// 注意：
//   - 必须配合 t.Parallel() **不**用（并发测试会污染基线）
//   - 如有 background goroutine（fsnotify watch、watcher 等），需手动 Verify()
package leakcheck

import (
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// baseline 包级基线（首次调用时记录）
var (
	baselineOnce sync.Once
	baseline     int
)

// tolerance 容忍差异（runtime 起内部 goroutine）
const tolerance = 0

// recordBaseline 记录包级基线。
func recordBaseline() {
	baselineOnce.Do(func() {
		baseline = runtime.NumGoroutine()
	})
}

// Check 注册一个 defer 检查，函数返回时校验 goroutine 数无净增长。
func Check(t *testing.T) {
	t.Helper()
	recordBaseline()

	startN := runtime.NumGoroutine()

	t.Cleanup(func() {
		// 等待可能的 in-flight goroutine 退出（最多 2s）
		deadline := time.Now().Add(2 * time.Second)
		var endN int
		for time.Now().Before(deadline) {
			endN = runtime.NumGoroutine()
			if endN <= startN {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}

		growth := endN - startN
		if growth > tolerance {
			buf := make([]byte, 64*1024)
			n := runtime.Stack(buf, true)
			t.Errorf("goroutine 泄漏: start=%d, end=%d, growth=%d\n--- stacks ---\n%s",
				startN, endN, growth, string(buf[:n]))
		}
	})
}

// CheckWithBaseline 检查函数返回后 goroutine 数回到包级基线。
//
// 用途：测试套件级别（TestMain）用一次；TestX 用 Check 即可。
func CheckWithBaseline(t *testing.T) {
	t.Helper()
	recordBaseline()

	t.Cleanup(func() {
		deadline := time.Now().Add(2 * time.Second)
		var endN int
		for time.Now().Before(deadline) {
			endN = runtime.NumGoroutine()
			if endN <= baseline {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if endN > baseline {
			t.Errorf("goroutine 泄漏（vs baseline）: baseline=%d, end=%d", baseline, endN)
		}
	})
}

// StackOf 拿某个指定 goroutine 的堆栈（按 name 搜索）。
// 用于：在测试中找泄漏的具体来源。
func StackOf(nameSubstr string) string {
	buf := make([]byte, 64*1024)
	n := runtime.Stack(buf, true)
	for _, line := range strings.Split(string(buf[:n]), "\n\n") {
		if strings.Contains(line, nameSubstr) {
			return line
		}
	}
	return ""
}
