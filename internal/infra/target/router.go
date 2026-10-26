// Router 按 target_biz_id 索引 Queue。
//
// 由 collect 模块调用 Enqueue，由 forward 模块调用 Dequeue。
// 配置热加载时刷新映射。
package target

import (
	"sync"

	"github.com/plcbridge/plcbridge/internal/infra/config"
	"github.com/plcbridge/plcbridge/internal/infra/logging"
)

// Router 按 target_biz_id 索引 Queue。
type Router struct {
	store  *config.Store
	mu     sync.RWMutex
	queues map[string]*Queue
}

// NewRouter 创建 Router。
func NewRouter(store *config.Store) *Router {
	r := &Router{
		store:  store,
		queues: make(map[string]*Queue),
	}
	r.refresh()
	return r
}

// refresh 从当前 config 重建 queue 映射。
//
// 保留已存在 queue（保留缓冲数据），仅添加新 target；移除已删除的 target。
func (r *Router) refresh() {
	cfg := r.store.Current()
	if cfg == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	seen := map[string]bool{}
	for _, dev := range cfg.Devices {
		for _, tgt := range dev.Targets {
			seen[tgt.TargetBizID] = true
			if _, exists := r.queues[tgt.TargetBizID]; !exists {
				size := 10000
				r.queues[tgt.TargetBizID] = NewQueue(size, tgt.TargetBizID)
				logging.Info(logging.EventTargetConnectOK).
					Str("target", tgt.TargetBizID).
					Int("queue_size", size).
					Msg("创建 Target 队列")
			}
		}
	}
	for id, q := range r.queues {
		if !seen[id] {
			// 先 close queue，让阻塞在 Raw()/Dequeue() 上的 forwarder 立即收到信号退出，
			// 避免旧 buffer 永久挂在内存里
			q.Close()
			delete(r.queues, id)
			logging.Info(logging.EventTargetDisconnect).
				Str("target", id).
				Msg("移除 Target 队列")
		}
	}
}

// Get 获取指定 target 的队列。
func (r *Router) Get(targetBizID string) *Queue {
	r.mu.RLock()
	q, ok := r.queues[targetBizID]
	r.mu.RUnlock()
	if !ok {
		r.refresh()
		r.mu.RLock()
		q = r.queues[targetBizID]
		r.mu.RUnlock()
	}
	return q
}

// AllQueues 返回所有队列（forward worker 启动时遍历）。
func (r *Router) AllQueues() map[string]*Queue {
	r.refresh()
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]*Queue, len(r.queues))
	for k, v := range r.queues {
		out[k] = v
	}
	return out
}

// ClearQueue 清空指定 target 的队列（停用 device 时调用，避免 forward worker
// 启动后把历史数据冲刷出去）。返回清空的元素个数。
func (r *Router) ClearQueue(targetBizID string) int {
	r.refresh()
	r.mu.RLock()
	q := r.queues[targetBizID]
	r.mu.RUnlock()
	if q == nil {
		return 0
	}
	return q.Clear()
}
