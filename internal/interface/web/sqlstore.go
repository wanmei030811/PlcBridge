package web

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/plcbridge/plcbridge/internal/infra/timefmt"
)

// ErrNotFound 资源不存在。
var ErrNotFound = errors.New("resource not found")

// ErrConflict 唯一约束冲突（重复 biz_id 等）。
var ErrConflict = errors.New("resource conflict")

// Store 封装所有 CRUD 数据访问。
type Store struct {
	db *sql.DB
}

// NewStore 创建 Store。
func NewStore(db *sql.DB) *Store { return &Store{db: db} }

// DB 返回底层 *sql.DB（用于 handler 直接查询）。
func (s *Store) DB() *sql.DB { return s.db }

// nowMillis 返回当前时间（epoch 毫秒，SQLite 统一时间单位）。
func nowMillis() int64 {
	now := msNow()
	return now
}

// msNow：把 time 包调用抽出来（避免和 model.go 时间处理搅在一起）。
func msNow() int64 {
	return timeNowMs()
}

// === Channels ===

func (s *Store) ListChannels(ctx context.Context) ([]ChannelDTO, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, channel_biz_id, role, type, address, config_json,
		       COALESCE(heartbeat_address,''), heartbeat_max_interval_ms,
		       heartbeat_enabled, heartbeat_debounce_ms, queue_size, timeout_ms,
		       created_at, updated_at
		FROM channels ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ChannelDTO{}
	for rows.Next() {
		var c ChannelDTO
		if err := rows.Scan(&c.ID, &c.ChannelBizID, &c.Role, &c.Type, &c.Address, &c.Config,
			&c.HeartbeatAddress, &c.HeartbeatMaxIntervalMs, &c.HeartbeatEnabled,
			&c.HeartbeatDebounceMs, &c.QueueSize, &c.TimeoutMs,
			&c.CreatedAtMs, &c.UpdatedAtMs); err != nil {
			return nil, err
		}
		applyTimes(&c)
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) GetChannel(ctx context.Context, id int64) (*ChannelDTO, error) {
	var c ChannelDTO
	err := s.db.QueryRowContext(ctx, `
		SELECT id, channel_biz_id, role, type, address, config_json,
		       COALESCE(heartbeat_address,''), heartbeat_max_interval_ms,
		       heartbeat_enabled, heartbeat_debounce_ms, queue_size, timeout_ms,
		       created_at, updated_at
		FROM channels WHERE id = ?`, id).Scan(
		&c.ID, &c.ChannelBizID, &c.Role, &c.Type, &c.Address, &c.Config,
		&c.HeartbeatAddress, &c.HeartbeatMaxIntervalMs, &c.HeartbeatEnabled,
		&c.HeartbeatDebounceMs, &c.QueueSize, &c.TimeoutMs,
		&c.CreatedAtMs, &c.UpdatedAtMs,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	applyTimes(&c)
	return &c, nil
}

func (s *Store) CreateChannel(ctx context.Context, c *ChannelDTO) (int64, error) {
	now := nowMillis()
	if c.HeartbeatMaxIntervalMs == 0 {
		c.HeartbeatMaxIntervalMs = 5000
	}
	if c.HeartbeatDebounceMs == 0 {
		c.HeartbeatDebounceMs = 50
	}
	if c.QueueSize == 0 {
		c.QueueSize = 10000
	}
	if c.TimeoutMs == 0 {
		c.TimeoutMs = 300
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO channels
		  (channel_biz_id, role, type, address, config_json,
		   heartbeat_address, heartbeat_max_interval_ms, heartbeat_enabled,
		   heartbeat_debounce_ms, queue_size, timeout_ms,
		   created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ChannelBizID, c.Role, c.Type, c.Address, c.Config,
		c.HeartbeatAddress, c.HeartbeatMaxIntervalMs, c.HeartbeatEnabled,
		c.HeartbeatDebounceMs, c.QueueSize, c.TimeoutMs, now, now,
	)
	if err != nil {
		if isUniqueConstraintErr(err) {
			return 0, fmt.Errorf("%w: channel_biz_id=%s", ErrConflict, c.ChannelBizID)
		}
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) UpdateChannel(ctx context.Context, id int64, c *ChannelDTO) error {
	now := nowMillis()
	res, err := s.db.ExecContext(ctx, `
		UPDATE channels SET
		  address=?, config_json=?, heartbeat_address=?,
		  heartbeat_max_interval_ms=?, heartbeat_enabled=?,
		  heartbeat_debounce_ms=?, queue_size=?, timeout_ms=?,
		  updated_at=?
		WHERE id=?`,
		c.Address, c.Config, c.HeartbeatAddress,
		c.HeartbeatMaxIntervalMs, c.HeartbeatEnabled,
		c.HeartbeatDebounceMs, c.QueueSize, c.TimeoutMs, now, id,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) DeleteChannel(ctx context.Context, id int64) error {
	// 先查 channel_biz_id（避免 FK 漏报时仍删；channel 没 FK 引用 sources/targets 是文本）
	var bizID string
	if err := s.db.QueryRowContext(ctx, `SELECT channel_biz_id FROM channels WHERE id=?`, id).Scan(&bizID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	// 检查 source / target 是否有引用（sources.channel_biz_id + targets.forward_channel_biz_id 都是 TEXT）
	var refCount int
	if err := s.db.QueryRowContext(ctx,
		`SELECT (SELECT COUNT(*) FROM sources WHERE channel_biz_id=?)+(SELECT COUNT(*) FROM targets WHERE forward_channel_biz_id=?)`,
		bizID, bizID).Scan(&refCount); err != nil {
		return err
	}
	if refCount > 0 {
		return fmt.Errorf("%w: channel 仍被 %d 条 source/target 引用", ErrConflict, refCount)
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM channels WHERE id=?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// === Devices ===

func (s *Store) ListDevices(ctx context.Context) ([]DeviceDTO, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, device_biz_id, name, enabled, created_at, updated_at FROM devices ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DeviceDTO{}
	for rows.Next() {
		var d DeviceDTO
		if err := rows.Scan(&d.ID, &d.DeviceBizID, &d.Name, &d.Enabled,
			&d.CreatedAtMs, &d.UpdatedAtMs); err != nil {
			return nil, err
		}
		applyTimesD(&d)
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Store) GetDevice(ctx context.Context, id int64) (*DeviceDTO, error) {
	var d DeviceDTO
	err := s.db.QueryRowContext(ctx,
		`SELECT id, device_biz_id, name, enabled, created_at, updated_at FROM devices WHERE id=?`,
		id).Scan(&d.ID, &d.DeviceBizID, &d.Name, &d.Enabled,
		&d.CreatedAtMs, &d.UpdatedAtMs)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	applyTimesD(&d)
	srcs, err := s.ListDeviceSources(ctx, id)
	if err != nil {
		return nil, err
	}
	// 嵌套填充 points：前端 DeviceDetail 页一次拿到完整树，避免
	// refresh() 时序窗口（device 整体替换但 points 还在串行加载）造成的
	// “添加 point 后消失 / 刷新一次才显示” 现象。
	if len(srcs) > 0 {
		srcIDs := make([]int64, 0, len(srcs))
		for _, src := range srcs {
			srcIDs = append(srcIDs, src.ID)
		}
		ptsBySrc, err := s.listPointsBySourceIDs(ctx, srcIDs)
		if err != nil {
			return nil, err
		}
		for i := range srcs {
			srcs[i].Points = ptsBySrc[srcs[i].ID]
			if srcs[i].Points == nil {
				srcs[i].Points = []PointDTO{}
			}
		}
	}
	d.Sources = srcs
	tgts, err := s.listDeviceTargets(ctx, id)
	if err != nil {
		return nil, err
	}
	d.Targets = tgts
	return &d, nil
}

func (s *Store) ListDeviceSources(ctx context.Context, deviceID int64) ([]SourceDTO, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT src.id, src.source_biz_id, src.device_id, d.device_biz_id, src.channel_biz_id,
		       src.collect_enabled, src.write_enabled, src.collect_interval_ms, COALESCE(src.timeout_ms,300),
		       src.created_at, src.updated_at
		FROM sources src JOIN devices d ON d.id = src.device_id
		WHERE src.device_id=? ORDER BY src.id`, deviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SourceDTO{}
	for rows.Next() {
		var src SourceDTO
		if err := rows.Scan(&src.ID, &src.SourceBizID, &src.DeviceID, &src.DeviceBizID, &src.ChannelBizID,
			&src.CollectEnabled, &src.WriteEnabled, &src.CollectIntervalMs, &src.TimeoutMs,
			&src.CreatedAtMs, &src.UpdatedAtMs); err != nil {
			return nil, err
		}
		applyTimesS(&src)
		out = append(out, src)
	}
	return out, rows.Err()
}

func (s *Store) listDeviceTargets(ctx context.Context, deviceID int64) ([]TargetDTO, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, target_biz_id, device_id, forward_channel_biz_id, COALESCE(topic,''), format,
		        created_at, updated_at FROM targets WHERE device_id=? ORDER BY id`, deviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TargetDTO{}
	tgtIDs := []int64{}
	for rows.Next() {
		var t TargetDTO
		if err := rows.Scan(&t.ID, &t.TargetBizID, &t.DeviceID, &t.ForwardChannelBizID,
			&t.Topic, &t.Format, &t.CreatedAtMs, &t.UpdatedAtMs); err != nil {
			return nil, err
		}
		applyTimesT(&t)
		out = append(out, t)
		tgtIDs = append(tgtIDs, t.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return out, nil
	}
	ptsByTgt, err := s.listTargetPointBizIDsBatch(ctx, tgtIDs)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].PointBizIDs = ptsByTgt[out[i].ID]
	}
	return out, nil
}

func (s *Store) listTargetPointBizIDsBatch(ctx context.Context, targetIDs []int64) (map[int64][]string, error) {
	if len(targetIDs) == 0 {
		return map[int64][]string{}, nil
	}
	placeholders := make([]string, len(targetIDs))
	args := make([]any, len(targetIDs))
	for i, id := range targetIDs {
		placeholders[i] = "?"
		args[i] = id
	}
	q := `SELECT tp.target_id, p.point_biz_id
	      FROM target_points tp JOIN points p ON p.id=tp.point_id
	      WHERE tp.target_id IN (` + strings.Join(placeholders, ",") + `)
	      ORDER BY p.point_biz_id`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64][]string{}
	for rows.Next() {
		var tid int64
		var biz string
		if err := rows.Scan(&tid, &biz); err != nil {
			return nil, err
		}
		out[tid] = append(out[tid], biz)
	}
	return out, rows.Err()
}

func (s *Store) CreateDevice(ctx context.Context, d *DeviceDTO) (int64, error) {
	now := nowMillis()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO devices (device_biz_id, name, enabled, created_at, updated_at) VALUES (?,?,?,?,?)`,
		d.DeviceBizID, d.Name, d.Enabled, now, now)
	if err != nil {
		if isUniqueConstraintErr(err) {
			return 0, fmt.Errorf("%w: device_biz_id=%s", ErrConflict, d.DeviceBizID)
		}
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) UpdateDevice(ctx context.Context, id int64, d *DeviceDTO) error {
	now := nowMillis()
	res, err := s.db.ExecContext(ctx,
		`UPDATE devices SET device_biz_id=?, name=?, enabled=?, updated_at=? WHERE id=?`,
		d.DeviceBizID, d.Name, d.Enabled, now, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) DeleteDevice(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM devices WHERE id=?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// === Sources ===

func (s *Store) GetSource(ctx context.Context, id int64) (*SourceDTO, error) {
	var src SourceDTO
	err := s.db.QueryRowContext(ctx, `
		SELECT src.id, src.source_biz_id, src.device_id, d.device_biz_id, src.channel_biz_id,
		       src.collect_enabled, src.write_enabled, src.collect_interval_ms, COALESCE(src.timeout_ms,300),
		       src.created_at, src.updated_at
		FROM sources src JOIN devices d ON d.id = src.device_id WHERE src.id=?`, id).Scan(
		&src.ID, &src.SourceBizID, &src.DeviceID, &src.DeviceBizID, &src.ChannelBizID,
		&src.CollectEnabled, &src.WriteEnabled, &src.CollectIntervalMs, &src.TimeoutMs,
		&src.CreatedAtMs, &src.UpdatedAtMs)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	applyTimesS(&src)
	return &src, nil
}

func (s *Store) CreateSource(ctx context.Context, src *SourceDTO) (int64, error) {
	now := nowMillis()
	if src.CollectIntervalMs == 0 {
		src.CollectIntervalMs = 1000
	}
	if src.TimeoutMs == 0 {
		src.TimeoutMs = 300
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO sources (source_biz_id, device_id, channel_biz_id, collect_enabled, write_enabled,
		                     collect_interval_ms, timeout_ms, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?)`,
		src.SourceBizID, src.DeviceID, src.ChannelBizID, src.CollectEnabled, src.WriteEnabled,
		src.CollectIntervalMs, src.TimeoutMs, now, now)
	if err != nil {
		if isUniqueConstraintErr(err) {
			return 0, fmt.Errorf("%w: source_biz_id=%s", ErrConflict, src.SourceBizID)
		}
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) UpdateSource(ctx context.Context, id int64, src *SourceDTO) error {
	now := nowMillis()
	res, err := s.db.ExecContext(ctx, `
		UPDATE sources SET channel_biz_id=?, collect_enabled=?, write_enabled=?,
		       collect_interval_ms=?, timeout_ms=?, updated_at=? WHERE id=?`,
		src.ChannelBizID, src.CollectEnabled, src.WriteEnabled,
		src.CollectIntervalMs, src.TimeoutMs, now, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) DeleteSource(ctx context.Context, id int64) error {
	// 显式事务 + 手动级联：避免依赖 SQLite FK CASCADE（外键启用与否、错误恢复行为不一致）
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM points WHERE source_id=?`, id); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM sources WHERE id=?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

func (s *Store) ListSourcePoints(ctx context.Context, sourceID int64) ([]PointDTO, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, point_biz_id, source_id, name, address, data_type,
		       collect_enabled, write_enabled, COALESCE(string_length,0), created_at, updated_at
		FROM points WHERE source_id=? ORDER BY id`, sourceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PointDTO{}
	for rows.Next() {
		var p PointDTO
		if err := rows.Scan(&p.ID, &p.PointBizID, &p.SourceID, &p.Name, &p.Address, &p.DataType,
			&p.CollectEnabled, &p.WriteEnabled, &p.StringLength, &p.CreatedAtMs, &p.UpdatedAtMs); err != nil {
			return nil, err
		}
		applyTimesP(&p)
		out = append(out, p)
	}
	return out, rows.Err()
}

// listPointsBySourceIDs 批量拿多点对应的所有 points，返回按 source_id 分组的 map。
// 用于 GetDevice 嵌套填充 source.points，避免 N 次循环单点查询。
func (s *Store) listPointsBySourceIDs(ctx context.Context, sourceIDs []int64) (map[int64][]PointDTO, error) {
	if len(sourceIDs) == 0 {
		return map[int64][]PointDTO{}, nil
	}
	// 构造 IN (?, ?, ?, …) 占位符
	placeholders := make([]string, len(sourceIDs))
	args := make([]any, len(sourceIDs))
	for i, id := range sourceIDs {
		placeholders[i] = "?"
		args[i] = id
	}
	q := `SELECT id, point_biz_id, source_id, name, address, data_type,
		       collect_enabled, write_enabled, COALESCE(string_length,0), created_at, updated_at
		FROM points WHERE source_id IN (` + strings.Join(placeholders, ",") + `) ORDER BY id`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[int64][]PointDTO, len(sourceIDs))
	for rows.Next() {
		var p PointDTO
		if err := rows.Scan(&p.ID, &p.PointBizID, &p.SourceID, &p.Name, &p.Address, &p.DataType,
			&p.CollectEnabled, &p.WriteEnabled, &p.StringLength, &p.CreatedAtMs, &p.UpdatedAtMs); err != nil {
			return nil, err
		}
		applyTimesP(&p)
		out[p.SourceID] = append(out[p.SourceID], p)
	}
	return out, rows.Err()
}

// === Points ===

func (s *Store) GetPoint(ctx context.Context, id int64) (*PointDTO, error) {
	var p PointDTO
	err := s.db.QueryRowContext(ctx, `
		SELECT id, point_biz_id, source_id, name, address, data_type,
		       collect_enabled, write_enabled, COALESCE(string_length,0), created_at, updated_at
		FROM points WHERE id=?`, id).Scan(
		&p.ID, &p.PointBizID, &p.SourceID, &p.Name, &p.Address, &p.DataType,
		&p.CollectEnabled, &p.WriteEnabled, &p.StringLength, &p.CreatedAtMs, &p.UpdatedAtMs)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	applyTimesP(&p)
	return &p, nil
}

func (s *Store) CreatePoint(ctx context.Context, p *PointDTO) (int64, error) {
	now := nowMillis()
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO points (point_biz_id, source_id, name, address, data_type,
		                    collect_enabled, write_enabled, string_length, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?)`,
		p.PointBizID, p.SourceID, p.Name, p.Address, p.DataType,
		p.CollectEnabled, p.WriteEnabled, p.StringLength, now, now)
	if err != nil {
		if isUniqueConstraintErr(err) {
			return 0, fmt.Errorf("%w: point_biz_id=%s", ErrConflict, p.PointBizID)
		}
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) UpdatePoint(ctx context.Context, id int64, p *PointDTO) error {
	now := nowMillis()
	res, err := s.db.ExecContext(ctx, `
		UPDATE points SET name=?, address=?, data_type=?,
		       collect_enabled=?, write_enabled=?, string_length=?, updated_at=? WHERE id=?`,
		p.Name, p.Address, p.DataType,
		p.CollectEnabled, p.WriteEnabled, p.StringLength, now, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) DeletePoint(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM points WHERE id=?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	// 同步清理 target_points 引用，避免悬挂关联
	if _, err := s.db.ExecContext(ctx, `DELETE FROM target_points WHERE point_id=?`, id); err != nil {
		return err
	}
	return nil
}

// === Targets ===

func (s *Store) ListTargets(ctx context.Context) ([]TargetDTO, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, target_biz_id, device_id, forward_channel_biz_id, COALESCE(topic,''),
		        format, created_at, updated_at FROM targets ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TargetDTO{}
	for rows.Next() {
		var t TargetDTO
		if err := rows.Scan(&t.ID, &t.TargetBizID, &t.DeviceID, &t.ForwardChannelBizID,
			&t.Topic, &t.Format, &t.CreatedAtMs, &t.UpdatedAtMs); err != nil {
			return nil, err
		}
		applyTimesT(&t)
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) GetTarget(ctx context.Context, id int64) (*TargetDTO, error) {
	var t TargetDTO
	err := s.db.QueryRowContext(ctx,
		`SELECT id, target_biz_id, device_id, forward_channel_biz_id, COALESCE(topic,''),
		        format, created_at, updated_at FROM targets WHERE id=?`, id).Scan(
		&t.ID, &t.TargetBizID, &t.DeviceID, &t.ForwardChannelBizID,
		&t.Topic, &t.Format, &t.CreatedAtMs, &t.UpdatedAtMs)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	applyTimesT(&t)
	pts, err := s.listTargetPointBizIDs(ctx, id)
	if err != nil {
		return nil, err
	}
	t.PointBizIDs = pts
	return &t, nil
}

func (s *Store) listTargetPointBizIDs(ctx context.Context, targetID int64) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT p.point_biz_id FROM target_points tp JOIN points p ON p.id=tp.point_id
		 WHERE tp.target_id=? ORDER BY p.point_biz_id`, targetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var biz string
		if err := rows.Scan(&biz); err != nil {
			return nil, err
		}
		out = append(out, biz)
	}
	return out, rows.Err()
}

func (s *Store) CreateTarget(ctx context.Context, t *TargetDTO) (int64, error) {
	now := nowMillis()
	if t.Format == "" {
		t.Format = "json"
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `
		INSERT INTO targets (target_biz_id, device_id, forward_channel_biz_id, topic,
		                     format, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?)`,
		t.TargetBizID, t.DeviceID, t.ForwardChannelBizID, t.Topic, t.Format, now, now)
	if err != nil {
		if isUniqueConstraintErr(err) {
			return 0, fmt.Errorf("%w: target_biz_id=%s", ErrConflict, t.TargetBizID)
		}
		return 0, err
	}
	id, _ := res.LastInsertId()
	if err := setTargetPointIDsTx(ctx, tx, id, t.PointBizIDs); err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

func (s *Store) UpdateTarget(ctx context.Context, id int64, t *TargetDTO) error {
	now := nowMillis()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `
		UPDATE targets SET target_biz_id=?, forward_channel_biz_id=?, topic=?, format=?, updated_at=? WHERE id=?`,
		t.TargetBizID, t.ForwardChannelBizID, t.Topic, t.Format, now, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	if err := setTargetPointIDsTx(ctx, tx, id, t.PointBizIDs); err != nil {
		return err
	}
	return tx.Commit()
}

func setTargetPointIDsTx(ctx context.Context, tx *sql.Tx, targetID int64, bizIDs []string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM target_points WHERE target_id=?`, targetID); err != nil {
		return err
	}
	for _, biz := range bizIDs {
		var ptID int64
		if err := tx.QueryRowContext(ctx, `SELECT id FROM points WHERE point_biz_id=?`, biz).Scan(&ptID); err != nil {
			return fmt.Errorf("point not found: %s", biz)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO target_points (target_id, point_id) VALUES (?,?)`, targetID, ptID); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) DeleteTarget(ctx context.Context, id int64) error {
	// 显式事务：先清 target_points 关联，再删 target，避免悬挂行
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM target_points WHERE target_id=?`, id); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM targets WHERE id=?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

// === Tokens（plcbridge §8.6 复用）===

func (s *Store) ListTokens(ctx context.Context) ([]TokenDTO, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, created_at, revoked_at FROM tokens ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TokenDTO{}
	for rows.Next() {
		var t TokenDTO
		var createdMs int64
		var revoked sql.NullInt64
		if err := rows.Scan(&t.ID, &t.Name, &createdMs, &revoked); err != nil {
			return nil, err
		}
		t.CreatedAt = timefmt.BJTMillis(createdMs)
		if revoked.Valid && revoked.Int64 > 0 {
			rt := timefmt.BJTMillis(revoked.Int64)
			t.RevokedAt = &rt
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) GetToken(ctx context.Context, id int64) (*TokenDTO, error) {
	var t TokenDTO
	var createdMs int64
	var revoked sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT id, token, name, created_at, revoked_at FROM tokens WHERE id=?`, id).Scan(
		&t.ID, &t.Token, &t.Name, &createdMs, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	t.CreatedAt = timefmt.BJTMillis(createdMs)
	if revoked.Valid && revoked.Int64 > 0 {
		rt := timefmt.BJTMillis(revoked.Int64)
		t.RevokedAt = &rt
	}
	return &t, nil
}

func (s *Store) CreateToken(ctx context.Context, t *TokenDTO) (int64, error) {
	t.CreatedAtMs = nowMillis()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO tokens (token, name, created_at) VALUES (?,?,?)`,
		t.Token, t.Name, t.CreatedAtMs)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) RevokeToken(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE tokens SET revoked_at=? WHERE id=? AND revoked_at IS NULL`, nowMillis(), id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// === Account ===

func (s *Store) GetAccount(ctx context.Context) (*AccountDTO, error) {
	var a AccountDTO
	var pwHash string
	err := s.db.QueryRowContext(ctx,
		`SELECT username, password_hash FROM accounts WHERE id=1`).Scan(&a.Username, &pwHash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

func (s *Store) UpsertAccount(ctx context.Context, username, passwordHash string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO accounts (id, username, password_hash, updated_at) VALUES (1,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET username=?, password_hash=?, updated_at=?`,
		username, passwordHash, nowMillis(),
		username, passwordHash, nowMillis())
	return err
}

func (s *Store) UpdatePasswordHash(ctx context.Context, username, newHash string) error {
	now := nowMillis()
	res, err := s.db.ExecContext(ctx,
		`UPDATE accounts SET password_hash=?, updated_at=? WHERE username=? AND id=1`,
		newHash, now, username)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// === Stats (Dashboard) ===

func (s *Store) ChannelCounts(ctx context.Context) (ChannelStats, error) {
	var st ChannelStats
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*),
		        COALESCE(SUM(CASE WHEN role='collect' THEN 1 ELSE 0 END),0),
		        COALESCE(SUM(CASE WHEN role='forward' THEN 1 ELSE 0 END),0)
		 FROM channels`).Scan(&st.Total, &st.Collect, &st.Forward)
	return st, err
}

func (s *Store) DeviceCounts(ctx context.Context) (DeviceStats, error) {
	var st DeviceStats
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(CASE WHEN enabled=1 THEN 1 ELSE 0 END),0) FROM devices`).
		Scan(&st.Total, &st.Enabled)
	return st, err
}

func (s *Store) SourceCounts(ctx context.Context) (SourceStats, error) {
	var st SourceStats
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*),
		        COALESCE(SUM(CASE WHEN collect_enabled=1 THEN 1 ELSE 0 END),0),
		        COALESCE(SUM(CASE WHEN write_enabled=1 THEN 1 ELSE 0 END),0)
		 FROM sources`).Scan(&st.Total, &st.CollectOn, &st.WriteOn)
	return st, err
}

func (s *Store) PointCounts(ctx context.Context) (PointStats, error) {
	var st PointStats
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*),
		        COALESCE(SUM(CASE WHEN collect_enabled=1 THEN 1 ELSE 0 END),0),
		        COALESCE(SUM(CASE WHEN write_enabled=1 THEN 1 ELSE 0 END),0)
		 FROM points`).Scan(&st.Total, &st.CollectOn, &st.WriteOn)
	return st, err
}

func (s *Store) TargetCount(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM targets`).Scan(&n)
	return n, err
}

func (s *Store) TokenActiveCount(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM tokens WHERE revoked_at IS NULL`).Scan(&n)
	return n, err
}

// === helper ===

// isUniqueConstraintErr 判断 SQLite 唯一约束冲突。
//
// modernc.org/sqlite 返回的错误形如：
//
//	"constraint failed: UNIQUE constraint failed: channels.channel_biz_id (2067)"
func isUniqueConstraintErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed")
}
