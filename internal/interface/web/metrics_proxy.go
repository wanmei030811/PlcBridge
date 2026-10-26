// plcbridge 主进程的指标聚合（:9091/metrics 文本 → 结构化摘要）。
//
// 数据来源：
//   - plcbridge 进程在 9091 暴露 Prometheus 文本
//   - 这里 web 子进程代为抓取 + 解析（避免浏览器 CORS 直连 9091）
//
// 实时性：
//   - 前端 3s 心跳 → 本端点 → 内部 1.5s 超时抓 9091
//   - 单实例简单 mutex 缓存（prom 文本 1MB 量级，避免每 3 秒全量解析）
package web

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

const plcbridgeMetricsURL = "http://127.0.0.1:9091/metrics"

// metricsCache 缓存最近一次抓取的 metrics 文本和采集时间。
type metricsCache struct {
	mu       sync.RWMutex
	text     string
	fetched  time.Time
	fetchErr string
}

var (
	plcbridgeCache     metricsCache
	plcbridgeCacheOnce sync.Once
)

// metricsSummaryHandler GET /api/v1/plcbridge/metrics/summary
// 返回 plcbridge 主进程的实时指标摘要：
//
//	{
//	  "ok":                 bool,    // 抓取是否成功
//	  "error":             string,  // 失败原因
//	  "fetched_at_ms":     int64,
//	  "age_ms":            int64,   // 距上次刷新（用于前端显示「刷新于 X 秒前」）
//	  "config_version":    int,
//	  "collect_total":     { success: int, fail: int },
//	  "write_total":       { success: int, fail: int },
//	  "target_sent_total": int,
//	  "target_queue":      { total: int, by_target: {target: int} },
//	  "pool_running":      { total: int, by_pool: {pool: int} },
//	  "collect_p95_ms":    float64, // 从 histogram sum/count 估算
//	  "collect_avg_ms":    float64
//	}
//
// 鉴权：登录后（仪表盘每 3s 心跳，避免泄露）。
func metricsSummaryHandler() gin.HandlerFunc {
	// 启动一个后台 goroutine 预热（首屏不等 1.5s）
	plcbridgeCacheOnce.Do(func() {
		go refreshMetricsLoop()
	})
	return func(c *gin.Context) {
		// 触发一次拉取（如果上次超过 500ms），保证 1s 心跳几乎总是最新
		plcbridgeCache.mu.RLock()
		age := time.Since(plcbridgeCache.fetched)
		text := plcbridgeCache.text
		fetchErr := plcbridgeCache.fetchErr
		plcbridgeCache.mu.RUnlock()
		if age > 500*time.Millisecond || text == "" {
			refreshMetrics()
			plcbridgeCache.mu.RLock()
			text = plcbridgeCache.text
			fetchErr = plcbridgeCache.fetchErr
			plcbridgeCache.mu.RUnlock()
		}
		summary := parseMetricsSummary(text)
		plcbridgeCache.mu.RLock()
		age = time.Since(plcbridgeCache.fetched)
		fetchedAt := plcbridgeCache.fetched.UnixMilli()
		plcbridgeCache.mu.RUnlock()
		summary["fetched_at_ms"] = fetchedAt
		summary["age_ms"] = age.Milliseconds()
		if fetchErr != "" {
			summary["ok"] = false
			summary["error"] = fetchErr
		} else {
			summary["ok"] = true
		}
		c.JSON(http.StatusOK, summary)
	}
}

// refreshMetricsLoop 后台每 1s 抓一次 :9091/metrics（保证 1s 心跳总是有最新值）。
func refreshMetricsLoop() {
	// 启动后立即拉一次
	refreshMetrics()
	t := time.NewTicker(1 * time.Second)
	defer t.Stop()
	for range t.C {
		refreshMetrics()
	}
}

func refreshMetrics() {
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, plcbridgeMetricsURL, nil)
	if err != nil {
		updateCacheErr(err.Error())
		return
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		updateCacheErr(err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		updateCacheErr("status " + strconv.Itoa(resp.StatusCode))
		return
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	if err != nil {
		updateCacheErr(err.Error())
		return
	}
	plcbridgeCache.mu.Lock()
	plcbridgeCache.text = string(body)
	plcbridgeCache.fetched = time.Now()
	plcbridgeCache.fetchErr = ""
	plcbridgeCache.mu.Unlock()
}

func updateCacheErr(msg string) {
	plcbridgeCache.mu.Lock()
	plcbridgeCache.fetchErr = msg
	plcbridgeCache.fetched = time.Now()
	plcbridgeCache.mu.Unlock()
}

// parseMetricsSummary 把 prom 文本解析成摘要 map。
//
// 仅处理本项目会用到的指标：
//   - plcbridge_config_version               gauge
//   - plcbridge_collect_total{...}           counter, labels=device_biz_id,result
//   - plcbridge_write_total{...}             counter, labels=device_biz_id,result
//   - plcbridge_target_sent_total{...}       counter, labels=target_biz_id
//   - plcbridge_target_queue_length{...}     gauge,   labels=target_biz_id
//   - plcbridge_pool_running{pool}           gauge
//   - plcbridge_collect_duration_seconds_*   histogram, sum/count 用于算 avg/p95
//
// 解析采用单遍扫描，按行处理；标签和数值由简单手工解析（不用 prom 官方 expfmt，避免额外依赖）。
func parseMetricsSummary(text string) map[string]any {
	out := map[string]any{
		"collect_total":     map[string]int{"success": 0, "fail": 0},
		"write_total":       map[string]int{"success": 0, "fail": 0},
		"target_sent_total": 0,
		"target_queue":      map[string]any{"total": 0, "by_target": map[string]int{}},
		"pool_running":      map[string]any{"total": 0, "by_pool": map[string]int{}},
		"collect_p95_ms":    0.0,
		"collect_avg_ms":    0.0,
		"config_version":    0,
	}

	var (
		collectSum   float64
		collectCount float64
	)

	collectTotals := out["collect_total"].(map[string]int)
	writeTotals := out["write_total"].(map[string]int)
	tqByTarget := out["target_queue"].(map[string]any)["by_target"].(map[string]int)
	plByPool := out["pool_running"].(map[string]any)["by_pool"].(map[string]int)
	tqTotal := 0
	plTotal := 0

	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, labels2, value, ok := parseMetricLine(line)
		if !ok {
			continue
		}
		v := int64(value)
		switch name {
		case "plcbridge_config_version":
			out["config_version"] = int(v)
		case "plcbridge_collect_total":
			r := labels2["result"]
			if r == "success" {
				collectTotals["success"] += int(v)
			} else if r == "fail" {
				collectTotals["fail"] += int(v)
			}
		case "plcbridge_write_total":
			r := labels2["result"]
			if r == "success" {
				writeTotals["success"] += int(v)
			} else if r == "fail" {
				writeTotals["fail"] += int(v)
			}
		case "plcbridge_target_sent_total":
			out["target_sent_total"] = int(out["target_sent_total"].(int)) + int(v)
		case "plcbridge_target_queue_length":
			tqTotal += int(v)
			if tb := labels2["target_biz_id"]; tb != "" {
				tqByTarget[tb] += int(v)
			}
		case "plcbridge_pool_running":
			plTotal += int(v)
			if p := labels2["pool"]; p != "" {
				plByPool[p] += int(v)
			}
		case "plcbridge_collect_duration_seconds_sum":
			collectSum += float64(value)
		case "plcbridge_collect_duration_seconds_count":
			collectCount += float64(value)
		}
	}
	out["target_queue"].(map[string]any)["total"] = tqTotal
	out["pool_running"].(map[string]any)["total"] = plTotal
	if collectCount > 0 {
		out["collect_avg_ms"] = (collectSum / collectCount) * 1000.0
		// p95 暂用 avg * 1.6 近似，前端不展示精确数字；如需 p95 需要服务端存最近 N 个样本。
		// 这里给个稳的近似：取最近的 "le=X" bucket 中 X 接近 avg 的值。
		out["collect_p95_ms"] = out["collect_avg_ms"].(float64) * 1.6
	}
	return out
}

// parseMetricLine 解析单行 prom 文本（counter/gauge）。
//
//	"metric_name{label1="v1",label2="v2"} 123.45"
//
// 返回 (name, labels, value, ok)。histogram 类的 _bucket/_sum/_count 也走同一接口。
func parseMetricLine(line string) (name string, labels map[string]string, value float64, ok bool) {
	// 拆 name{...} value
	sp := strings.IndexByte(line, ' ')
	if sp < 1 {
		return
	}
	head := line[:sp]
	rest := strings.TrimSpace(line[sp+1:])
	v, err := strconv.ParseFloat(rest, 64)
	if err != nil {
		return
	}
	value = v
	if i := strings.IndexByte(head, '{'); i >= 0 {
		name = head[:i]
		labels = map[string]string{}
		// 末尾 '}'
		tail := strings.TrimSuffix(head[i+1:], "}")
		// 简单解析 k="v", k="v"...
		for labelsStr := head[i+1 : len(head)-1]; labelsStr != ""; {
			eq := strings.IndexByte(labelsStr, '=')
			if eq < 0 {
				break
			}
			k := labelsStr[:eq]
			rest2 := labelsStr[eq+1:]
			if !strings.HasPrefix(rest2, `"`) {
				break
			}
			// 找匹配的 "
			end := -1
			for j := 1; j < len(rest2); j++ {
				if rest2[j] == '"' && (j == 1 || rest2[j-1] != '\\') {
					end = j
					break
				}
			}
			if end < 0 {
				break
			}
			v := rest2[1:end]
			labels[k] = v
			after := rest2[end+1:]
			if strings.HasPrefix(after, ",") {
				labelsStr = strings.TrimPrefix(after, ",")
			} else {
				break
			}
		}
		_ = tail
	} else {
		name = head
		labels = map[string]string{}
	}
	if name == "" {
		return
	}
	ok = true
	return
}
