// Plan2 主题 C 5.3：service 层校验单元测试。
package web

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	_ "github.com/plcbridge/plcbridge/internal/infra/protocol/plugins" // 注册内置协议（测试必须）
	_ "modernc.org/sqlite"
)

// newMigratedStore 创建含 schema 的内存 SQLite Store。
func newMigratedStore(t *testing.T) *Store {
	t.Helper()
	// 注意：`:memory:` 会让每个连接得到一个独立的库；这里用 shared cache + 唯一名，
	// 保证同一次测试内所有连接看到同一份数据（WithTx / 查询可能用到不同连接）。
	dsn := fmt.Sprintf("file:plcbridge_test_%d?mode=memory&cache=shared", time.Now().UnixNano())
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	// 保持至少一条连接存活，否则 shared cache 的库会被回收
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	s := &Store{db: db}

	// 最小 schema：channel / device / source / target / point / target_points
	schema := `
		CREATE TABLE channels (id INTEGER PRIMARY KEY, channel_biz_id TEXT UNIQUE, role TEXT);
		CREATE TABLE devices (id INTEGER PRIMARY KEY, device_biz_id TEXT UNIQUE, name TEXT, enabled INTEGER, created_at INTEGER, updated_at INTEGER);
		CREATE TABLE sources (id INTEGER PRIMARY KEY, source_biz_id TEXT UNIQUE, device_id INTEGER, channel_biz_id TEXT, collect_enabled INTEGER, write_enabled INTEGER, collect_interval_ms INTEGER, timeout_ms INTEGER, created_at INTEGER, updated_at INTEGER);
		CREATE TABLE points (id INTEGER PRIMARY KEY, point_biz_id TEXT UNIQUE, source_id INTEGER, name TEXT, address TEXT, data_type TEXT, collect_enabled INTEGER, write_enabled INTEGER, string_length INTEGER, created_at INTEGER, updated_at INTEGER);
		CREATE TABLE targets (id INTEGER PRIMARY KEY, target_biz_id TEXT UNIQUE, device_id INTEGER, forward_channel_biz_id TEXT, topic TEXT, format TEXT, created_at INTEGER, updated_at INTEGER);
		CREATE TABLE target_points (target_id INTEGER, point_id INTEGER, PRIMARY KEY (target_id, point_id));
	`
	if _, err := db.Exec(schema); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestEnsureChannelExists(t *testing.T) {
	s := newMigratedStore(t)
	ctx := context.Background()
	_, _ = s.db.ExecContext(ctx, `INSERT INTO channels (channel_biz_id, role) VALUES (?, ?)`, "ch-1", "collect")

	// 存在 → nil
	if err := s.EnsureChannelExists(ctx, "ch-1"); err != nil {
		t.Errorf("存在的 channel 应通过: %v", err)
	}
	// 不存在 → ErrChannelNotFound
	err := s.EnsureChannelExists(ctx, "ch-nope")
	if !errors.Is(err, ErrChannelNotFound) {
		t.Errorf("不存在应返回 ErrChannelNotFound, got %v", err)
	}
}

func TestEnsureChannelRole(t *testing.T) {
	s := newMigratedStore(t)
	ctx := context.Background()
	_, _ = s.db.ExecContext(ctx, `INSERT INTO channels (channel_biz_id, role) VALUES (?, ?)`, "ch-collect", "collect")
	_, _ = s.db.ExecContext(ctx, `INSERT INTO channels (channel_biz_id, role) VALUES (?, ?)`, "ch-fwd", "forward")

	// 角色匹配
	if err := s.EnsureChannelRole(ctx, "ch-collect", "collect"); err != nil {
		t.Errorf("角色匹配应通过: %v", err)
	}
	// 角色不匹配
	err := s.EnsureChannelRole(ctx, "ch-collect", "forward")
	if !errors.Is(err, ErrChannelRoleMismatch) {
		t.Errorf("角色不匹配应返回 ErrChannelRoleMismatch, got %v", err)
	}
	// channel 不存在
	err = s.EnsureChannelRole(ctx, "ch-nope", "collect")
	if !errors.Is(err, ErrChannelNotFound) {
		t.Errorf("channel 不存在应返回 ErrChannelNotFound, got %v", err)
	}
}

func TestEnsureDeviceExists(t *testing.T) {
	s := newMigratedStore(t)
	ctx := context.Background()
	_, _ = s.db.ExecContext(ctx, `INSERT INTO devices (device_biz_id, name) VALUES (?, ?)`, "dev-1", "P")

	if err := s.EnsureDeviceExists(ctx, 1); err != nil {
		t.Errorf("device_id=1 存在, got %v", err)
	}
	err := s.EnsureDeviceExists(ctx, 999)
	if !errors.Is(err, ErrDeviceNotFound) {
		t.Errorf("不存在应返回 ErrDeviceNotFound, got %v", err)
	}
}

func TestValidateSourceForCreate_AllChecksOrder(t *testing.T) {
	s := newMigratedStore(t)
	ctx := context.Background()
	_, _ = s.db.ExecContext(ctx, `INSERT INTO channels (channel_biz_id, role) VALUES (?, ?)`, "ch-x", "forward") // 故意 forward
	_, _ = s.db.ExecContext(ctx, `INSERT INTO devices (device_biz_id, name) VALUES (?, ?)`, "dev-1", "P")

	// 缺 channel → ErrChannelNotFound
	src := &SourceDTO{ChannelBizID: "nonexistent", DeviceID: 1}
	err := s.ValidateSourceForCreate(ctx, src)
	if !errors.Is(err, ErrChannelNotFound) {
		t.Errorf("缺 channel → ErrChannelNotFound, got %v", err)
	}

	// channel 角色不匹配 → ErrChannelRoleMismatch
	src = &SourceDTO{ChannelBizID: "ch-x", DeviceID: 1} // ch-x 是 forward
	err = s.ValidateSourceForCreate(ctx, src)
	if !errors.Is(err, ErrChannelRoleMismatch) {
		t.Errorf("角色错 → ErrChannelRoleMismatch, got %v", err)
	}

	// device 缺 → ErrDeviceNotFound
	_, _ = s.db.ExecContext(ctx, `INSERT INTO channels (channel_biz_id, role) VALUES (?, ?)`, "ch-collect", "collect")
	src = &SourceDTO{ChannelBizID: "ch-collect", DeviceID: 999}
	err = s.ValidateSourceForCreate(ctx, src)
	if !errors.Is(err, ErrDeviceNotFound) {
		t.Errorf("缺 device → ErrDeviceNotFound, got %v", err)
	}

	// 全部通过
	src = &SourceDTO{ChannelBizID: "ch-collect", DeviceID: 1}
	if err := s.ValidateSourceForCreate(ctx, src); err != nil {
		t.Errorf("全部 OK, got %v", err)
	}
}

func TestValidateSourceForUpdate_EmptyChannelSkips(t *testing.T) {
	s := newMigratedStore(t)
	ctx := context.Background()
	// ChannelBizID 空 → 跳过校验
	if err := s.ValidateSourceForUpdate(ctx, &SourceDTO{ChannelBizID: ""}); err != nil {
		t.Errorf("空 ChannelBizID 应跳过校验, got %v", err)
	}
	// 非空但 channel 角色错 → 报错
	_, _ = s.db.ExecContext(ctx, `INSERT INTO channels (channel_biz_id, role) VALUES (?, ?)`, "ch-fwd", "forward")
	err := s.ValidateSourceForUpdate(ctx, &SourceDTO{ChannelBizID: "ch-fwd"})
	if !errors.Is(err, ErrChannelRoleMismatch) {
		t.Errorf("非空 + forward channel 应 ErrChannelRoleMismatch, got %v", err)
	}
}

func TestValidateTargetForCreate(t *testing.T) {
	s := newMigratedStore(t)
	ctx := context.Background()
	_, _ = s.db.ExecContext(ctx, `INSERT INTO channels (channel_biz_id, role) VALUES (?, ?)`, "ch-collect", "collect") // 故意 collect
	_, _ = s.db.ExecContext(ctx, `INSERT INTO channels (channel_biz_id, role) VALUES (?, ?)`, "ch-fwd", "forward")
	_, _ = s.db.ExecContext(ctx, `INSERT INTO devices (device_biz_id, name) VALUES (?, ?)`, "dev-1", "P")

	// 缺 channel
	tgt := &TargetDTO{ForwardChannelBizID: "nope", DeviceID: 1}
	if err := s.ValidateTargetForCreate(ctx, tgt); !errors.Is(err, ErrChannelNotFound) {
		t.Errorf("缺 channel → ErrChannelNotFound, got %v", err)
	}
	// channel 角色不匹配（collect 给了 target）
	tgt = &TargetDTO{ForwardChannelBizID: "ch-collect", DeviceID: 1}
	if err := s.ValidateTargetForCreate(ctx, tgt); !errors.Is(err, ErrChannelRoleMismatch) {
		t.Errorf("角色错 → ErrChannelRoleMismatch, got %v", err)
	}
	// 全通过
	tgt = &TargetDTO{ForwardChannelBizID: "ch-fwd", DeviceID: 1}
	if err := s.ValidateTargetForCreate(ctx, tgt); err != nil {
		t.Errorf("全通过, got %v", err)
	}
}

func TestValidateChannelType_AliasNormalization(t *testing.T) {
	// modbus 是 modbus_tcp 别名 → 应通过 collect 校验
	if err := ValidateChannelType("collect", "modbus"); err != nil {
		t.Errorf("modbus 作为 modbus_tcp 别名应通过 collect 校验: %v", err)
	}
	// modbus 不能作为 forward
	if err := ValidateChannelType("forward", "modbus"); err == nil {
		t.Errorf("modbus 不能作为 forward（modbus 是协议不是 target）")
	}
	// 未知 type
	if err := ValidateChannelType("collect", "made-up"); err == nil {
		t.Errorf("未知 type 应报错")
	}
	// 非法 role
	if err := ValidateChannelType("weird", "s7"); err == nil {
		t.Errorf("非法 role 应报错")
	}
}
