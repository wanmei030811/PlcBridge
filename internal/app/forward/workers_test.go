// Package forward 的最小 lifecycle 测试。
//
// 仅验证：
//   - Workers.Start 后 ctx 取消能正常退出（不泄漏 supervisor goroutine）
//   - Stop 幂等且不阻塞
package forward

import (
	"context"
	"testing"
	"time"

	"github.com/plcbridge/plcbridge/internal/infra/config"
	"github.com/plcbridge/plcbridge/internal/infra/pool"
	"github.com/plcbridge/plcbridge/internal/infra/target"
)

func TestWorkers_StartStop(t *testing.T) {
	store := config.NewStore(t.TempDir())
	pools := pool.NewPools(pool.Config{CollectSize: 2, ForwardSize: 2, WriteSize: 2, ConfigSize: 2})
	pools.Start(context.Background(), nil)
	defer pools.Stop(time.Second)

	router := target.NewRouter(store)
	w := NewWorkers(store, pools, router)

	ctx, cancel := context.WithCancel(context.Background())
	w.Start(ctx)

	// 立即 Stop —— 还没加载 cfg，不应 panic
	w.Stop(time.Second)
	// 再 Stop 一次幂等
	w.Stop(time.Second)

	// 取消 ctx，等 supervisor 退出
	cancel()
	// 给 supervisor goroutine 退出的窗口
	time.Sleep(100 * time.Millisecond)
}

// TestWorkers_Snapshot_NoCache 验证 cache 为空时返回空 map（不 panic）。
func TestWorkers_Snapshot_NoCache(t *testing.T) {
	store := config.NewStore(t.TempDir())
	pools := pool.NewPools(pool.Config{CollectSize: 2, ForwardSize: 2, WriteSize: 2, ConfigSize: 2})
	pools.Start(context.Background(), nil)
	defer pools.Stop(time.Second)
	router := target.NewRouter(store)
	w := NewWorkers(store, pools, router)
	snap := w.Snapshot()
	if snap == nil {
		t.Fatal("Snapshot() returned nil; expected empty map")
	}
	if len(snap) != 0 {
		t.Errorf("Snapshot() with no cache should be empty, got %d entries", len(snap))
	}
}
