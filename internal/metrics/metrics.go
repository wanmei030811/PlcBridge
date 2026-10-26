// Package metrics 实现 plcbridge Prometheus 指标（§8.4）。
//
// 关键指标（按 §10.3 日志级别表 + §8.4）：
//   - plcbridge_collect_duration_seconds{device_biz_id, source_biz_id}
//   - plcbridge_collect_total{device_biz_id, result}
//   - plcbridge_target_queue_length{target_biz_id}
//   - plcbridge_target_dropped_total{target_biz_id, reason}
//   - plcbridge_target_sent_total{target_biz_id}
//   - plcbridge_write_total{device_biz_id, result}
//   - plcbridge_config_version
//   - plcbridge_config_reload_total{trigger}
//   - plcbridge_pool_running{pool}
//   - plcbridge_pool_rejected_total{pool}
package metrics

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	registry = prometheus.NewRegistry()

	CollectDurationSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name: "plcbridge_collect_duration_seconds",
			Help: "单次批量读耗时（秒）",
			// 用更细的低端 bucket，便于在 mock/快链路中分辨 1ms vs 30ms。
			Buckets: []float64{0.001, 0.005, 0.01, 0.02, 0.05, 0.1, 0.2, 0.3, 0.5, 1.0, 2.0},
		},
		[]string{"device_biz_id", "source_biz_id"},
	)

	CollectTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "plcbridge_collect_total",
			Help: "采集结果计数",
		},
		[]string{"device_biz_id", "result"},
	)

	TargetQueueLength = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "plcbridge_target_queue_length",
			Help: "Target 队列当前长度",
		},
		[]string{"target_biz_id"},
	)

	TargetDroppedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "plcbridge_target_dropped_total",
			Help: "Target 丢弃消息计数",
		},
		[]string{"target_biz_id", "reason"},
	)

	TargetSentTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "plcbridge_target_sent_total",
			Help: "Target 成功发送消息计数",
		},
		[]string{"target_biz_id"},
	)

	WriteSuccessTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "plcbridge_write_total",
			Help: "回写结果计数（result=success/fail/timeout）",
		},
		[]string{"device_biz_id", "result"},
	)

	WriteFailTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "plcbridge_write_fail_total",
			Help: "回写失败计数（细分）",
		},
		[]string{"device_biz_id"},
	)

	ConfigVersion = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "plcbridge_config_version",
			Help: "当前生效配置版本号",
		},
	)

	ConfigReloadTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "plcbridge_config_reload_total",
			Help: "配置热重载计数（trigger=fsnotify/mtime/initial）",
		},
		[]string{"trigger"},
	)

	PoolRunning = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "plcbridge_pool_running",
			Help: "Worker 池当前运行任务数",
		},
		[]string{"pool"},
	)

	PoolRejectedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "plcbridge_pool_rejected_total",
			Help: "Worker 池拒绝任务计数（池满）",
		},
		[]string{"pool"},
	)

	// §6.3.4 心跳失败计数（按 channel_biz_id 维度），用于 Grafana 告警/仪表盘量化 S7 翻转心跳失败率。
	HeartbeatWriteFailTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "plcbridge_heartbeat_write_fail_total",
			Help: "S7 翻转心跳写入 PLC 失败计数（channel_biz_id 维度）",
		},
		[]string{"channel_biz_id"},
	)

	// === 协议特定指标（Plan2 主题 E 7.1）===
	//
	// 通用 collect_duration 掩盖了不同协议特征（OPC UA 有 Session 续期、ModbusTCP 有 RTT）。
	// 以下指标按协议 driver 内部埋点，用于诊断特定协议的运行状态。
	// 注意：当前实现未强制各 driver 埋点（保持 MVP 兼容），但 metrics 已在 registry 注册，
	// 各 driver 后续 PR 按需调用 RecordS7ConnectDuration / RecordModbusRequestDuration 等。

	// S7ConnectDurationSeconds S7 连接建立耗时（connect / reconnect）。
	S7ConnectDurationSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "plcbridge_s7_connect_duration_seconds",
			Help:    "S7 协议 TCP 连接建立耗时（秒）",
			Buckets: []float64{0.05, 0.1, 0.2, 0.5, 1.0, 2.0, 5.0},
		},
		[]string{"channel_biz_id"},
	)

	// OPCUASessionRenewTotal OPC UA Session 续期计数（成功 / 失败）。
	OPCUASessionRenewTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "plcbridge_opcua_session_renew_total",
			Help: "OPC UA Session 续期计数（result=success/fail）",
		},
		[]string{"channel_biz_id", "result"},
	)

	// ModbusRequestDurationSeconds Modbus 请求 RTT 耗时。
	ModbusRequestDurationSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "plcbridge_modbus_request_duration_seconds",
			Help:    "Modbus TCP 单次请求 RTT（秒）",
			Buckets: []float64{0.005, 0.01, 0.02, 0.05, 0.1, 0.2, 0.5, 1.0},
		},
		[]string{"channel_biz_id", "func_code"},
	)

	// MQTTPublishDurationSeconds MQTT publish 耗时。
	MQTTPublishDurationSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "plcbridge_mqtt_publish_duration_seconds",
			Help:    "MQTT publish 调用耗时（秒）",
			Buckets: []float64{0.005, 0.01, 0.05, 0.1, 0.5, 1.0, 5.0},
		},
		[]string{"target_biz_id", "topic"},
	)

	registerOnce sync.Once
)

// MustRegister 注册全部指标到全局 registry（main 启动时调用一次）。
func MustRegister() {
	registerOnce.Do(func() {
		registry.MustRegister(
			CollectDurationSeconds,
			CollectTotal,
			TargetQueueLength,
			TargetDroppedTotal,
			TargetSentTotal,
			WriteSuccessTotal,
			WriteFailTotal,
			ConfigVersion,
			ConfigReloadTotal,
			PoolRunning,
			PoolRejectedTotal,
			HeartbeatWriteFailTotal,
			S7ConnectDurationSeconds,
			OPCUASessionRenewTotal,
			ModbusRequestDurationSeconds,
			MQTTPublishDurationSeconds,
		)
	})
}

// PromHandler 返回 /metrics 处理器。
func PromHandler() http.Handler {
	return promhttp.HandlerFor(registry, promhttp.HandlerOpts{})
}

// Server 是独立的 Prometheus 监听服务（端口 9091）。
type Server struct {
	srv *http.Server
}

func NewServer(addr string) *Server {
	mux := http.NewServeMux()
	mux.Handle("/metrics", PromHandler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
	return &Server{
		srv: &http.Server{Addr: addr, Handler: mux},
	}
}

func (s *Server) Start() error {
	return s.srv.ListenAndServe()
}

func (s *Server) Shutdown(timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return s.srv.Shutdown(ctx)
}
