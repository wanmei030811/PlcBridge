// 保真抽稀（decimate.go）单元测试。
//
// 背景（2026-10-22）：用户反馈「落库的数值是 1、2、3、4，
// 查询到的是 1、2.1、3.67」。根因是查询层用 mean() 做桶聚合，
// 既会算出库里不存在的值，又会吞掉短命的值。
// 修法是「取原始点 + Go 侧保真抽稀」，本文件覆盖抽稀算法的正确性。
package influxdb

import (
	"testing"
	"time"
)

// mkRows 构造 n 个点：第 i 个点的值为 vals[i]，时间戳从 base 开始每秒递增。
func mkRows(base int64, vals []any) []map[string]any {
	rows := make([]map[string]any, 0, len(vals))
	for i, v := range vals {
		rows = append(rows, map[string]any{
			"_time":  time.UnixMilli(base + int64(i)*1000),
			"_value": v,
		})
	}
	return rows
}

// valuesOf 提取行里的值序列。
func valuesOf(rows []map[string]any) []any {
	out := make([]any, 0, len(rows))
	for _, r := range rows {
		out = append(out, r["_value"])
	}
	return out
}

// TestDecimateHistoryRows_UnderLimit 验证点数不超过 limit 时原样返回。
func TestDecimateHistoryRows_UnderLimit(t *testing.T) {
	rows := mkRows(1000, []any{1.0, 2.0, 3.0})
	out := decimateHistoryRows(rows, 100)
	if len(out) != 3 {
		t.Fatalf("返回 %d 行, want 3", len(out))
	}
}

// TestDecimateHistoryRows_PreservesAllChanges 验证所有数值变化都被保留。
//
// 这是本次修复的核心诉求（2026-10-22）：
// 落库 1→2→3→4，查询必须能看到 1、2、3、4 全部四个值，
// 而不是像 mean() 那样压成 2.5，或像桶聚合那样吞掉中间的 2。
func TestDecimateHistoryRows_PreservesAllChanges(t *testing.T) {
	// 1000 个点，其中第 100 / 500 / 900 个点分别是 2 / 3 / 4，其余是 1。
	vals := make([]any, 1000)
	for i := range vals {
		vals[i] = 1.0
	}
	vals[100] = 2.0
	vals[500] = 3.0
	vals[900] = 4.0

	rows := mkRows(1000, vals)
	out := decimateHistoryRows(rows, 100)

	// 抽稀后必须仍能看到 2 / 3 / 4 三个变化值。
	seen := map[float64]bool{}
	for _, v := range valuesOf(out) {
		f, ok := toFloat64(v)
		if ok {
			seen[f] = true
		}
	}
	for _, want := range []float64{2, 3, 4} {
		if !seen[want] {
			t.Errorf("变化值 %v 在抽稀后丢失（返回 %d 行）", want, len(out))
		}
	}
	if len(out) > 100 {
		t.Errorf("返回 %d 行, 超过 limit 100", len(out))
	}
}

// TestDecimateHistoryRows_ShortLivedValue 验证短暂的值不会被吞掉。
//
// 回归测试（2026-10-22）：受控实验里写入 1、2、3、4（相隔 1 秒），
// 桶聚合（哪怕用 last()）只返回 4、3、1——值 2 被吞了。
// 保真抽稀必须把 2 保住。
func TestDecimateHistoryRows_ShortLivedValue(t *testing.T) {
	rows := mkRows(1000, []any{1.0, 2.0, 3.0, 4.0})
	out := decimateHistoryRows(rows, 2)

	// limit=2 装不下 4 个变化点 → 变化点被等间隔抽样。
	// 至少要保证抽出来的点都是真实写入过的值（不能出现 2.5 这种）。
	for _, v := range valuesOf(out) {
		f, ok := toFloat64(v)
		if !ok {
			continue
		}
		if f != 1 && f != 2 && f != 3 && f != 4 {
			t.Errorf("抽稀产生了库里不存在的值: %v", f)
		}
	}
}

// TestDecimateHistoryRows_NoFabricatedValues 验证任何情况下都不会造出库里没有的值。
//
// 这是 mean() 最严重的缺陷：1/2/3/4 平均出 2.5，库里根本没有 2.5。
// 抽稀只做「选点」，不做「算值」，所以必然满足。
func TestDecimateHistoryRows_NoFabricatedValues(t *testing.T) {
	rows := mkRows(1000, []any{1.0, 2.0, 3.0, 4.0})
	for _, limit := range []int{1, 2, 3, 4, 5, 10, 100} {
		out := decimateHistoryRows(rows, limit)
		for _, v := range valuesOf(out) {
			f, ok := toFloat64(v)
			if !ok {
				continue
			}
			if f != 1 && f != 2 && f != 3 && f != 4 {
				t.Errorf("limit=%d 抽稀产生库里不存在的值: %v", limit, f)
			}
		}
	}
}

// TestDecimateHistoryRows_SortedAscending 验证输出按时间升序。
func TestDecimateHistoryRows_SortedAscending(t *testing.T) {
	rows := mkRows(1000, []any{1.0, 2.0, 3.0, 4.0, 5.0})
	// 故意打乱输入顺序。
	shuffled := []map[string]any{rows[3], rows[0], rows[4], rows[1], rows[2]}
	out := decimateHistoryRows(shuffled, 10)
	for i := 1; i < len(out); i++ {
		pt, ok := out[i]["_time"].(time.Time)
		if !ok {
			t.Fatalf("_time 类型 = %T", out[i]["_time"])
		}
		prev, ok := out[i-1]["_time"].(time.Time)
		if !ok {
			t.Fatalf("_time 类型 = %T", out[i-1]["_time"])
		}
		if pt.Before(prev) {
			t.Errorf("输出未按时间升序: %v 在 %v 之前", pt, prev)
		}
	}
}

// TestDecimateHistoryRows_StringValues 验证字符串值也能正确比较与保留。
func TestDecimateHistoryRows_StringValues(t *testing.T) {
	rows := mkRows(1000, []any{"a", "a", "b", "b", "c"})
	out := decimateHistoryRows(rows, 3)
	seen := map[string]bool{}
	for _, v := range valuesOf(out) {
		if s, ok := v.(string); ok {
			seen[s] = true
		}
	}
	for _, want := range []string{"a", "b", "c"} {
		if !seen[want] {
			t.Errorf("字符串值 %q 在抽稀后丢失", want)
		}
	}
}

// TestDecimateHistoryRows_Empty 验证空输入不 panic。
func TestDecimateHistoryRows_Empty(t *testing.T) {
	if out := decimateHistoryRows(nil, 100); len(out) != 0 {
		t.Errorf("空输入应返回 0 行, got %d", len(out))
	}
}

// TestDecimateHistoryRows_ConstantValues 验证全部同值时不崩溃。
func TestDecimateHistoryRows_ConstantValues(t *testing.T) {
	vals := make([]any, 500)
	for i := range vals {
		vals[i] = 42.0
	}
	out := decimateHistoryRows(mkRows(1000, vals), 50)
	if len(out) == 0 {
		t.Fatal("同值数据不应返回空")
	}
	if len(out) > 50 {
		t.Errorf("返回 %d 行, 超过 limit 50", len(out))
	}
}

// TestSameHistoryValue 验证数值跨类型比较。
func TestSameHistoryValue(t *testing.T) {
	cases := []struct {
		a, b any
		want bool
	}{
		{1.0, 1.0, true},
		{float64(1), int64(1), true},
		{int32(2), float64(2), true},
		{1.0, 2.0, false},
		{"a", "a", true},
		{"a", "b", false},
		{nil, nil, true},
		{nil, 1.0, false},
	}
	for _, c := range cases {
		if got := sameHistoryValue(c.a, c.b); got != c.want {
			t.Errorf("sameHistoryValue(%v, %v) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}
