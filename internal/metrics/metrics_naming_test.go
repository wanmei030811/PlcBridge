package metrics

import (
	"regexp"
	"strings"
	"testing"
)

// 指标命名规范（Phase 1.10）：
//   - 必须以 "plcbridge_" 前缀
//   - 全小写 + 下划线
//   - 单位后缀：seconds / total / bytes / ratio
var (
	prefixRe = regexp.MustCompile(`^plcbridge_[a-z][a-z0-9_]*$`)
	unitRe   = regexp.MustCompile(`_(seconds|total|bytes|ratio)$`)
)

// TestAllMetrics_HaveValidName 防止新增指标忘加前缀或错命名。
func TestAllMetrics_HaveValidName(t *testing.T) {
	names := []string{
		"plcbridge_collect_duration_seconds",
		"plcbridge_collect_total",
		"plcbridge_target_queue_length",
		"plcbridge_target_dropped_total",
		"plcbridge_target_sent_total",
		"plcbridge_write_total",
		"plcbridge_write_fail_total",
		"plcbridge_config_reload_total",
		"plcbridge_pool_running",
		"plcbridge_pool_rejected_total",
		"plcbridge_heartbeat_write_fail_total",
		// 协议特定指标（Plan2 主题 E 7.1）
		"plcbridge_s7_connect_duration_seconds",
		"plcbridge_opcua_session_renew_total",
		"plcbridge_modbus_request_duration_seconds",
		"plcbridge_mqtt_publish_duration_seconds",
	}

	for _, name := range names {
		if !strings.HasPrefix(name, "plcbridge_") {
			t.Errorf("metric %s: must start with 'plcbridge_'", name)
		}
		if !prefixRe.MatchString(name) {
			t.Errorf("metric %s: invalid format (lowercase + underscore only)", name)
		}
	}
}

// getAllMetricNames 返回所有业务指标名（与 metrics.go 注册顺序保持一致）。
//
// 警告：新增指标时务必同步此列表，否则 TestMetricCount 会失败。
func getAllMetricNames() []string {
	return []string{
		"plcbridge_collect_duration_seconds",
		"plcbridge_collect_total",
		"plcbridge_target_queue_length",
		"plcbridge_target_dropped_total",
		"plcbridge_target_sent_total",
		"plcbridge_write_total",
		"plcbridge_write_fail_total",
		"plcbridge_config_reload_total",
		"plcbridge_pool_running",
		"plcbridge_pool_rejected_total",
		"plcbridge_heartbeat_write_fail_total",
		// 协议特定指标（Plan2 主题 E 7.1）
		"plcbridge_s7_connect_duration_seconds",
		"plcbridge_opcua_session_renew_total",
		"plcbridge_modbus_request_duration_seconds",
		"plcbridge_mqtt_publish_duration_seconds",
	}
}

// TestMetricsNamingConsistency 所有指标都用 _seconds/_total/_bytes/_ratio 后缀。
func TestMetricsNamingConsistency(t *testing.T) {
	allowedSuffixes := []string{"_seconds", "_total", "_bytes", "_ratio"}
	for _, name := range getAllMetricNames() {
		hasUnit := false
		for _, s := range allowedSuffixes {
			if strings.HasSuffix(name, s) {
				hasUnit = true
				break
			}
		}
		// _length / _version / _running 是 gauge，无单位
		gaugeExceptions := map[string]bool{
			"plcbridge_target_queue_length": true,
			"plcbridge_pool_running":        true,
		}
		if !hasUnit && !gaugeExceptions[name] {
			t.Errorf("metric %s: no unit suffix (_seconds/_total/_bytes/_ratio)", name)
		}
	}
}
