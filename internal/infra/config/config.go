// Package config 实现 plcbridge 配置热加载（fsnotify L1 + mtime L2）。
//
// 设计要点（参考 Plan.md §6.6）：
//  1. Web 后台写 SQLite → 导出 config.json 到平台配置目录
//  2. plcbridge 通过 fsnotify 监听文件变更（L1）+ mtime 轮询兜底（L2）
//  3. atomic.Pointer 原子替换内存缓存，业务无中断
//  4. 跨平台路径探测（Linux /etc/plcbridge/  Windows %PROGRAMDATA%\plcbridge\）
//
// 本包仅负责配置加载与热更新，不定义领域模型（领域模型在 platform 包）。
package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/plcbridge/plcbridge/internal/infra/logging"
	"github.com/plcbridge/plcbridge/internal/metrics"
	"github.com/plcbridge/plcbridge/internal/platform"
)

// Store 是运行时配置容器，提供原子替换 + 跨进程文件监听。
type Store struct {
	current atomic.Pointer[platform.Config]
	dir     string
	// gen 每次成功 reload 后自增（覆盖 cfg.Version），用于驱动 supervisor
	// 重新生成 worker。即使 config.json 的 Version 字段没变，也能触发 spawnAll。
	gen atomic.Uint64
}

// NewStore 创建配置存储（不立即加载，等显式 Load）。
func NewStore(dir string) *Store {
	return &Store{dir: dir}
}

// ErrConfigNotFound 配置文件不存在。
var ErrConfigNotFound = errors.New("config.json not found")

// LoadWithRetry 首次加载（启动时用）。配置文件不存在时等待重试。
//
// Plan.md §5.5：plcbridge 启动时无配置文件 → 等待重试，不报错退出。
//
// 此函数不会更新传入 store 的 current；调用方需要自行管理。
// 推荐用法：创建 store → LoadWithRetry → store.TryLoad()（同步覆盖）→ StartWatcher。
func LoadWithRetry(dir string, interval time.Duration) (*platform.Config, error) {
	tmp := NewStore(dir)
	deadline := time.Now().Add(60 * time.Second) // 最多等 60 秒
	for {
		cfg, err := tmp.tryLoad("initial")
		if err == nil {
			return cfg, nil
		}
		if !errors.Is(err, ErrConfigNotFound) {
			return nil, err
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("等待 config.json 超过 60 秒仍未出现")
		}
		logging.Info(logging.EventConfigReload).Str("dir", dir).Msg("config.json 尚未生成，等待...")
		time.Sleep(interval)
	}
}

// TryLoad 尝试加载一次（不重试）。
func (s *Store) TryLoad() (*platform.Config, error) {
	return s.tryLoad("manual")
}

// tryLoad 加载配置并上报 Prometheus 指标。
//
// trigger 参数说明（对应 metrics.ConfigReloadTotal 的 label）：
//   - "initial"  启动时首次加载
//   - "fsnotify" 文件事件触发（热加载主路径）
//   - "mtime"    L2 轮询兜底触发
//   - "manual"   显式调用（测试 / 运维手动触发）
//   - "error"    加载失败
//
// 为什么单独一层：TryLoad 是公开 API，测试与调用方都直接用它，
// 不应该为了埋点而改签名（会破坏调用方与测试）。
func (s *Store) tryLoad(trigger string) (*platform.Config, error) {
	path := filepath.Join(s.dir, "config.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			metrics.ConfigReloadTotal.WithLabelValues("not_found").Inc()
			return nil, ErrConfigNotFound
		}
		metrics.ConfigReloadTotal.WithLabelValues("error").Inc()
		return nil, fmt.Errorf("读取 %s 失败: %w", path, err)
	}

	var cfg platform.Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		metrics.ConfigReloadTotal.WithLabelValues("error").Inc()
		return nil, fmt.Errorf("解析 config.json 失败: %w", err)
	}

	// 业务校验（Phase 1.4：完整校验替代原基础 validate）
	if err := Validate(&cfg); err != nil {
		metrics.ConfigReloadTotal.WithLabelValues("error").Inc()
		return nil, fmt.Errorf("config.json 业务校验失败: %w", err)
	}

	// 用 monotonic generation 覆盖 cfg.Version，确保每次成功 reload 都能让
	// 依赖 cfg.Version 触发 worker 重生成的 supervisor 立刻反应（即使 web 端
	// 导出的 config.json 自己没更新 version 字段）。
	cfg.Version = int(s.gen.Add(1))

	s.current.Store(&cfg)
	metrics.ConfigReloadTotal.WithLabelValues(trigger).Inc()
	metrics.ConfigVersion.Set(float64(cfg.Version))
	logging.Info(logging.EventConfigReload).
		Str("path", path).
		Int("version", cfg.Version).
		Int("channels", len(cfg.Channels)).
		Int("devices", len(cfg.Devices)).
		Msg("config loaded")
	return &cfg, nil
}

// Current 获取当前配置快照（atomic load）。
func (s *Store) Current() *platform.Config {
	return s.current.Load()
}

// SyncFrom 用外部加载的 cfg 强制同步到当前 store（启动时 LoadWithRetry 后调用）。
func (s *Store) SyncFrom(cfg *platform.Config) {
	s.current.Store(cfg)
}

// validate 业务校验的旧入口（保留签名以避免破坏外部调用）。
//
// Phase 1.4：完整校验逻辑已迁移到 validate.go 的 Validate 函数。
// 本函数保留为内部委托。
func (s *Store) validate(cfg *platform.Config) error {
	return Validate(cfg)
}

// StartWatcher 启动配置监听（fsnotify L1 + mtime L2 兜底）。
//
// 在独立 goroutine 中运行；ctx 取消时停止。
func (s *Store) StartWatcher(ctx context.Context) error {
	go s.watchFsnotify(ctx)
	go s.watchMtime(ctx)
	return nil
}

func (s *Store) watchFsnotify(ctx context.Context) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		logging.Error(logging.EventConfigReloadFail).Err(err).Msg("fsnotify 创建失败，依赖 L2 mtime 轮询")
		return
	}
	defer watcher.Close()

	if err := watcher.Add(s.dir); err != nil {
		logging.Error(logging.EventConfigReloadFail).Err(err).Str("dir", s.dir).Msg("fsnotify.Add 失败")
		return
	}

	// 去抖：500ms 内的多次事件合并（§3 关键参数）
	const debounceMs = 500
	var debounceTimer *time.Timer

	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			if filepath.Base(event.Name) != "config.json" {
				continue
			}
			if debounceTimer != nil {
				debounceTimer.Stop()
			}
			debounceTimer = time.AfterFunc(debounceMs*time.Millisecond, func() {
				if _, err := s.tryLoad("fsnotify"); err != nil {
					logging.Error(logging.EventConfigReloadFail).Err(err).Msg("热加载 config.json 失败")
				}
			})
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			logging.Warn(logging.EventConfigReloadFail).Err(err).Msg("fsnotify error")
		}
	}
}

func (s *Store) watchMtime(ctx context.Context) {
	const intervalSec = 60 // Plan.md §6.6 L2 兜底
	ticker := time.NewTicker(time.Duration(intervalSec) * time.Second)
	defer ticker.Stop()

	var lastMtime time.Time
	if mt, err := getMtime(filepath.Join(s.dir, "config.json")); err == nil {
		lastMtime = mt
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			path := filepath.Join(s.dir, "config.json")
			mt, err := getMtime(path)
			if err != nil {
				continue
			}
			if mt.After(lastMtime) {
				lastMtime = mt
				if _, err := s.tryLoad("mtime"); err != nil {
					logging.Error(logging.EventConfigReloadFail).Err(err).Msg("L2 mtime 热加载失败")
				}
			}
		}
	}
}

func getMtime(path string) (time.Time, error) {
	info, err := os.Stat(path)
	if err != nil {
		return time.Time{}, err
	}
	return info.ModTime(), nil
}

// WriteInitial Web 后台启动时如无 config.json，则生成初始空配置。
func WriteInitial(dir string) error {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	path := filepath.Join(dir, "config.json")
	if _, err := os.Stat(path); err == nil {
		return nil
	}

	initial := &platform.Config{
		Version:  1,
		Channels: []*platform.Channel{},
		Devices:  []*platform.Device{},
	}
	data, err := json.MarshalIndent(initial, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}
