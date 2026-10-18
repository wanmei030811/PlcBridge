// Package main 是 plcbridge 主进程入口。
//
// plcbridge 是单进程多 worker 池架构：
//   - collectPool（默认 200 goroutine）: PLC 批量读 + 心跳读 + 健康探测
//   - forwardPool（默认 100 goroutine）: 转发到 MQTT / InfluxDB
//   - writePool  （默认 50 goroutine）: 回写指令消费 + PLC 写入
//   - configPool（默认 4 goroutine）: 配置监听 + 热加载
//
// 4 个 pool 物理隔离（独立 goroutine 池 + 独立 context），通过 channel 通信。
// 详细见 md/Plan.md §6.2.2。
package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/plcbridge/plcbridge/internal/app/collect"
	"github.com/plcbridge/plcbridge/internal/app/forward"
	"github.com/plcbridge/plcbridge/internal/app/write"
	"github.com/plcbridge/plcbridge/internal/infra/api"
	"github.com/plcbridge/plcbridge/internal/infra/config"
	"github.com/plcbridge/plcbridge/internal/infra/logging"
	"github.com/plcbridge/plcbridge/internal/infra/pool"
	"github.com/plcbridge/plcbridge/internal/infra/protocol"
	"github.com/plcbridge/plcbridge/internal/infra/target"
	"github.com/plcbridge/plcbridge/internal/metrics"
)

func main() {
	configDir := flag.String("config", defaultConfigDir(), "配置文件目录（绝对路径）")
	logLevel := flag.String("log-level", "INFO", "日志级别：ERROR/WARN/INFO/DEBUG")
	flag.Parse()

	// 1. 初始化日志
	logging.Init(*logLevel)
	logger := logging.Info("plcbridge starting").Str("service", "plcbridge")
	logger.Send()

	// 2. 解析命令行
	if *configDir == "" {
		logging.Error("--config 参数不能为空").Str("service", "plcbridge").Send()
		os.Exit(1)
	}

	// 2.5 启动时检查：configDir 必须存在（2026-10-12 修复 dual-dir 漂移 bug）
	// 历史背景：曾支持 PLCBRIDGE_CONFIG env + /etc/plcbridge 探测 + PROGRAMDATA 降级，
	// 导致同一台机器 plcbridge / plcbridge-web 跑在不同的 config dir，DB/密码错位。
	// 现在若 env / flag 指定的目录不存在则拒启动（不做兜底，不自动降级 PROGRAMDATA）。
	if info, err := os.Stat(*configDir); err != nil || !info.IsDir() {
		logging.Error("configDir 不存在或不是目录，拒绝启动").
			Str("config_dir", *configDir).
			Bool("exists", err == nil).
			Str("hint", "请用 -config=<绝对路径> 或 PLCBRIDGE_CONFIG 环境变量显式指定存在的配置目录").
			Send()
		os.Exit(1)
	}

	// 3. 创建 config.Store 并加载（带重试，等待 Web 后台先生成 config.json）
	store := config.NewStore(*configDir)
	cfg, err := config.LoadWithRetry(*configDir, 5*time.Second)
	if err != nil {
		logging.Error("加载配置失败").Err(err).Send()
		os.Exit(1)
	}
	store.SyncFrom(cfg)

	// 4. 启动配置监听（fsnotify L1 + mtime L2 兜底）
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if err := store.StartWatcher(ctx); err != nil {
		logging.Error("配置监听启动失败").Err(err).Send()
	}

	// 5. 注册内置协议（S7 / ModbusTCP / OPCUA，均为完整实现）
	protocol.MustRegisterDefaults()

	// 6. 注册内置 Target（MQTT / InfluxDB，均为完整实现）
	target.MustRegisterDefaults()

	// 7. 初始化 Prometheus 指标
	metrics.MustRegister()

	// 8. 启动 4 个 worker 池
	pools := pool.NewPools(pool.Config{
		CollectSize: 200,
		ForwardSize: 100,
		WriteSize:   50,
		ConfigSize:  4,
	})
	pools.Start(ctx, nil)
	defer pools.Stop(5 * time.Second)

	// 9. 启动采集 worker（在 collectPool 中）
	collectWorkers := collect.NewWorkers(store, pools)
	collectWorkers.Start(ctx)

	// 10. 启动转发 worker（在 forwardPool 中）
	forwardWorkers := forward.NewWorkers(store, pools, collectWorkers.TargetRouter())
	forwardWorkers.Start(ctx)

	// 11. 启动回写 worker（在 writePool 中）
	writeWorkers := write.NewWorkers(store, pools, collectWorkers.Channels())
	writeWorkers.Start(ctx)

	// 12. 启动 HTTP API
	apiServer := api.NewServer(store, pools, ":8082")
	apiServer.SetWriter(writeWorkers)
	apiServer.SetChannelRegistry(collectWorkers.Channels())
	apiServer.SetForwardWorkers(forwardWorkers) // forward.Workers 实现了 forwardWorkersSnapshotter 接口
	go func() {
		if err := apiServer.Start(); err != nil {
			logging.Error("HTTP API 启动失败").Err(err).Send()
		}
	}()
	defer apiServer.Shutdown(5 * time.Second)

	// 13. 启动 Prometheus metrics 服务（独立端口）
	metricsServer := metrics.NewServer(":9091")
	go func() {
		if err := metricsServer.Start(); err != nil {
			logging.Error("Metrics 服务启动失败").Err(err).Send()
		}
	}()
	defer metricsServer.Shutdown(5 * time.Second)

	logging.Info(logging.EventServiceStart).
		Str("service", "plcbridge").
		Str("config_dir", *configDir).
		Str("api_addr", ":8082").
		Str("metrics_addr", ":9091").
		Msg("plcbridge started")

	// 14. 等待终止信号
	<-ctx.Done()
	logging.Info(logging.EventServiceStop).Msg("收到终止信号，开始优雅退出")

	// 关闭阶段（Phase 1.6 强化）：
	//   1. 先停 worker（停止新的采集/回写）→ 让 in-flight 操作有缓冲完成
	//   2. 再停 API / metrics 服务（让前端感知 / 不再产生新流量）
	//   3. 最后停 pools（清空残留 goroutine）+ config 监听随 ctx 自动退出
	shutdownStart := time.Now()

	shutdownOnce := func(name string, stop func(time.Duration)) {
		stepStart := time.Now()
		stop(5 * time.Second)
		logging.Info(logging.EventServiceStop).
			Str("step", name).
			Dur("dur_ms", time.Since(stepStart)).
			Msg("shutdown 步骤完成")
	}

	// 顺序：worker → API → metrics → pools
	shutdownOnce("collect", collectWorkers.Stop)
	shutdownOnce("forward", forwardWorkers.Stop)
	shutdownOnce("write", writeWorkers.Stop)
	apiServer.Shutdown(5 * time.Second)
	metricsServer.Shutdown(5 * time.Second)
	// pools.Stop 在 defer 中（5s 超时）
	logging.Info(logging.EventServiceStop).
		Dur("total_shutdown_ms", time.Since(shutdownStart)).
		Msg("plcbridge stopped")
}

// defaultConfigDir 返回配置目录。
//
// 优先级（2026-10-12 重新设计）：
//   1. -config 命令行参数（最高，由 flag.String 兜底）
//   2. PLCBRIDGE_CONFIG 环境变量
//   3. /etc/plcbridge（仅 Linux/macOS）
//   4. %PROGRAMDATA%\plcbridge（仅 Windows，PROGRAMDATA 未设置时降级 C:\ProgramData）
//
// 历史：2026-10-12 上午曾写死 PROGRAMDATA 拒启动，但用户实际工作流是显式 env
// 指向 F:\Project\PlcBridge\config（项目级配置，方便 git 同步 + 备份）。重写支持
// env 优先，让运维能显式控制。
func defaultConfigDir() string {
	if env := os.Getenv("PLCBRIDGE_CONFIG"); env != "" {
		return env
	}
	if _, err := os.Stat("/etc/plcbridge"); err == nil {
		return "/etc/plcbridge"
	}
	if progdata := os.Getenv("PROGRAMDATA"); progdata != "" {
		return progdata + `\plcbridge`
	}
	return `C:\ProgramData\plcbridge`
}
