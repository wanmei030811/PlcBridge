package timeconst

import (
	"testing"
	"time"
)

// 验证关键常量值符合预期（防止误改）。
func TestConstants_Positive(t *testing.T) {
	all := map[string]time.Duration{
		"DefaultIOReadTimeout":         DefaultIOReadTimeout,
		"DefaultProbeTimeout":          DefaultProbeTimeout,
		"DefaultReconnectTimeout":      DefaultReconnectTimeout,
		"DefaultChannelConnectTimeout": DefaultChannelConnectTimeout,
		"AcquireRetryInterval":         AcquireRetryInterval,
		"MaxAcquireWaitTime":           MaxAcquireWaitTime,
		"ConfigReloadDebounce":         ConfigReloadDebounce,
		"DefaultShutdownTimeout":       DefaultShutdownTimeout,
		"PoolStopForceTimeout":         PoolStopForceTimeout,
		"DefaultCollectInterval":       DefaultCollectInterval,
		"MinCollectInterval":           MinCollectInterval,
		"ProbeIdleThreshold":           ProbeIdleThreshold,
		"ReconnectThrottle":            ReconnectThrottle,
		"ReconnectThrottleFast":        ReconnectThrottleFast,
		"FailedRetryInterval":          FailedRetryInterval,
		"ChannelDisconnectGrace":       ChannelDisconnectGrace,
		"HealthCheckInterval":          HealthCheckInterval,
	}
	for name, d := range all {
		if d <= 0 {
			t.Errorf("%s must be > 0, got %v", name, d)
		}
	}
}

// 验证关系约束。
func TestConstants_Relationships(t *testing.T) {
	// ReconnectThrottleFast < ReconnectThrottle（fast 用于 handler 死亡，更激进）
	if ReconnectThrottleFast >= ReconnectThrottle {
		t.Errorf("ReconnectThrottleFast (%v) should be < ReconnectThrottle (%v)",
			ReconnectThrottleFast, ReconnectThrottle)
	}
	// MinCollectInterval < DefaultCollectInterval
	if MinCollectInterval >= DefaultCollectInterval {
		t.Errorf("MinCollectInterval (%v) should be < DefaultCollectInterval (%v)",
			MinCollectInterval, DefaultCollectInterval)
	}
	// FailedRetryInterval < ReconnectThrottle
	if FailedRetryInterval >= ReconnectThrottle {
		t.Errorf("FailedRetryInterval (%v) should be < ReconnectThrottle (%v)",
			FailedRetryInterval, ReconnectThrottle)
	}
}
