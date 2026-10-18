// Package web 实现 plcbridge-web 后台进程：SQLite 配置存储 + REST CRUD API + 内嵌 H5 SPA。
//
// 顶层文件：
//   - web.go           ：SQLite 打开 / 迁移 / 路由注册入口
//   - model.go         ：ORM-like 数据模型（DTO）
//   - sqlstore.go      ：CRUD 数据访问层（事务封装）
//   - auth.go          ：单账号密码 + Cookie 鉴权
//   - handler_*.go     ：Gin HTTP handlers（按资源拆）
//   - dashboard.go     ：仪表盘聚合接口
//   - snapshot.go      ：导出 config.json 快照（被 plcbridge 监听）
//   - embed.go         ：内嵌 H5 SPA 静态资源（dist）
package web

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/gin-gonic/gin"

	_ "modernc.org/sqlite"
)

// webStartTime 记录 plcbridge-web 进程的启动时间，用于 /api/v1/health 返回 uptime。
var webStartTime = time.Now()

// OpenDB 打开 SQLite 数据库（纯 Go 驱动，无 CGo 依赖）。
//
// 重要：modernc.org/sqlite 默认 PRAGMA foreign_keys=OFF，每个连接都需要开启。
// 启用 WAL 模式（允许并发读写）+ 适当并发连接数。
func OpenDB(dbPath string) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", dbPath+"?_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	// 启用 WAL 模式：允许多个读连接和单写连接并发工作
	if _, err := db.ExecContext(context.Background(), "PRAGMA journal_mode = WAL"); err != nil {
		return nil, fmt.Errorf("enable WAL failed: %w", err)
	}
	if _, err := db.ExecContext(context.Background(), "PRAGMA synchronous = NORMAL"); err != nil {
		return nil, fmt.Errorf("set synchronous failed: %w", err)
	}
	// 允许多个连接并发（WAL 模式下读写可并发）；上限避免过多连接耗尽文件描述符
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(4)
	db.SetConnMaxLifetime(0)
	return db, nil
}

// Migrate 应用 MVP schema（§7.7）。
func Migrate(db *sql.DB) error {
	// 旧版本残留:source_biz_ids 已废除,t.target_sources 不再创建;先安全地清理。
	stmts := []string{
		`DROP TABLE IF EXISTS target_sources`,
		`DROP INDEX IF EXISTS idx_target_sources_source`,

		`CREATE TABLE IF NOT EXISTS channels (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			channel_biz_id TEXT UNIQUE NOT NULL,
			role TEXT NOT NULL CHECK(role IN ('collect','forward')),
			type TEXT NOT NULL,
			address TEXT NOT NULL,
			config_json TEXT,
			heartbeat_address TEXT,
			heartbeat_max_interval_ms INTEGER DEFAULT 5000,
			heartbeat_enabled INTEGER DEFAULT 0,
			heartbeat_debounce_ms INTEGER DEFAULT 50,
			queue_size INTEGER DEFAULT 10000,
			timeout_ms INTEGER DEFAULT 300,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_channels_role ON channels(role)`,
		`CREATE INDEX IF NOT EXISTS idx_channels_type ON channels(type)`,

		`CREATE TABLE IF NOT EXISTS devices (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			device_biz_id TEXT UNIQUE NOT NULL,
			name TEXT NOT NULL,
			enabled INTEGER NOT NULL DEFAULT 1,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		)`,

		`CREATE TABLE IF NOT EXISTS sources (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			source_biz_id TEXT UNIQUE NOT NULL,
			device_id INTEGER NOT NULL,
			channel_biz_id TEXT NOT NULL,
			collect_enabled INTEGER NOT NULL DEFAULT 1,
			write_enabled INTEGER NOT NULL DEFAULT 0,
			collect_interval_ms INTEGER NOT NULL DEFAULT 1000,
			timeout_ms INTEGER DEFAULT 300,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			FOREIGN KEY (device_id) REFERENCES devices(id) ON DELETE CASCADE,
			FOREIGN KEY (channel_biz_id) REFERENCES channels(channel_biz_id) ON DELETE RESTRICT
		)`,
		`CREATE INDEX IF NOT EXISTS idx_sources_device ON sources(device_id)`,
		`CREATE INDEX IF NOT EXISTS idx_sources_channel ON sources(channel_biz_id)`,

		`CREATE TABLE IF NOT EXISTS points (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			point_biz_id TEXT UNIQUE NOT NULL,
			source_id INTEGER NOT NULL,
			name TEXT NOT NULL,
			address TEXT NOT NULL,
			data_type TEXT NOT NULL,
			collect_enabled INTEGER NOT NULL DEFAULT 1,
			write_enabled INTEGER NOT NULL DEFAULT 0,
			string_length INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			FOREIGN KEY (source_id) REFERENCES sources(id) ON DELETE CASCADE
		)`,
		`CREATE INDEX IF NOT EXISTS idx_points_source ON points(source_id)`,

		`CREATE TABLE IF NOT EXISTS targets (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			target_biz_id TEXT UNIQUE NOT NULL,
			device_id INTEGER NOT NULL,
			forward_channel_biz_id TEXT NOT NULL,
			topic TEXT,
			format TEXT DEFAULT 'json',
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			FOREIGN KEY (device_id) REFERENCES devices(id) ON DELETE CASCADE,
			FOREIGN KEY (forward_channel_biz_id) REFERENCES channels(channel_biz_id) ON DELETE RESTRICT
		)`,
		`CREATE INDEX IF NOT EXISTS idx_targets_device ON targets(device_id)`,
		`CREATE INDEX IF NOT EXISTS idx_targets_channel ON targets(forward_channel_biz_id)`,

		`CREATE TABLE IF NOT EXISTS target_points (
			target_id INTEGER NOT NULL,
			point_id INTEGER NOT NULL,
			PRIMARY KEY (target_id, point_id),
			FOREIGN KEY (target_id) REFERENCES targets(id) ON DELETE CASCADE,
			FOREIGN KEY (point_id) REFERENCES points(id) ON DELETE CASCADE
		)`,
		`CREATE INDEX IF NOT EXISTS idx_target_points_point ON target_points(point_id)`,

		// 单账号 + Bearer Token 管理表（Plan §8.6 + §A.2 单账号）
		`CREATE TABLE IF NOT EXISTS accounts (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			username TEXT UNIQUE NOT NULL,
			password_hash TEXT NOT NULL,
			updated_at INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS tokens (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			token TEXT UNIQUE NOT NULL,
			name TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			revoked_at INTEGER
		)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			return err
		}
	}

	// Schema migrations：IF NOT EXISTS 不会改旧表，字段重命名等变更必须显式迁移。
	// 当前活跃迁移：targets.topic_prefix → targets.topic（2026-10-09 字段语义重整）
	if err := renameColumnIfExists(db, "targets", "topic_prefix", "topic"); err != nil {
		return fmt.Errorf("migration rename targets.topic_prefix→topic: %w", err)
	}
	// 2026-10-10：points.string_length（S7 STRING layout 字符数；protocol 层读 4+N 字节）
	if err := addColumnIfMissing(db, "points", "string_length", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return fmt.Errorf("migration add points.string_length: %w", err)
	}

	return nil
}

// renameColumnIfExists 当旧列存在且新列不存在时，ALTER TABLE RENAME COLUMN。
// SQLite 3.25+ 原生支持 RENAME COLUMN；modernc.org/sqlite v1.28 内部为 3.45+ 引擎。
func renameColumnIfExists(db *sql.DB, table, oldCol, newCol string) error {
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return err
	}
	hasOld, hasNew := false, false
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			rows.Close()
			return err
		}
		switch name {
		case oldCol:
			hasOld = true
		case newCol:
			hasNew = true
		}
	}
	rows.Close()
	if !hasOld || hasNew {
		return nil // 已是目标状态
	}
	_, err = db.Exec(`ALTER TABLE ` + table + ` RENAME COLUMN ` + oldCol + ` TO ` + newCol)
	return err
}

// addColumnIfMissing 当列不存在时 ADD COLUMN（SQLite 原生支持）。columnDef 是 "NAME TYPE [constraints]"。
func addColumnIfMissing(db *sql.DB, table, col, columnDef string) error {
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			rows.Close()
			return err
		}
		if name == col {
			rows.Close()
			return nil
		}
	}
	rows.Close()
	_, err = db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + col + ` ` + columnDef)
	return err
}

// RegisterRoutes 注册 Web 路由。
//
// 路由结构：
//
//	GET  /                       H5 SPA 入口（内嵌 dist/index.html）
//	GET  /assets/*               H5 SPA 静态资源
//	POST /api/v1/auth/login      单账号登录（设 Cookie）
//	POST /api/v1/auth/logout     清除 Cookie
//	GET  /api/v1/auth/me         当前登录态
//	POST /api/v1/auth/password   改密（要求已登录）
//
//	/api/v1/tokens               list / 创建 / 撤销 Bearer Token（§8.6，复用 plcbridge）
//
//	/api/v1/channels             CRUD（role=collect|forward，type 决定 protocol/target 插件）
//	/api/v1/devices              CRUD
//	/api/v1/devices/:id/sources  nested list
//	/api/v1/sources/:id          GET / PUT / DELETE
//	/api/v1/sources/:id/points   nested list
//	/api/v1/points               POST；GET/PUT/DELETE /api/v1/points/:id
//	/api/v1/targets              CRUD
//	/api/v1/targets/:id/points   PUT 替换 Point 关联
//
//	/api/v1/dashboard            仪表盘聚合（计数 + 最近错误）
//
//	GET  /api/v1/snapshot        导出当前 SQLite → config.json 快照到 configDir
func RegisterRoutes(r *gin.Engine, db *sql.DB, configDir string, sess *SessionStore) {
	// CORS：同源部署（前端内置在 plcbridge-web）无需 CORS，但跨域场景（如外部 dashboard）需放行。
	// OPTIONS 预检直接 204，避免 MountSPA 的 NoRoute 把 OPTIONS 当 405 返回。
	r.Use(corsMiddleware())

	// 公开路由
	// /api/v1/health 返回：
	//   - status / db           : 健康/数据库状态
	//   - process.pid           : plcbridge-web 进程 PID（os.Getpid）
	//   - process.started_at_ms : plcbridge-web 进程启动时间（unix ms）
	//   - process.uptime_ms     : 已运行时长（毫秒）
	//   - process.rss_bytes     : 常驻内存（process.ResidentMemoryBytes，从 /proc/self/status 读取）
	//   - process.num_goroutine : 当前 goroutine 数（runtime.NumGoroutine）
	// 保持公开路由，仪表盘 3s 心跳调用，避免每 3 秒都要重新登录。
	r.GET("/api/v1/health", healthHandler(db))
	// /api/v1/health/ready：深度健康检查（Phase 1.5），用于 K8s readinessProbe / 外部 LB。
	// 与 /api/v1/health 区别：/health 永远 200（liveness），/health/ready 受依赖影响（readiness）。
	r.GET("/api/v1/health/ready", readyHandler(db))

	r.POST("/api/v1/auth/login", loginHandler(db, sess))

	// 需要登录的 API
	auth := r.Group("/api/v1")
	auth.Use(RequireSession(sess))
	{
		auth.POST("/auth/logout", logoutHandler(sess))
		auth.GET("/auth/me", meHandler(db))
		auth.POST("/auth/password", changePasswordHandler(db))

		auth.GET("/channels", listChannels(db))
		auth.POST("/channels", createChannel(db))
		auth.GET("/channels/:id", getChannel(db))
		auth.PUT("/channels/:id", updateChannel(db))
		auth.DELETE("/channels/:id", deleteChannel(db))

		auth.GET("/devices", listDevices(db))
		auth.POST("/devices", createDevice(db))
		auth.GET("/devices/:id", getDevice(db))
		auth.PUT("/devices/:id", updateDevice(db))
		auth.DELETE("/devices/:id", deleteDevice(db))
		auth.GET("/devices/:id/sources", listDeviceSources(db))

		auth.POST("/sources", createSource(db))
		auth.GET("/sources/:id", getSource(db))
		auth.PUT("/sources/:id", updateSource(db))
		auth.DELETE("/sources/:id", deleteSource(db))
		auth.GET("/sources/:id/points", listSourcePoints(db))

		auth.POST("/points", createPoint(db))
		auth.GET("/points/:id", getPoint(db))
		auth.PUT("/points/:id", updatePoint(db))
		auth.DELETE("/points/:id", deletePoint(db))

		auth.GET("/targets", listTargets(db))
		auth.POST("/targets", createTarget(db))
		auth.GET("/targets/:id", getTarget(db))
		auth.PUT("/targets/:id", updateTarget(db))
		auth.DELETE("/targets/:id", deleteTarget(db))
		auth.PUT("/targets/:id/points", setTargetPoints(db))

		auth.GET("/tokens", listTokens(db))
		auth.POST("/tokens", createToken(db))
		auth.DELETE("/tokens/:id", revokeToken(db))

		auth.GET("/dashboard", dashboard(db))
		auth.POST("/snapshot", exportSnapshot(db, configDir))
		auth.GET("/plcbridge/metrics/summary", metricsSummaryHandler())
		auth.GET("/plcbridge/channels/runtime", channelsRuntimeHandler())

		// 进程控制（仪表盘启停 plcbridge / 重载配置 / 重启 web）
		auth.POST("/process/control", processControlHandler(db, configDir))
		auth.GET("/process/supervisor", supervisorStatusHandler(configDir))
	}

	// H5 SPA：静态资源 + SPA fallback（路由到 dist/index.html）
	MountSPA(r)
}
