// plcbridge 主进程的插件元信息代理（Plan2 主题 A 3.4）。
//
// 用途：前端 Vue 端动态生成协议下拉框 + 配置表单，调用 /api/v1/protocols。
// plcbridge 主进程已将该接口公开（无鉴权），web 后端转发一次缓存，
// 避免每次 Vue 调用都跨进程打 plcbridge。
//
// 数据流：
//
//	Vue (axios /api/v1/protocols)
//	  → web 后端 (本文件协议代理)  ← 1.5s 内命中缓存
//	  → plcbridge :8082 /api/v1/protocols
//
// 为什么用缓存：
//   - 协议元数据在进程生命周期内几乎不变（init() 一次性注册）
//   - 每次刷新通道页/创建通道表单都调，缓存降低 plcbridge 端 < 1ms 的 CPU 压力
//   - 与 metrics_proxy 一致：1s ticker 后台预热 + 内存 mutex cache
package web

import (
	"context"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

const plcbridgeProtocolsURL = "http://127.0.0.1:8082/api/v1/protocols"

// protocolsCache 缓存最近一次拉取的 /api/v1/protocols 响应。
type protocolsCache struct {
	mu       sync.RWMutex
	body     []byte
	fetched  time.Time
	fetchErr string
}

var (
	plcbridgeProtocolsCache     protocolsCache
	plcbridgeProtocolsCacheOnce sync.Once
)

// protocolsProxyHandler GET /api/v1/protocols
//
// 返回 plcbridge 主进程的已注册协议 / Target 元信息（{collect: [...], forward: [...]}）。
//
// 鉴权：跟随 web 后端的 auth group（已登录用户才能调）。
//
// 缓存策略：
//   - 启动后台 5s ticker 拉一次（协议注册表启动后基本不变，5s 足够）
//   - 命中 5s 内缓存直接返回（前端每次操作都调，高频读）
//   - 失败时返回缓存的旧值（不报错给前端），保证 plcbridge 重启期间前端不卡
func protocolsProxyHandler() gin.HandlerFunc {
	// 启动后台预热（首屏不等 5s）
	plcbridgeProtocolsCacheOnce.Do(func() {
		go refreshProtocolsLoop()
	})
	return func(c *gin.Context) {
		// 1. 命中缓存：直接返回
		plcbridgeProtocolsCache.mu.RLock()
		age := time.Since(plcbridgeProtocolsCache.fetched)
		body := plcbridgeProtocolsCache.body
		fetchErr := plcbridgeProtocolsCache.fetchErr
		plcbridgeProtocolsCache.mu.RUnlock()

		// 缓存未初始化 或 过期 → 同步拉一次
		if body == nil || age > 5*time.Second {
			refreshProtocols()
			plcbridgeProtocolsCache.mu.RLock()
			body = plcbridgeProtocolsCache.body
			fetchErr = plcbridgeProtocolsCache.fetchErr
			plcbridgeProtocolsCache.mu.RUnlock()
		}

		// 2. 完全没拉到（plcbridge 还没启动）：返回 503 而不是缓存
		if body == nil {
			// Plan2 4.1 一致：503 也走 {code, msg} 格式
			WriteError(c, ErrPlcbridgeUnavailable.With("plcbridge 主进程暂不可达: "+fetchErr))
			return
		}

		// 3. 透传响应（含 Content-Type 让前端能正确解析）
		c.Data(http.StatusOK, "application/json; charset=utf-8", body)
	}
}

// refreshProtocolsLoop 后台每 5s 拉一次 :8082/api/v1/protocols。
//
// 为什么 5s 而不是 1s：协议元数据是注册表驱动，进程内几乎不变；高频无意义。
func refreshProtocolsLoop() {
	refreshProtocols()
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for range t.C {
		refreshProtocols()
	}
}

func refreshProtocols() {
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, plcbridgeProtocolsURL, nil)
	if err != nil {
		updateProtocolsCacheErr(err.Error())
		return
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		updateProtocolsCacheErr(err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		updateProtocolsCacheErr("status " + http.StatusText(resp.StatusCode))
		return
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1*1024*1024)) // 1MB 上限，配置 schema 可能较大
	if err != nil {
		updateProtocolsCacheErr(err.Error())
		return
	}
	plcbridgeProtocolsCache.mu.Lock()
	plcbridgeProtocolsCache.body = body
	plcbridgeProtocolsCache.fetched = time.Now()
	plcbridgeProtocolsCache.fetchErr = ""
	plcbridgeProtocolsCache.mu.Unlock()
}

func updateProtocolsCacheErr(msg string) {
	plcbridgeProtocolsCache.mu.Lock()
	plcbridgeProtocolsCache.fetchErr = msg
	plcbridgeProtocolsCache.fetched = time.Now()
	plcbridgeProtocolsCache.mu.Unlock()
}
