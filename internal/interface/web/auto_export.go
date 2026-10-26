// auto_export.go：配置变更后自动导出 config.json（可选开关，默认关闭）。
//
// 为什么需要"合并/防抖"而不是每次写操作都立刻导出：
//  1. 批量导入场景（CSV 导入 500 个点位）会产生成百上千次写请求，
//     每次都重写 config.json → plcbridge 被 fsnotify 触发几百次热重载，
//     每次重载都要重建全部连接池 —— 会把正在跑的采集打残。
//  2. 人为连续调参时（改一个地址 → 看效果 → 再改一个）也没必要每次都重载。
//
// 所以这里用**尾沿防抖 + 合并**：收到触发信号后等 autoExportDebounce 毫秒，
// 期间再来信号就重置计时器，最后只导出一次。
//
// 触发时机与 autoSnapshotMiddleware 完全一致（同一份 watched 路径表），
// 保证"自动导出"和"脏状态角标"看到的是同一批改动 —— 不会出现
// "角标说还有未导出，但自动导出已经导了"的矛盾状态。
package web

import (
	"context"
	"database/sql"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/plcbridge/plcbridge/internal/infra/logging"
)

// autoExportDebounce 自动导出的合并窗口。
//
// 取 1.5s：足够把一次批量导入的连续写请求合并成一次，
// 又短到用户感知不到"延迟生效"。
const autoExportDebounce = 1500 * time.Millisecond

// autoExportTriggerMiddleware 是 gin 中间件：配置变更成功后触发自动导出。
//
// 与 autoSnapshotMiddleware 用**完全相同**的路径判定（isConfigMutatingPath），
// 保证"自动导出"和"脏状态角标"看到同一批改动。
//
// 关键：开关默认关闭，且**每次触发都重新读设置** —— 用户在前端开了开关后，
// 下一次改动立即生效，不用重启 web。
//
// 触发是异步的（Trigger 立即返回），绝不在请求路径上阻塞等待导出。
func (s *Store) autoExportTriggerMiddleware(exporter *autoExporter) gin.HandlerFunc {
	return func(c *gin.Context) {
		path := c.Request.URL.Path
		method := c.Request.Method

		if !isConfigMutatingPath(method, path) {
			c.Next()
			return
		}

		c.Next() // 先执行 handler

		// 只有成功才触发。
		if c.Writer.Status() < 200 || c.Writer.Status() >= 300 {
			return
		}

		// 读开关（失败视为关闭）。
		ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
		enabled := s.autoExportEnabled(ctx)
		cancel()
		if !enabled {
			return
		}

		exporter.Trigger()
	}
}

// autoExporter 负责合并触发信号并执行导出。
type autoExporter struct {
	db        *sql.DB
	configDir string

	mu      sync.Mutex
	timer   *time.Timer
	pending bool
	// running 标记当前是否有导出正在进行（避免并发导出互相覆盖 config.json）。
	running bool
	// closed 标记导出器已关闭（进程退出），关闭后不再触发任何导出。
	closed bool
}

// newAutoExporter 创建自动导出器。
func newAutoExporter(db *sql.DB, configDir string) *autoExporter {
	return &autoExporter{db: db, configDir: configDir}
}

// Trigger 请求一次导出（合并窗口内的重复调用只会导致一次实际导出）。
//
// 立即返回 —— 绝不能在 HTTP 请求处理路径上阻塞等待导出完成
// （导出要查全库 + 写文件，可能上百毫秒）。
func (a *autoExporter) Trigger() {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.closed {
		return
	}
	// 标记有未导出的改动。
	a.pending = true
	if a.timer != nil {
		// 已有待触发的计时器 → 重置它（把窗口往后推，合并连续触发）。
		a.timer.Reset(autoExportDebounce)
		return
	}
	a.timer = time.AfterFunc(autoExportDebounce, a.fire)
}

// fire 计时器回调：真正执行导出。
//
// 状态机（pending / running / timer 三个标志的组合）：
//
//	pending=false            → 无事可做，退出
//	pending=true, running=false → 正常导出
//	pending=true, running=true  → 上一次导出还没结束 → 不能并发写 config.json
//	                                → 保持 pending，等本次导出结束后再排一次
//
// 关键：导出**期间**到达的改动（pending 被置 true）不能丢 ——
// 导出读到的是"导出开始那一刻"的数据库状态，之后的改动不在里面。
// 所以导出结束后若 pending 仍为 true，必须再导一次。
func (a *autoExporter) fire() {
	a.mu.Lock()
	a.timer = nil
	if a.closed || !a.pending {
		a.mu.Unlock()
		return
	}
	if a.running {
		// 上一次导出还在跑 → 不并发导出，本次 fire 直接退出。
		// pending 保持 true，导出结束时会重新排一次（见下方 doExport 之后）。
		a.mu.Unlock()
		return
	}
	a.pending = false
	a.running = true
	a.mu.Unlock()

	err := a.doExport()

	a.mu.Lock()
	a.running = false
	// 导出期间又来了新触发（pending=true）→ 再排一次，把那些改动也导出去。
	// 注意 timer==nil 是必须的：否则会和 Trigger 里 Reset 的计时器重复排期。
	if a.pending && a.timer == nil && !a.closed {
		a.timer = time.AfterFunc(autoExportDebounce, a.fire)
	}
	a.mu.Unlock()

	if err != nil {
		logging.Warn(logging.EventConfigReloadFail).
			Str("err", err.Error()).
			Msg("auto export config failed")
	}
}

// doExport 执行实际导出（复用与手动导出完全相同的逻辑）。
func (a *autoExporter) doExport() error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return reloadConfigViaExport(ctx, a.db, a.configDir)
}

// Close 停止自动导出器（进程退出时调用，清理待触发的计时器）。
func (a *autoExporter) Close() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.closed = true
	if a.timer != nil {
		a.timer.Stop()
		a.timer = nil
	}
	a.pending = false
}

// autoExportEnabled 读取自动导出开关。
//
// 每次触发时都重新读（不缓存）：用户在前端开了开关后，下一次改动就应立即生效，
// 不需要重启 web。
func (s *Store) autoExportEnabled(ctx context.Context) bool {
	enabled, err := s.GetSettingBool(ctx, SettingAutoExportConfig)
	if err != nil {
		// 读失败（表不存在等）→ 视为关闭，绝不因读取失败就意外开启自动导出。
		return false
	}
	return enabled
}
