package web

import (
	"context"
	"database/sql"
	"encoding/json"
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

// Tx 是事务句柄（Plan2 主题 C 5.2 抽象）。
//
// 暴露 Exec / QueryRow / Query 三个原子方法，签名与 *sql.Tx 完全对齐。
// 业务层拿到 Tx 后**不知道是 sql.Tx 还是 mock Tx**，便于测试注入 mock。
type Tx interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// sqlTx 包装 *sql.Tx 以实现 Tx 接口（解耦 sql.Tx 便于替换）。
type sqlTx struct{ inner *sql.Tx }

func (t *sqlTx) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return t.inner.ExecContext(ctx, query, args...)
}
func (t *sqlTx) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return t.inner.QueryRowContext(ctx, query, args...)
}
func (t *sqlTx) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return t.inner.QueryContext(ctx, query, args...)
}

// WithTx 在事务里跑 fn（Plan2 主题 C 5.2）。
//
// 语义：
//   - fn 返回 nil → Commit
//   - fn 返回 err → Rollback（fn 返回的 err 透传给调用方）
//   - 事务 Begin 失败 → 直接返回 error，fn 不执行
//
// 典型用法（Store 内部事务方法）：
//
//	func (s *Store) DoMultiStep(ctx context.Context) error {
//	    return "", s.WithTx(ctx, func(tx Tx) error {
//	        if err := tx.ExecContext(ctx, "UPDATE ...", ...); err != nil { return err }
//	        if err := tx.ExecContext(ctx, "INSERT ...", ...); err != nil { return err }
//	        return nil
//	    })
//	}
//
// 当前仅 SQLite 进程内使用，**未做并发嵌套检测**（database/sql 本身支持，但需要 savepoint）。
func (s *Store) WithTx(ctx context.Context, fn func(Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() // Commit 后 Rollback 是 no-op，安全
	if err := fn(&sqlTx{inner: tx}); err != nil {
		return err
	}
	return tx.Commit()
}

// nowMillis 返回当前时间（epoch 毫秒，SQLite 统一时间单位）。
func nowMillis() int64 {
	now := msNow()
	return now
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
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
	// heartbeat_enabled=true 时 heartbeat_address 必填（runtime 守护，否则 config.Validate 启动时才发现 → 启动失败）
	// 之前仅 config 包 Validate 校验，SQL 层缺失；这里补上，handler 创建就能立刻 fail-fast。
	if c.HeartbeatEnabled && strings.TrimSpace(c.HeartbeatAddress) == "" {
		return 0, fmt.Errorf("%w: heartbeat_enabled=true 时 heartbeat_address 必填", ErrInvalidParam)
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
	// 与 CreateChannel 一致：heartbeat_enabled=true 时 heartbeat_address 必填
	if c.HeartbeatEnabled && strings.TrimSpace(c.HeartbeatAddress) == "" {
		return fmt.Errorf("%w: heartbeat_enabled=true 时 heartbeat_address 必填", ErrInvalidParam)
	}
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

// GetSourceByBizID 按业务 ID 查 Source（Plan3 §3.1 CSV 导入用）。
func (s *Store) GetSourceByBizID(ctx context.Context, bizID string) (*SourceDTO, error) {
	var src SourceDTO
	err := s.db.QueryRowContext(ctx, `
		SELECT src.id, src.source_biz_id, src.device_id, d.device_biz_id, src.channel_biz_id,
		       src.collect_enabled, src.write_enabled, src.collect_interval_ms, COALESCE(src.timeout_ms,300),
		       src.created_at, src.updated_at
		FROM sources src JOIN devices d ON d.id = src.device_id WHERE src.source_biz_id=?`, bizID).Scan(
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
	return s.WithTx(ctx, func(tx Tx) error {
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
		return nil
	})
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
	var id int64
	err := s.WithTx(ctx, func(tx Tx) error {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO targets (target_biz_id, device_id, forward_channel_biz_id, topic,
			                     format, created_at, updated_at)
			VALUES (?,?,?,?,?,?,?)`,
			t.TargetBizID, t.DeviceID, t.ForwardChannelBizID, t.Topic, t.Format, now, now)
		if err != nil {
			if isUniqueConstraintErr(err) {
				return fmt.Errorf("%w: target_biz_id=%s", ErrConflict, t.TargetBizID)
			}
			return err
		}
		id, _ = res.LastInsertId()
		return setTargetPointIDsTx(ctx, tx, id, t.PointBizIDs)
	})
	if err != nil {
		return 0, err
	}
	return id, nil
}

func (s *Store) UpdateTarget(ctx context.Context, id int64, t *TargetDTO) error {
	now := nowMillis()
	return s.WithTx(ctx, func(tx Tx) error {
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
		return setTargetPointIDsTx(ctx, tx, id, t.PointBizIDs)
	})
}

func setTargetPointIDsTx(ctx context.Context, tx Tx, targetID int64, bizIDs []string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM target_points WHERE target_id=?`, targetID); err != nil {
		return err
	}
	// 去重：应用层把重复的 point_biz_id 折叠为一条。
	// 原因：target_points 表 PRIMARY KEY (target_id, point_id)，如果请求体含重复 biz_id，
	// 第二次 INSERT 会触发 UNIQUE constraint → 整个事务回滚 → 用户目标 PUT 失败。
	// SQL 层用 INSERT OR IGNORE 也可以，但应用层 dedup 更友好（不会让用户看到 "UNIQUE constraint failed"）。
	seen := make(map[string]struct{}, len(bizIDs))
	for _, biz := range bizIDs {
		if _, dup := seen[biz]; dup {
			continue
		}
		seen[biz] = struct{}{}
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
	return s.WithTx(ctx, func(tx Tx) error {
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
		return nil
	})
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

// === Config Snapshots (Plan3 3.3) ===

// 快照 JSON 的实体结构定义。
// 这些类型在 BuildFullConfigJSON（写）和 RollbackToSnapshot（读）之间共享，
// 保证序列化 / 反序列化字段一一对应。
type snapChannel struct {
	ID                     int64  `json:"id"`
	ChannelBizID           string `json:"channel_biz_id"`
	Role                   string `json:"role"`
	Type                   string `json:"type"`
	Address                string `json:"address"`
	ConfigJSON             string `json:"config_json,omitempty"`
	HeartbeatAddress       string `json:"heartbeat_address,omitempty"`
	HeartbeatMaxIntervalMs int    `json:"heartbeat_max_interval_ms"`
	HeartbeatEnabled       bool   `json:"heartbeat_enabled"`
	HeartbeatDebounceMs    int    `json:"heartbeat_debounce_ms"`
	QueueSize              int    `json:"queue_size"`
	TimeoutMs              int    `json:"timeout_ms"`
}

type snapDevice struct {
	ID          int64  `json:"id"`
	DeviceBizID string `json:"device_biz_id"`
	Name        string `json:"name"`
	Enabled     bool   `json:"enabled"`
}

type snapSource struct {
	ID                int64  `json:"id"`
	SourceBizID       string `json:"source_biz_id"`
	DeviceID          int64  `json:"device_id"`
	ChannelBizID      string `json:"channel_biz_id"`
	CollectEnabled    bool   `json:"collect_enabled"`
	WriteEnabled      bool   `json:"write_enabled"`
	CollectIntervalMs int    `json:"collect_interval_ms"`
	TimeoutMs         int    `json:"timeout_ms"`
}

type snapPoint struct {
	ID           int64  `json:"id"`
	PointBizID   string `json:"point_biz_id"`
	SourceID     int64  `json:"source_id"`
	Name         string `json:"name"`
	Address      string `json:"address"`
	DataType     string `json:"data_type"`
	CollectEn    bool   `json:"collect_enabled"`
	WriteEn      bool   `json:"write_enabled"`
	StringLength int    `json:"string_length"`
}

type snapTarget struct {
	ID           int64    `json:"id"`
	TargetBizID  string   `json:"target_biz_id"`
	DeviceID     int64    `json:"device_id"`
	ChannelBizID string   `json:"channel_biz_id"`
	Topic        string   `json:"topic,omitempty"`
	Format       string   `json:"format"`
	PointBizIDs  []string `json:"point_biz_ids"`
}

// snapConfig 是完整快照的顶层结构。
type snapConfig struct {
	Version  int           `json:"version"`
	Channels []snapChannel `json:"channels"`
	Devices  []snapDevice  `json:"devices"`
	Sources  []snapSource  `json:"sources"`
	Points   []snapPoint   `json:"points"`
	Targets  []snapTarget  `json:"targets"`
}

// ConfigSnapshotRow is a snapshot record.
type ConfigSnapshotRow struct {
	ID          int64
	Description string
	ConfigJSON  string
	CreatedBy   string
	RecordCount int
	IsAuto      bool
	CreatedAt   int64 // Unix ms
}

// buildFullConfigJSON returns a JSON snapshot of ALL current database entities.
// This is the "config JSON" stored in each snapshot.
func (s *Store) BuildFullConfigJSON(ctx context.Context) (string, int, error) {
	// Gather all channels
	rows, err := s.db.QueryContext(ctx, `SELECT id, channel_biz_id, role, type, address,
		config_json, heartbeat_address, heartbeat_max_interval_ms, heartbeat_enabled,
		heartbeat_debounce_ms, queue_size, timeout_ms FROM channels`)
	if err != nil {
		return "", 0, err
	}
	defer rows.Close()

	var channels []snapChannel
	for rows.Next() {
		var ch snapChannel
		var cfg, hbAddr string
		var hbMax, hbDeb, qSize, to int
		var hbEn bool
		if err := rows.Scan(&ch.ID, &ch.ChannelBizID, &ch.Role, &ch.Type, &ch.Address,
			&cfg, &hbAddr, &hbMax, &hbEn, &hbDeb, &qSize, &to); err != nil {
			return "", 0, err
		}
		ch.ConfigJSON = cfg
		ch.HeartbeatAddress = hbAddr
		ch.HeartbeatMaxIntervalMs = hbMax
		ch.HeartbeatEnabled = hbEn
		ch.HeartbeatDebounceMs = hbDeb
		ch.QueueSize = qSize
		ch.TimeoutMs = to
		channels = append(channels, ch)
	}
	_ = rows.Err()

	// Devices
	devRows, err := s.db.QueryContext(ctx, `SELECT id, device_biz_id, name, enabled FROM devices`)
	if err != nil {
		return "", 0, err
	}
	var devices []snapDevice
	for devRows.Next() {
		var d snapDevice
		if err := devRows.Scan(&d.ID, &d.DeviceBizID, &d.Name, &d.Enabled); err != nil {
			devRows.Close()
			return "", 0, err
		}
		devices = append(devices, d)
	}
	devRows.Close()

	// Sources
	srcRows, err := s.db.QueryContext(ctx, `SELECT id, source_biz_id, device_id, channel_biz_id,
		collect_enabled, write_enabled, collect_interval_ms, timeout_ms FROM sources`)
	if err != nil {
		return "", 0, err
	}
	var sources []snapSource
	for srcRows.Next() {
		var s snapSource
		var ce, we bool
		if err := srcRows.Scan(&s.ID, &s.SourceBizID, &s.DeviceID, &s.ChannelBizID,
			&ce, &we, &s.CollectIntervalMs, &s.TimeoutMs); err != nil {
			srcRows.Close()
			return "", 0, err
		}
		s.CollectEnabled = ce
		s.WriteEnabled = we
		sources = append(sources, s)
	}
	srcRows.Close()

	// Points
	ptRows, err := s.db.QueryContext(ctx, `SELECT id, point_biz_id, source_id, name, address,
		data_type, collect_enabled, write_enabled, string_length FROM points`)
	if err != nil {
		return "", 0, err
	}
	var points []snapPoint
	for ptRows.Next() {
		var p snapPoint
		var ce, we bool
		if err := ptRows.Scan(&p.ID, &p.PointBizID, &p.SourceID, &p.Name, &p.Address,
			&p.DataType, &ce, &we, &p.StringLength); err != nil {
			ptRows.Close()
			return "", 0, err
		}
		p.CollectEn = ce
		p.WriteEn = we
		points = append(points, p)
	}
	ptRows.Close()

	// Target → point biz_ids 映射（一次性查询，避免嵌套查询在 SQLite :memory: 下拿不到同库）
	pointMapByTarget := map[int64][]string{}
	mapRows, err := s.db.QueryContext(ctx,
		`SELECT tp.target_id, p.point_biz_id FROM target_points tp
		 JOIN points p ON p.id = tp.point_id`)
	if err != nil {
		return "", 0, err
	}
	for mapRows.Next() {
		var tid int64
		var pb string
		if err := mapRows.Scan(&tid, &pb); err != nil {
			mapRows.Close()
			return "", 0, err
		}
		pointMapByTarget[tid] = append(pointMapByTarget[tid], pb)
	}
	mapRows.Close()

	// Targets
	tgtRows, err := s.db.QueryContext(ctx, `SELECT id, target_biz_id, device_id,
		forward_channel_biz_id, topic, format FROM targets`)
	if err != nil {
		return "", 0, err
	}
	var targets []snapTarget
	for tgtRows.Next() {
		var t snapTarget
		var topic string
		if err := tgtRows.Scan(&t.ID, &t.TargetBizID, &t.DeviceID, &t.ChannelBizID, &topic, &t.Format); err != nil {
			tgtRows.Close()
			return "", 0, err
		}
		t.Topic = topic
		t.PointBizIDs = pointMapByTarget[t.ID]
		if t.PointBizIDs == nil {
			t.PointBizIDs = []string{}
		}
		targets = append(targets, t)
	}
	tgtRows.Close()

	cfg := snapConfig{
		Version:  2,
		Channels: channels,
		Devices:  devices,
		Sources:  sources,
		Points:   points,
		Targets:  targets,
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		return "", 0, err
	}
	recordCount := len(channels) + len(devices) + len(sources) + len(points) + len(targets)
	return string(data), recordCount, nil
}

// ListSnapshots returns all snapshots ordered by created_at DESC.
func (s *Store) ListSnapshots(ctx context.Context) ([]ConfigSnapshotRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, description, config_json, created_by, record_count, is_auto, created_at
		 FROM config_snapshots ORDER BY created_at DESC LIMIT 100`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ConfigSnapshotRow
	for rows.Next() {
		var r ConfigSnapshotRow
		var isAuto int
		if err := rows.Scan(&r.ID, &r.Description, &r.ConfigJSON, &r.CreatedBy,
			&r.RecordCount, &isAuto, &r.CreatedAt); err != nil {
			return nil, err
		}
		r.IsAuto = isAuto != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetSnapshot returns a single snapshot by ID.
func (s *Store) GetSnapshot(ctx context.Context, id int64) (*ConfigSnapshotRow, error) {
	var r ConfigSnapshotRow
	var isAuto int
	err := s.db.QueryRowContext(ctx,
		`SELECT id, description, config_json, created_by, record_count, is_auto, created_at
		 FROM config_snapshots WHERE id=?`, id).
		Scan(&r.ID, &r.Description, &r.ConfigJSON, &r.CreatedBy, &r.RecordCount, &isAuto, &r.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	r.IsAuto = isAuto != 0
	return &r, nil
}

// CreateSnapshot inserts a new snapshot. Also prunes old snapshots (keeps 50).
func (s *Store) CreateSnapshot(ctx context.Context, description, createdBy string, configJSON string, recordCount int, isAuto bool) (int64, error) {
	now := nowMillis()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO config_snapshots (description, config_json, created_by, record_count, is_auto, created_at)
		 VALUES (?,?,?,?,?,?)`,
		description, configJSON, createdBy, recordCount, boolToInt(isAuto), now)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()

	// Prune: keep latest 50
	s.db.ExecContext(ctx,
		`DELETE FROM config_snapshots WHERE id IN (
			SELECT id FROM config_snapshots ORDER BY created_at DESC LIMIT -1 OFFSET 50
		)`)
	return id, nil
}

// RollbackToSnapshot restores the database to a previous snapshot.
// Returns a description of what was rolled back.
func (s *Store) RollbackToSnapshot(ctx context.Context, snapshotID int64, snapshotJSON, configJSON string) (string, error) {
	return "", s.WithTx(ctx, func(tx Tx) error {
		// 1. Parse the snapshot config
		var cfg snapConfig
		if err := json.Unmarshal([]byte(snapshotJSON), &cfg); err != nil {
			return fmt.Errorf("parse snapshot JSON: %w", err)
		}

		// 2. Wipe all data (in correct FK order)
		if _, err := tx.ExecContext(ctx, `DELETE FROM target_points`); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM points`); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM sources`); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM targets`); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM devices`); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM channels`); err != nil {
			return err
		}

		// 3. Rebuild channels（保留原始 id，避免外键断裂）
		for _, ch := range cfg.Channels {
			_, err := tx.ExecContext(ctx,
				`INSERT INTO channels (id, channel_biz_id, role, type, address, config_json,
				 heartbeat_address, heartbeat_max_interval_ms, heartbeat_enabled,
				 heartbeat_debounce_ms, queue_size, timeout_ms, created_at, updated_at)
				 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
				ch.ID, ch.ChannelBizID, ch.Role, ch.Type, ch.Address, ch.ConfigJSON,
				ch.HeartbeatAddress, ch.HeartbeatMaxIntervalMs, ch.HeartbeatEnabled,
				ch.HeartbeatDebounceMs, ch.QueueSize, ch.TimeoutMs, nowMillis(), nowMillis())
			if err != nil {
				return fmt.Errorf("restore channel %s: %w", ch.ChannelBizID, err)
			}
		}

		// 4. Rebuild devices
		for _, d := range cfg.Devices {
			_, err := tx.ExecContext(ctx,
				`INSERT INTO devices (id, device_biz_id, name, enabled, created_at, updated_at)
				 VALUES (?,?,?,?,?,?)`,
				d.ID, d.DeviceBizID, d.Name, d.Enabled, nowMillis(), nowMillis())
			if err != nil {
				return fmt.Errorf("restore device %s: %w", d.DeviceBizID, err)
			}
		}

		// 5. Rebuild sources
		for _, src := range cfg.Sources {
			_, err := tx.ExecContext(ctx,
				`INSERT INTO sources (id, source_biz_id, device_id, channel_biz_id,
				 collect_enabled, write_enabled, collect_interval_ms, timeout_ms, created_at, updated_at)
				 VALUES (?,?,?,?,?,?,?,?,?,?)`,
				src.ID, src.SourceBizID, src.DeviceID, src.ChannelBizID,
				src.CollectEnabled, src.WriteEnabled, src.CollectIntervalMs, src.TimeoutMs, nowMillis(), nowMillis())
			if err != nil {
				return fmt.Errorf("restore source %s: %w", src.SourceBizID, err)
			}
		}

		// 6. Rebuild points
		for _, pt := range cfg.Points {
			_, err := tx.ExecContext(ctx,
				`INSERT INTO points (id, point_biz_id, source_id, name, address, data_type,
				 collect_enabled, write_enabled, string_length, created_at, updated_at)
				 VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
				pt.ID, pt.PointBizID, pt.SourceID, pt.Name, pt.Address, pt.DataType,
				pt.CollectEn, pt.WriteEn, pt.StringLength, nowMillis(), nowMillis())
			if err != nil {
				return fmt.Errorf("restore point %s: %w", pt.PointBizID, err)
			}
		}

		// 7. Rebuild targets + point mappings
		for _, tgt := range cfg.Targets {
			_, err := tx.ExecContext(ctx,
				`INSERT INTO targets (id, target_biz_id, device_id, forward_channel_biz_id, topic, format, created_at, updated_at)
				 VALUES (?,?,?,?,?,?,?,?)`,
				tgt.ID, tgt.TargetBizID, tgt.DeviceID, tgt.ChannelBizID, tgt.Topic, tgt.Format, nowMillis(), nowMillis())
			if err != nil {
				return fmt.Errorf("restore target %s: %w", tgt.TargetBizID, err)
			}
			for _, pb := range tgt.PointBizIDs {
				var ptID int64
				err := tx.QueryRowContext(ctx,
					`SELECT id FROM points WHERE point_biz_id=?`, pb).Scan(&ptID)
				if err != nil {
					continue // point not found, skip
				}
				tx.ExecContext(ctx, `INSERT INTO target_points (target_id, point_id) VALUES (?,?)`, tgt.ID, ptID)
			}
		}

		// 8. Also write the config JSON snapshot to disk
		if configJSON != "" {
			// configJSON here is the global plcbridge config (not the DB snapshot JSON)
			// Write to the same location used by config exporter
			// The caller will handle config.json sync via the exporter
		}

		return nil
	})
}

// DeleteSnapshot removes a snapshot by ID.
func (s *Store) DeleteSnapshot(ctx context.Context, id int64) error {
	n, err := s.db.ExecContext(ctx, `DELETE FROM config_snapshots WHERE id=?`, id)
	if err != nil {
		return err
	}
	r, _ := n.RowsAffected()
	if r == 0 {
		return ErrNotFound
	}
	return nil
}

// === ChannelCounts / Stats ===

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
