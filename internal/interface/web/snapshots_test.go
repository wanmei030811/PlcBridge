// Plan3 3.3：配置变更历史与回滚单元测试。
package web

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	_ "modernc.org/sqlite" // SQLite 驱动
)

// newSnapshotTestStore 创建带完整 schema 的测试 Store。
func newSnapshotTestStore(t *testing.T) *Store {
	t.Helper()
	// 注意：`:memory:` 会让每个连接得到一个独立的库；这里用 shared cache + 唯一名，
	// 保证同一次测试内所有连接看到同一份数据。
	// 这一点对本文件尤其关键：rollback handler 会调用 reloadConfigViaExport，
	// 它用另一条查询路径读库（buildExport），如果拿到的是另一个空库，
	// 导出会报 "no such table: points"，误判为"回滚没导出"。
	dsn := fmt.Sprintf("file:plcbridge_snap_test_%d?mode=memory&cache=shared", time.Now().UnixNano())
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return &Store{db: db}
}

// seedData 插入一条 channel / device / source / point / target。
func seedData(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()
	now := nowMillis()

	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO channels (channel_biz_id, role, type, address, config_json,
		 heartbeat_address, heartbeat_max_interval_ms, heartbeat_enabled,
		 heartbeat_debounce_ms, queue_size, timeout_ms, created_at, updated_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		"ch1", "collect", "modbus_tcp", "127.0.0.1:502", "{}", "",
		5000, 0, 50, 1000, 300, now, now); err != nil {
		t.Fatalf("insert channel: %v", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO devices (device_biz_id, name, enabled, created_at, updated_at)
		 VALUES (?,?,?,?,?)`, "dev1", "设备1", 1, now, now); err != nil {
		t.Fatalf("insert device: %v", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO sources (source_biz_id, device_id, channel_biz_id, collect_enabled,
		 write_enabled, collect_interval_ms, timeout_ms, created_at, updated_at)
		 VALUES (?,?,?,?,?,?,?,?,?)`,
		"src1", 1, "ch1", 1, 0, 1000, 300, now, now); err != nil {
		t.Fatalf("insert source: %v", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO points (point_biz_id, source_id, name, address, data_type,
		 collect_enabled, write_enabled, string_length, created_at, updated_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?)`,
		"pt1", 1, "点位1", "40001", "int16", 1, 0, 0, now, now); err != nil {
		t.Fatalf("insert point: %v", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO targets (target_biz_id, device_id, forward_channel_biz_id, topic, format, created_at, updated_at)
		 VALUES (?,?,?,?,?,?,?)`, "tg1", 1, "ch1", "topic/a", "json", now, now); err != nil {
		t.Fatalf("insert target: %v", err)
	}
	// target_points 关联
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO target_points (target_id, point_id) VALUES (1, 1)`); err != nil {
		t.Fatalf("insert target_points: %v", err)
	}
}

// TestBuildFullConfigJSON 验证快照 JSON 包含所有实体。
func TestBuildFullConfigJSON(t *testing.T) {
	s := newSnapshotTestStore(t)
	seedData(t, s)

	cfgJSON, count, err := s.BuildFullConfigJSON(context.Background())
	if err != nil {
		t.Fatalf("BuildFullConfigJSON: %v", err)
	}
	if count != 5 {
		t.Errorf("count = %d, want 5", count)
	}
	var cfg map[string]any
	if err := json.Unmarshal([]byte(cfgJSON), &cfg); err != nil {
		t.Fatalf("unmarshal snapshot: %v", err)
	}
	for _, ent := range []string{"channels", "devices", "sources", "points", "targets"} {
		arr, ok := cfg[ent].([]any)
		if !ok {
			t.Errorf("entity %s missing or not array", ent)
			continue
		}
		if len(arr) != 1 {
			t.Errorf("entity %s len = %d, want 1", ent, len(arr))
		}
	}
}

// TestCreateAndListSnapshot 验证快照创建与列表。
func TestCreateAndListSnapshot(t *testing.T) {
	s := newSnapshotTestStore(t)
	seedData(t, s)
	ctx := context.Background()

	cfgJSON, count, err := s.BuildFullConfigJSON(ctx)
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.CreateSnapshot(ctx, "测试快照", "tester", cfgJSON, count, false)
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	if id <= 0 {
		t.Errorf("id = %d, want > 0", id)
	}

	rows, err := s.ListSnapshots(ctx)
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("len(rows) = %d, want 1", len(rows))
	}
	if rows[0].Description != "测试快照" {
		t.Errorf("Description = %q, want 测试快照", rows[0].Description)
	}
	if rows[0].CreatedBy != "tester" {
		t.Errorf("CreatedBy = %q, want tester", rows[0].CreatedBy)
	}
}

// TestGetSnapshotNotFound 验证不存在的快照返回 ErrNotFound。
func TestGetSnapshotNotFound(t *testing.T) {
	s := newSnapshotTestStore(t)
	_, err := s.GetSnapshot(context.Background(), 999)
	if err != ErrNotFound {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// TestRollbackToSnapshot 验证回滚能恢复被删除的实体。
func TestRollbackToSnapshot(t *testing.T) {
	s := newSnapshotTestStore(t)
	seedData(t, s)
	ctx := context.Background()

	// 快照
	cfgJSON, count, err := s.BuildFullConfigJSON(ctx)
	if err != nil {
		t.Fatal(err)
	}
	snapID, err := s.CreateSnapshot(ctx, "回滚测试", "tester", cfgJSON, count, false)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := s.GetSnapshot(ctx, snapID)
	if err != nil {
		t.Fatal(err)
	}

	// 删除所有数据
	if _, err := s.db.ExecContext(ctx, `DELETE FROM target_points`); err != nil {
		t.Fatal(err)
	}
	for _, tb := range []string{"points", "sources", "targets", "devices", "channels"} {
		if _, err := s.db.ExecContext(ctx, "DELETE FROM "+tb); err != nil {
			t.Fatal(err)
		}
	}

	// 回滚
	if _, err := s.RollbackToSnapshot(ctx, snapID, snap.ConfigJSON, ""); err != nil {
		t.Fatalf("RollbackToSnapshot: %v", err)
	}

	// 验证恢复
	for _, tb := range []string{"channels", "devices", "sources", "points", "targets"} {
		var n int
		if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+tb).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Errorf("%s count = %d, want 1", tb, n)
		}
	}
	// target_points 也应恢复
	var tp int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM target_points`).Scan(&tp); err != nil {
		t.Fatal(err)
	}
	if tp != 1 {
		t.Errorf("target_points count = %d, want 1", tp)
	}
}

// TestDiffConfigJSON 验证 diff 能检测新增 / 删除 / 修改。
func TestDiffConfigJSON(t *testing.T) {
	old := `{"channels":[{"channel_biz_id":"ch1","address":"1.1.1.1:502"}],
	         "devices":[],"sources":[],"points":[],"targets":[]}`
	new := `{"channels":[{"channel_biz_id":"ch1","address":"2.2.2.2:502"},
	                    {"channel_biz_id":"ch2","address":"3.3.3.3:502"}],
	         "devices":[],"sources":[],"points":[],"targets":[]}`

	diffs := diffConfigJSON(old, new)
	if len(diffs) == 0 {
		t.Fatal("expected diffs, got none")
	}
	var hasModify, hasAdd bool
	for _, d := range diffs {
		if d.Key == "ch1" && d.Field == "address" && d.Change == "modified" {
			hasModify = true
		}
		if d.Key == "ch2" && d.Change == "added" {
			hasAdd = true
		}
	}
	if !hasModify {
		t.Error("expected modified diff for ch1.address")
	}
	if !hasAdd {
		t.Error("expected added diff for ch2")
	}
}

// TestSnapshotPrune 验证最多保留 50 个快照。
func TestSnapshotPrune(t *testing.T) {
	s := newSnapshotTestStore(t)
	ctx := context.Background()
	for i := 0; i < 60; i++ {
		if _, err := s.CreateSnapshot(ctx, "s", "u", "{}", 0, true); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM config_snapshots`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 50 {
		t.Errorf("snapshot count = %d, want 50", n)
	}
}

// TestRollbackHandler_ExportsConfigForHotReload 验证回滚 handler 会导出 config.json。
//
// 回归测试（2026-10-24）：之前 rollbackSnapshotHandler 只改了 SQLite，
// 没有写 configDir/config.json。而 plcbridge 主进程是靠 fsnotify 监听
// config.json 变更来热加载的 → 回滚接口返回 200，但主进程仍跑旧配置，
// 用户以为回滚成功、实际没生效。Plan3 3.3 验收明确要求"plcbridge 立即热加载"。
//
// 这个测试锁定"回滚后 config.json 必须被更新且内容与回滚后的库一致"。
func TestRollbackHandler_ExportsConfigForHotReload(t *testing.T) {
	s := newSnapshotTestStore(t)
	seedData(t, s)
	ctx := context.Background()

	// 建快照
	cfgJSON, count, err := s.BuildFullConfigJSON(ctx)
	if err != nil {
		t.Fatal(err)
	}
	snapID, err := s.CreateSnapshot(ctx, "回滚测试", "tester", cfgJSON, count, false)
	if err != nil {
		t.Fatal(err)
	}

	// 改库（改 channel 地址），让回滚有实际效果
	if _, err := s.db.ExecContext(ctx,
		`UPDATE channels SET address = '9.9.9.9:9999' WHERE channel_biz_id = 'ch1'`); err != nil {
		t.Fatal(err)
	}

	// 挂 handler：需要一个能写 config.json 的 configDir
	cfgDir := t.TempDir()
	r := gin.New()
	r.POST("/api/v1/config/snapshots/:id/rollback", s.rollbackSnapshotHandler(cfgDir))

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/api/v1/config/snapshots/%d/rollback", snapID), nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200. body: %s", w.Code, w.Body.String())
	}

	// 响应应带上 reload=true（表示已触发热加载）
	var resp struct {
		Rollback bool `json:"rollback"`
		Reload   bool `json:"reload"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Rollback {
		t.Errorf("rollback = false, want true")
	}
	if !resp.Reload {
		t.Errorf("reload = false, want true（回滚后应导出 config.json 触发热加载）")
	}

	// 关键断言：config.json 必须存在，且内容反映回滚后的库（地址是回滚前的 127.0.0.1:502）
	cfgPath := filepath.Join(cfgDir, "config.json")
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("回滚后应生成 config.json（触发 plcbridge 热加载）: %v", err)
	}
	if !strings.Contains(string(data), "127.0.0.1:502") {
		t.Errorf("config.json 未反映回滚后的数据:\n%s", string(data))
	}
	if strings.Contains(string(data), "9.9.9.9:9999") {
		t.Errorf("config.json 仍含回滚前的值:\n%s", string(data))
	}
}
