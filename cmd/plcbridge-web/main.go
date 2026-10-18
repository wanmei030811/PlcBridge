// Package main 是 plcbridge-web 后台进程入口。
//
// 功能：
//   - 监听 :8080
//   - SQLite 配置真源（CRUD API + H5 SPA）
//   - 生成 config.json 快照到平台配置目录（plcbridge fsnotify 自动 reload）
//   - 单账号密码登录 + Bearer Token 管理（§8.6 + §A.2）
//   - 内嵌 Vue3 H5 SPA（dist/ 由前端 build 产物填充）
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	_ "modernc.org/sqlite"

	"github.com/gin-gonic/gin"

	"github.com/plcbridge/plcbridge/internal/infra/config"
	"github.com/plcbridge/plcbridge/internal/infra/logging"
	"github.com/plcbridge/plcbridge/internal/interface/web"
)

func main() {
	configDir := flag.String("config", defaultConfigDirWeb(), "配置文件目录（绝对路径）")
	addr := flag.String("addr", ":8080", "Web 监听地址")
	dbPath := flag.String("db", "", "SQLite 数据库路径（默认 <configDir>/data/plcbridge.db）")
	adminUser := flag.String("admin-user", "admin", "初始管理员账号")
	adminPassEnv := os.Getenv("PLCBRIDGE_WEB_ADMIN_PASSWORD")
	adminPass := flag.String("admin-password", adminPassEnv, "初始管理员密码（为空则随机生成并打印 + 写盘）")
	hmacFile := flag.String("hmac-key", "", "session HMAC key 持久化文件路径（默认 <configDir>/web.hmac）")
	resetAdminPass := flag.String("reset-admin", "", "非空时重置 admin 账号密码为该值并退出（用于密码丢失救援）")
	flag.Parse()

	logging.Init("INFO")

	// 0. 救援通道：-reset-admin=<新密码> → 重置后直接退出
	if *resetAdminPass != "" {
		// 救援路径也用 defaultConfigDirWeb（env 优先，与正常路径一致；2026-10-12 修复 dual-dir 漂移）
		if *configDir == "" {
			*configDir = defaultConfigDirWeb()
		}
		if *dbPath == "" {
			*dbPath = filepath.Join(*configDir, "data", "plcbridge.db")
		}
		// 先记录"准备 reset"，即使后续失败也能溯源
		recordPasswordLog(*configDir, "RESET_START", map[string]any{
			"db_path":         *dbPath,
			"new_password":    *resetAdminPass,
			"caller_pid":      os.Getpid(),
		})
		if err := resetAdmin(*dbPath, *adminUser, *resetAdminPass); err != nil {
			recordPasswordLog(*configDir, "RESET_FAILED", map[string]any{"error": err.Error()})
			fmt.Fprintf(os.Stderr, "reset-admin failed: %v\n", err)
			os.Exit(1)
		}
		recordPasswordLog(*configDir, "RESET_OK", map[string]any{
			"db_path":      *dbPath,
			"new_password": *resetAdminPass,
		})
		fmt.Printf("admin 密码已重置为: %s\n", *resetAdminPass)
		return
	}

	// 启动检查：configDir 必须存在（2026-10-12 修复 dual-dir 漂移 bug）
	// 优先级已由 defaultConfigDirWeb 决定：env > /etc > PROGRAMDATA。
	// 不存在即拒启动，不自动降级。
	if info, err := os.Stat(*configDir); err != nil || !info.IsDir() {
		fmt.Fprintf(os.Stderr, "configDir 不存在或不是目录: %s (请确认 PLCBRIDGE_CONFIG env 或 -config 参数)\n", *configDir)
		os.Exit(1)
	}
	if *dbPath == "" {
		*dbPath = filepath.Join(*configDir, "data", "plcbridge.db")
	}

	// 1. 打开 SQLite
	if err := os.MkdirAll(filepath.Dir(*dbPath), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "mkdir db dir failed: %v\n", err)
		os.Exit(1)
	}
	db, err := web.OpenDB(*dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open db failed: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	// 2. 迁移
	if err := web.Migrate(db); err != nil {
		fmt.Fprintf(os.Stderr, "migrate failed: %v\n", err)
		os.Exit(1)
	}

	// 3. 加载/生成 HMAC key
	if *hmacFile == "" {
		*hmacFile = filepath.Join(*configDir, "web.hmac")
	}
	hmacKey, err := loadOrCreateHMAC(*hmacFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hmac key load failed: %v\n", err)
		os.Exit(1)
	}
	sess := web.NewSessionStore(hmacKey)
	defer sess.Stop()

	// 4. 初始化单账号。EnsureAccount 仅在本次新生成密码（非显式指定）时返回非空明文，
	//    账号已存在或显式指定密码时均返回 ""，避免覆盖写入 web.bootstrap。
	//    启动前先快照 accounts 表 / web.bootstrap 状态以便溯源。
	bootSnap := snapshotAccountState(*configDir, *dbPath, *adminUser, *adminPass, adminPassEnv)
	recordPasswordLog(*configDir, "BOOT_BEFORE_ENSURE", bootSnap)
	actualPwd, err := web.EnsureAccount(db, *adminUser, *adminPass)
	if err != nil {
		recordPasswordLog(*configDir, "BOOT_ENSURE_FAILED", map[string]any{"error": err.Error()})
		fmt.Fprintf(os.Stderr, "ensure account failed: %v\n", err)
		os.Exit(1)
	}
	if *adminPass == "" && adminPassEnv == "" && actualPwd != "" {
		_ = os.MkdirAll(*configDir, 0o755)
		bootPath := filepath.Join(*configDir, "web.bootstrap")
		if werr := os.WriteFile(bootPath, []byte(actualPwd), 0o600); werr == nil {
			fmt.Fprintln(os.Stderr, "WEB 管理员初始密码同时写入:", bootPath)
		}
	}
	// 启动后把 EnsureAccount 的结果也记录（用于核对"是否被新生成/是否写盘"）
	recordPasswordLog(*configDir, "BOOT_AFTER_ENSURE", map[string]any{
		"admin_user":          *adminUser,
		"admin_pass_set":      *adminPass != "" || adminPassEnv != "",
		"actual_pwd_returned": actualPwd != "",
		"bootstrap_path":      filepath.Join(*configDir, "web.bootstrap"),
		"bootstrap_existed":   bootSnap["bootstrap_existed"],
		"bootstrap_will_keep": !(*adminPass == "" && adminPassEnv == "" && actualPwd != ""),
	})

	// 5. 生成初始空配置（如无）
	if _, err := os.Stat(filepath.Join(*configDir, "config.json")); os.IsNotExist(err) {
		if err := os.MkdirAll(*configDir, 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "mkdir config dir failed: %v\n", err)
			os.Exit(1)
		}
		if err := config.WriteInitial(*configDir); err != nil {
			fmt.Fprintf(os.Stderr, "write initial config failed: %v\n", err)
			os.Exit(1)
		}
		logging.Info("已生成初始 config.json").Str("path", *configDir).Send()
	}

	// 6. 注册路由（生产模式：减少 stdout 噪音）
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())
	r.Use(requestLogger())
	web.RegisterRoutes(r, db, *configDir, sess)

	srv := &http.Server{
		Addr:    *addr,
		Handler: r,
	}

	httpErrCh := make(chan error, 1)
	go func() {
		logging.Info("plcbridge-web 启动").Str("addr", *addr).Send()
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			httpErrCh <- err
		}
		close(httpErrCh)
	}()

	// 8. 启动后兜底：检查 supervisor 是否在跑，缺则拉起。
	// supervisor 自己不能保活自己（无 watchdog）—— 物理网络异常 / OS 重启 / 异常退出后只能依赖 web。
	// 这里 web 起来后 5 秒做检测（不阻塞 Listen），避免启动风暴。
	go func() {
		time.Sleep(5 * time.Second)
		logging.Info("plcbridge-web 后台检测 supervisor 状态").Send()
		if err := web.AutoStartSupervisorIfMissing(*configDir); err != nil {
			logging.Warn("自动启动 supervisor 失败").Err(err).Send()
		}
	}()

	// 7. 等待终止信号或 HTTP 致命错误
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	select {
	case <-ctx.Done():
		logging.Info(logging.EventServiceStop).Msg("plcbridge-web 收到终止信号，开始关闭")
	case err := <-httpErrCh:
		if err != nil {
			logging.Error(logging.EventServiceStop).Err(err).Msg("HTTP 服务异常退出")
		}
	case <-web.ProcessSelfShutdownCh():
		logging.Info(logging.EventServiceStop).Msg("plcbridge-web 收到仪表盘'重启 Web'指令，开始关闭")
	}

	// 优雅关闭阶段（Phase 1.6 强化）：
	//   1. srv.Shutdown：停接新请求，等旧请求完成（最多 5s）
	//   2. 二次强制：超 10s 总时间后强制退出
	shutdownStart := time.Now()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logging.Error(logging.EventServiceStop).Err(err).Msg("graceful shutdown 超时，强制关闭")
		_ = srv.Close() // 立即关闭剩余连接
	}
	shutdownCancel()

	// 总耗时记录
	logging.Info(logging.EventServiceStop).
		Dur("shutdown_duration_ms", time.Since(shutdownStart)).
		Msg("plcbridge-web 已停止")
}

// loadOrCreateHMAC 加载或生成 session HMAC key 持久化文件。
func loadOrCreateHMAC(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err == nil && len(b) >= 32 {
		return b, nil
	}
	key := web.RandomHMACKey()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, key, 0o600); err != nil {
		return nil, err
	}
	return key, nil
}

// resetAdmin 救援通道：用 bcrypt 哈希覆盖 accounts.id=1 的密码。
func resetAdmin(dbPath, username, newPassword string) error {
	if newPassword == "" {
		return fmt.Errorf("新密码不能为空")
	}
	if len(newPassword) < 8 {
		return fmt.Errorf("新密码至少 8 位")
	}
	db, err := web.OpenDB(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := web.Migrate(db); err != nil {
		return err
	}
	hash, err := web.HashPassword(newPassword)
	if err != nil {
		return err
	}
	// 强制更新；若账号不存在则插入一条（id=1）
	res, err := db.Exec(
		`UPDATE accounts SET username=?, password_hash=?, updated_at=? WHERE id=1`,
		username, hash, time.Now().UnixMilli())
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		if _, err := db.Exec(
			`INSERT INTO accounts (id, username, password_hash, updated_at) VALUES (1, ?, ?, ?)`,
			username, hash, time.Now().UnixMilli()); err != nil {
			return err
		}
	}
	return nil
}

// requestLogger 简单访问日志。
func requestLogger() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		logging.Info("http").
			Str("method", c.Request.Method).
			Str("path", c.Request.URL.Path).
			Int("status", c.Writer.Status()).
			Dur("dur", time.Since(start)).
			Send()
	}
}

// defaultConfigDirWeb 返回配置目录。
//
// 优先级（2026-10-12 重新设计）：
//   1. -config 命令行参数（最高，由 flag.String 兜底）
//   2. PLCBRIDGE_CONFIG 环境变量
//   3. /etc/plcbridge（仅 Linux/macOS）
//   4. %PROGRAMDATA%\plcbridge（仅 Windows，PROGRAMDATA 未设置时降级 C:\ProgramData）
//
// 历史：2026-10-12 上午曾写死 PROGRAMDATA，但用户实际工作流是显式 env 指向
// F:\Project\PlcBridge\config（项目级配置，方便 git 同步 + 备份）。重写支持 env 优先。
func defaultConfigDirWeb() string {
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

// recordPasswordLog 把每一次 web 启动/密码变更的事实落到 <configDir>/web.password.log。
// 目的：用户多次反馈"我没改过密码但登不进去"——把"是否新生成/是否被 reset/启动参数是什么"
// 都写盘，便于事后比对，不再靠口头回忆。
//
// 文件格式：每行一条 JSON，字段固定为 ts / event / config_dir / db_path / pid / details。
//
// 失败容错：写不进盘也只 fmt.Println 到 stderr，不影响主流程。
func recordPasswordLog(configDir, event string, details map[string]any) {
	if configDir == "" {
		configDir = "."
	}
	logPath := filepath.Join(configDir, "web.password.log")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "[password-log] mkdir failed: %v\n", err)
		return
	}
	rec := map[string]any{
		"ts":         time.Now().Format("2006-01-02T15:04:05.000-07:00"),
		"event":      event,
		"config_dir": configDir,
		"pid":        os.Getpid(),
		"details":    details,
	}
	buf, err := jsonMarshal(rec)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[password-log] marshal failed: %v\n", err)
		return
	}
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[password-log] open failed: %v\n", err)
		return
	}
	defer f.Close()
	if _, err := f.Write(append(buf, '\n')); err != nil {
		fmt.Fprintf(os.Stderr, "[password-log] write failed: %v\n", err)
	}
}

// snapshotAccountState 收集启动时的"现状"快照：db 文件存在与否、accounts 是否已有 admin、
// web.bootstrap 是否存在及其内容长度/哈希（绝不写明文）。
func snapshotAccountState(configDir, dbPath, adminUser, adminPass, adminPassEnv string) map[string]any {
	out := map[string]any{
		"config_dir":         configDir,
		"db_path":            dbPath,
		"db_exists":          false,
		"account_exists":     false,
		"bootstrap_exists":   false,
		"bootstrap_len":      0,
		"admin_pass_set":     adminPass != "" || adminPassEnv != "",
		"admin_pass_env_set": adminPassEnv != "",
		"caller_pid":         os.Getpid(),
	}
	if fi, err := os.Stat(dbPath); err == nil {
		out["db_exists"] = true
		out["db_size"] = fi.Size()
		out["db_mtime"] = fi.ModTime().Format("2006-01-02T15:04:05-07:00")
	}
	if fi, err := os.Stat(filepath.Join(configDir, "web.bootstrap")); err == nil {
		out["bootstrap_exists"] = true
		out["bootstrap_len"] = fi.Size()
		out["bootstrap_mtime"] = fi.ModTime().Format("2006-01-02T15:04:05-07:00")
		// 只记前 4 字符的指纹，方便肉眼比对"是不是同一个 bootstrap"
		if b, err := os.ReadFile(filepath.Join(configDir, "web.bootstrap")); err == nil && len(b) >= 4 {
			out["bootstrap_fp4"] = string(b[:4])
		}
	}
	// accounts 表是否存在 admin：直接查 sqlite（依赖 modernc sqlite，已在主程序链接）
	if db, err := openDBReadOnly(dbPath); err == nil {
		defer db.Close()
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM accounts WHERE id=1`).Scan(&n); err == nil {
			out["account_exists"] = n > 0
		} else {
			out["account_query_error"] = err.Error()
		}
	} else {
		out["db_open_error"] = err.Error()
	}
	return out
}

// jsonMarshal 复用 stdlib，避免额外依赖。
func jsonMarshal(v any) ([]byte, error) {
	// 手写最小化 JSON 序列化：只支持 map[string]any + 基本类型
	// 比引 encoding/json 更省事（也避免 main.go 重复 import）。
	return jsonEncodeValue(v), nil
}

func jsonEncodeValue(v any) []byte {
	switch x := v.(type) {
	case nil:
		return []byte("null")
	case bool:
		if x {
			return []byte("true")
		}
		return []byte("false")
	case string:
		return append(append([]byte(`"`), jsonEscapeString(x)...), '"')
	case int:
		return []byte(fmt.Sprintf("%d", x))
	case int64:
		return []byte(fmt.Sprintf("%d", x))
	case float64:
		return []byte(fmt.Sprintf("%g", x))
	case map[string]any:
		buf := []byte{'{'}
		first := true
		// 顺序固定为 key 字典序，避免同一事实写出多份 log 文本抖动
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sortStrings(keys)
		for _, k := range keys {
			if !first {
				buf = append(buf, ',')
			}
			first = false
			buf = append(buf, '"')
			buf = append(buf, jsonEscapeString(k)...)
			buf = append(buf, '"', ':')
			buf = append(buf, jsonEncodeValue(x[k])...)
		}
		buf = append(buf, '}')
		return buf
	default:
		return []byte(fmt.Sprintf("%q", fmt.Sprintf("%v", x)))
	}
}

func jsonEscapeString(s string) []byte {
	out := make([]byte, 0, len(s))
	for _, r := range s {
		switch r {
		case '"':
			out = append(out, '\\', '"')
		case '\\':
			out = append(out, '\\', '\\')
		case '\n':
			out = append(out, '\\', 'n')
		case '\r':
			out = append(out, '\\', 'r')
		case '\t':
			out = append(out, '\\', 't')
		default:
			if r < 0x20 {
				out = append(out, []byte(fmt.Sprintf(`\u%04x`, r))...)
			} else {
				out = append(out, byte(r))
			}
		}
	}
	return out
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

// openDBReadOnly 用 modernc/sqlite（项目主程序已链）只读开库，仅用于快照。
func openDBReadOnly(path string) (*sql.DB, error) {
	dsn := fmt.Sprintf("file:%s?mode=ro", filepath.ToSlash(path))
	return sql.Open("sqlite", dsn)
}
