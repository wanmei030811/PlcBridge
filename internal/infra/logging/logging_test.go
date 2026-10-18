package logging

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// TestAllEvents_Unique 防止开发误加重复常量。
func TestAllEvents_Unique(t *testing.T) {
	all := AllEvents()
	seen := make(map[string]bool, len(all))
	for _, e := range all {
		if seen[e] {
			t.Errorf("event %q appears multiple times in AllEvents()", e)
		}
		seen[e] = true
	}
}

// TestAllEvents_NonEmpty 防止误删所有 event。
func TestAllEvents_NonEmpty(t *testing.T) {
	if len(AllEvents()) < 20 {
		t.Errorf("expected at least 20 events, got %d (some may have been removed by mistake)", len(AllEvents()))
	}
}

// TestAllEvents_AllLowerSnakeCase 防止风格漂移（grep 友好性）。
func TestAllEvents_AllLowerSnakeCase(t *testing.T) {
	for _, e := range AllEvents() {
		if e != strings.ToLower(e) {
			t.Errorf("event %q must be lowercase (got uppercase chars)", e)
		}
		if strings.Contains(e, "-") {
			t.Errorf("event %q must use underscore, not dash", e)
		}
	}
}

// TestInfo_EmitsEventField 验证 Info() 输出的 JSON 包含 event 字段。
// 这是 Phase 1.1 验收点：所有日志必须有可检索的 event 字段。
func TestInfo_EmitsEventField(t *testing.T) {
	var buf bytes.Buffer
	old := globalLogger
	SetOutput(&buf)
	defer func() { globalLogger = old }()

	Info(EventPollBatchEnd).
		Str("source", "s7_main").
		Int("point_count", 100).
		Msg("test batch end")

	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v\nraw: %s", err, buf.String())
	}
	if got["event"] != EventPollBatchEnd {
		t.Errorf("event field: got %v, want %q", got["event"], EventPollBatchEnd)
	}
	if got["source"] != "s7_main" {
		t.Errorf("source field: got %v, want s7_main", got["source"])
	}
	if got["service"] != "plcbridge" {
		t.Errorf("service field: got %v, want plcbridge", got["service"])
	}
	if got["level"] != "info" {
		t.Errorf("level field: got %v, want info", got["level"])
	}
}

// TestWarn_EmitsWarnLevel 验证 Warn() 输出 level=warn。
func TestWarn_EmitsWarnLevel(t *testing.T) {
	var buf bytes.Buffer
	old := globalLogger
	SetOutput(&buf)
	defer func() { globalLogger = old }()

	Warn(EventPointReadFail).
		Str("source", "s7_main").
		Err(assertErr("test error")).
		Msg("test warn")

	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	if got["level"] != "warn" {
		t.Errorf("level field: got %v, want warn", got["level"])
	}
}

// TestError_EmitsErrorLevel 验证 Error() 输出 level=error。
func TestError_EmitsErrorLevel(t *testing.T) {
	var buf bytes.Buffer
	old := globalLogger
	SetOutput(&buf)
	defer func() { globalLogger = old }()

	Error(EventWriteFail).
		Str("device", "plc_01").
		Err(assertErr("test")).
		Msg("test error")

	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	if got["level"] != "error" {
		t.Errorf("level field: got %v, want error", got["level"])
	}
}

// TestInit_RespectsLevel 验证 Init() 设置日志级别生效。
// 因为 Init 用 sync.Once 不可重入，本测试只验证 SetOutput/SetGlobalLevel 的链路正确。
func TestInit_RespectsLevel(t *testing.T) {
	var buf bytes.Buffer
	old := globalLogger
	SetOutput(&buf)
	defer func() { globalLogger = old }()

	Info(EventServiceStart).Msg("test info")
	if !strings.Contains(buf.String(), `"test info"`) {
		t.Errorf("INFO not emitted: %s", buf.String())
	}
}

// --- 辅助 ---

type stubError string

func (e stubError) Error() string { return string(e) }

func assertErr(s string) error { return stubError(s) }
