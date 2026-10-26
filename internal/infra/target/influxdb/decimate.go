// decimate.go：历史数据的「保真抽稀」。
//
// 背景（2026-10-22）：
//
//	用户反馈「落库的数值是 1、2、3、4，查询到的是 1、2.1、3.67」。
//	第一层原因是用 mean() 聚合 → 会算出库里根本不存在的值。
//	改成 last() 之后（见 history.go），返回的每个值都确实是真实写入过的，
//	但**短命的值仍然会丢**：桶聚合一个桶只能返回一个值。
//
//	受控实验（在库里写入 1、2、3、4，相隔 1 秒）：
//
//		新查询 last() + GROUP BY time(2s) → 返回 4, 3, 1   ← 值 2 消失了
//		旧查询 mean() + GROUP BY time(1m) → 返回 2.5        ← 库里没有这个值
//
//	所以桶聚合从原理上就无法还原「值序列」。
//
//	本文件提供保真抽稀：先从 InfluxDB 取**原始点**（不聚合），
//	再在 Go 侧压缩到 limit 个点，压缩时**保证每一次数值变化都被保留**。
//
//	算法：
//	  1. 找出所有「值发生变化」的点（每个连续同值段的起点）——这些是必须保留的。
//	  2. 必须保留的点数 > limit 时，对它们等间隔抽样（首尾必留）。
//	  3. 否则全数保留，再用剩余名额从同值段内部均匀补点，还原曲线形状。
package influxdb

import (
	"reflect"
	"sort"
	"time"
)

// historyPoint 是历史查询结果的内部表示。
type historyPoint struct {
	t time.Time
	v any
}

// decimateHistoryRows 把原始点压缩到最多 limit 个，
// 同时保证所有「值发生变化」的点都被保留。
//
// 入参 rows 可以是任意顺序，内部会按时间升序重排。
// 返回值仍是 []map[string]any（"_time" / "_value"），与查询层返回的类型一致。
func decimateHistoryRows(rows []map[string]any, limit int) []map[string]any {
	if limit <= 0 {
		limit = historyDefaultLimit
	}
	pts := toHistoryPoints(rows)
	if len(pts) == 0 {
		return nil
	}
	// 无论是否超限都要先排序：
	// 查询层用 ORDER BY time DESC 取「最近的点」，返回的是最新在前；
	// 图表要求 x 轴从左到右时间递增，所以统一在这里排成升序。
	sort.SliceStable(pts, func(i, j int) bool { return pts[i].t.Before(pts[j].t) })
	if len(pts) <= limit {
		return buildHistoryRows(pts, allIndexes(len(pts)))
	}

	// 1) 标记「值发生变化」的点：每个连续同值段的起点。
	keep := make([]bool, len(pts))
	keep[0] = true
	for i := 1; i < len(pts); i++ {
		if !sameHistoryValue(pts[i].v, pts[i-1].v) {
			keep[i] = true
		}
	}

	var mandatory []int
	var optional []int
	for i, k := range keep {
		if k {
			mandatory = append(mandatory, i)
		} else {
			optional = append(optional, i)
		}
	}

	// 2) 变化点超过 limit → 对变化点等间隔抽样。
	//    （变化点太多时无法全部保留，这是 limit 的固有约束。）
	if len(mandatory) > limit {
		return buildHistoryRows(pts, pickEvenly(mandatory, limit))
	}

	// 3) 变化点全留 + 用剩余名额补点，还原曲线形状。
	selected := make([]int, 0, limit)
	selected = append(selected, mandatory...)
	if budget := limit - len(selected); budget > 0 && len(optional) > 0 {
		selected = append(selected, pickEvenly(optional, budget)...)
	}
	sort.Ints(selected)
	return buildHistoryRows(pts, selected)
}

// allIndexes 返回 [0, n) 的全部下标。
func allIndexes(n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = i
	}
	return out
}

// pickEvenly 从 items 中等间隔挑出最多 k 个下标。
//
// 等间隔（而不是取前 k 个）是为了让抽稀后的点均匀铺满整个时间窗口，
// 避免图上只有开头一段有点、后面全是空白。
func pickEvenly(items []int, k int) []int {
	if k <= 0 || len(items) == 0 {
		return nil
	}
	if len(items) <= k {
		out := make([]int, len(items))
		copy(out, items)
		return out
	}
	if k == 1 {
		return []int{items[len(items)/2]}
	}
	out := make([]int, 0, k)
	seen := make(map[int]bool, k)
	for i := 0; i < k; i++ {
		idx := items[i*(len(items)-1)/(k-1)]
		if seen[idx] {
			continue
		}
		seen[idx] = true
		out = append(out, idx)
	}
	return out
}

// buildHistoryRows 按下标集合还原成 []map[string]any。
func buildHistoryRows(pts []historyPoint, idxs []int) []map[string]any {
	out := make([]map[string]any, 0, len(idxs))
	for _, i := range idxs {
		if i < 0 || i >= len(pts) {
			continue
		}
		out = append(out, map[string]any{
			"_time":  pts[i].t,
			"_value": pts[i].v,
		})
	}
	return out
}

// toHistoryPoints 把查询结果行转成内部结构。
func toHistoryPoints(rows []map[string]any) []historyPoint {
	out := make([]historyPoint, 0, len(rows))
	for _, r := range rows {
		t, ok := r["_time"].(time.Time)
		if !ok {
			continue
		}
		v, has := r["_value"]
		if !has {
			continue
		}
		out = append(out, historyPoint{t: t, v: v})
	}
	return out
}

// sameHistoryValue 比较两个历史值是否相等。
//
// 数值按数值比（float64(1) == int64(1) 视为相等），
// 其它类型退回 reflect.DeepEqual。
func sameHistoryValue(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	af, aok := toFloat64(a)
	bf, bok := toFloat64(b)
	if aok && bok {
		return af == bf
	}
	return reflect.DeepEqual(a, b)
}

// toFloat64 尝试把值转成 float64。
func toFloat64(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int8:
		return float64(n), true
	case int16:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint:
		return float64(n), true
	case uint8:
		return float64(n), true
	case uint16:
		return float64(n), true
	case uint32:
		return float64(n), true
	case uint64:
		return float64(n), true
	}
	return 0, false
}
