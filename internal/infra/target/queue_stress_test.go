package target

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestQueue_Stress_CloseVsEnqueue 压力测试：多 producer 持续 Enqueue，
// 与 Close 高频并发。验证：
//  1. 无 data race（-race）
//  2. 无 panic（send on closed channel）
//  3. 无死锁（测试能超时退出）
//  4. 关闭后不再有值被计入 enqueued
func TestQueue_Stress_CloseVsEnqueue(t *testing.T) {
	// 注意：t.Fatalf 只能从测试主 goroutine 调用（go vet 会拦）。
	// 所以子 goroutine 只负责跑压力，通过 errCh 把失败原因回传主 goroutine。
	errCh := make(chan string, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for round := 0; round < 50; round++ {
			q := NewQueue(16)
			var wg sync.WaitGroup
			var sent int64
			wg.Add(8)
			for p := 0; p < 8; p++ {
				go func() {
					defer wg.Done()
					for i := 0; i < 500; i++ {
						q.Enqueue(PointValue{Value: i})
						atomic.AddInt64(&sent, 1)
					}
				}()
			}
			// Close 在 producer 运行中途发生
			time.Sleep(time.Duration(round%5) * 100 * time.Microsecond)
			q.Close()
			wg.Wait()

			// 关闭后 Enqueue 必须走 drop，不增加 enqueued
			_, _, enqBefore := q.Stats()
			q.Enqueue(PointValue{Value: 999})
			_, droppedAfter, enqAfter := q.Stats()
			if enqAfter != enqBefore {
				select {
				case errCh <- fmt.Sprintf("round %d: close 后 enqueued 增加了 (%d -> %d)", round, enqBefore, enqAfter):
				default:
				}
				return
			}
			if droppedAfter <= 0 {
				select {
				case errCh <- fmt.Sprintf("round %d: close 后 Enqueue 应计入 dropped", round):
				default:
				}
				return
			}
		}
	}()

	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("压力测试超时：疑似死锁")
	}
	select {
	case msg := <-errCh:
		t.Fatal(msg)
	default:
	}
}

// TestQueue_ConcurrentCloseAndClear 验证 Clear 与 Close 并发时安全。
func TestQueue_ConcurrentCloseAndClear(t *testing.T) {
	q := NewQueue(64)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 2000; i++ {
			q.Enqueue(PointValue{Value: i})
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 2000; i++ {
			q.Clear()
		}
	}()
	wg.Wait()
	q.Close()
}
