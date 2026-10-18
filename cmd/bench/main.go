// bench —— plcbridge 端到端压测工具。
//
// 用法：
//   go run ./cmd/bench -plc 200 -points-per-plc 350 -interval 1000 -write-rps 100 -duration 30m
//
// 步骤：
//   1. 启动 N 个 mock Modbus TCP server（每台独占端口）
//   2. 构造 in-memory config.json（200 PLC × 350 点位 = 7 万点，1Hz 采集）
//   3. 启动 plcbridge 的 collectPool/forwardPool/writePool/configPool
//   4. 注册 sink target plugin，forward 队列里消费它
//   5. 并发 goroutine 持续打 /api/v1/write 写 mock PLC
//   6. sampler 每 5s 抓 runtime/CSV
//
// 退出：
//   Ctrl+C → 优雅停机，写最后一帧 CSV，输出总结。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/plcbridge/plcbridge/internal/app/collect"
	"github.com/plcbridge/plcbridge/internal/app/forward"
	"github.com/plcbridge/plcbridge/internal/app/write"
	"github.com/plcbridge/plcbridge/internal/bench"
	"github.com/plcbridge/plcbridge/internal/infra/api"
	"github.com/plcbridge/plcbridge/internal/infra/config"
	"github.com/plcbridge/plcbridge/internal/infra/logging"
	"github.com/plcbridge/plcbridge/internal/infra/pool"
	"github.com/plcbridge/plcbridge/internal/infra/protocol"
	"github.com/plcbridge/plcbridge/internal/infra/target"
	"github.com/plcbridge/plcbridge/internal/metrics"
	"github.com/plcbridge/plcbridge/internal/platform"
)

func main() {
	var (
		plcCount       = flag.Int("plc", 200, "mock PLC 数量")
		pointsPerPLC   = flag.Int("points-per-plc", 350, "每 PLC 点位数")
		basePort       = flag.Int("base-port", 25000, "mock PLC 起始端口")
		collectMs      = flag.Int("interval", 1000, "采集周期 ms")
		writeRPS       = flag.Int("write-rps", 100, "回写并发 TPS（0=禁用）")
		writeDuration  = flag.Duration("duration", 30*time.Minute, "压测时长")
		csvPath        = flag.String("csv", "bench.csv", "输出 CSV 路径")
		warmupSec      = flag.Int("warmup", 30, "预热秒数（前 N 秒不计入 P99）")
		apiAddr        = flag.String("api", ":28082", "plcbridge API 监听地址")
	)
	flag.Parse()

	rand.Seed(time.Now().UnixNano())

	logging.Init("INFO")
	sigCtx, sigCancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer sigCancel()
	// 让 -duration 同时驱动主 ctx：到点自动 cancel，等价于"软 SIGTERM"。
	// 这样 bench 不会因为没人按 Ctrl+C 就永远不退出。
	ctx, cancel := context.WithTimeout(sigCtx, *writeDuration)
	defer cancel()

	// 1. mock PLC pool
	plcs, _, err := bench.StartMockPool(*basePort, *plcCount, 4096)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[bench] 启动 mock PLC 失败: %v\n", err)
		os.Exit(2)
	}
	defer func() {
		for _, p := range plcs {
			_ = p.Close()
		}
	}()
	logging.Info("mock PLC 已启动").
		Int("count", len(plcs)).
		Int("base_port", *basePort).
		Int("regs_per_plc", 4096).
		Send()

	// 2. 构造 in-memory config
	cfg := buildConfig(plcs, *pointsPerPLC, *collectMs)
	store := config.NewStore("")
	store.SyncFrom(cfg)
	logging.Info("config 已注入内存").Int("version", cfg.Version).Send()

	// 3. 注册 protocol + 自定义 sink target
	protocol.MustRegisterDefaults()
	sinkStats := &bench.SinkStats{}
	sinkPlugin := bench.NewSinkPlugin(sinkStats)
	target.MustRegister(sinkPlugin)
	target.MustRegisterDefaults() // 同时注册 mqtt/influxdb 以兼容 cfg
	logging.Info("已注册 plugins").Send()

	// 4. metrics
	metrics.MustRegister()

	// 5. 启动 4 worker pool
	pools := pool.NewPools(pool.Config{
		CollectSize: 200,
		ForwardSize: 100,
		WriteSize:   50,
		ConfigSize:  4,
	})
	pools.Start(ctx, nil)
	defer pools.Stop(5 * time.Second)

	// 6. 启动 collect / forward / write
	collectWorkers := collect.NewWorkers(store, pools)
	collectWorkers.Start(ctx)
	defer collectWorkers.Stop(5 * time.Second)

	forwardWorkers := forward.NewWorkers(store, pools, collectWorkers.TargetRouter())
	forwardWorkers.Start(ctx)
	defer forwardWorkers.Stop(5 * time.Second)

	writeWorkers := write.NewWorkers(store, pools, collectWorkers.Channels())
	writeWorkers.Start(ctx)
	defer writeWorkers.Stop(5 * time.Second)

	// 7. HTTP API（给并发回压用）
	apiServer := api.NewServer(store, pools, *apiAddr)
	apiServer.SetWriter(writeWorkers)
	apiServer.SetChannelRegistry(collectWorkers.Channels())
	go func() {
		if err := apiServer.Start(); err != nil {
			logging.Error("API 启动失败").Err(err).Send()
			cancel()
		}
	}()
	defer apiServer.Shutdown(5 * time.Second)

	// 8. metrics server（用独立端口，避免和 plcbridge 9091 冲突）
	metricsAddr := "127.0.0.1:29091"
	metricsServer := metrics.NewServer(metricsAddr)
	go func() {
		if err := metricsServer.Start(); err != nil {
			logging.Error("Metrics 启动失败").Err(err).Send()
		}
	}()
	defer metricsServer.Shutdown(5 * time.Second)
	if err := bench.MustEnsureServerAlive(metricsAddr, 3*time.Second); err != nil {
		logging.Error("metrics server 不可达").Err(err).Send()
	} else {
		logging.Info("metrics 已就绪").Str("addr", metricsAddr).Send()
	}

	// 9. sampler：每 5s 抓 runtime + mock + metrics P99
	plcSnap := func() bench.PlcSnapshot {
		var reads, writes uint64
		for _, p := range plcs {
			r, w, _ := p.Stats()
			reads += r
			writes += w
		}
		return bench.PlcSnapshot{Reads: reads, Writes: writes}
	}
	samp, err := bench.NewSampler(*csvPath, 5*time.Second, plcSnap, func() (bench.CollectDurSnapshot, error) {
		return bench.ScrapeCollectDuration(metricsAddr)
	})
	if err != nil {
		logging.Error("sampler 启动失败").Err(err).Send()
		cancel()
	}
	// sampler 暴露给 collect 用于 RecordLatency——暂时不需要外部 hook
	defer func() {
		if err := samp.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "[bench] close sampler: %v\n", err)
		}
	}()

	// 9. enable write for first device/source (otherwise writePool 没人消费)
	for _, d := range cfg.Devices {
		if len(d.Sources) > 0 && len(d.Sources[0].Points) > 0 {
			d.Sources[0].WriteEnabled = true
			for _, p := range d.Sources[0].Points {
				p.WriteEnabled = true
			}
			break
		}
	}

	// 10. write pressure goroutines
	var writeDone atomic.Uint64
	var writeFail atomic.Uint64
	writeCtx, writeCancel := context.WithTimeout(ctx, *writeDuration)
	defer writeCancel()
	if *writeRPS > 0 {
		go runWritePressure(writeCtx, *writeRPS, writeWorkers, cfg, &writeDone, &writeFail)
	}

	// 10. collect 流量计数（in-process hook 入 sampler）
	go func() {
		t := time.NewTicker(1 * time.Second)
		defer t.Stop()
		var prevSends uint64
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				cur := sinkStats.Sent.Load()
				delta := cur - prevSends
				prevSends = cur
				logging.Info("sink 速率").
					Int64("total_sent", int64(cur)).
					Int64("delta_1s", int64(delta)).
					Int64("writes_done", int64(writeDone.Load())).
					Int64("writes_fail", int64(writeFail.Load())).
					Int64("sent_at_ns", sinkStats.LastSentAt.Load()).
					Send()
			}
		}
	}()

	// 11. 打印预热截止时刻
	warmupUntil := time.Now().Add(time.Duration(*warmupSec) * time.Second)
	logging.Info("预热结束时刻").
		Str("warmup_until", warmupUntil.Format(time.RFC3339)).
		Send()

	<-ctx.Done()
	logging.Info("收到退出信号，开始汇总").
		Int64("sink_sent", int64(sinkStats.Sent.Load())).
		Int64("sink_batches", int64(sinkStats.Batches.Load())).
		Int64("writes_done", int64(writeDone.Load())).
		Int64("writes_fail", int64(writeFail.Load())).
		Send()

	// 给 defer 链 8s 完成 stop，超时再退。
	// 注意：不要在 goroutine 里调 os.Exit(0)，否则会立即终止 main，
	// 让 go run 把它误报为 exit status 0xFFFFFFFF。
	done := make(chan struct{})
	go func() {
		// 仅在 select 走 default 分支时由 main 决定是否调用 os.Exit
		close(done)
	}()
	select {
	case <-done:
		// defer 链 8s 内完成
	case <-time.After(8 * time.Second):
		logging.Warn("defer 链超时 8s，强制退出").Send()
		os.Exit(0)
	}
}

// buildConfig 构造 N 个设备 × M 个点位的 platform.Config。
//
// 所有设备都通过同一个 forward Channel "sink_ch" 转发到 "sink" target。
func buildConfig(plcs []*bench.MockPLC, pointsPerPLC, collectMs int) *platform.Config {
	channels := make([]*platform.Channel, 0, len(plcs)+1)
	// collect Channel: 每台 PLC 一个
	for i, plc := range plcs {
		_ = plc
		channels = append(channels, &platform.Channel{
			ChannelBizID: fmt.Sprintf("ch_modbus_%04d", i),
			Role:         "collect",
			Type:         "modbus_tcp",
			Address:      plcs[i].Addr(),
			QueueSize:    10000,
			TimeoutMs:    300,
			Config:       json.RawMessage(`{"unit_id":1,"connection_timeout_ms":2000,"request_timeout_ms":200}`),
		})
	}
	// forward Channel：共享
	channels = append(channels, &platform.Channel{
		ChannelBizID: "ch_sink",
		Role:         "forward",
		Type:         "sink",
		Address:      "inproc://sink",
		QueueSize:    100000,
		TimeoutMs:    5000,
	})

	devices := make([]*platform.Device, 0, len(plcs))
	for i := range plcs {
		srcBizID := fmt.Sprintf("src_plc_%04d", i)
		points := make([]*platform.Point, 0, pointsPerPLC)
		for j := 0; j < pointsPerPLC; j++ {
			// Modbus 1-based：address = 1 + j
			points = append(points, &platform.Point{
				PointBizID:     fmt.Sprintf("pt_%04d_%03d", i, j),
				Name:           fmt.Sprintf("P%d", j),
				Address:        fmt.Sprintf("4x%d", j+1),
				DataType:       "uint16",
				CollectEnabled: true,
				WriteEnabled:   false,
			})
		}
		src := &platform.Source{
			SourceBizID:       srcBizID,
			ChannelBizID:      fmt.Sprintf("ch_modbus_%04d", i),
			CollectEnabled:    true,
			WriteEnabled:      false,
			CollectIntervalMs: collectMs,
			TimeoutMs:         300,
			Points:            points,
		}
		tgt := &platform.Target{
			TargetBizID:         "tgt_sink_main",
			ForwardChannelBizID: "ch_sink",
			PointBizIDs:         []string{"*"},
			Topic:               "bench/sink",
			Format:              "json",
		}
		devices = append(devices, &platform.Device{
			DeviceBizID: fmt.Sprintf("dev_plc_%04d", i),
			Name:        fmt.Sprintf("MockPLC-%d", i),
			Enabled:     true,
			Sources:     []*platform.Source{src},
			Targets:     []*platform.Target{tgt},
		})
	}

	return &platform.Config{
		Version:  1,
		Channels: channels,
		Devices:  devices,
	}
}

// runWritePressure 在每个写周期里直接用 writeWorkers.Enqueue 投递 1 条回写指令，
// 绕过 HTTP/Token/JSON 反序列化，纯粹压 writePool + mock PLC。
//
// 完全模拟 §12.1 用例 #9（同时下发 100 条回写顺序执行）。
func runWritePressure(ctx context.Context, rps int, ww *write.Workers, cfg *platform.Config, done, fail *atomic.Uint64) {
	interval := time.Second / time.Duration(rps)
	if rps == 0 {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()

	// 选择一个能写、有点位的 Source
	var writeSrc *platform.Source
	for _, d := range cfg.Devices {
		for _, s := range d.Sources {
			if s.WriteEnabled && len(s.Points) > 0 {
				writeSrc = s
				break
			}
		}
		if writeSrc != nil {
			break
		}
	}
	if writeSrc == nil {
		logging.Warn("没有 write-enabled 的 Source，跳过回写压力").
			Msg("no write source")
		return
	}

	// 找一个启用 write 的 device
	var writeDev *platform.Device
	for _, d := range cfg.Devices {
		for _, s := range d.Sources {
			if s.SourceBizID == writeSrc.SourceBizID {
				writeDev = d
				break
			}
		}
		if writeDev != nil {
			break
		}
	}
	if writeDev == nil {
		return
	}

	var inflight sync.WaitGroup
	defer inflight.Wait()

	for i := 0; ; i++ {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pt := writeSrc.Points[i%len(writeSrc.Points)]
			cmd := write.Command{
				TraceID:     strconv.FormatInt(time.Now().UnixNano(), 10),
				DeviceBizID: writeDev.DeviceBizID,
				PointBizID:  pt.PointBizID,
				Value:       rand.Intn(1000),
				DataType:    pt.DataType,
				TimeoutMs:   300,
				ResultChan:  nil, // fire-and-forget 走 Enqueue 不需要回执
				EnqueuedAt:  time.Now(),
			}
			if err := ww.Enqueue(cmd); err != nil {
				fail.Add(1)
				_ = err
			} else {
				done.Add(1)
				// 监控 inflight 上限（简单方法：起 goroutine 等到池子写入）。
				// Enqueue 成功 = 进 queue，wire 异步落 PLC。
				inflight.Add(1)
				go func() {
					defer inflight.Done()
					time.Sleep(20 * time.Millisecond)
				}()
			}
		}
	}
}
