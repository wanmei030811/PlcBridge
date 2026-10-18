// plcbridge 主进程 8082 上的 GET /api/v1/channels/runtime 在 web 端代理。
//
// 用途：前端「通道（Channel）列表」展示连接状态（绿/黄/红），3s 轮询。
//
// 实时性：
//   - 前端 3s 心跳 → 本端点 → 内部 1.5s 超时抓 8082
//   - 单实例简单 mutex 缓存（runtime 文本小，避免每 3 秒都全量解析/序列化）
//
// 鉴权：需要登录（与 /api/v1/plcbridge/metrics/summary 保持一致）。
package web

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

const plcbridgeChannelsRuntimeURL = "http://127.0.0.1:8082/runtime/channels"

// channelsRuntimeCache 缓存最近一次抓取的 channels runtime 文本。
type channelsRuntimeCache struct {
	mu       sync.RWMutex
	text     string
	fetched  time.Time
	fetchErr string
}

var (
	channelsRuntimeCacheOnce sync.Once
	runtimeCache             channelsRuntimeCache
)

// 初始化缓存对象（lazy，初次访问时建好）。
func ensureChannelsRuntimeCache() {
	channelsRuntimeCacheOnce.Do(func() {
		runtimeCache = channelsRuntimeCache{}
	})
}

// fetchChannelsRuntimeFromCore 抓取 plcbridge 主进程 runtime 状态。
// 失败返回原始错误（给前端展示"plcbridge 不可达"）。
func fetchChannelsRuntimeFromCore(ctx context.Context) (string, time.Time, error) {
	cctx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, plcbridgeChannelsRuntimeURL, nil)
	if err != nil {
		return "", time.Time{}, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", time.Time{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// 读取部分 body 用作错误信息
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", time.Time{}, &runtimeProxyError{status: resp.StatusCode, body: string(b)}
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", time.Time{}, err
	}
	return string(b), time.Now(), nil
}

type runtimeProxyError struct {
	status int
	body   string
}

func (e *runtimeProxyError) Error() string {
	if e.body != "" {
		return "plcbridge " + http.StatusText(e.status) + ": " + e.body
	}
	return "plcbridge " + http.StatusText(e.status)
}

// channelsRuntimeHandler GET /api/v1/plcbridge/channels/runtime
//
// 返回结构（与 plcbridge 8082 端点形状一致，前端直接消费）：
//
//	{
//	  "ok":                 bool,    // 抓取是否成功
//	  "error":             string,  // 失败原因
//	  "fetched_at_ms":     int64,
//	  "age_ms":            int64,   // 距上次刷新（用于前端显示「刷新于 X 秒前」）
//	  "channels": {        // 来自 plcbridge 原样转发
//	    "<channel_biz_id>": {
//	      "connected":       bool,
//	      "last_active_ms":  int64,
//	      "last_error":      string
//	    }
//	  }
//	}
func channelsRuntimeHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		ensureChannelsRuntimeCache()

		// 1) 抓取最新（如果当前没有缓存、或缓存超过 300ms）
		// OFF→ON / ON→OFF 转换的用户感知延迟 = web 缓存 + 前端轮询间隔。
		// 后端 plcbridge 内部检测已 < 100ms，所以这里 1.5s 缓存是最大瓶颈。
		// 缩到 300ms 让 web→plcbridge 的等待接近即时；前端轮询间隔本身在 dist 里
		// （硬编码 setInterval），这里改不到；用户可让前端同学配合调到 500~1000ms。
		runtimeCache.mu.RLock()
		needRefresh := runtimeCache.text == "" || time.Since(runtimeCache.fetched) > 300*time.Millisecond
		runtimeCache.mu.RUnlock()

		if needRefresh {
			text, fetched, err := fetchChannelsRuntimeFromCore(c.Request.Context())
			runtimeCache.mu.Lock()
			if err != nil {
				runtimeCache.fetchErr = err.Error()
				// 失败时保留旧 text 字段，便于降级显示
			} else {
				runtimeCache.text = text
				runtimeCache.fetched = fetched
				runtimeCache.fetchErr = ""
			}
			runtimeCache.mu.Unlock()
		}

		// 2) 组装响应
		runtimeCache.mu.RLock()
		text := runtimeCache.text
		fetched := runtimeCache.fetched
		fetchErr := runtimeCache.fetchErr
		runtimeCache.mu.RUnlock()

		now := time.Now()
		var ageMs int64
		if !fetched.IsZero() {
			ageMs = now.Sub(fetched).Milliseconds()
		}

		// 尝试解析内层 "channels" map（保持原结构）
		var inner struct {
			Channels map[string]json.RawMessage `json:"channels"`
		}
		channels := map[string]json.RawMessage{}
		if text != "" {
			if err := json.Unmarshal([]byte(text), &inner); err == nil && inner.Channels != nil {
				channels = inner.Channels
			}
		}

		ok := fetchErr == "" && text != ""
		resp := gin.H{
			"ok":            ok,
			"error":         fetchErr,
			"fetched_at_ms": unixMs(fetched),
			"age_ms":        ageMs,
			"channels":      channels,
		}
		c.JSON(http.StatusOK, resp)
	}
}

func unixMs(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}
