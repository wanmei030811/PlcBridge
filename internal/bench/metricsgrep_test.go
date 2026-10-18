// 验证 bench 的 metrics scrape parser 正确还原 P50/P99。
package bench

import (
	"strings"
	"testing"
)

const sampleMetrics = `# HELP plcbridge_collect_duration_seconds foo
# TYPE plcbridge_collect_duration_seconds histogram
plcbridge_collect_duration_seconds_bucket{device_biz_id="dev_a",source_biz_id="src_a",le="0.001"} 5
plcbridge_collect_duration_seconds_bucket{device_biz_id="dev_a",source_biz_id="src_a",le="0.005"} 50
plcbridge_collect_duration_seconds_bucket{device_biz_id="dev_a",source_biz_id="src_a",le="0.01"} 90
plcbridge_collect_duration_seconds_bucket{device_biz_id="dev_a",source_biz_id="src_a",le="0.02"} 99
plcbridge_collect_duration_seconds_bucket{device_biz_id="dev_a",source_biz_id="src_a",le="0.05"} 100
plcbridge_collect_duration_seconds_bucket{device_biz_id="dev_a",source_biz_id="src_a",le="0.1"} 100
plcbridge_collect_duration_seconds_bucket{device_biz_id="dev_a",source_biz_id="src_a",le="0.2"} 100
plcbridge_collect_duration_seconds_bucket{device_biz_id="dev_a",source_biz_id="src_a",le="+Inf"} 100
plcbridge_collect_duration_seconds_sum{device_biz_id="dev_a",source_biz_id="src_a"} 0.5
plcbridge_collect_duration_seconds_count{device_biz_id="dev_a",source_biz_id="src_a"} 100
plcbridge_collect_duration_seconds_bucket{device_biz_id="dev_b",source_biz_id="src_b",le="0.001"} 1
plcbridge_collect_duration_seconds_bucket{device_biz_id="dev_b",source_biz_id="src_b",le="0.005"} 5
plcbridge_collect_duration_seconds_bucket{device_biz_id="dev_b",source_biz_id="src_b",le="0.01"} 5
plcbridge_collect_duration_seconds_bucket{device_biz_id="dev_b",source_biz_id="src_b",le="0.02"} 5
plcbridge_collect_duration_seconds_bucket{device_biz_id="dev_b",source_biz_id="src_b",le="0.05"} 5
plcbridge_collect_duration_seconds_bucket{device_biz_id="dev_b",source_biz_id="src_b",le="0.1"} 5
plcbridge_collect_duration_seconds_bucket{device_biz_id="dev_b",source_biz_id="src_b",le="+Inf"} 5
plcbridge_collect_duration_seconds_count{device_biz_id="dev_b",source_biz_id="src_b"} 5
`

func TestParseCollectDuration(t *testing.T) {
	snap, err := parseCollectDuration(sampleMetrics)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Count != 105 {
		t.Errorf("Count = %d, want 105", snap.Count)
	}
	// dev_a has count 100, so it should be best series
	// P50 of dev_a: target=50, first bucket Count>=50 is le=0.005 → 5ms
	// P99 of dev_a: target=99, first bucket Count>=99 is le=0.02 → 20ms
	if snap.P50MS < 4.99 || snap.P50MS > 5.01 {
		t.Errorf("P50MS = %f, want ~5.0", snap.P50MS)
	}
	if snap.P99MS < 19.99 || snap.P99MS > 20.01 {
		t.Errorf("P99MS = %f, want ~20.0", snap.P99MS)
	}
	// Max bucket (excluding +Inf) is 0.2 → 200ms
	if snap.MaxMS < 199.99 || snap.MaxMS > 200.01 {
		t.Errorf("MaxMS = %f, want ~200.0", snap.MaxMS)
	}
	// Sum should be 0.5
	if snap.SumSec != 0.5 {
		t.Errorf("SumSec = %f, want 0.5", snap.SumSec)
	}
}

func TestExtractSeriesKey(t *testing.T) {
	line := `plcbridge_collect_duration_seconds_count{device_biz_id="dev_a",source_biz_id="src_a"} 100`
	got := extractSeriesKey(line)
	want := `{device_biz_id="dev_a",source_biz_id="src_a"}`
	if !strings.Contains(got, "dev_a") || !strings.Contains(got, "src_a") {
		t.Errorf("extractSeriesKey = %q, want contain dev_a and src_a", got)
	}
	_ = want
}
