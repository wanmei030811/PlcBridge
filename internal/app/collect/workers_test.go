// Package collect 的最小 lifecycle 测试。
package collect

import (
	"context"
	"testing"
	"time"

	"github.com/plcbridge/plcbridge/internal/infra/config"
	"github.com/plcbridge/plcbridge/internal/infra/pool"
)

func TestWorkers_StartStop(t *testing.T) {
	store := config.NewStore(t.TempDir())
	pools := pool.NewPools(pool.Config{CollectSize: 2, ForwardSize: 2, WriteSize: 2, ConfigSize: 2})
	pools.Start(context.Background(), nil)
	defer pools.Stop(time.Second)

	w := NewWorkers(store, pools)

	ctx, cancel := context.WithCancel(context.Background())
	w.Start(ctx)

	w.Stop(time.Second)
	w.Stop(time.Second) // 幂等

	cancel()
	time.Sleep(50 * time.Millisecond)

	// Channels 仍可访问（stop 后 registry 保留）
	if w.Channels() == nil {
		t.Errorf("Channels() 应返回非 nil registry")
	}
}
