// settings.go：Web 控制台运行时设置（key-value 存 SQLite）。
//
// 为什么用 SQLite 而不是 config.json / 独立文件：
//   - 这些设置是**运行时行为开关**，不是 plcbridge 的采集配置，
//     不能写进 config.json（那个文件是 plcbridge 监听的触发源，写它会触发热重载）。
//   - 放 SQLite 里跟账号/Token 一样，天然随 web 进程持久化，重启不丢。
//
// 当前已登记的设置项：
//   - auto_export_config（bool，默认 false）：配置变更后是否自动导出 config.json。
package web

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// SettingKey 设置项的键名（强枚举，避免拼写错误导致静默失效）。
type SettingKey string

const (
	// SettingAutoExportConfig 配置变更后自动导出 config.json（触发 plcbridge 热重载）。
	//
	// 默认 false —— 保持"手动导出"的老行为不变，避免用户没准备好时
	// 配置被自动推给运行中的 plcbridge（现场改配置常常是"先改后看"，
	// 自动导出会让正在跑的采集跟着变）。
	SettingAutoExportConfig SettingKey = "auto_export_config"
)

// knownSettings 所有合法的设置键 → 默认值。
//
// 新增设置项时必须在这里登记，否则会被拒绝写入（防止前端传错键静默失效）。
var knownSettings = map[SettingKey]bool{
	SettingAutoExportConfig: false,
}

// ErrUnknownSetting 未登记的设置键。
var ErrUnknownSetting = errors.New("unknown setting key")

// StoreSettings 是所有设置的快照（map 便于扩展）。
type StoreSettings map[SettingKey]bool

// GetSettingBool 读取一个布尔设置；未登记的键返回错误。
func (s *Store) GetSettingBool(ctx context.Context, key SettingKey) (bool, error) {
	def, ok := knownSettings[key]
	if !ok {
		return false, fmt.Errorf("%w: %s", ErrUnknownSetting, key)
	}
	var v string
	err := s.db.QueryRowContext(ctx,
		`SELECT value FROM web_settings WHERE key = ?`, string(key)).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return def, nil // 未设置过 → 用默认值
	}
	if err != nil {
		return false, err
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		// 存了非法值（手工改库？）→ 退回默认值，不让整个设置面板挂掉。
		return def, nil
	}
	return b, nil
}

// SetSettingBool 写入一个布尔设置。
func (s *Store) SetSettingBool(ctx context.Context, key SettingKey, val bool) error {
	if _, ok := knownSettings[key]; !ok {
		return fmt.Errorf("%w: %s", ErrUnknownSetting, key)
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO web_settings (key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		string(key), strconv.FormatBool(val), time.Now().UnixMilli())
	return err
}

// ListSettings 返回所有设置（键 → 值）。
//
// 未显式设置过的键也会返回，用默认值填充 —— 前端一次拿全，不用管哪些设过。
func (s *Store) ListSettings(ctx context.Context) (StoreSettings, error) {
	out := StoreSettings{}
	for k, def := range knownSettings {
		out[k] = def
	}
	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM web_settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		key := SettingKey(k)
		if _, ok := knownSettings[key]; !ok {
			continue // 未知键（老版本留下的）→ 忽略
		}
		b, err := strconv.ParseBool(v)
		if err != nil {
			continue
		}
		out[key] = b
	}
	return out, rows.Err()
}
