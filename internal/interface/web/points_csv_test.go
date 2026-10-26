// Plan3 §3.1 / §3.7：CSV 导入导出 + 设备克隆的单元测试。
package web

import (
	"context"
	"encoding/csv"
	"errors"
	"strings"
	"testing"
	"time"

	_ "github.com/plcbridge/plcbridge/internal/infra/protocol/plugins"
	_ "github.com/plcbridge/plcbridge/internal/infra/target/plugins"
)

// setupCloneFixture 造一个含 channel/device/source/point/target 的最小数据集。
func setupCloneFixture(t *testing.T, s *Store) (deviceID int64, pointID int64) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UnixMilli()
	_, _ = s.db.ExecContext(ctx, `INSERT INTO channels (channel_biz_id, role) VALUES (?, ?)`, "ch-collect", "collect")
	_, _ = s.db.ExecContext(ctx, `INSERT INTO channels (channel_biz_id, role) VALUES (?, ?)`, "ch-fwd", "forward")
	res, _ := s.db.ExecContext(ctx, `INSERT INTO devices (device_biz_id, name, enabled, created_at, updated_at) VALUES (?,?,?,?,?)`, "dev-1", "设备一", 1, now, now)
	deviceID, _ = res.LastInsertId()
	res, _ = s.db.ExecContext(ctx, `INSERT INTO sources (source_biz_id, device_id, channel_biz_id, collect_enabled, write_enabled, collect_interval_ms, timeout_ms, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?)`,
		"src-1", deviceID, "ch-collect", 1, 1, 1000, 300, now, now)
	srcID, _ := res.LastInsertId()
	res, _ = s.db.ExecContext(ctx, `INSERT INTO points (point_biz_id, source_id, name, address, data_type, collect_enabled, write_enabled, string_length, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?)`,
		"p-1", srcID, "温度", "DB1.DBD0", "float32", 1, 1, 0, now, now)
	pointID, _ = res.LastInsertId()
	res, _ = s.db.ExecContext(ctx, `INSERT INTO targets (target_biz_id, device_id, forward_channel_biz_id, topic, format, created_at, updated_at) VALUES (?,?,?,?,?,?,?)`,
		"t-1", deviceID, "ch-fwd", "a/b", "json", now, now)
	tgtID, _ := res.LastInsertId()
	_, _ = s.db.ExecContext(ctx, `INSERT INTO target_points (target_id, point_id) VALUES (?,?)`, tgtID, pointID)
	return deviceID, pointID
}

// === CSV 解析 ===

func TestParsePointCSV_Valid(t *testing.T) {
	csvText := `point_biz_id,name,address,data_type,collect_enabled,write_enabled,string_length
p_001,温度,DB1.DBD0,float32,true,false,0
p_002,压力,DB1.DBD4,int16,1,0,0
`
	rows, errs := parsePointCSV(csvText)
	if len(errs) != 0 {
		t.Fatalf("不应有解析错误, got %v", errs)
	}
	if len(rows) != 2 {
		t.Fatalf("应解析出 2 行, got %d", len(rows))
	}
	if rows[0].PointBizID != "p_001" || rows[0].Address != "DB1.DBD0" || rows[0].DataType != "float32" {
		t.Errorf("第 1 行解析错误: %+v", rows[0])
	}
	if !rows[0].CollectEnabled {
		t.Errorf("collect_enabled=true 应为 true")
	}
	if rows[1].CollectEnabled != true {
		t.Errorf("collect_enabled=1 应为 true")
	}
}

func TestParsePointCSV_MissingRequiredColumn(t *testing.T) {
	csvText := `point_biz_id,name,data_type
p_001,温度,float32
`
	_, errs := parsePointCSV(csvText)
	if len(errs) == 0 {
		t.Fatalf("缺 address 列应报错")
	}
}

func TestParsePointCSV_InvalidDataType(t *testing.T) {
	csvText := `point_biz_id,address,data_type
p_001,DB1.DBD0,float999
`
	_, errs := parsePointCSV(csvText)
	if len(errs) != 1 {
		t.Fatalf("非法 data_type 应报 1 个错, got %d", len(errs))
	}
	if errs[0].Field != "data_type" {
		t.Errorf("错误字段应为 data_type, got %s", errs[0].Field)
	}
}

func TestParsePointCSV_StringLengthRange(t *testing.T) {
	csvText := `point_biz_id,address,data_type,string_length
p_001,DB1.DBD0,string,0
`
	_, errs := parsePointCSV(csvText)
	if len(errs) != 1 || errs[0].Field != "string_length" {
		t.Errorf("string 类型 string_length=0 应报错, got %v", errs)
	}
}

func TestParsePointCSV_NonStringResetsStringLength(t *testing.T) {
	csvText := `point_biz_id,address,data_type,string_length
p_001,DB1.DBD0,int16,20
`
	rows, errs := parsePointCSV(csvText)
	if len(errs) != 0 {
		t.Fatalf("不应有错: %v", errs)
	}
	if rows[0].StringLength != 0 {
		t.Errorf("非 string 类型应把 string_length 归零, got %d", rows[0].StringLength)
	}
}

// === CSV 导入 ===

func TestImportPointRows_Insert(t *testing.T) {
	s := newMigratedStore(t)
	ctx := context.Background()
	deviceID, _ := setupCloneFixture(t, s)
	srcs, err := s.ListDeviceSources(ctx, deviceID)
	if err != nil {
		t.Fatalf("ListDeviceSources err: %v", err)
	}
	if len(srcs) == 0 {
		var cnt int
		_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sources`).Scan(&cnt)
		t.Fatalf("未找到 source (sources 表 %d 行, deviceID=%d)", cnt, deviceID)
	}
	sourceID := srcs[0].ID

	rows := []csvPointRow{
		{PointBizID: "new-1", Name: "n1", Address: "DB2.DBW0", DataType: "int16", CollectEnabled: true},
		{PointBizID: "new-2", Name: "n2", Address: "DB2.DBW2", DataType: "int16", CollectEnabled: true},
	}
	imported, failed, skipped, errs := s.importPointRows(ctx, sourceID, rows, false)
	if failed != 0 || len(errs) != 0 {
		t.Fatalf("导入应成功: failed=%d errs=%v", failed, errs)
	}
	if imported != 2 {
		t.Errorf("应导入 2 条, got %d", imported)
	}
	if skipped != 0 {
		t.Errorf("skipped 应为 0, got %d", skipped)
	}
	var cnt int
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM points WHERE point_biz_id LIKE 'new-%'`).Scan(&cnt)
	if cnt != 2 {
		t.Errorf("DB 应有 2 条新点位, got %d", cnt)
	}
}

func TestImportPointRows_IdempotentSkip(t *testing.T) {
	s := newMigratedStore(t)
	ctx := context.Background()
	deviceID, _ := setupCloneFixture(t, s)
	srcs, _ := s.ListDeviceSources(ctx, deviceID)
	sourceID := srcs[0].ID

	rows := []csvPointRow{{PointBizID: "p-1", Address: "DB1.DBD0", DataType: "float32"}}
	imported, failed, skipped, _ := s.importPointRows(ctx, sourceID, rows, false)
	if imported != 0 || failed != 0 || skipped != 1 {
		t.Errorf("重复点位应被跳过: imported=%d failed=%d skipped=%d", imported, failed, skipped)
	}
}

func TestImportPointRows_SourceWriteDisabledForcesFalse(t *testing.T) {
	s := newMigratedStore(t)
	ctx := context.Background()
	deviceID, _ := setupCloneFixture(t, s)
	srcs, _ := s.ListDeviceSources(ctx, deviceID)
	sourceID := srcs[0].ID

	rows := []csvPointRow{{PointBizID: "nw-1", Address: "DB1.DBD0", DataType: "int16", WriteEnabled: true}}
	_, _, _, _ = s.importPointRows(ctx, sourceID, rows, false) // source 禁止回写

	var we bool
	_ = s.db.QueryRowContext(ctx, `SELECT write_enabled FROM points WHERE point_biz_id='nw-1'`).Scan(&we)
	if we {
		t.Errorf("source 未开启回写时点位 write_enabled 应为 false")
	}
}

// === CSV 导出 ===

func TestExportPoints_BySource(t *testing.T) {
	s := newMigratedStore(t)
	ctx := context.Background()
	deviceID, _ := setupCloneFixture(t, s)
	_ = deviceID

	rows, err := s.queryExportPoints(ctx, "src-1", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].PointBizID != "p-1" {
		t.Fatalf("应导出 1 条点位 p-1, got %+v", rows)
	}
}

func TestExportPoints_ByDevice(t *testing.T) {
	s := newMigratedStore(t)
	ctx := context.Background()
	setupCloneFixture(t, s)

	rows, err := s.queryExportPoints(ctx, "", "dev-1", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("应导出 1 条, got %d", len(rows))
	}
}

func TestExportCSV_HeaderAndRows(t *testing.T) {
	s := newMigratedStore(t)
	ctx := context.Background()
	setupCloneFixture(t, s)

	rows, _ := s.queryExportPoints(ctx, "src-1", "", "")
	var buf strings.Builder
	w := csv.NewWriter(&buf)
	_ = w.Write(csvPointHeader)
	for _, p := range rows {
		_ = w.Write([]string{p.PointBizID, p.Name, p.Address, p.DataType, "true", "false", "0"})
	}
	w.Flush()

	recs, err := csv.NewReader(strings.NewReader(buf.String())).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 {
		t.Fatalf("应有表头 + 1 行数据, got %d", len(recs))
	}
	if recs[0][0] != "point_biz_id" {
		t.Errorf("表头首列应为 point_biz_id, got %s", recs[0][0])
	}
}

// === 设备克隆 ===

func TestCloneDevice_CopiesEverything(t *testing.T) {
	s := newMigratedStore(t)
	ctx := context.Background()
	deviceID, _ := setupCloneFixture(t, s)

	res, err := s.cloneDevice(ctx, deviceID, "dev-2", "设备二", "c_")
	if err != nil {
		t.Fatalf("克隆失败: %v", err)
	}
	if res.Sources != 1 || res.Points != 1 || res.Targets != 1 {
		t.Errorf("克隆数量不对: %+v", res)
	}

	// 新的 device 存在
	var cnt int
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM devices WHERE device_biz_id='dev-2'`).Scan(&cnt)
	if cnt != 1 {
		t.Errorf("新设备未创建")
	}
	// source / point / target biz_id 已加前缀
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sources WHERE source_biz_id='c_src-1'`).Scan(&cnt)
	if cnt != 1 {
		t.Errorf("新 source 未创建（前缀未生效）")
	}
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM points WHERE point_biz_id='c_p-1'`).Scan(&cnt)
	if cnt != 1 {
		t.Errorf("新 point 未创建（前缀未生效）")
	}
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM targets WHERE target_biz_id='c_t-1'`).Scan(&cnt)
	if cnt != 1 {
		t.Errorf("新 target 未创建（前缀未生效）")
	}
}

func TestCloneDevice_TargetPointLinkRemapped(t *testing.T) {
	s := newMigratedStore(t)
	ctx := context.Background()
	deviceID, _ := setupCloneFixture(t, s)

	if _, err := s.cloneDevice(ctx, deviceID, "dev-2", "设备二", "c_"); err != nil {
		t.Fatal(err)
	}
	// 新 target 应关联到新 point（c_p-1），而不是旧 point
	var cnt int
	_ = s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM target_points tp
		JOIN targets t ON t.id = tp.target_id
		JOIN points p ON p.id = tp.point_id
		WHERE t.target_biz_id='c_t-1' AND p.point_biz_id='c_p-1'`).Scan(&cnt)
	if cnt != 1 {
		t.Errorf("新 target 应关联到新 point, got %d", cnt)
	}
}

func TestCloneDevice_ConflictRollsBack(t *testing.T) {
	s := newMigratedStore(t)
	ctx := context.Background()
	deviceID, _ := setupCloneFixture(t, s)

	// 目标 device_biz_id 已存在 → 冲突
	_, _ = s.db.ExecContext(ctx, `INSERT INTO devices (device_biz_id, name, enabled) VALUES (?,?,?)`, "dev-2", "已存在", 1)

	_, err := s.cloneDevice(ctx, deviceID, "dev-2", "设备二", "c_")
	if err == nil {
		t.Fatalf("biz_id 冲突应报错")
	}
	if !errors.Is(err, ErrConflict) {
		t.Errorf("应返回 ErrConflict, got %v", err)
	}
	// 回滚：不应留下任何 clone_ 前缀的 source
	var cnt int
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sources WHERE source_biz_id LIKE 'c_%'`).Scan(&cnt)
	if cnt != 0 {
		t.Errorf("回滚后不应残留克隆数据, got %d", cnt)
	}
}

func TestCloneDevice_DefaultPrefix(t *testing.T) {
	s := newMigratedStore(t)
	ctx := context.Background()
	deviceID, _ := setupCloneFixture(t, s)

	res, err := s.cloneDevice(ctx, deviceID, "dev-2", "设备二", defaultClonePrefix)
	if err != nil {
		t.Fatal(err)
	}
	if res.PrefixApplied != "clone_" {
		t.Errorf("默认前缀应为 clone_, got %s", res.PrefixApplied)
	}
}
