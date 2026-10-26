// Plan2 主题 C 5.3：业务规则下沉到 service 层。
//
// 原则：
//   - handler 只做"绑定 + 翻译"（HTTP ↔ DTO）
//   - service 装业务规则（跨表校验、复合条件）
//   - service 返回 *web.APIError，handler 走 WriteError
//
// 过渡期：service 仍内联在 web 包（同包私有函数）。后续独立成 internal/service 包
// 时只需移动文件 + import 调整，handler 调用点不变。
package web

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"

	"github.com/plcbridge/plcbridge/internal/infra/protocol"
	"github.com/plcbridge/plcbridge/internal/infra/target"
)

// === Channel 相关 ===

// EnsureChannelExists 校验 channel_biz_id 存在（Plan2 5.3 service 层）。
//
// 返回值：nil=存在；*APIError=业务错误。
// 不存在时返回 ErrChannelNotFound；DB 错误透传。
func (s *Store) EnsureChannelExists(ctx context.Context, bizID string) error {
	var x int
	err := s.DB().QueryRowContext(ctx, `SELECT 1 FROM channels WHERE channel_biz_id=?`, bizID).Scan(&x)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrChannelNotFound.With("channel_biz_id 不存在: " + bizID)
	}
	return err
}

// EnsureChannelRole 校验 channel 角色是否符合预期（source → collect, target → forward）。
//
// 不存在 → ErrChannelNotFound；角色不匹配 → ErrChannelRoleMismatch。
func (s *Store) EnsureChannelRole(ctx context.Context, bizID, wantRole string) error {
	var role string
	err := s.DB().QueryRowContext(ctx, `SELECT role FROM channels WHERE channel_biz_id=?`, bizID).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrChannelNotFound.With("channel_biz_id 不存在: " + bizID)
	}
	if err != nil {
		return err
	}
	if role != wantRole {
		return ErrChannelRoleMismatch.With(fmt.Sprintf("channel_biz_id=%s role=%s，需 %s", bizID, role, wantRole))
	}
	return nil
}

// === Device / Source ===

// EnsureDeviceExists 校验 device_id 存在；不存在返回 ErrDeviceNotFound。
func (s *Store) EnsureDeviceExists(ctx context.Context, id int64) error {
	var x int
	err := s.DB().QueryRowContext(ctx, `SELECT 1 FROM devices WHERE id=?`, id).Scan(&x)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrDeviceNotFound.With("device_id 不存在: " + strconv.FormatInt(id, 10))
	}
	return err
}

// EnsureSourceExists 校验 source_id 存在；不存在返回 ErrSourceNotFound。
func (s *Store) EnsureSourceExists(ctx context.Context, id int64) error {
	var x int
	err := s.DB().QueryRowContext(ctx, `SELECT 1 FROM sources WHERE id=?`, id).Scan(&x)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrSourceNotFound.With("source_id 不存在: " + strconv.FormatInt(id, 10))
	}
	return err
}

// === 复合校验 ===

// ValidateSourceForCreate 复合校验：source 创建前的所有业务规则。
//
// 顺序：
//  1. channel_biz_id 必须存在
//  2. channel.role 必须 == "collect"
//  3. device_id 必须存在
//
// 错误用 errors.Is 匹配（APIError 实现 Is）。
func (s *Store) ValidateSourceForCreate(ctx context.Context, src *SourceDTO) error {
	if err := s.EnsureChannelExists(ctx, src.ChannelBizID); err != nil {
		return err
	}
	if err := s.EnsureChannelRole(ctx, src.ChannelBizID, "collect"); err != nil {
		return err
	}
	return s.EnsureDeviceExists(ctx, src.DeviceID)
}

// ValidateSourceForUpdate 复合校验：source 更新时的 channel_biz_id 角色约束（仅当请求体包含时）。
//
// 防止把 source 绑到 forward channel 导致采集死循环。
func (s *Store) ValidateSourceForUpdate(ctx context.Context, src *SourceDTO) error {
	if src.ChannelBizID == "" {
		return nil // 部分更新，未指定 channel 不校验
	}
	return s.EnsureChannelRole(ctx, src.ChannelBizID, "collect")
}

// ValidateTargetForCreate 复合校验：target 创建前的所有业务规则。
func (s *Store) ValidateTargetForCreate(ctx context.Context, tgt *TargetDTO) error {
	if err := s.EnsureChannelExists(ctx, tgt.ForwardChannelBizID); err != nil {
		return err
	}
	if err := s.EnsureChannelRole(ctx, tgt.ForwardChannelBizID, "forward"); err != nil {
		return err
	}
	return s.EnsureDeviceExists(ctx, tgt.DeviceID)
}

// ValidateTargetForUpdate 复合校验：target 更新时 forward channel 必须是 forward 角色。
func (s *Store) ValidateTargetForUpdate(ctx context.Context, tgt *TargetDTO) error {
	if tgt.ForwardChannelBizID == "" {
		return nil
	}
	return s.EnsureChannelRole(ctx, tgt.ForwardChannelBizID, "forward")
}

// ValidateChannelType 校验 channel.type 在 role 下是否合法（Plan2 主题 A 3.1 查表化）。
//
// 别名处理：先 protocol.CanonicalizeChannelType 归一化（如 modbus → modbus_tcp）。
// 返回 nil 表示合法，error 表示不匹配。
func ValidateChannelType(role, typ string) error {
	canonicalType := protocol.CanonicalizeChannelType(typ)
	switch role {
	case "collect":
		if _, err := protocol.Get(canonicalType); err == nil {
			return nil
		}
	case "forward":
		if _, err := target.Get(canonicalType); err == nil {
			return nil
		}
	}
	return ErrInvalidParam.With("type 与 role 不匹配或未知 type=" + typ)
}
