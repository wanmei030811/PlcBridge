// history.go：InfluxDB 历史查询（Plan3 3.5）——V1 与 V2 双路径。
//
// 背景（2026-10-21 修正）：
//
//	历史查询最初只在 V2 客户端实现，且接口签名是 QueryHistory(ctx, query string)，
//	query 由 api 层拼好的 Flux 文本。后果：
//	  1. V1 客户端（version=1.x）无法实现该接口 → 现场配置的 1.x 通道
//	     查历史永远返回 history_not_supported。
//	  2. Flux 方言被硬编码进通用接口，Target 实现无法自由选择查询语法。
//	现在接口传结构化 target.HistoryQuery，由各版本客户端自行翻译：
//	  - V2/V1.8+/V3 → Flux（buildInfluxFluxQuery）
//	  - V1.0–1.7    → InfluxQL（buildInfluxQLQuery）
//
// 写入侧与查询侧的字段对应关系（必须一致，否则查不到数据）：
//
//	measurement := Target.Topic              （见 resolveMeasurement）
//	field      := Point.PointBizID           （见 buildInfluxPoint / buildV1LineProtocol）
package influxdb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/plcbridge/plcbridge/internal/infra/target"
)

// 历史查询的时间范围上限（7 天）与点数上限（10000）。
const (
	historyMaxDuration  = "7d"
	historyMaxLimit     = 10000
	historyDefaultLimit = 1000
)

// durationRe 匹配 "<数字><单位>" 形式的时间窗口。
var durationRe = regexp.MustCompile("^(\\d+)([smhdw])$")

// sanitizeDuration 收敛时间窗口到 1m ~ 7d。
//
// 返回值保证一定能被 InfluxDB 解析；非法输入回退 "1h"。
func sanitizeDuration(s string) string {
	if s == "" {
		return "1h"
	}
	m := durationRe.FindStringSubmatch(s)
	if m == nil {
		return "1h"
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n <= 0 {
		return "1h"
	}
	unit := m[2]
	// 各单位对应的最大条数（换算到分钟）
	var maxN int
	switch unit {
	case "s":
		maxN = 7 * 24 * 60
	case "m":
		maxN = 7 * 24 * 60
	case "h":
		maxN = 7 * 24
	case "d":
		maxN = 7
	case "w":
		maxN = 1
	default:
		return "1h"
	}
	if n > maxN {
		n = maxN
	}
	return strconv.Itoa(n) + unit
}

// sanitizeIdent 只保留 Flux / InfluxQL 标识符安全字符。
//
// measurement / field 来自用户输入（URL query），必须做白名单过滤，
// 否则可注入查询语句。
func sanitizeIdent(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		}
	}
	return b.String()
}

// sanitizeLimit 收敛点数上限到 1 ~ 10000。
func sanitizeLimit(n int) int {
	if n <= 0 {
		return historyDefaultLimit
	}
	if n > historyMaxLimit {
		return historyMaxLimit
	}
	return n
}

// buildInfluxFluxQuery 构造受控的 Flux 查询语句（V2 / V1.8+ / V3）。
//
// 安全约束：
//   - measurement / field 名称做字符白名单过滤（只允许字母数字下划线横线）
//   - duration 限制在 1m ~ 7d
//   - limit 硬上限 10000
//
// 聚合函数用 last() 而不是 mean()（2026-10-21 修正）：
//
//	落库的是离散的真实值（1 / 2 / 3 / 4），mean() 会把一个窗口内的所有
//	采样取平均，产出的根本不是「任何一个真实写入过的值」——
//	用户看到 1、2.1、3.67，而库里只有 1、2、3、4。
//	last() 取窗口内最后一个值，语义上等于「此刻设备的状态」，与实时值一致。
//
// 桶宽（every）由 historyBucketFor 按时间窗口自适应，
// 目的是让相邻的真实采样点落在不同桶里，避免把 1→2→3→4 合并成一个点。
func buildInfluxFluxQuery(measurement, field, duration string, limit int) string {
	m := sanitizeIdent(measurement)
	f := sanitizeIdent(field)
	if m == "" {
		m = "_default"
	}
	if f == "" {
		f = "_value"
	}
	d := sanitizeDuration(duration)
	limit = sanitizeLimit(limit)
	bucket := historyBucketFor(duration)
	return fmt.Sprintf(
		`from(bucket: "%s")`+
			` |> range(start: -%s)`+
			` |> filter(fn: (r) => r._measurement == "%s")`+
			` |> filter(fn: (r) => r._field == "%s")`+
			` |> aggregateWindow(every: %s, fn: last, createEmpty: false)`+
			` |> sort(columns: ["_time"], desc: true)`+
			` |> limit(n: %d)`,
		"plcbridge", d, m, f, bucket, limit,
	)
}

// buildInfluxFluxRawQuery 构造「取原始点」的 Flux 查询语句（不做任何聚合）。
//
// 用途：与 V1 路径保持一致——取原始点后在 Go 侧保真抽稀
// （见 decimate.go），而不是用桶聚合。
//
// 为什么不用 aggregateWindow（受控实验结论）：
//
//	往库里写 1、2、3、4（相隔 1 秒），再查：
//	  mean() + aggregateWindow(1m) → 2.5       ← 库里根本没有这个值
//	  last() + aggregateWindow(2s) → 4, 3, 1  ← 值 2 被桶吞掉了
//
//	桶聚合一个桶只能吐一个值，短命的值必然丢失。
//	要 100% 还原「值序列」，只能取原始点。
func buildInfluxFluxRawQuery(measurement, field, duration string, limit int) string {
	m := sanitizeIdent(measurement)
	f := sanitizeIdent(field)
	if m == "" {
		m = "_default"
	}
	if f == "" {
		f = "_value"
	}
	d := sanitizeDuration(duration)
	limit = sanitizeLimit(limit)
	return fmt.Sprintf(
		`from(bucket: "%s")`+
			` |> range(start: -%s)`+
			` |> filter(fn: (r) => r._measurement == "%s")`+
			` |> filter(fn: (r) => r._field == "%s")`+
			` |> sort(columns: ["_time"], desc: true)`+
			` |> limit(n: %d)`,
		"plcbridge", d, m, f, limit,
	)
}

// historyBucketFor 根据时间窗口选择聚合桶宽度。
//
// 桶宽直接决定图表能不能还原「真实写入值」：
// 如果相邻的采样点落进同一个桶，聚合后只会剩一个点（值被合并丢失）。
// 例如写入值 1→2→3→4 在 8 秒内依次变化，用 1m 桶的话只有 1 个点，
// 用 2s 桶则能完整还原 4 个点。
//
// 同时桶数要控制在 limit 之内（前端 limit=2000），避免被截断丢数据。
// 各档位桶数：15m→900、1h→1800、6h→1440、24h→1440、3d→864、7d→672。
func historyBucketFor(duration string) string {
	d := sanitizeDuration(duration)
	if d == "" {
		return "1m"
	}
	n, err := strconv.Atoi(d[:len(d)-1])
	if err != nil {
		return "1m"
	}
	// 末位是时间单位（s/m/h/d/w）。
	switch d[len(d)-1] {
	case 's':
		return "1s"
	case 'm':
		if n <= 15 {
			return "1s"
		}
		return "2s"
	case 'h':
		switch {
		case n <= 1:
			return "2s"
		case n <= 6:
			return "15s"
		default:
			return "1m"
		}
	case 'd':
		if n <= 3 {
			return "5m"
		}
		return "15m"
	case 'w':
		return "15m"
	}
	return "1m"
}

// buildInfluxQLQuery 构造受控的 InfluxQL 查询语句（V1.0–1.7）。
//
// V1 没有 Flux 引擎，只能用 InfluxQL（POST /query?db=...&q=...）。
// 语义与 Flux 路径保持一致：按 measurement + field 过滤，取时间窗口内数据。
//
// 聚合函数用 last() 而不是 mean()（2026-10-21 修正）：
//
//	落库的是离散的真实值（1 / 2 / 3 / 4），mean() 会把窗口内所有采样取平均，
//	产出的不是任何一个真实写入过的值——用户看到 1、2.1、3.67，
//	而库里只有 1、2、3、4。last() 取窗口内最后一个值，
//	语义上等于「此刻设备的状态」，与实时值一致。
//
// last() 对数值和字符串 field 都通用，因此不再需要 mean() 失败后的兜底重试。
//
// 桶宽（GROUP BY time(...)）由 historyBucketFor 按时间窗口自适应，
// 保证相邻的真实采样点落在不同桶里，不被合并成一个点。
//
// ORDER BY time DESC：InfluxDB 的 LIMIT 默认取「最早」的 N 行，
// 那会把窗口最老的数据留下、最新的丢掉——趋势图最该看的就是最新的一段。
// 加 DESC 后才拿到最近的 N 个桶。
func buildInfluxQLQuery(measurement, field, duration string, limit int) string {
	m := sanitizeIdent(measurement)
	f := sanitizeIdent(field)
	if m == "" {
		m = "_default"
	}
	if f == "" {
		f = "_value"
	}
	d := sanitizeDuration(duration)
	limit = sanitizeLimit(limit)
	bucket := historyBucketFor(duration)
	// measurement / field 已被 sanitizeIdent 过滤成安全字符集，
	// 因此这里用 "%s" 拼接而非 %q（%q 会加引号，InfluxQL 里 measurement 不该带引号）。
	return fmt.Sprintf(
		`SELECT last("%s") AS "%s" FROM "%s" WHERE time > now() - %s GROUP BY time(%s) fill(none) ORDER BY time DESC LIMIT %d`,
		f, f, m, d, bucket, limit,
	)
}

// buildInfluxQLRawQuery 构造「取原始点」的 InfluxQL 查询（不做任何聚合）。
//
// 用途：作为 last() 查询失败时的兜底。
// 原始点逐个返回，能 100% 还原写入的真实值序列，
// 代价是点数多（采集 1s 一次的话，1h 就是 3600 个点）。
// 不带 GROUP BY，因此不能用 fill(none)（InfluxDB 会报
// "fill(none) must be used with a function" / "GROUP BY requires at least one aggregate function"）。
func buildInfluxQLRawQuery(measurement, field, duration string, limit int) string {
	m := sanitizeIdent(measurement)
	f := sanitizeIdent(field)
	if m == "" {
		m = "_default"
	}
	if f == "" {
		f = "_value"
	}
	d := sanitizeDuration(duration)
	limit = sanitizeLimit(limit)
	return fmt.Sprintf(
		`SELECT "%s" AS "%s" FROM "%s" WHERE time > now() - %s ORDER BY time DESC LIMIT %d`,
		f, f, m, d, limit,
	)
}

// parseInfluxQLRows 解析 InfluxQL 查询返回的 JSON（InfluxDB 1.x /query 格式）。
//
// 返回值形状与 Flux 路径一致：[]map[string]any，每行含 "_time" / "_value"。
func parseInfluxQLRows(body []byte) ([]map[string]any, error) {
	var resp struct {
		Results []struct {
			Series []struct {
				Name    string              `json:"name"`
				Columns []string            `json:"columns"`
				Values  [][]json.RawMessage `json:"values"`
			} `json:"series"`
			Error string `json:"error"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("InfluxQL 响应解析失败: %w", err)
	}
	rows := []map[string]any{}
	for _, res := range resp.Results {
		if res.Error != "" {
			return nil, fmt.Errorf("InfluxQL 查询错误: %s", res.Error)
		}
		for _, series := range res.Series {
			// 找 time / value 列下标。
			//
			// 关键（2026-10-21 修复）：value 列的名字取决于查询语句。
			//   - `SELECT mean("M100") AS "M100" ...` → 列名是 "M100"（别名 = field 名）
			//   - `SELECT mean("M100") ...`（无别名）→ 列名也是 "M100"（InfluxDB 用 field 名当列名）
			//   - 只有 `SELECT value FROM ...` 这类写法才会出现字面量 "value" 列
			// 之前只认 "value"，导致 `AS "M100"` 的查询解析出 0 行（count=0）。
			// 现在按优先级找：字面量 "value" → 与请求 field 同名的列 → 第一个非 time 列。
			timeIdx, valIdx := -1, -1
			for i, col := range series.Columns {
				switch {
				case col == "time":
					timeIdx = i
				case col == "value" && valIdx < 0:
					valIdx = i
				}
			}
			// 没有字面量 "value" 列 → 取 time 之外的第一列（即 field 别名列）
			if valIdx < 0 {
				for i := range series.Columns {
					if i != timeIdx {
						valIdx = i
						break
					}
				}
			}
			if timeIdx < 0 || valIdx < 0 {
				continue
			}
			for _, vals := range series.Values {
				if timeIdx >= len(vals) || valIdx >= len(vals) {
					continue
				}
				// 每格是独立 JSON，先解成 any 再按类型处理。
				var timeCell, valCell any
				if err := json.Unmarshal(vals[timeIdx], &timeCell); err != nil {
					continue
				}
				if err := json.Unmarshal(vals[valIdx], &valCell); err != nil {
					continue
				}
				row := map[string]any{}
				// time 列有两种可能形态：
				//   - RFC3339 字符串（默认 epoch 格式）
				//   - 数字（请求带 epoch=ms 时返回 Unix 毫秒）
				// 两种都要支持，否则 epoch=ms 的响应会解析出 0 行。
				switch tv := timeCell.(type) {
				case string:
					if t, err := time.Parse(time.RFC3339Nano, tv); err == nil {
						row["_time"] = t
					}
				case float64:
					row["_time"] = time.UnixMilli(int64(tv))
				case int64:
					row["_time"] = time.UnixMilli(tv)
				case json.Number:
					if ms, err := tv.Int64(); err == nil {
						row["_time"] = time.UnixMilli(ms)
					}
				}
				// value 列可能是数字 / 字符串 / null
				row["_value"] = valCell
				if _, ok := row["_time"]; !ok {
					continue
				}
				if _, ok := row["_value"]; !ok {
					continue
				}
				rows = append(rows, row)
			}
		}
	}
	return rows, nil
}

// QueryHistory 通过 InfluxQL 查询 V1 InfluxDB 历史数据（Plan3 3.5）。
//
// V1.0–1.7 没有 Flux 引擎，走 POST /query?db=<db>&q=<InfluxQL>。
//
// 安全约束：
//   - measurement / field 走白名单过滤（sanitizeIdent）
//   - duration 收敛到 1m ~ 7d
//   - limit 硬上限 10000
//
// 取数策略（2026-10-22 修正）：**取原始点 + Go 侧保真抽稀**，不用桶聚合。
//
// 为什么不用桶聚合（受控实验结论）：
//
//	往库里写 1、2、3、4（相隔 1 秒），再查：
//	  mean() + GROUP BY time(1m) → 2.5       ← 库里根本没有这个值
//	  last() + GROUP BY time(2s) → 4, 3, 1  ← 值 2 被桶吞掉了
//
//	桶聚合一个桶只能吐一个值，短命的值必然丢失。
//	要 100% 还原「值序列」，只能取原始点。
//
// 为什么不直接返回原始点（点数会爆）：
//
//	采集 1s 一次时，1h 窗口 = 3600 个点，7d 窗口 = 60 万个点。
//	所以取原始点后在 Go 侧抽稀（见 decimate.go），
//	抽稀时**保证每一次数值变化都被保留**，其余名额用来还原曲线形状。
func (c *influxDBV1Client) QueryHistory(ctx context.Context, hq target.HistoryQuery) (any, error) {
	c.ensureClient()
	if c.httpCli == nil {
		return nil, errors.New("InfluxDB client not connected")
	}
	if c.cfg.DB == "" {
		return nil, errors.New("InfluxDB V1 未配置 db，无法查询历史数据")
	}

	limit := sanitizeLimit(hq.Limit)
	// 多取一些原始点，给抽稀留出余量：
	// 否则被 LIMIT 砍掉的区间里可能正藏着数值变化。
	rawLimit := sanitizeLimit(limit * 10)

	rows, err := c.queryInfluxQL(ctx, buildInfluxQLRawQuery(hq.Measurement, hq.Field, hq.Duration, rawLimit))
	if err != nil {
		// 原始点查询失败（极老版本 InfluxDB / 特殊配置）时，
		// 退到桶聚合查询：数据会少一些，但至少能出图。
		aggRows, aggErr := c.queryInfluxQL(ctx, buildInfluxQLQuery(hq.Measurement, hq.Field, hq.Duration, limit))
		if aggErr != nil {
			return nil, err
		}
		return aggRows, nil
	}
	return decimateHistoryRows(rows, limit), nil
}

// isUnsupportedAggregateErr 判断错误是否是「聚合函数不支持该 field 类型」。
//
// InfluxDB 1.x 对 string field 用 mean() 会返回
// "unsupported mean iterator type: *query.stringInterruptIterator"。
func isUnsupportedAggregateErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "unsupported mean iterator type") ||
		strings.Contains(msg, "unsupported last iterator type") ||
		strings.Contains(msg, "iterator type")
}

// queryInfluxQL 执行一条 InfluxQL 查询并解析结果。
func (c *influxDBV1Client) queryInfluxQL(ctx context.Context, query string) ([]map[string]any, error) {
	u, err := url.Parse(c.addr)
	if err != nil {
		return nil, fmt.Errorf("InfluxDB V1 address 解析失败: %w", err)
	}
	u.Path = "/query"
	q := u.Query()
	q.Set("db", c.cfg.DB)
	q.Set("q", query)
	q.Set("epoch", "ms")
	u.RawQuery = q.Encode()

	reqCtx, cancel := context.WithTimeout(ctx, time.Duration(c.cfg.ConnectionTimeoutMs)*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, u.String(), nil)
	if err != nil {
		return nil, err
	}
	if c.cfg.Username != "" {
		req.SetBasicAuth(c.cfg.Username, c.cfg.Password)
	}
	resp, err := c.httpCli.Do(req)
	if err != nil {
		c.markUnhealthy()
		c.setLastError(err)
		c.errorCount.Add(1)
		c.lastErrorMs.Store(time.Now().UnixMilli())
		return nil, fmt.Errorf("InfluxDB V1 查询失败: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 8*1024*1024))
	if err != nil {
		return nil, fmt.Errorf("InfluxDB V1 响应读取失败: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		err := fmt.Errorf("InfluxDB V1 /query 返回 %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		c.setLastError(err)
		c.errorCount.Add(1)
		c.lastErrorMs.Store(time.Now().UnixMilli())
		return nil, err
	}
	return parseInfluxQLRows(body)
}
