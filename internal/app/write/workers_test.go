// Package write 的最小 lifecycle 测试。
package write

import (
	"context"
	"testing"
	"time"

	"github.com/plcbridge/plcbridge/internal/infra/config"
	"github.com/plcbridge/plcbridge/internal/infra/pool"
	"github.com/plcbridge/plcbridge/internal/infra/protocol"
)

func TestWorkers_StartStop(t *testing.T) {
	store := config.NewStore(t.TempDir())
	pools := pool.NewPools(pool.Config{CollectSize: 2, ForwardSize: 2, WriteSize: 2, ConfigSize: 2})
	pools.Start(context.Background(), nil)
	defer pools.Stop(time.Second)

	registry := protocol.NewChannelRegistry()
	w := NewWorkers(store, pools, registry)

	ctx, cancel := context.WithCancel(context.Background())
	w.Start(ctx)

	w.Stop(time.Second)
	w.Stop(time.Second)

	cancel()
	time.Sleep(50 * time.Millisecond)
}

// 校验 pruneQueues 对 nil cfg 不 panic，且能正常完成。
func TestWorkers_pruneQueues_Nil(t *testing.T) {
	store := config.NewStore(t.TempDir())
	pools := pool.NewPools(pool.Config{CollectSize: 2, ForwardSize: 2, WriteSize: 2, ConfigSize: 2})
	pools.Start(context.Background(), nil)
	defer pools.Stop(time.Second)

	registry := protocol.NewChannelRegistry()
	w := NewWorkers(store, pools, registry)
	w.pruneQueues(nil) // nil snapshot 安全
}
