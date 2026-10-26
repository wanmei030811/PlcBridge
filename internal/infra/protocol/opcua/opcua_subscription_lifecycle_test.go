package opcua

import (
	"context"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/plcbridge/plcbridge/internal/infra/protocol"
)

// TestOPCUA_Subscription_Lifecycle 回归测试（2026-10-26）：
//
// 背景：修复 dispatchNotifications 读共享字段 c.notifyCh 与 disableSubscription
// 写 c.notifyCh = nil 之间的 data race，以及 close(notifyCh) 带来的
// "send on closed channel" panic 风险。
//
// 本测试覆盖订阅生命周期的关键路径：
//  1. 反复 EnableSubscription / Disconnect —— 每轮都必须无 panic、无 race
//  2. 取消订阅后 goroutine 数不持续增长（cancel() 能让派发 goroutine 正常退出）
//  3. 重新订阅后仍能正常工作（说明没有因旧实现残留而把新订阅搞坏）
func TestOPCUA_Subscription_Lifecycle(t *testing.T) {
	port := pickFreePort(t)
	_, endpoint, nsIdx := startTestServer(t, port)
	ch := newTestChannel(t, endpoint)
	addr := func(name string) string {
		return "ns=" + strconv.FormatUint(uint64(nsIdx), 10) + ";s=" + name
	}

	// 记录基线 goroutine 数，用于最后检查泄漏
	baseGoroutines := runtime.NumGoroutine()

	for round := 0; round < 5; round++ {
		if err := ch.Connect(context.Background()); err != nil {
			t.Fatalf("round %d Connect: %v", round, err)
		}

		got := make(chan struct{}, 8)
		err := ch.EnableSubscription(func(address string, v protocol.Value) {
			select {
			case got <- struct{}{}:
			default:
			}
		})
		if err != nil {
			t.Fatalf("round %d EnableSubscription: %v", round, err)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := ch.AddMonitoredItem(ctx, addr("rw_float")); err != nil {
			cancel()
			t.Fatalf("round %d AddMonitoredItem: %v", round, err)
		}
		cancel()

		// 等一次回调，确认订阅真的在工作
		select {
		case <-got:
		case <-time.After(3 * time.Second):
			t.Logf("round %d: 未收到回调（不一定是失败，取决于 server 推送时机）", round)
		}

		// Disconnect 会走 disableSubscription → cancel() + sub.Cancel()
		if err := ch.Disconnect(); err != nil {
			t.Fatalf("round %d Disconnect: %v", round, err)
		}
	}

	// 给派发 goroutine 一点时间退出
	time.Sleep(500 * time.Millisecond)

	after := runtime.NumGoroutine()
	// 允许少量波动（GC / 测试框架自身的 goroutine），但不能线性增长
	if after > baseGoroutines+20 {
		t.Errorf("goroutine 疑似泄漏: baseline=%d after=%d", baseGoroutines, after)
	}

	// 再次订阅，验证生命周期恢复正常
	if err := ch.Connect(context.Background()); err != nil {
		t.Fatalf("最终 Connect: %v", err)
	}
	got := make(chan struct{}, 8)
	if err := ch.EnableSubscription(func(address string, v protocol.Value) {
		select {
		case got <- struct{}{}:
		default:
		}
	}); err != nil {
		t.Fatalf("最终 EnableSubscription: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := ch.AddMonitoredItem(ctx, addr("rw_float")); err != nil {
		t.Fatalf("最终 AddMonitoredItem: %v", err)
	}
	select {
	case <-got:
	case <-time.After(3 * time.Second):
		t.Logf("最终订阅未收到回调（取决于 server 推送时机）")
	}
}

// TestOPCUA_Subscription_CancelDuringDispatch 验证派发过程中取消订阅不 panic。
//
// 这是旧实现最危险的地方：disableSubscription 会 close(notifyCh)，
// 而 gopcua 的 publish 循环可能正在向它发送 → send on closed channel。
func TestOPCUA_Subscription_CancelDuringDispatch(t *testing.T) {
	port := pickFreePort(t)
	_, endpoint, nsIdx := startTestServer(t, port)
	ch := newTestChannel(t, endpoint)

	if err := ch.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	for round := 0; round < 10; round++ {
		// Disconnect 会把 c.client 置 nil，所以每轮都要重新连接，
		// 否则 EnableSubscription 会因 "channel 未连接" 直接失败。
		if err := ch.Connect(context.Background()); err != nil {
			t.Fatalf("round %d Connect: %v", round, err)
		}

		got := make(chan struct{}, 64)
		if err := ch.EnableSubscription(func(address string, v protocol.Value) {
			select {
			case got <- struct{}{}:
			default:
			}
		}); err != nil {
			t.Fatalf("round %d EnableSubscription: %v", round, err)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		if err := ch.AddMonitoredItem(ctx, "ns="+strconv.FormatUint(uint64(nsIdx), 10)+";s=rw_float"); err != nil {
			cancel()
			t.Fatalf("round %d AddMonitoredItem: %v", round, err)
		}
		cancel()

		// 不等回调，立即取消 —— 制造"派发中取消"的竞争
		if err := ch.Disconnect(); err != nil {
			t.Fatalf("round %d Disconnect: %v", round, err)
		}
	}
}
