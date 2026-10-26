// InfluxDB 历史查询测试（Plan3 3.5）——Flux（V2）与 InfluxQL（V1）双路径。
package influxdb

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/plcbridge/plcbridge/internal/infra/target"
	"github.com/plcbridge/plcbridge/internal/platform"
)

// === 单元：sanitizeIdent ===

func TestSanitizeIdent(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"plc/line1/values", "plcline1values"},
		{"a_b-c1", "a_b-c1"},
		{"'; DROP --", "DROP--"},
		{"", ""},
	}
	for _, c := range cases {
		if got := sanitizeIdent(c.in); got != c.want {
			t.Errorf("sanitizeIdent(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// === 单元：sanitizeDuration ===

func TestSanitizeDuration(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"1h", "1h"},
		{"15m", "15m"},
		{"30d", "7d"},
		{"99w", "1w"},
		{"1x", "1h"},
		{"", "1h"},
		{"abc", "1h"},
		{"0h", "1h"},
	}
	for _, c := range cases {
		if got := sanitizeDuration(c.in); got != c.want {
			t.Errorf("sanitizeDuration(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// === 单元：sanitizeLimit ===

func TestSanitizeLimit(t *testing.T) {
	cases := []struct {
		in   int
		want int
	}{
		{0, 1000},
		{-5, 1000},
		{100, 100},
		{999999, 10000},
	}
	for _, c := range cases {
		if got := sanitizeLimit(c.in); got != c.want {
			t.Errorf("sanitizeLimit(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}

// === 单元：buildInfluxFluxQuery（V2 路径） ===

func TestBuildInfluxFluxQuery(t *testing.T) {
	q := buildInfluxFluxQuery("plc/line1", "_value", "1h", 100)
	if !strings.Contains(q, `r._measurement == "plcline1"`) {
		t.Errorf("query missing sanitized measurement: %s", q)
	}
	if !strings.Contains(q, "limit(n: 100)") {
		t.Errorf("query missing limit: %s", q)
	}
	// limit 超过上限时必须被截断
	q2 := buildInfluxFluxQuery("m", "_value", "1h", 999999)
	if !strings.Contains(q2, "limit(n: 10000)") {
		t.Errorf("limit not clamped: %s", q2)
	}
}

// TestBuildInfluxFluxQuery_UsesLastNotMean 验证 Flux 路径也用 last()。
//
// 回归测试（2026-10-21）：mean() 会把窗口内所有采样取平均，
// 落库的真实值 1/2/3/4 被渲染成 1/2.1/3.67 —— 不是任何一个真实写入过的值。
func TestBuildInfluxFluxQuery_UsesLastNotMean(t *testing.T) {
	q := buildInfluxFluxQuery("sss", "M100", "1h", 2000)
	if strings.Contains(q, "fn: mean") {
		t.Errorf("不应使用 fn: mean（会算出库里不存在的值）: %s", q)
	}
	if !strings.Contains(q, "fn: last") {
		t.Errorf("应使用 fn: last: %s", q)
	}
	// 桶宽应随时间窗口自适应（1h → 2s）
	if !strings.Contains(q, "every: 2s") {
		t.Errorf("1h 窗口的桶宽应为 2s: %s", q)
	}
}

// === 单元：buildInfluxQLQuery（V1 路径） ===

func TestBuildInfluxQLQuery(t *testing.T) {
	q := buildInfluxQLQuery("plc/line1", "M100", "1h", 100)
	if !strings.Contains(q, `FROM "plcline1"`) {
		t.Errorf("query missing sanitized measurement: %s", q)
	}
	if !strings.Contains(q, `last("M100")`) {
		t.Errorf("query missing field: %s", q)
	}
	if !strings.Contains(q, "LIMIT 100") {
		t.Errorf("query missing limit: %s", q)
	}
	// 注入尝试必须被过滤：单引号 / 分号 / 空格全部剥离，
	// 只剩安全字符集，且整串被当作单个带引号的标识符（不会成为第二条语句）。
	q2 := buildInfluxQLQuery("a'; DROP --", "x", "1h", 10)
	if strings.Contains(q2, "';") || strings.Contains(q2, "'; ") {
		t.Errorf("注入内容未被过滤: %s", q2)
	}
	if !strings.Contains(q2, `FROM "aDROP--"`) {
		t.Errorf("注入串应被夹在单个标识符内: %s", q2)
	}
}

// TestBuildInfluxQLQuery_UsesLastNotMean 验证查询用 last() 而不是 mean()。
//
// 回归测试（2026-10-21）：mean() 会把窗口内所有采样取平均，
// 落库的真实值 1/2/3/4 被渲染成 1/2.1/3.67 —— 不是任何一个真实写入过的值。
// last() 取窗口内最后一个值，语义上等于「此刻设备的状态」，与实时值一致。
func TestBuildInfluxQLQuery_UsesLastNotMean(t *testing.T) {
	q := buildInfluxQLQuery("sss", "M100", "1h", 2000)
	if strings.Contains(q, "mean(") {
		t.Errorf("不应使用 mean()（会算出库里不存在的值）: %s", q)
	}
	if !strings.Contains(q, `last("M100")`) {
		t.Errorf("应使用 last(): %s", q)
	}
}

// TestBuildInfluxQLQuery_OrderByDesc 验证查询按时间倒序。
//
// 回归测试（2026-10-21）：InfluxDB 的 LIMIT 默认取「最早」的 N 行，
// 不加 ORDER BY time DESC 的话，窗口越新数据越多，被截断掉的恰恰是
// 用户最想看的最新一段。
func TestBuildInfluxQLQuery_OrderByDesc(t *testing.T) {
	q := buildInfluxQLQuery("sss", "M100", "1h", 2000)
	if !strings.Contains(q, "ORDER BY time DESC") {
		t.Errorf("应包含 ORDER BY time DESC: %s", q)
	}
}

// TestBuildInfluxQLQuery_BucketWidth 验证桶宽随时间窗口自适应。
//
// 回归测试（2026-10-21）：桶宽太大时，相邻的真实采样点会落进同一个桶，
// 1→2→3→4 只剩一个点，趋势图完全看不出变化过程。
func TestBuildInfluxQLQuery_BucketWidth(t *testing.T) {
	cases := []struct {
		duration string
		want     string
	}{
		{"15m", "1s"},
		{"1h", "2s"},
		{"6h", "15s"},
		{"24h", "1m"},
		{"3d", "5m"},
		{"7d", "15m"},
	}
	for _, c := range cases {
		q := buildInfluxQLQuery("sss", "M100", c.duration, 2000)
		if !strings.Contains(q, "GROUP BY time("+c.want+")") {
			t.Errorf("duration=%s 桶宽应为 %s, got: %s", c.duration, c.want, q)
		}
	}
}

// TestHistoryBucketFor 验证桶宽选择逻辑。
func TestHistoryBucketFor(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"15m", "1s"},
		{"30m", "2s"},
		{"1h", "2s"},
		{"6h", "15s"},
		{"12h", "1m"},
		{"24h", "1m"},
		{"3d", "5m"},
		{"7d", "15m"},
		{"1w", "15m"},
		{"", "2s"}, // sanitizeDuration 回退到 1h → 桶宽 2s
		{"bad", "2s"},
	}
	for _, c := range cases {
		if got := historyBucketFor(c.in); got != c.want {
			t.Errorf("historyBucketFor(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// === 单元：parseInfluxQLRows ===

func TestParseInfluxQLRows(t *testing.T) {
	body := []byte(`{"results":[{"series":[{"name":"plc","columns":["time","value"],
		"values":[["2026-10-21T10:00:00Z",1.5],["2026-10-21T10:01:00Z",2.5]]}]}]}`)
	rows, err := parseInfluxQLRows(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("解析出 %d 行, want 2", len(rows))
	}
	ts, ok := rows[0]["_time"].(time.Time)
	if !ok {
		t.Fatalf("_time 应为 time.Time, got %T", rows[0]["_time"])
	}
	if ts.UnixMilli() != 1792576800000 {
		t.Errorf("时间戳 = %d", ts.UnixMilli())
	}
	if rows[0]["_value"] != 1.5 {
		t.Errorf("_value = %v, want 1.5", rows[0]["_value"])
	}
}

func TestParseInfluxQLRows_Error(t *testing.T) {
	body := []byte(`{"results":[{"error":"database not found"}]}`)
	if _, err := parseInfluxQLRows(body); err == nil {
		t.Error("InfluxQL error 应被透传为 error")
	}
}

// TestParseInfluxQLRows_AliasedColumn 验证带别名的列名能被识别。
//
// 回归测试（2026-10-21）：实际查询是
//
//	SELECT mean("M100") AS "M100" FROM "sss" ...
//
// 返回的列名是 ["time","M100"]，不是字面量 "value"。
// 之前只认 "value" 列 → count=0（历史图永远空）。
func TestParseInfluxQLRows_AliasedColumn(t *testing.T) {
	body := []byte(`{"results":[{"series":[{"name":"sss","columns":["time","M100"],
		"values":[["2026-10-21T15:06:45Z",1],["2026-10-21T15:06:46Z",2]]}]}]}`)
	rows, err := parseInfluxQLRows(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("解析出 %d 行, want 2（别名列未被识别）", len(rows))
	}
	if rows[0]["_value"] != float64(1) {
		t.Errorf("_value = %v, want 1", rows[0]["_value"])
	}
	if rows[1]["_value"] != float64(2) {
		t.Errorf("_value = %v, want 2", rows[1]["_value"])
	}
}

// TestParseInfluxQLRows_EpochMs 验证 epoch=ms 时数字时间戳能被解析。
//
// 回归测试（2026-10-21）：请求带 epoch=ms 时，time 列是 Unix 毫秒数字，
// 之前只解析 RFC3339 字符串 → 解析出 0 行。
func TestParseInfluxQLRows_EpochMs(t *testing.T) {
	body := []byte(`{"results":[{"series":[{"name":"sss","columns":["time","M100"],
		"values":[[1792595160000,1],[1792595220000,2]]}]}]}`)
	rows, err := parseInfluxQLRows(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("解析出 %d 行, want 2（epoch 数字时间戳未被识别）", len(rows))
	}
	ts, ok := rows[0]["_time"].(time.Time)
	if !ok {
		t.Fatalf("_time 应为 time.Time, got %T", rows[0]["_time"])
	}
	if ts.UnixMilli() != 1792595160000 {
		t.Errorf("时间戳 = %d, want 1792595160000", ts.UnixMilli())
	}
	if rows[0]["_value"] != float64(1) {
		t.Errorf("_value = %v, want 1", rows[0]["_value"])
	}
}

// TestParseInfluxQLRows_MultiColumn 验证多 field 列时只取第一个非 time 列。
func TestParseInfluxQLRows_MultiColumn(t *testing.T) {
	body := []byte(`{"results":[{"series":[{"name":"sss","columns":["time","M100","No"],
		"values":[["2026-10-21T15:06:45Z",1,"x"]]}]}]}`)
	rows, err := parseInfluxQLRows(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("解析出 %d 行, want 1", len(rows))
	}
	if rows[0]["_value"] != float64(1) {
		t.Errorf("_value = %v, want 1（应取第一个非 time 列）", rows[0]["_value"])
	}
}

func TestParseInfluxQLRows_Empty(t *testing.T) {
	body := []byte(`{"results":[{"statement_id":0}]}`)
	rows, err := parseInfluxQLRows(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("空结果应返回 0 行, got %d", len(rows))
	}
}

// === 集成：V1 客户端走 InfluxQL 查询 ===

func TestInfluxDBV1_QueryHistory(t *testing.T) {
	var gotQuery, gotDB string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/query" {
			http.NotFound(w, r)
			return
		}
		gotQuery = r.URL.Query().Get("q")
		gotDB = r.URL.Query().Get("db")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []any{map[string]any{
				"series": []any{map[string]any{
					"name":    "plc",
					"columns": []string{"time", "value"},
					"values":  [][]any{{"2026-10-21T10:00:00Z", 42}},
				}},
			}},
		})
	}))
	defer ts.Close()

	cfgJSON, _ := json.Marshal(InfluxDBConfig{Version: InfluxV1, DB: "plcbridge", Precision: "ns"})
	channel := &platform.Channel{ChannelBizID: "c", Type: "influxdb", Address: ts.URL, Config: cfgJSON}
	tgt := &platform.Target{TargetBizID: "t", ForwardChannelBizID: "c", Topic: "sss"}
	cli, err := newPlugin().CreateTarget(tgt, channel)
	if err != nil {
		t.Fatal(err)
	}

	out, err := cli.(target.HistoryQueryer).QueryHistory(context.Background(), target.HistoryQuery{
		Measurement: "sss",
		Field:       "M100",
		Duration:    "1h",
		Limit:       2000,
	})
	if err != nil {
		t.Fatal(err)
	}
	rows, ok := out.([]map[string]any)
	if !ok {
		t.Fatalf("返回类型 = %T, want []map[string]any", out)
	}
	if len(rows) != 1 {
		t.Fatalf("返回 %d 行, want 1", len(rows))
	}
	if rows[0]["_value"] != float64(42) {
		t.Errorf("_value = %v, want 42", rows[0]["_value"])
	}
	if gotDB != "plcbridge" {
		t.Errorf("db = %q, want plcbridge", gotDB)
	}
	if !strings.Contains(gotQuery, "sss") {
		t.Errorf("查询应含 measurement sss, got: %s", gotQuery)
	}
	if !strings.Contains(gotQuery, "M100") {
		t.Errorf("查询应含 field M100, got: %s", gotQuery)
	}
}

// TestBuildInfluxQLRawQuery 验证原始点兜底查询的形态。
//
// 兜底查询不做聚合，因此：不能带 GROUP BY（InfluxDB 会报
// "GROUP BY requires at least one aggregate function"），
// 也不能带 fill(none)（"fill(none) must be used with a function"）。
func TestBuildInfluxQLRawQuery(t *testing.T) {
	q := buildInfluxQLRawQuery("plc/line1", "M100", "1h", 100)
	if !strings.Contains(q, `SELECT "M100" AS "M100"`) {
		t.Errorf("query 应直接选 field: %s", q)
	}
	if !strings.Contains(q, `FROM "plcline1"`) {
		t.Errorf("query missing sanitized measurement: %s", q)
	}
	if !strings.Contains(q, "LIMIT 100") {
		t.Errorf("query missing limit: %s", q)
	}
	if !strings.Contains(q, "ORDER BY time DESC") {
		t.Errorf("query 应按时间倒序: %s", q)
	}
	if strings.Contains(q, "GROUP BY") {
		t.Errorf("原始点查询不应带 GROUP BY: %s", q)
	}
	if strings.Contains(q, "fill(") {
		t.Errorf("原始点查询不应带 fill(): %s", q)
	}
}

// TestIsUnsupportedAggregateErr 验证聚合函数不支持的错误能被识别。
func TestIsUnsupportedAggregateErr(t *testing.T) {
	yes := []string{
		"InfluxQL 查询错误: unsupported mean iterator type: *query.stringInterruptIterator",
		"unsupported last iterator type: *query.stringInterruptIterator",
		"unsupported mean iterator type: *query.booleanInterruptIterator",
	}
	for _, s := range yes {
		if !isUnsupportedAggregateErr(errors.New(s)) {
			t.Errorf("应识别为聚合不支持: %s", s)
		}
	}
	no := []string{
		"database not found",
		"connection refused",
		"",
	}
	for _, s := range no {
		if isUnsupportedAggregateErr(errors.New(s)) {
			t.Errorf("不应识别为聚合不支持: %s", s)
		}
	}
	if isUnsupportedAggregateErr(nil) {
		t.Error("nil 不应被识别为聚合不支持")
	}
}

// TestInfluxDBV1_QueryHistory_StringField 验证 string field 直接用原始点查询成功。
//
// 回归测试（2026-10-22）：取数策略改为「原始点 + Go 侧保真抽稀」，
// 主查询不再带 GROUP BY / 聚合函数，
// 所以 string field 一次就成功，不会再触发「mean() 报错 → 重试 last()」。
func TestInfluxDBV1_QueryHistory_StringField(t *testing.T) {
	var queries []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		queries = append(queries, q)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"series":[{"name":"sss","columns":["time","No"],
			"values":[[1792595160000,"abc"]]}]}]}`))
	}))
	defer ts.Close()

	cfgJSON, _ := json.Marshal(InfluxDBConfig{Version: InfluxV1, DB: "plcbridge", Precision: "ns"})
	channel := &platform.Channel{ChannelBizID: "c", Type: "influxdb", Address: ts.URL, Config: cfgJSON}
	tgt := &platform.Target{TargetBizID: "t", ForwardChannelBizID: "c", Topic: "sss"}
	cli, err := newPlugin().CreateTarget(tgt, channel)
	if err != nil {
		t.Fatal(err)
	}

	out, err := cli.(target.HistoryQueryer).QueryHistory(context.Background(), target.HistoryQuery{
		Measurement: "sss",
		Field:       "No",
		Duration:    "1h",
		Limit:       100,
	})
	if err != nil {
		t.Fatal(err)
	}
	rows, ok := out.([]map[string]any)
	if !ok {
		t.Fatalf("返回类型 = %T", out)
	}
	if len(rows) != 1 {
		t.Fatalf("返回 %d 行, want 1", len(rows))
	}
	if rows[0]["_value"] != "abc" {
		t.Errorf("_value = %v, want abc", rows[0]["_value"])
	}
	// string field 应该一次查询就成功（不再需要 mean() → last() 的重试）。
	if len(queries) != 1 {
		t.Fatalf("string field 应只发起 1 次查询, got %d: %v", len(queries), queries)
	}
	if strings.Contains(queries[0], "GROUP BY") {
		t.Errorf("主查询不应带 GROUP BY（要取原始点）: %s", queries[0])
	}
}

// TestInfluxDBV1_QueryHistory_AggregateFallback 验证原始点查询失败时回退到桶聚合查询。
//
// 回归测试（2026-10-22）：主查询取原始点。若某个 InfluxDB 版本/配置下
// 原始点查询不可用，应回退到桶聚合查询（数据会少一些，但至少能出图），
// 而不是整个查询失败。
func TestInfluxDBV1_QueryHistory_AggregateFallback(t *testing.T) {
	var queries []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		queries = append(queries, q)
		w.Header().Set("Content-Type", "application/json")
		if !strings.Contains(q, "GROUP BY") {
			// 第一次：原始点查询不可用
			_, _ = w.Write([]byte(`{"results":[{"error":"unsupported last iterator type: *query.stringInterruptIterator"}]}`))
			return
		}
		// 第二次：桶聚合查询成功
		_, _ = w.Write([]byte(`{"results":[{"series":[{"name":"sss","columns":["time","No"],
			"values":[[1792595160000,"abc"]]}]}]}`))
	}))
	defer ts.Close()

	cfgJSON, _ := json.Marshal(InfluxDBConfig{Version: InfluxV1, DB: "plcbridge", Precision: "ns"})
	channel := &platform.Channel{ChannelBizID: "c", Type: "influxdb", Address: ts.URL, Config: cfgJSON}
	tgt := &platform.Target{TargetBizID: "t", ForwardChannelBizID: "c", Topic: "sss"}
	cli, err := newPlugin().CreateTarget(tgt, channel)
	if err != nil {
		t.Fatal(err)
	}

	out, err := cli.(target.HistoryQueryer).QueryHistory(context.Background(), target.HistoryQuery{
		Measurement: "sss",
		Field:       "No",
		Duration:    "1h",
		Limit:       100,
	})
	if err != nil {
		t.Fatal(err)
	}
	rows, ok := out.([]map[string]any)
	if !ok {
		t.Fatalf("返回类型 = %T", out)
	}
	if len(rows) != 1 {
		t.Fatalf("返回 %d 行, want 1", len(rows))
	}
	if rows[0]["_value"] != "abc" {
		t.Errorf("_value = %v, want abc", rows[0]["_value"])
	}
	if len(queries) != 2 {
		t.Fatalf("应发起 2 次查询(原始点失败 → 桶聚合兜底), got %d: %v", len(queries), queries)
	}
	if !strings.Contains(queries[1], "GROUP BY") {
		t.Errorf("兜底查询应带 GROUP BY: %s", queries[1])
	}
}

// TestInfluxDBV1_QueryHistory_NoDB 验证未配置 db 时报错而非 panic。
func TestInfluxDBV1_QueryHistory_NoDB(t *testing.T) {
	c := &influxDBV1Client{addr: "http://127.0.0.1:1", cfg: InfluxDBConfig{Version: InfluxV1}}
	_, err := c.QueryHistory(context.Background(), target.HistoryQuery{Measurement: "x"})
	if err == nil {
		t.Error("未配置 db 应报错")
	}
}
