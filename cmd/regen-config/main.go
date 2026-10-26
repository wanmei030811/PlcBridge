// regen-config reads the plcbridge SQLite db and exports its state to config.json.
// Usage:
//
//	regen-config <dbPath> [configDir]
//
// If configDir is omitted, it is derived from dbPath: strips the "data/plcbridge.db"
// suffix, so the db's parent directory is used.
//
// Use this when config.json has been accidentally overwritten with empty/partial state
// but the db still contains the correct configuration.
package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Config struct {
	Version  int        `json:"version"`
	Channels []*Channel `json:"channels"`
	Devices  []*Device  `json:"devices"`
}

type Channel struct {
	ChannelBizID           string          `json:"channel_biz_id"`
	Role                   string          `json:"role"`
	Type                   string          `json:"type"`
	Address                string          `json:"address"`
	Config                 json.RawMessage `json:"config,omitempty"`
	HeartbeatAddress       string          `json:"heartbeat_address,omitempty"`
	HeartbeatMaxIntervalMs int             `json:"heartbeat_max_interval_ms,omitempty"`
	HeartbeatEnabled       bool            `json:"heartbeat_enabled"`
	HeartbeatDebounceMs    int             `json:"heartbeat_debounce_ms,omitempty"`
	QueueSize              int             `json:"queue_size"`
	TimeoutMs              int             `json:"timeout_ms"`
	CreatedAt              int64           `json:"created_at"`
	UpdatedAt              int64           `json:"updated_at"`
}

type Device struct {
	DeviceBizID string    `json:"device_biz_id"`
	Name        string    `json:"name"`
	Enabled     bool      `json:"enabled"`
	CreatedAt   int64     `json:"created_at"`
	UpdatedAt   int64     `json:"updated_at"`
	Sources     []*Source `json:"sources"`
	Targets     []*Target `json:"targets"`
}

type Target struct {
	TargetBizID         string   `json:"target_biz_id"`
	ForwardChannelBizID string   `json:"forward_channel_biz_id"`
	PointBizIDs         []string `json:"point_biz_ids"`
	Topic               string   `json:"topic,omitempty"`
	Format              string   `json:"format,omitempty"`
	CreatedAt           int64    `json:"created_at"`
	UpdatedAt           int64    `json:"updated_at"`
}

type Source struct {
	SourceBizID       string  `json:"source_biz_id"`
	ChannelBizID      string  `json:"channel_biz_id"`
	CollectEnabled    bool    `json:"collect_enabled"`
	WriteEnabled      bool    `json:"write_enabled"`
	CollectIntervalMs int     `json:"collect_interval_ms"`
	TimeoutMs         int     `json:"timeout_ms"`
	CreatedAt         int64   `json:"created_at"`
	UpdatedAt         int64   `json:"updated_at"`
	Points            []Point `json:"points"`
}

type Point struct {
	PointBizID     string `json:"point_biz_id"`
	Name           string `json:"name"`
	Address        string `json:"address"`
	DataType       string `json:"data_type"`
	CollectEnabled bool   `json:"collect_enabled"`
	WriteEnabled   bool   `json:"write_enabled"`
	StringLength   int    `json:"string_length,omitempty"`
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "Usage: regen-config <dbPath> [configDir]")
		os.Exit(1)
	}

	dbPath := os.Args[1]
	var configDir string
	if len(os.Args) >= 3 {
		configDir = os.Args[2]
	} else {
		s := dbPath
		for _, suffix := range []string{"data/plcbridge.db", "data\\plcbridge.db"} {
			s = strings.TrimSuffix(s, suffix)
		}
		configDir = s
	}

	fmt.Printf("db:     %s\n", dbPath)
	fmt.Printf("config: %s\n", configDir)

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		os.Exit(1)
	}
	defer db.Close()

	channels, err := loadChannels(db)
	if err != nil {
		fmt.Fprintln(os.Stderr, "channels:", err)
		os.Exit(1)
	}
	fmt.Printf("channels: %d\n", len(channels))
	for _, c := range channels {
		fmt.Printf("  %s: %s/%s @ %s\n", c.ChannelBizID, c.Type, c.Role, c.Address)
	}

	devices, err := loadDevices(db)
	if err != nil {
		fmt.Fprintln(os.Stderr, "devices:", err)
		os.Exit(1)
	}
	fmt.Printf("devices: %d\n", len(devices))
	for _, d := range devices {
		fmt.Printf("  %s: [%s] enabled=%v\n", d.DeviceBizID, d.Name, d.Enabled)
		for _, s := range d.Sources {
			fmt.Printf("    source: %s (channel=%s collect=%v pts=%d)\n",
				s.SourceBizID, s.ChannelBizID, s.CollectEnabled, len(s.Points))
			for _, p := range s.Points {
				fmt.Printf("      point: %s @ %s type=%s\n", p.PointBizID, p.Address, p.DataType)
			}
		}
	}

	// Build initial config; bump version so plcbridge triggers a respawn.
	cfg := &Config{Version: readCurrentVersion(configDir) + 1, Channels: channels, Devices: devices}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, "marshal:", err)
		os.Exit(1)
	}

	configPath := filepath.Join(configDir, "config.json")
	if err := os.WriteFile(configPath, data, 0644); err != nil {
		fmt.Fprintln(os.Stderr, "write:", err)
		os.Exit(1)
	}
	fmt.Printf("\nWritten: %s (%d bytes, version=%d)\n", configPath, len(data), cfg.Version)

	os.Chtimes(configPath, time.Now(), time.Now())
	fmt.Println("Done. plcbridge will reload config within ~5s.")
}

func readCurrentVersion(dir string) int {
	data, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		return 0
	}
	var probe struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return 0
	}
	return probe.Version
}

func loadChannels(db *sql.DB) ([]*Channel, error) {
	rows, err := db.Query(`SELECT channel_biz_id, role, type, address, config_json,
		heartbeat_address, heartbeat_max_interval_ms, heartbeat_enabled, heartbeat_debounce_ms,
		queue_size, timeout_ms, created_at, updated_at FROM channels ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// normalize type: db may have legacy names that validator doesn't accept
	normalizeType := func(t string) string {
		switch t {
		case "modbus":
			return "modbus_tcp"
		default:
			return t
		}
	}

	var chans []*Channel
	for rows.Next() {
		var c Channel
		var hbAddr, cfgJSON string
		var hbEnabled int
		if err := rows.Scan(&c.ChannelBizID, &c.Role, &c.Type, &c.Address, &cfgJSON,
			&hbAddr, &c.HeartbeatMaxIntervalMs, &hbEnabled, &c.HeartbeatDebounceMs,
			&c.QueueSize, &c.TimeoutMs, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		c.Type = normalizeType(c.Type)
		c.Config = json.RawMessage(cfgJSON)
		c.HeartbeatAddress = hbAddr
		c.HeartbeatEnabled = hbEnabled != 0
		chans = append(chans, &c)
	}
	return chans, nil
}

// loadDevices loads devices + their sources + their points from the db.
func loadDevices(db *sql.DB) ([]*Device, error) {
	// 1. preload points by source_id
	ptRows, err := db.Query(`SELECT point_biz_id, source_id, name, address, data_type,
		collect_enabled, write_enabled, string_length FROM points ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer ptRows.Close()
	pointsBySource := map[int][]Point{}
	for ptRows.Next() {
		var p Point
		var ce, we int
		var sid int
		if err := ptRows.Scan(&p.PointBizID, &sid, &p.Name, &p.Address, &p.DataType,
			&ce, &we, &p.StringLength); err != nil {
			return nil, err
		}
		p.CollectEnabled = ce != 0
		p.WriteEnabled = we != 0
		pointsBySource[sid] = append(pointsBySource[sid], p)
	}

	// 2. preload sources by device_id
	srcRows, err := db.Query(`SELECT id, source_biz_id, device_id, channel_biz_id,
		collect_enabled, write_enabled, collect_interval_ms, timeout_ms,
		created_at, updated_at FROM sources ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer srcRows.Close()
	srcByDev := map[int][]*Source{}
	for srcRows.Next() {
		var s Source
		var ce, we int
		var deviceID, srcID int
		if err := srcRows.Scan(&srcID, &s.SourceBizID, &deviceID, &s.ChannelBizID,
			&ce, &we, &s.CollectIntervalMs, &s.TimeoutMs, &s.CreatedAt, &s.UpdatedAt); err != nil {
			return nil, err
		}
		s.CollectEnabled = ce != 0
		s.WriteEnabled = we != 0
		s.Points = pointsBySource[srcID]
		srcByDev[deviceID] = append(srcByDev[deviceID], &s)
	}

	// 3. load devices and attach sources
	devRows, err := db.Query(`SELECT id, device_biz_id, name, enabled, created_at, updated_at FROM devices ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer devRows.Close()

	// 4. preload targets by device_id (point_biz_ids via target_points table)
	tgtRows, err := db.Query(`SELECT id, target_biz_id, device_id, forward_channel_biz_id, topic, format,
		created_at, updated_at FROM targets ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer tgtRows.Close()
	tgtByDev := map[int][]*Target{}
	tgtIDByBiz := map[string]int{}
	for tgtRows.Next() {
		var t Target
		var deviceID, tgtID int
		if err := tgtRows.Scan(&tgtID, &t.TargetBizID, &deviceID, &t.ForwardChannelBizID,
			&t.Topic, &t.Format, &t.CreatedAt, &t.UpdatedAt); err != nil {
			return nil, err
		}
		tgtIDByBiz[t.TargetBizID] = tgtID
		tgtByDev[deviceID] = append(tgtByDev[deviceID], &t)
	}
	// resolve point_biz_ids per target
	tpRows, err := db.Query(`SELECT tp.target_id, p.point_biz_id FROM target_points tp
		JOIN points p ON p.id = tp.point_id ORDER BY tp.target_id, p.point_biz_id`)
	if err != nil {
		return nil, err
	}
	defer tpRows.Close()
	pointsByTgt := map[int][]string{}
	for tpRows.Next() {
		var tgtID int
		var pb string
		if err := tpRows.Scan(&tgtID, &pb); err != nil {
			return nil, err
		}
		pointsByTgt[tgtID] = append(pointsByTgt[tgtID], pb)
	}
	for devID, tgts := range tgtByDev {
		for _, t := range tgts {
			t.PointBizIDs = pointsByTgt[tgtIDByBiz[t.TargetBizID]]
			if len(t.PointBizIDs) == 0 {
				// 用 "*" 替代空（plcbridge 期望 "[]" 或 '["*"]'）
				t.PointBizIDs = []string{"*"}
			}
		}
		_ = devID
	}

	var devs []*Device
	for devRows.Next() {
		var d Device
		var enabled int
		var id int
		if err := devRows.Scan(&id, &d.DeviceBizID, &d.Name, &enabled, &d.CreatedAt, &d.UpdatedAt); err != nil {
			return nil, err
		}
		d.Enabled = enabled != 0
		d.Sources = srcByDev[id]
		d.Targets = tgtByDev[id]
		devs = append(devs, &d)
	}
	return devs, nil
}
