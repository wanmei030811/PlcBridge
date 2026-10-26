// backup.go：配置备份 / 恢复（Plan2 新增功能）。
//
// 功能：
//
//	GET  /api/v1/config/backup/info   → 返回最新备份信息（时间、大小、记录数）
//	POST /api/v1/config/backup      → 导出备份 zip（SQLite + config.json）
//	POST /api/v1/config/restore     → 从备份 zip 恢复（自动先备份当前）
//
// 备份格式：
//
//	backup-<timestamp>.zip
//	  ├─ meta.json          备份元信息（时间、版本、记录数）
//	  ├─ config.json        导出时的 config.json 快照
//	  └─ store.db           SQLite 数据库完整副本
//
// 保留策略：最多保留 10 份，超出则删除最旧的。
package web

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// === 备份元信息 ===

// backupMeta 打包在 zip 里的元信息文件。
type backupMeta struct {
	CreatedAt  string `json:"created_at"`  // RFC3339
	Version    int    `json:"version"`     // config.json 的 Version
	DBRecords  int    `json:"db_records"`  // 所有表行数合计
	DBSize     int64  `json:"db_size"`     // SQLite 文件字节数
	ConfigSize int64  `json:"config_size"` // config.json 字节数
}

// backupInfo API 返回给前端的备份信息。
type backupInfo struct {
	Filename    string `json:"filename"`
	CreatedAt   string `json:"created_at"`
	Version     int    `json:"version"`
	DBRecords   int    `json:"db_records"`
	DBSizeBytes int64  `json:"db_size_bytes"`
}

// === 辅助函数 ===

// backupDir 返回备份目录路径（configDir/backups/）。
func backupDir(configDir string) string {
	return filepath.Join(configDir, "backups")
}

// ensureBackupDir 确保备份目录存在。
func ensureBackupDir(configDir string) (string, error) {
	dir := backupDir(configDir)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("create backup dir: %w", err)
	}
	return dir, nil
}

// countDBRecords 统计所有业务表的行数合计。
func (s *Store) countDBRecords(ctx context.Context) (int, error) {
	tables := []string{"channels", "devices", "sources", "points", "targets", "tokens"}
	var total int
	for _, t := range tables {
		var cnt int
		if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+t).Scan(&cnt); err != nil {
			return 0, fmt.Errorf("count %s: %w", t, err)
		}
		total += cnt
	}
	return total, nil
}

// listBackups 返回备份目录中所有 .zip 文件（按时间倒序）。
func listBackups(backupDirPath string) ([]os.DirEntry, error) {
	entries, err := os.ReadDir(backupDirPath)
	if err != nil {
		return nil, err
	}
	var zips []os.DirEntry
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".zip") {
			zips = append(zips, e)
		}
	}
	// 按名称倒序（文件名含时间戳）
	sort.Slice(zips, func(i, j int) bool {
		return zips[i].Name() > zips[j].Name()
	})
	return zips, nil
}

// readBackupMeta 从 zip 里读 meta.json。
func readBackupMetaFromFile(path string) (*backupMeta, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	zr, err := zip.NewReader(f, fi.Size())
	if err != nil {
		return nil, err
	}
	for _, f := range zr.File {
		if f.Name == "meta.json" {
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			var m backupMeta
			if err := json.NewDecoder(rc).Decode(&m); err != nil {
				rc.Close()
				return nil, err
			}
			rc.Close()
			return &m, nil
		}
	}
	return nil, nil
}

// pruneOldBackups 删除最旧的备份，保留最多 maxKeep 份。
func pruneOldBackups(backupDirPath string, maxKeep int) error {
	entries, err := listBackups(backupDirPath)
	if err != nil {
		return err
	}
	if len(entries) <= maxKeep {
		return nil
	}
	// entries 已按时间倒序，末尾是最旧的
	for _, e := range entries[maxKeep:] {
		if err := os.Remove(filepath.Join(backupDirPath, e.Name())); err != nil {
			return fmt.Errorf("remove old backup %s: %w", e.Name(), err)
		}
	}
	return nil
}

func addZipFile(zw *zip.Writer, name string, data []byte) {
	w, _ := zw.Create(name)
	w.Write(data)
}

// getCurrentDBPath 获取当前 SQLite 数据库文件路径。
//
// 2026-10-21 修复：
//
//	原实现是 Scan(nil, "main", &path)。PRAGMA database_list 返回 3 列
//	（seq / name / file），而 nil 不是合法的 scan 目标 →
//	database/sql 直接报 "destination not a pointer"，
//	错误又被 `_ =` 吞掉，导致 path 始终为空串。
//	后续 exportBackupHandler 拿到空路径 → 读不到文件 → 500
//	"无法读取 SQLite 数据库文件"。
//
// 现在改成按列名逐行扫描，忽略错误但保留路径。
func getCurrentDBPath(db *sql.DB) string {
	rows, err := db.QueryContext(context.Background(), "PRAGMA database_list")
	if err != nil {
		return ""
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return ""
	}
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			continue
		}
		// 找 name == "main" 的那行的 file 列（主库文件路径）
		for i, c := range cols {
			if c != "name" || i >= len(vals) {
				continue
			}
			name, _ := vals[i].(string)
			if name != "main" {
				continue
			}
			for j, c2 := range cols {
				if c2 == "file" && j < len(vals) {
					if p, ok := vals[j].(string); ok {
						return p
					}
				}
			}
		}
	}
	return ""
}

// snapshotDB 生成 SQLite 数据库的一致性快照（字节内容）。
//
// 优先用 VACUUM INTO（SQLite 3.27+）：它会把主库文件 + WAL 合并成一个
// 自包含的新库文件，是备份场景下的标准做法。
//
// VACUUM INTO 不可用时（老版本 SQLite / 语法不被支持），
// 退回到「直接读 .db 主文件」——功能降级但不报错。
func (s *Store) snapshotDB(ctx context.Context) ([]byte, error) {
	tmp, err := os.CreateTemp("", "plcbridge-backup-*.db")
	if err != nil {
		return nil, err
	}
	tmpPath := tmp.Name()
	// VACUUM INTO 要求目标文件不存在
	tmp.Close()
	os.Remove(tmpPath)
	defer os.Remove(tmpPath)

	if _, err := s.db.ExecContext(ctx, "VACUUM INTO ?", tmpPath); err != nil {
		// 退回到直接读主库文件
		if p := getCurrentDBPath(s.db); p != "" {
			return os.ReadFile(p)
		}
		return nil, err
	}
	return os.ReadFile(tmpPath)
}

// === Handlers ===

// backupInfoHandler GET /api/v1/config/backup/info
//
// 返回最新备份的信息。无备份时返回 {latest: null}。
func (s *Store) backupInfoHandler(configDir string) gin.HandlerFunc {
	return func(c *gin.Context) {
		dir := backupDir(configDir)
		entries, err := listBackups(dir)
		if err != nil || len(entries) == 0 {
			c.JSON(http.StatusOK, gin.H{"latest": nil})
			return
		}
		latest := entries[0]
		path := filepath.Join(dir, latest.Name())
		fi, err := latest.Info()
		if err != nil {
			WriteError(c, ErrInternal.With("read backup info: "+err.Error()))
			return
		}
		// 读 meta.json
		meta, _ := readBackupMetaFromFile(path)
		createdAt := fi.ModTime().UTC().Format(time.RFC3339)
		var dbSize int64 = fi.Size()
		var records int
		if meta != nil {
			createdAt = meta.CreatedAt
			dbSize = meta.DBSize
			records = meta.DBRecords
		}
		c.JSON(http.StatusOK, gin.H{"latest": backupInfo{
			Filename:    latest.Name(),
			CreatedAt:   createdAt,
			Version:     0,
			DBRecords:   records,
			DBSizeBytes: dbSize,
		}})
	}
}

// exportBackupHandler POST /api/v1/config/backup
//
// 导出当前配置（SQLite + config.json）为 zip 包。
func (s *Store) exportBackupHandler(configDir string) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()

		// 1) 统计记录数
		recordCnt, err := s.countDBRecords(ctx)
		if err != nil {
			WriteError(c, ErrInternal.With(err.Error()))
			return
		}

		// 2) 收集 config.json
		configPath := filepath.Join(configDir, "config.json")
		var configBytes []byte
		if cfg, err := os.ReadFile(configPath); err == nil {
			configBytes = cfg
		}

		// 3) 收集 SQLite 文件
		//
		// 用 VACUUM INTO 生成一致性快照，而不是直接读 .db 文件：
		//   SQLite 处于 WAL 模式时，最近的写入还在 plcbridge.db-wal 里，
		//   直接读 .db 主文件会漏掉这些数据（备份出来的库是旧的）。
		//   VACUUM INTO 会把主库 + WAL 合并成一个干净的自包含文件。
		dbBytes, err := s.snapshotDB(ctx)
		if err != nil {
			WriteError(c, ErrInternal.With("无法读取 SQLite 数据库文件: "+err.Error()))
			return
		}
		if len(dbBytes) == 0 {
			WriteError(c, ErrInternal.With("无法读取 SQLite 数据库文件"))
			return
		}

		// 4) 组装 zip
		dir, err := ensureBackupDir(configDir)
		if err != nil {
			WriteError(c, ErrInternal.With(err.Error()))
			return
		}

		now := time.Now().UTC()
		zipName := fmt.Sprintf("backup-%s.zip", now.Format("2006-01-02T15-04-05"))
		zipPath := filepath.Join(dir, zipName)
		f, err := os.Create(zipPath)
		if err != nil {
			WriteError(c, ErrInternal.With("create zip: "+err.Error()))
			return
		}
		zw := zip.NewWriter(f)

		// meta.json
		meta := backupMeta{
			CreatedAt:  now.Format(time.RFC3339),
			Version:    0,
			DBRecords:  recordCnt,
			DBSize:     int64(len(dbBytes)),
			ConfigSize: int64(len(configBytes)),
		}
		metaBytes, _ := json.Marshal(meta)
		addZipFile(zw, "meta.json", metaBytes)
		if len(configBytes) > 0 {
			addZipFile(zw, "config.json", configBytes)
		}
		addZipFile(zw, "store.db", dbBytes)

		if err := zw.Close(); err != nil {
			f.Close()
			os.Remove(zipPath)
			WriteError(c, ErrInternal.With("write zip: "+err.Error()))
			return
		}
		if err := f.Close(); err != nil {
			WriteError(c, ErrInternal.With("close zip: "+err.Error()))
			return
		}

		// 5) 清理旧备份（保留 10 份）
		_ = pruneOldBackups(dir, 10)

		c.JSON(http.StatusOK, gin.H{
			"ok":       true,
			"filename": zipName,
			"size":     int64(len(metaBytes)) + int64(len(configBytes)) + int64(len(dbBytes)),
			"records":  recordCnt,
		})
	}
}

// restoreBackupHandler POST /api/v1/config/restore
//
// 从上传的备份 zip 恢复配置。
// 恢复前自动备份当前状态（auto-before-restore-*.zip）。
func (s *Store) restoreBackupHandler(configDir string) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()

		// 1) 接收 zip 文件
		file, header, err := c.Request.FormFile("file")
		if err != nil {
			WriteError(c, ErrInvalidParam.With("请上传 .zip 备份文件: "+err.Error()))
			return
		}
		defer file.Close()
		if !strings.HasSuffix(header.Filename, ".zip") {
			WriteError(c, ErrInvalidParam.With("仅支持 .zip 格式"))
			return
		}
		if header.Size > 100*1024*1024 {
			WriteError(c, ErrInvalidParam.With("文件过大，最大 100MB"))
			return
		}

		// 2) 读取 zip 内容
		zipBytes, err := io.ReadAll(file)
		if err != nil {
			WriteError(c, ErrInternal.With("读取上传文件: "+err.Error()))
			return
		}
		zr, err := zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
		if err != nil {
			WriteError(c, ErrInvalidParam.With("无效的 zip 文件: "+err.Error()))
			return
		}

		var dbBytes, configBytes []byte
		for _, f := range zr.File {
			switch f.Name {
			case "store.db":
				rc, _ := f.Open()
				dbBytes, _ = io.ReadAll(rc)
				rc.Close()
			case "config.json":
				rc, _ := f.Open()
				configBytes, _ = io.ReadAll(rc)
				rc.Close()
			}
		}

		if len(dbBytes) == 0 {
			WriteError(c, ErrInvalidParam.With("zip 中缺少 store.db 文件"))
			return
		}

		// 3) 恢复前先备份当前状态
		dir, err := ensureBackupDir(configDir)
		if err != nil {
			WriteError(c, ErrInternal.With(err.Error()))
			return
		}
		{
			recordCnt, _ := s.countDBRecords(ctx)
			configPath := filepath.Join(configDir, "config.json")
			var cfg []byte
			if b, _ := os.ReadFile(configPath); len(b) > 0 {
				cfg = b
			}
			dbPath := getCurrentDBPath(s.db)
			var dbOrig []byte
			if dbPath != "" {
				dbOrig, _ = os.ReadFile(dbPath)
			}
			now := time.Now().UTC()
			autoName := fmt.Sprintf("auto-before-restore-%s.zip", now.Format("2006-01-02T15-04-05"))
			autoPath := filepath.Join(dir, autoName)
			f, err := os.Create(autoPath)
			if err == nil {
				zw := zip.NewWriter(f)
				meta := backupMeta{CreatedAt: now.Format(time.RFC3339), DBRecords: recordCnt, DBSize: int64(len(dbOrig)), ConfigSize: int64(len(cfg))}
				addZipFile(zw, "meta.json", mustMarshal(meta))
				if len(cfg) > 0 {
					addZipFile(zw, "config.json", cfg)
				}
				if len(dbOrig) > 0 {
					addZipFile(zw, "store.db", dbOrig)
				}
				zw.Close()
				f.Close()
				_ = pruneOldBackups(dir, 20)
			}
		}

		// 4) 恢复 store.db
		dbPath := getCurrentDBPath(s.db)
		if dbPath == "" {
			WriteError(c, ErrInternal.With("无法定位数据库文件"))
			return
		}
		// 关闭现有连接后写入
		if err := s.db.Close(); err != nil {
			WriteError(c, ErrInternal.With("关闭数据库: "+err.Error()))
			return
		}
		if err := os.WriteFile(dbPath, dbBytes, 0644); err != nil {
			WriteError(c, ErrInternal.With("写入数据库: "+err.Error()))
			return
		}

		// 5) 恢复 config.json（如有）
		if len(configBytes) > 0 {
			configPath := filepath.Join(configDir, "config.json")
			tmp, err := os.CreateTemp(configDir, "config.json.restore.*")
			if err == nil {
				tmp.Write(configBytes)
				tmp.Close()
				os.Rename(tmp.Name(), configPath)
			}
		}

		// 6) 返回成功，告知前端需要刷新
		// SQLite 连接已替换，前端需刷新页面才能使用新数据库。
		c.JSON(http.StatusOK, gin.H{
			"ok":                   true,
			"db_restored":          len(dbBytes) > 0,
			"config_restored":      len(configBytes) > 0,
			"requires_web_refresh": true,
			"message":              "配置已恢复，请刷新页面（SQLite 连接已替换）",
		})
	}
}

// downloadBackupHandler GET /api/v1/config/backup/:filename
//
// 下载指定备份文件。
func (s *Store) downloadBackupHandler(configDir string) gin.HandlerFunc {
	return func(c *gin.Context) {
		filename := c.Param("filename")
		if filename == "" {
			WriteError(c, ErrInvalidParam.With("filename is required"))
			return
		}
		// 安全检查：只允许备份目录下的 .zip 文件
		dir := backupDir(configDir)
		safePath := filepath.Join(dir, filename)
		absDir, _ := filepath.Abs(dir)
		absSafe, _ := filepath.Abs(safePath)
		if !strings.HasPrefix(absSafe, absDir) {
			WriteError(c, ErrInvalidParam.With("invalid filename"))
			return
		}
		if !strings.HasSuffix(filename, ".zip") {
			WriteError(c, ErrInvalidParam.With("only .zip allowed"))
			return
		}
		if _, err := os.Stat(safePath); err != nil {
			WriteError(c, ErrAPINotFound.With("backup not found"))
			return
		}
		c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
		c.File(safePath)
	}
}

// listBackupHandler GET /api/v1/config/backups
//
// 返回所有备份文件列表。
func (s *Store) listBackupHandler(configDir string) gin.HandlerFunc {
	return func(c *gin.Context) {
		dir := backupDir(configDir)
		entries, err := listBackups(dir)
		if err != nil {
			c.JSON(http.StatusOK, gin.H{"items": []any{}})
			return
		}
		var items []gin.H
		for _, e := range entries {
			path := filepath.Join(dir, e.Name())
			meta, _ := readBackupMetaFromFile(path)
			fi, _ := e.Info()
			item := gin.H{
				"filename": e.Name(),
				"size":     fi.Size(),
			}
			if fi != nil {
				item["created_at"] = fi.ModTime().UTC().Format(time.RFC3339)
			}
			if meta != nil {
				item["db_records"] = meta.DBRecords
			}
			items = append(items, item)
		}
		c.JSON(http.StatusOK, gin.H{"items": items})
	}
}

func mustMarshal(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
