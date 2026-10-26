// device_clone.go：设备克隆（Plan3 §3.7）。
//
// 功能：
//
//	POST /api/v1/devices/:id/clone → 深拷贝 Device + Sources + Points + Targets
//
// 语义：
//   - Channel **不拷贝**（复用现有 Channel，符合"连接层共享"的设计原则）
//   - 所有业务 ID 加前缀（默认 "clone_"），保证全局唯一
//   - Target 的 point_biz_ids 同步加前缀（保持与克隆出的 Point 对应）
//   - 全程单事务：任一步失败整体回滚
package web

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// CloneRequest 克隆请求体。
type CloneRequest struct {
	NewDeviceBizID string `json:"new_device_biz_id" binding:"required"`
	NewDeviceName  string `json:"new_device_name"`
	BizIDPrefix    string `json:"biz_id_prefix"`
}

// CloneResult 克隆结果。
type CloneResult struct {
	DeviceID      int64  `json:"device_id"`
	DeviceBizID   string `json:"device_biz_id"`
	Sources       int    `json:"sources"`
	Points        int    `json:"points"`
	Targets       int    `json:"targets"`
	PrefixApplied string `json:"prefix_applied"`
}

// defaultClonePrefix 默认业务 ID 前缀。
const defaultClonePrefix = "clone_"

// cloneDeviceHandler POST /api/v1/devices/:id/clone
func (s *Store) cloneDeviceHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()

		id, ok := parseID(c)
		if !ok {
			return
		}

		var req CloneRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			WriteError(c, ErrInvalidParam.With(err.Error()))
			return
		}
		if strings.TrimSpace(req.NewDeviceBizID) == "" {
			WriteError(c, ErrInvalidParam.With("new_device_biz_id 必填"))
			return
		}
		if req.NewDeviceName == "" {
			req.NewDeviceName = req.NewDeviceBizID
		}
		prefix := req.BizIDPrefix
		if prefix == "" {
			prefix = defaultClonePrefix
		}

		// 源设备必须存在
		if _, err := s.GetDevice(ctx, id); err != nil {
			if errors.Is(err, ErrNotFound) {
				WriteError(c, ErrDeviceNotFound.With("device 不存在"))
				return
			}
			WriteError(c, ErrInternal.With(err.Error()))
			return
		}

		// 目标 device_biz_id 不能与已存在的冲突（事务内也会再检查）
		var exists int
		if err := s.db.QueryRowContext(ctx, `SELECT 1 FROM devices WHERE device_biz_id=?`, req.NewDeviceBizID).Scan(&exists); err == nil {
			WriteError(c, ErrDeviceConflict.With("device_biz_id 已存在: "+req.NewDeviceBizID))
			return
		} else if !errors.Is(err, sql.ErrNoRows) {
			WriteError(c, ErrInternal.With(err.Error()))
			return
		}

		res, err := s.cloneDevice(ctx, id, req.NewDeviceBizID, req.NewDeviceName, prefix)
		if err != nil {
			if errors.Is(err, ErrConflict) {
				WriteError(c, ErrDeviceConflict.With(err.Error()))
				return
			}
			WriteError(c, ErrInternal.With(err.Error()))
			return
		}

		c.JSON(http.StatusCreated, res)
	}
}

// cloneDevice 执行克隆（单事务）。
func (s *Store) cloneDevice(ctx context.Context, srcDeviceID int64, newBizID, newName, prefix string) (*CloneResult, error) {
	res := &CloneResult{
		DeviceBizID:   newBizID,
		PrefixApplied: prefix,
	}

	// 读取源设备完整树
	dev, err := s.GetDevice(ctx, srcDeviceID)
	if err != nil {
		return nil, err
	}
	sources, err := s.ListDeviceSources(ctx, srcDeviceID)
	if err != nil {
		return nil, err
	}
	ptsBySrc, err := s.listPointsBySourceIDs(ctx, sourceIDsOf(sources))
	if err != nil {
		return nil, err
	}
	targets, err := s.listDeviceTargets(ctx, srcDeviceID)
	if err != nil {
		return nil, err
	}

	err = s.WithTx(ctx, func(tx Tx) error {
		now := nowMillis()

		// 1) 新设备
		res2, err := tx.ExecContext(ctx,
			`INSERT INTO devices (device_biz_id, name, enabled, created_at, updated_at) VALUES (?,?,?,?,?)`,
			newBizID, newName, dev.Enabled, now, now)
		if err != nil {
			if isUniqueConstraintErr(err) {
				return fmt.Errorf("%w: device_biz_id=%s", ErrConflict, newBizID)
			}
			return err
		}
		newDeviceID, err := res2.LastInsertId()
		if err != nil {
			return err
		}
		res.DeviceID = newDeviceID

		// 2) Sources + Points
		// point_biz_id 映射：旧 → 新（用于 Target 关联）
		pointMap := map[string]string{}
		// 新 point_biz_id → 物理 id（用于 target_points 关联）
		pointIDMap := map[string]int64{}
		for _, src := range sources {
			newSourceBizID := prefix + src.SourceBizID
			sres, err := tx.ExecContext(ctx, `
				INSERT INTO sources (source_biz_id, device_id, channel_biz_id, collect_enabled, write_enabled,
				                     collect_interval_ms, timeout_ms, created_at, updated_at)
				VALUES (?,?,?,?,?,?,?,?,?)`,
				newSourceBizID, newDeviceID, src.ChannelBizID, src.CollectEnabled, src.WriteEnabled,
				src.CollectIntervalMs, src.TimeoutMs, now, now)
			if err != nil {
				if isUniqueConstraintErr(err) {
					return fmt.Errorf("%w: source_biz_id=%s", ErrConflict, newSourceBizID)
				}
				return err
			}
			newSourceID, err := sres.LastInsertId()
			if err != nil {
				return err
			}
			res.Sources++

			for _, p := range ptsBySrc[src.ID] {
				newPointBizID := prefix + p.PointBizID
				pres, err := tx.ExecContext(ctx, `
					INSERT INTO points (point_biz_id, source_id, name, address, data_type,
					                    collect_enabled, write_enabled, string_length, created_at, updated_at)
					VALUES (?,?,?,?,?,?,?,?,?,?)`,
					newPointBizID, newSourceID, p.Name, p.Address, p.DataType,
					p.CollectEnabled, p.WriteEnabled, p.StringLength, now, now)
				if err != nil {
					if isUniqueConstraintErr(err) {
						return fmt.Errorf("%w: point_biz_id=%s", ErrConflict, newPointBizID)
					}
					return err
				}
				newPointID, err := pres.LastInsertId()
				if err != nil {
					return err
				}
				pointMap[p.PointBizID] = newPointBizID
				pointIDMap[newPointBizID] = newPointID
				res.Points++
			}
		}

		// 3) Targets + point 关联
		for _, t := range targets {
			newTargetBizID := prefix + t.TargetBizID
			tres, err := tx.ExecContext(ctx, `
				INSERT INTO targets (target_biz_id, device_id, forward_channel_biz_id, topic, format, created_at, updated_at)
				VALUES (?,?,?,?,?,?,?)`,
				newTargetBizID, newDeviceID, t.ForwardChannelBizID, t.Topic, t.Format, now, now)
			if err != nil {
				if isUniqueConstraintErr(err) {
					return fmt.Errorf("%w: target_biz_id=%s", ErrConflict, newTargetBizID)
				}
				return err
			}
			newTargetID, err := tres.LastInsertId()
			if err != nil {
				return err
			}
			res.Targets++

			for _, pb := range t.PointBizIDs {
				newPointBizID, ok := pointMap[pb]
				if !ok {
					continue // 源 target 引用了不在本 device 内的点位，跳过
				}
				newPointID, ok := pointIDMap[newPointBizID]
				if !ok {
					continue
				}
				if _, err := tx.ExecContext(ctx,
					`INSERT INTO target_points (target_id, point_id) VALUES (?,?)`,
					newTargetID, newPointID); err != nil {
					return err
				}
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

// sourceIDsOf 提取 source id 列表。
func sourceIDsOf(srcs []SourceDTO) []int64 {
	out := make([]int64, 0, len(srcs))
	for _, s := range srcs {
		out = append(out, s.ID)
	}
	return out
}
