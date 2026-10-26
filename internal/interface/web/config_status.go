// config_status.go：暴露"数据库配置"与"已导出 config.json"之间的差异。
//
// 背景（2026-10-25）：
//
//	用户每次改完配置都要切到仪表盘点「导出配置」，否则 plcbridge 不会热重载。
//	页面来回切很麻烦，所以前端需要一个"随时可见"的状态：
//	  - dirty = true  → 改了配置但还没导出，plcbridge 跑的还是旧配置
//	  - dirty = false → config.json 已是最新
//
// 判定方式（不引入新表，纯派生计算）：
//
//	last_change_ms  = MAX(updated_at) over channels/devices/sources/points/targets
//	last_export_ms  = config.json 的 mtime
//	dirty           = last_change_ms > last_export_ms
//
// 为什么不存"已导出版本号"到 DB：
//
//	config.json 是 fsnotify 热重载的触发源，它的 mtime 天然就是"最后一次导出时间"，
//	再存一份 DB 字段反而会出现两处状态不一致（改了 DB 里的版本号但没真的写文件）。
package web

import (
	"context"
	"database/sql"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/gin-gonic/gin"
)

// configStatusResp 描述"数据库配置"与"已导出 config.json"之间的差异。
type configStatusResp struct {
	OK bool `json:"ok"`
	// ConfigPath config.json 的绝对路径。
	ConfigPath string `json:"config_path"`
	// LastExportMs config.json 最后一次写入的时间（epoch ms）；0 = 从未导出过。
	LastExportMs int64 `json:"last_export_ms"`
	// LastChangeMs 数据库里最后一次配置变更的时间（epoch ms）；0 = 没有任何变更。
	LastChangeMs int64 `json:"last_change_ms"`
	// Dirty true = 数据库比 config.json 新（改了配置还没导出）。
	Dirty bool `json:"dirty"`
	// PendingChanges 受影响的实体行数（channels/devices/sources/points/targets）。
	PendingChanges int `json:"pending_changes"`
}

// lastChangeMs 读取所有配置实体的最大 updated_at。
//
// 用 UNION ALL 把各表的 updated_at 汇到一起再取 MAX：
//
//	单独 SELECT MAX(updated_at) FROM x UNION ALL SELECT MAX(...) 会在某个表为空时
//	混入 NULL 行，用 COALESCE 兜底；这里直接把所有行 union 后再 MAX，语义更直白。
func lastChangeMs(ctx context.Context, db *sql.DB) (int64, error) {
	var v sql.NullInt64
	err := db.QueryRowContext(ctx, `
		SELECT MAX(updated_at) FROM (
			SELECT updated_at FROM channels
			UNION ALL SELECT updated_at FROM devices
			UNION ALL SELECT updated_at FROM sources
			UNION ALL SELECT updated_at FROM points
			UNION ALL SELECT updated_at FROM targets
		)`).Scan(&v)
	if err != nil {
		return 0, err
	}
	if !v.Valid {
		return 0, nil
	}
	return v.Int64, nil
}

// countPendingChanges 统计 updated_at > since 的配置行数。
//
// since <= 0 表示"从未导出过 config.json" → 所有配置行都算待导出，
// 此时统计全量行数（而不是 0），否则用户看到角标为空却实际全部没导出。
func countPendingChanges(ctx context.Context, db *sql.DB, since int64) (int, error) {
	var n int
	if since <= 0 {
		err := db.QueryRowContext(ctx, `
			SELECT (SELECT COUNT(*) FROM channels)
			     + (SELECT COUNT(*) FROM devices)
			     + (SELECT COUNT(*) FROM sources)
			     + (SELECT COUNT(*) FROM points)
			     + (SELECT COUNT(*) FROM targets)`).Scan(&n)
		if err != nil {
			return 0, err
		}
		return n, nil
	}
	err := db.QueryRowContext(ctx, `
		SELECT (SELECT COUNT(*) FROM channels     WHERE updated_at > ?)
		     + (SELECT COUNT(*) FROM devices      WHERE updated_at > ?)
		     + (SELECT COUNT(*) FROM sources      WHERE updated_at > ?)
		     + (SELECT COUNT(*) FROM points       WHERE updated_at > ?)
		     + (SELECT COUNT(*) FROM targets      WHERE updated_at > ?)`,
		since, since, since, since, since).Scan(&n)
	if err != nil {
		return 0, err
	}
	return n, nil
}

// configStatusHandler GET /api/v1/config/status
//
// 返回前端顶部"导出配置"按钮所需的脏状态：
//
//	{
//	  "ok": true,
//	  "config_path": "F:\\...\\config.json",
//	  "last_export_ms": 1761300000000,
//	  "last_change_ms": 1761300100000,
//	  "dirty": true,
//	  "pending_changes": 3
//	}
func configStatusHandler(db *sql.DB, configDir string) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
		defer cancel()

		out := configStatusResp{OK: true, ConfigPath: filepath.Join(configDir, "config.json")}

		// config.json 的 mtime = 最后一次导出的时间。
		if fi, err := os.Stat(out.ConfigPath); err == nil {
			out.LastExportMs = fi.ModTime().UnixMilli()
		}

		changeMs, err := lastChangeMs(ctx, db)
		if err != nil {
			WriteError(c, ErrInternal.With(err.Error()))
			return
		}
		out.LastChangeMs = changeMs
		out.Dirty = changeMs > out.LastExportMs

		n, err := countPendingChanges(ctx, db, out.LastExportMs)
		if err != nil {
			WriteError(c, ErrInternal.With(err.Error()))
			return
		}
		out.PendingChanges = n

		c.JSON(http.StatusOK, out)
	}
}
