// Package bench —— metricsgrep：周期性 scrape Prometheus metrics server，
// 从 plcbridge_collect_duration_seconds histogram 反推 P50/P99。
//
// 不直接改生产 collect 包，零侵入；只读默认 /metrics endpoint。
package bench

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// CollectDurSnapshot 是单次 scrape 后的延迟分布快照。
type CollectDurSnapshot struct {
	Timestamp time.Time
	Count     uint64  // plcbridge_collect_duration_seconds_count
	SumSec    float64 // plcbridge_collect_duration_seconds_sum
	P50MS     float64 // 50 分位（ms）
	P99MS     float64 // 99 分位（ms）
	MaxMS     float64 // max bucket（ms）
}

// ScrapeCollectDuration 拉一次 metrics server，把 collect_duration 的 histogram
// 拼成 P50/P99。
func ScrapeCollectDuration(addr string) (CollectDurSnapshot, error) {
	cli := &http.Client{Timeout: 3 * time.Second}
	resp, err := cli.Get("http://" + addr + "/metrics")
	if err != nil {
		return CollectDurSnapshot{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return CollectDurSnapshot{}, err
	}
	return parseCollectDuration(string(body))
}

// parseCollectDuration 解析 prometheus exposition 格式里的
//
//	plcbridge_collect_duration_seconds_bucket{le="0.3"} 12345
//	plcbridge_collect_duration_seconds_bucket{le="+Inf"} 99999
//	plcbridge_collect_duration_seconds_count 99999
//	plcbridge_collect_duration_seconds_sum 1234.5
//
// 把 bucket 拼成 CDF → P50/P99。
//
// 多 series 时（不同 device/source）以 sum 聚合：每行计数相加，buckets 按各 series
// 独立保持（简化：取最后一次出现的 series 的 buckets，按总 count 重算分位）。
func parseCollectDuration(text string) (CollectDurSnapshot, error) {
	snap := CollectDurSnapshot{Timestamp: time.Now()}
	// series 标签为 device_biz_id="..." source_biz_id="..."
	// 每个 series 单独算分位，最后 max 报告 P99。
	type series struct {
		buckets []bucketRow
		count   uint64
		sumSec  float64
	}
	seriesMap := map[string]*series{}
	for _, line := range strings.Split(text, "\n") {
		if !strings.HasPrefix(line, "plcbridge_collect_duration_seconds_") {
			continue
		}
		rest := strings.TrimPrefix(line, "plcbridge_collect_duration_seconds_")
		fields := strings.Fields(rest)
		if len(fields) < 2 {
			continue
		}
		// series key：提取所有 label key=value
		key := extractSeriesKey(line)
		s, ok := seriesMap[key]
		if !ok {
			s = &series{}
			seriesMap[key] = s
		}
		switch {
		case strings.HasPrefix(rest, "bucket{"):
			leStr := extractLabel(rest, "le")
			if leStr == "" {
				continue
			}
			val := fields[len(fields)-1]
			v, err := strconv.ParseFloat(val, 64)
			if err != nil {
				continue
			}
			var le float64
			if leStr == "+Inf" {
				le = 1e9
			} else {
				le, err = strconv.ParseFloat(leStr, 64)
				if err != nil {
					continue
				}
			}
			s.buckets = append(s.buckets, bucketRow{LE: le, Count: v})
		case strings.HasPrefix(rest, "count ") || strings.HasPrefix(rest, "count{"):
			val := fields[len(fields)-1]
			v, err := strconv.ParseFloat(val, 64)
			if err != nil {
				continue
			}
			s.count = uint64(v)
		case strings.HasPrefix(rest, "sum ") || strings.HasPrefix(rest, "sum{"):
			val := fields[len(fields)-1]
			v, err := strconv.ParseFloat(val, 64)
			if err != nil {
				continue
			}
			s.sumSec = v
		}
	}
	if len(seriesMap) == 0 {
		return snap, nil
	}
	// 总 count / sum
	var totalCount uint64
	var totalSum float64
	for _, s := range seriesMap {
		totalCount += s.count
		totalSum += s.sumSec
	}
	snap.Count = totalCount
	snap.SumSec = totalSum
	// 分位：找 count 最大的那个 series（也就是最近采集最频繁的），用它算 P99
	// 因为 P99 在并发系统中通常是 max-of-series；用最大 series 的 buckets 做分位估算
	var bestBuckets []bucketRow
	var bestCount uint64
	for k, s := range seriesMap {
		tlog("series %q count=%d buckets=%d\n", k, s.count, len(s.buckets))
		if s.count > bestCount {
			bestCount = s.count
			bestBuckets = s.buckets
		}
	}
	if bestCount == 0 || len(bestBuckets) == 0 {
		return snap, nil
	}
	snap.P50MS = percentileFromBuckets(bestBuckets, bestCount, 0.50) * 1000
	snap.P99MS = percentileFromBuckets(bestBuckets, bestCount, 0.99) * 1000
	snap.MaxMS = maxBucket(bestBuckets) * 1000
	return snap, nil
}

func extractSeriesKey(line string) string {
	// 提取所有 "{...}" 标签内容，但剔除 le="..." 标签。
	// le 是 histogram bucket 上界，会让同一 series 的不同 bucket 行散到不同的 key。
	start := strings.Index(line, "{")
	end := strings.LastIndex(line, "}")
	if start < 0 || end < 0 || end <= start {
		return ""
	}
	labels := line[start+1 : end]
	kept := make([]string, 0, 4)
	for _, lbl := range splitLabels(labels) {
		eq := strings.IndexByte(lbl, '=')
		if eq < 0 {
			continue
		}
		key := lbl[:eq]
		if key == "le" {
			continue
		}
		kept = append(kept, lbl)
	}
	return "{" + strings.Join(kept, ",") + "}"
}

// splitLabels 按顶层逗号切分 label 列表。label 内的引号不会破坏切分
// （Prometheus label 值不包含逗号）。
func splitLabels(s string) []string {
	var out []string
	depth := 0
	start := 0
	for i, r := range s {
		switch r {
		case '"':
			if depth == 0 {
				depth = 1
			} else {
				depth = 0
			}
		case ',':
			if depth == 0 {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	if start <= len(s) {
		out = append(out, s[start:])
	}
	return out
}

type bucketRow struct {
	LE    float64
	Count float64
}

func extractLabel(line, key string) string {
	idx := strings.Index(line, key+"=")
	if idx < 0 {
		return ""
	}
	rest := line[idx+len(key)+1:]
	if len(rest) == 0 {
		return ""
	}
	if rest[0] == '"' {
		rest = rest[1:]
		end := strings.IndexByte(rest, '"')
		if end < 0 {
			return ""
		}
		return rest[:end]
	}
	end := strings.IndexAny(rest, ",} ")
	if end < 0 {
		return rest
	}
	return rest[:end]
}

// percentileFromBuckets 按累计计数查找给定分位对应的桶上界。
func percentileFromBuckets(buckets []bucketRow, total uint64, q float64) float64 {
	if total == 0 {
		return 0
	}
	target := float64(total) * q
	for _, b := range buckets {
		if b.Count >= target {
			return b.LE
		}
	}
	return 1e9
}

func tlog(format string, args ...any) {
	// 默认静默；调试时改回 fmt.Fprintf(os.Stderr, ...)
	_ = format
	_ = args
}

func tlogEnabled() bool { return false }

func maxBucket(buckets []bucketRow) float64 {
	m := 0.0
	for _, b := range buckets {
		if b.LE < 1e9 && b.LE > m {
			m = b.LE
		}
	}
	return m
}

// MustEnsureServerAlive 等待 metrics server 在 addr 上 ready（用于启动顺序）。
func MustEnsureServerAlive(addr string, max time.Duration) error {
	deadline := time.Now().Add(max)
	for {
		_, err := ScrapeCollectDuration(addr)
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("metrics server at %s not alive within %s", addr, max)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
