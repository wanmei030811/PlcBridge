// snapshot.go：把当前 SQLite 配置导出为 config.json 快照到 configDir。
//
// plcbridge 主进程通过 fsnotify 监听 configDir/config.json 变更并自动 reload（Plan §6.6）。
// 这里每次 H5 点 "保存配置" 时手动 export + 重写文件。
package web

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/plcbridge/plcbridge/internal/infra/logging"
)

// exportedConfig 是导出的 config.json 顶层结构。
type exportedConfig struct {
	Channels   []exportedChannel `json:"channels"`
	Devices    []exportedDevice  `json:"devices"`
	Tokens     []exportedToken   `json:"tokens"`
	Version    int64             `json:"version"`
	ExportedAt int64             `json:"exported_at"`
}

type exportedChannel struct {
	ChannelBizID           string          `json:"channel_biz_id"`
	Role                   string          `json:"role"`
	Type                   string          `json:"type"`
	Address                string          `json:"address"`
	Config                 json.RawMessage `json:"config"`
	HeartbeatAddress       string          `json:"heartbeat_address"`
	HeartbeatMaxIntervalMs int             `json:"heartbeat_max_interval_ms"`
	HeartbeatEnabled       bool            `json:"heartbeat_enabled"`
	HeartbeatDebounceMs    int             `json:"heartbeat_debounce_ms"`
	QueueSize              int             `json:"queue_size"`
	TimeoutMs              int             `json:"timeout_ms"`
}

type exportedDevice struct {
	DeviceBizID string           `json:"device_biz_id"`
	Name        string           `json:"name"`
	Enabled     bool             `json:"enabled"`
	Sources     []exportedSource `json:"sources"`
	Targets     []exportedTarget `json:"targets"`
}

type exportedSource struct {
	SourceBizID       string          `json:"source_biz_id"`
	ChannelBizID      string          `json:"channel_biz_id"`
	CollectEnabled    bool            `json:"collect_enabled"`
	WriteEnabled      bool            `json:"write_enabled"`
	CollectIntervalMs int             `json:"collect_interval_ms"`
	TimeoutMs         int             `json:"timeout_ms"`
	Points            []exportedPoint `json:"points"`
}

type exportedPoint struct {
	PointBizID     string `json:"point_biz_id"`
	Name           string `json:"name"`
	Address        string `json:"address"`
	DataType       string `json:"data_type"`
	CollectEnabled bool   `json:"collect_enabled"`
	WriteEnabled   bool   `json:"write_enabled"`
	StringLength   int    `json:"string_length,omitempty"`
}

type exportedTarget struct {
	TargetBizID         string   `json:"target_biz_id"`
	ForwardChannelBizID string   `json:"forward_channel_biz_id"`
	Topic               string   `json:"topic"`
	Format              string   `json:"format"`
	PointBizIDs         []string `json:"point_biz_ids"`
}

type exportedToken struct {
	Token string `json:"token"`
	Name  string `json:"name"`
}

func exportSnapshot(db *sql.DB, configDir string) gin.HandlerFunc {
	return func(c *gin.Context) {
		// 用独立 ctx，避免 client 断开导致 query 卡死
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		cfg, err := buildExport(ctx, db)
		if err != nil {
			WriteError(c, ErrInternal.With(err.Error()))
			return
		}
		if err := os.MkdirAll(configDir, 0o755); err != nil {
			WriteError(c, ErrInternal.With(err.Error()))
			return
		}
		path := filepath.Join(configDir, "config.json")
		tmp, err := os.CreateTemp(configDir, "config.json.tmp.*")
		if err != nil {
			WriteError(c, ErrInternal.With(err.Error()))
			return
		}
		enc := json.NewEncoder(tmp)
		enc.SetIndent("", "  ")
		if err := enc.Encode(cfg); err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
			WriteError(c, ErrInternal.With(err.Error()))
			return
		}
		if err := tmp.Close(); err != nil {
			os.Remove(tmp.Name())
			WriteError(c, ErrInternal.With(err.Error()))
			return
		}
		if err := os.Rename(tmp.Name(), path); err != nil {
			// 修复:os.Rename 失败时也要清理临时文件（之前 3 个分支都清理就这里漏了）
			_ = os.Remove(tmp.Name())
			WriteError(c, ErrInternal.With(err.Error()))
			return
		}
		logging.Info(logging.EventConfigExport).
			Str("service", "web").
			Str("path", path).
			Int64("version", cfg.Version).
			Msg("config 快照已导出")
		c.JSON(http.StatusOK, gin.H{"ok": true, "path": path, "version": cfg.Version})
	}
}

func buildExport(ctx context.Context, db *sql.DB) (exportedConfig, error) {
	out := exportedConfig{
		Version:    time.Now().UnixMilli(),
		ExportedAt: time.Now().UnixMilli(),
		Channels:   []exportedChannel{},
		Devices:    []exportedDevice{},
		Tokens:     []exportedToken{},
	}

	rows, err := db.QueryContext(ctx, `
		SELECT channel_biz_id, role, type, address, config_json,
		       COALESCE(heartbeat_address,''), heartbeat_max_interval_ms,
		       heartbeat_enabled, heartbeat_debounce_ms, queue_size, timeout_ms
		FROM channels ORDER BY id`)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var ch exportedChannel
		var cfgStr sql.NullString
		if err := rows.Scan(&ch.ChannelBizID, &ch.Role, &ch.Type, &ch.Address, &cfgStr,
			&ch.HeartbeatAddress, &ch.HeartbeatMaxIntervalMs, &ch.HeartbeatEnabled,
			&ch.HeartbeatDebounceMs, &ch.QueueSize, &ch.TimeoutMs); err != nil {
			rows.Close()
			return out, err
		}
		// config_json 在 SQLite 中是 JSON 字符串或空字符串；转 RawMessage 输出为 JSON object
		if cfgStr.Valid && cfgStr.String != "" {
			ch.Config = json.RawMessage(cfgStr.String)
		} else {
			ch.Config = json.RawMessage("{}")
		}
		// 历史遗留：SQLite 存的 "modbus" 应规范化为标准名 "modbus_tcp"
		if ch.Type == "modbus" {
			ch.Type = "modbus_tcp"
		}
		out.Channels = append(out.Channels, ch)
	}
	rows.Close()

	rows, err = db.QueryContext(ctx, `SELECT id, device_biz_id, name, enabled FROM devices ORDER BY id`)
	if err != nil {
		return out, err
	}
	type devRow struct {
		ID          int64
		DeviceBizID string
		Name        string
		Enabled     bool
	}
	var devRows []devRow
	for rows.Next() {
		var d devRow
		if err := rows.Scan(&d.ID, &d.DeviceBizID, &d.Name, &d.Enabled); err != nil {
			rows.Close()
			return out, err
		}
		devRows = append(devRows, d)
	}
	rows.Close()

	for _, dr := range devRows {
		d := exportedDevice{
			DeviceBizID: dr.DeviceBizID,
			Name:        dr.Name,
			Enabled:     dr.Enabled,
			Sources:     []exportedSource{},
			Targets:     []exportedTarget{},
		}
		srows, err := db.QueryContext(ctx, `
			SELECT source_biz_id, channel_biz_id, collect_enabled, write_enabled,
			       collect_interval_ms, COALESCE(timeout_ms,300)
			FROM sources WHERE device_id=? ORDER BY id`, dr.ID)
		if err != nil {
			return out, err
		}
		// 先把 sources 全部读进内存并关闭游标，再逐个查 points。
		//
		// 为什么不能嵌套：在 srows 游标还开着的时候再开 prows，
		// 当连接池只有 1 条连接时会死锁（prows 等连接，连接被 srows 占着）。
		// 生产配置 MaxOpenConns=8 时侥幸能过，但这是真实的连接池饥饿隐患，
		// 且在单连接测试里必然复现。统一改成"读完即关"。
		type srcRow struct {
			SourceBizID       string
			ChannelBizID      string
			CollectEnabled    bool
			WriteEnabled      bool
			CollectIntervalMs int
			TimeoutMs         int
		}
		var srcRows []srcRow
		for srows.Next() {
			var sr srcRow
			if err := srows.Scan(&sr.SourceBizID, &sr.ChannelBizID, &sr.CollectEnabled, &sr.WriteEnabled,
				&sr.CollectIntervalMs, &sr.TimeoutMs); err != nil {
				srows.Close()
				return out, err
			}
			srcRows = append(srcRows, sr)
		}
		srows.Close()

		for _, sr := range srcRows {
			s := exportedSource{
				SourceBizID:       sr.SourceBizID,
				ChannelBizID:      sr.ChannelBizID,
				CollectEnabled:    sr.CollectEnabled,
				WriteEnabled:      sr.WriteEnabled,
				CollectIntervalMs: sr.CollectIntervalMs,
				TimeoutMs:         sr.TimeoutMs,
				Points:            []exportedPoint{},
			}
			prows, err := db.QueryContext(ctx, `
				SELECT point_biz_id, name, address, data_type, collect_enabled, write_enabled,
				       COALESCE(string_length,0)
				FROM points WHERE source_id=(SELECT id FROM sources WHERE source_biz_id=?) ORDER BY id`,
				sr.SourceBizID)
			if err != nil {
				return out, err
			}
			for prows.Next() {
				var p exportedPoint
				if err := prows.Scan(&p.PointBizID, &p.Name, &p.Address, &p.DataType, &p.CollectEnabled, &p.WriteEnabled, &p.StringLength); err != nil {
					prows.Close()
					return out, err
				}
				s.Points = append(s.Points, p)
			}
			prows.Close()
			d.Sources = append(d.Sources, s)
		}

		trows, err := db.QueryContext(ctx, `
			SELECT target_biz_id, forward_channel_biz_id, COALESCE(topic,''),
			       COALESCE(format,'json') FROM targets WHERE device_id=? ORDER BY id`, dr.ID)
		if err != nil {
			return out, err
		}
		// 同 sources：先读完并关闭游标，再查 target_points，避免嵌套游标占死连接。
		type tgtRow struct {
			TargetBizID         string
			ForwardChannelBizID string
			Topic               string
			Format              string
		}
		var tgtRows []tgtRow
		for trows.Next() {
			var tr tgtRow
			if err := trows.Scan(&tr.TargetBizID, &tr.ForwardChannelBizID, &tr.Topic, &tr.Format); err != nil {
				trows.Close()
				return out, err
			}
			tgtRows = append(tgtRows, tr)
		}
		trows.Close()

		for _, tr := range tgtRows {
			t := exportedTarget{
				TargetBizID:         tr.TargetBizID,
				ForwardChannelBizID: tr.ForwardChannelBizID,
				Topic:               tr.Topic,
				Format:              tr.Format,
				PointBizIDs:         []string{},
			}
			pt, err1 := loadTargetPointBizIDs(ctx, db, tr.TargetBizID)
			if err1 != nil {
				return out, err1
			}
			t.PointBizIDs = pt
			d.Targets = append(d.Targets, t)
		}
		out.Devices = append(out.Devices, d)
	}

	trows, err := db.QueryContext(ctx, `SELECT token, name FROM tokens WHERE revoked_at IS NULL ORDER BY id`)
	if err != nil {
		return out, err
	}
	for trows.Next() {
		var t exportedToken
		if err := trows.Scan(&t.Token, &t.Name); err != nil {
			trows.Close()
			return out, err
		}
		out.Tokens = append(out.Tokens, t)
	}
	trows.Close()

	return out, nil
}

func loadTargetPointBizIDs(ctx context.Context, db *sql.DB, targetBizID string) ([]string, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT p.point_biz_id FROM target_points tp
		JOIN points p ON p.id=tp.point_id
		WHERE tp.target_id=(SELECT id FROM targets WHERE target_biz_id=?)
		ORDER BY p.point_biz_id`, targetBizID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var b string
		if err := rows.Scan(&b); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}
