// Package config 业务校验器（Phase 1.4）。
//
// 职责：对 platform.Config 做完整引用完整性 + 字段合法性校验。
// 与 Web 后台 SQLite 层校验（assertChannelRole 等）的关系：
//   - SQLite 层：写入单条记录时的即时校验
//   - 本包：导出 config.json 后，plcbridge 加载前的兜底校验
//
// 设计原则：
//   - 校验失败返回 *ValidationError，包含所有错误（聚合），不首个失败就返回
//   - 校验逻辑**幂等**，可被多次调用（hot reload 场景）
//   - 不修改输入 cfg
package config

import (
	"errors"
	"fmt"
	"strings"

	"github.com/plcbridge/plcbridge/internal/platform"
)

// ValidationError 聚合多个校验错误。
type ValidationError struct {
	Errs []error
}

func (e *ValidationError) Error() string {
	if len(e.Errs) == 0 {
		return ""
	}
	msgs := make([]string, 0, len(e.Errs))
	for _, err := range e.Errs {
		msgs = append(msgs, err.Error())
	}
	return "config 校验失败:\n  - " + strings.Join(msgs, "\n  - ")
}

// Unwrap 返回首个错误（errors.Is / errors.As 兼容）。
func (e *ValidationError) Unwrap() error {
	if len(e.Errs) == 0 {
		return nil
	}
	return e.Errs[0]
}

// Errors 返回所有错误副本。
func (e *ValidationError) Errors() []error {
	out := make([]error, len(e.Errs))
	copy(out, e.Errs)
	return out
}

// validDataTypes 是平台层支持的点位数据类型（§7.5）。
// 新增类型必须在这里登记。
var validDataTypes = map[string]bool{
	"bool":    true,
	"int8":    true,
	"int16":   true,
	"int32":   true,
	"int64":   true,
	"uint8":   true,
	"uint16":  true,
	"uint32":  true,
	"uint64":  true,
	"float32": true,
	"float64": true,
	"string":  true,
}

// validChannelRoles 是 channel.role 合法值。
var validChannelRoles = map[string]bool{
	"collect": true,
	"forward": true,
}

// validChannelTypes 是 channel.type 合法值。
var validChannelTypes = map[string]bool{
	"s7":         true,
	"modbus_tcp": true,
	"modbus":     true, // 历史别名，等价于 modbus_tcp
	"opcua":      true,
	"mqtt":       true,
	"influxdb":   true,
}

// Validate 完整校验 platform.Config。
//
// 校验项：
//   1. channel_biz_id 唯一 + 非空 + role/type 合法
//   2. device_biz_id 唯一 + 非空
//   3. source_biz_id 在 device 内唯一 + 全局唯一 + channel_biz_id 引用存在
//   4. target_biz_id 在 device 内唯一 + 全局唯一 + forward_channel_biz_id 引用存在
//   5. point_biz_id 在 source 内唯一 + 全局唯一 + data_type 合法 + address 非空
//   6. 跨 device 的 source_biz_id 重复会拒绝
//   7. target.point_biz_ids 中引用必须真实存在
//
// 返回 *ValidationError 聚合所有错误。
func Validate(cfg *platform.Config) error {
	if cfg == nil {
		return &ValidationError{Errs: []error{errors.New("config 为 nil")}}
	}

	ve := &ValidationError{}

	// 1. Channel 校验
	channelByID := validateChannels(cfg, ve)
	// 2. Device 校验
	deviceByID := validateDevices(cfg, ve)
	// 3. Source / Point 校验（需要 channelByID）
	allSources := validateSourcesAndPoints(cfg, channelByID, ve)
	// 4. Target 校验
	validateTargets(cfg, channelByID, deviceByID, allSources, ve)

	if len(ve.Errs) > 0 {
		return ve
	}
	return nil
}

func validateChannels(cfg *platform.Config, ve *ValidationError) map[string]*platform.Channel {
	channelByID := make(map[string]*platform.Channel, len(cfg.Channels))
	for i, ch := range cfg.Channels {
		loc := fmt.Sprintf("channels[%d]", i)
		if ch.ChannelBizID == "" {
			ve.Errs = append(ve.Errs, fmt.Errorf("%s.channel_biz_id 不能为空", loc))
			continue
		}
		if _, dup := channelByID[ch.ChannelBizID]; dup {
			ve.Errs = append(ve.Errs, fmt.Errorf("%s channel_biz_id 重复: %s", loc, ch.ChannelBizID))
			continue
		}
		if !validChannelRoles[ch.Role] {
			ve.Errs = append(ve.Errs, fmt.Errorf("%s role 非法 %q (期望 collect|forward)", loc, ch.Role))
		}
		if !validChannelTypes[ch.Type] {
			ve.Errs = append(ve.Errs, fmt.Errorf("%s type 非法 %q", loc, ch.Type))
		}
		if strings.TrimSpace(ch.Address) == "" {
			ve.Errs = append(ve.Errs, fmt.Errorf("%s address 不能为空", loc))
		}
		// 心跳参数合法性
		if ch.HeartbeatEnabled {
			if ch.HeartbeatAddress == "" {
				ve.Errs = append(ve.Errs, fmt.Errorf("%s heartbeat_enabled=true 时 heartbeat_address 必填", loc))
			}
			if ch.HeartbeatMaxMs < 0 {
				ve.Errs = append(ve.Errs, fmt.Errorf("%s heartbeat_max_interval_ms < 0: %d", loc, ch.HeartbeatMaxMs))
			}
		}
		channelByID[ch.ChannelBizID] = ch
	}
	return channelByID
}

func validateDevices(cfg *platform.Config, ve *ValidationError) map[string]*platform.Device {
	deviceByID := make(map[string]*platform.Device, len(cfg.Devices))
	for i, dev := range cfg.Devices {
		loc := fmt.Sprintf("devices[%d]", i)
		if dev.DeviceBizID == "" {
			ve.Errs = append(ve.Errs, fmt.Errorf("%s device_biz_id 不能为空", loc))
			continue
		}
		if _, dup := deviceByID[dev.DeviceBizID]; dup {
			ve.Errs = append(ve.Errs, fmt.Errorf("%s device_biz_id 重复: %s", loc, dev.DeviceBizID))
			continue
		}
		deviceByID[dev.DeviceBizID] = dev
	}
	return deviceByID
}

func validateSourcesAndPoints(cfg *platform.Config, channelByID map[string]*platform.Channel, ve *ValidationError) map[string]*platform.Source {
	allSources := make(map[string]*platform.Source)
	for di, dev := range cfg.Devices {
		devLoc := fmt.Sprintf("devices[%d]=%s", di, dev.DeviceBizID)
		for si, src := range dev.Sources {
			srcLoc := fmt.Sprintf("%s.sources[%d]", devLoc, si)
			if src.SourceBizID == "" {
				ve.Errs = append(ve.Errs, fmt.Errorf("%s source_biz_id 不能为空", srcLoc))
				continue
			}
			if _, dup := allSources[src.SourceBizID]; dup {
				ve.Errs = append(ve.Errs, fmt.Errorf("%s source_biz_id 全局重复: %s", srcLoc, src.SourceBizID))
				continue
			}
			// channel 引用必须存在
			if _, ok := channelByID[src.ChannelBizID]; !ok {
				ve.Errs = append(ve.Errs, fmt.Errorf("%s channel_biz_id 引用不存在: %s", srcLoc, src.ChannelBizID))
			} else {
				// 必须是 collect 角色
				if channelByID[src.ChannelBizID].Role != "collect" {
					ve.Errs = append(ve.Errs, fmt.Errorf("%s 引用的 channel role=%q，必须是 collect",
						srcLoc, channelByID[src.ChannelBizID].Role))
				}
			}
			// 采集参数合法性
			if src.CollectIntervalMs < 0 {
				ve.Errs = append(ve.Errs, fmt.Errorf("%s collect_interval_ms < 0: %d", srcLoc, src.CollectIntervalMs))
			}
			allSources[src.SourceBizID] = src
			// Point 校验
			validatePoints(src, srcLoc, ve)
		}
	}
	return allSources
}

func validatePoints(src *platform.Source, srcLoc string, ve *ValidationError) {
	seenPoint := map[string]bool{}
	for pi, p := range src.Points {
		pLoc := fmt.Sprintf("%s.points[%d]", srcLoc, pi)
		if p.PointBizID == "" {
			ve.Errs = append(ve.Errs, fmt.Errorf("%s point_biz_id 不能为空", pLoc))
			continue
		}
		if seenPoint[p.PointBizID] {
			ve.Errs = append(ve.Errs, fmt.Errorf("%s point_biz_id 在 source 内重复: %s", pLoc, p.PointBizID))
		}
		seenPoint[p.PointBizID] = true
		if p.DataType != "" && !validDataTypes[p.DataType] {
			ve.Errs = append(ve.Errs, fmt.Errorf("%s data_type 非法: %q", pLoc, p.DataType))
		}
		if strings.TrimSpace(p.Address) == "" {
			ve.Errs = append(ve.Errs, fmt.Errorf("%s address 不能为空", pLoc))
		}
	}
}

func validateTargets(cfg *platform.Config, channelByID map[string]*platform.Channel,
	deviceByID map[string]*platform.Device, allSources map[string]*platform.Source, ve *ValidationError) {
	// 收集所有 point_biz_id（全局）
	allPoints := make(map[string]bool)
	for _, src := range allSources {
		for _, p := range src.Points {
			allPoints[p.PointBizID] = true
		}
	}

	seenTarget := map[string]bool{}
	for di, dev := range cfg.Devices {
		devLoc := fmt.Sprintf("devices[%d]=%s", di, dev.DeviceBizID)
		for ti, tgt := range dev.Targets {
			tLoc := fmt.Sprintf("%s.targets[%d]", devLoc, ti)
			if tgt.TargetBizID == "" {
				ve.Errs = append(ve.Errs, fmt.Errorf("%s target_biz_id 不能为空", tLoc))
				continue
			}
			if seenTarget[tgt.TargetBizID] {
				ve.Errs = append(ve.Errs, fmt.Errorf("%s target_biz_id 全局重复: %s", tLoc, tgt.TargetBizID))
				continue
			}
			seenTarget[tgt.TargetBizID] = true
			// forward_channel_biz_id 引用存在 + 角色为 forward
			if _, ok := channelByID[tgt.ForwardChannelBizID]; !ok {
				ve.Errs = append(ve.Errs, fmt.Errorf("%s forward_channel_biz_id 引用不存在: %s",
					tLoc, tgt.ForwardChannelBizID))
			} else if channelByID[tgt.ForwardChannelBizID].Role != "forward" {
				ve.Errs = append(ve.Errs, fmt.Errorf("%s forward_channel_biz_id role=%q, 必须是 forward",
					tLoc, channelByID[tgt.ForwardChannelBizID].Role))
			}
			// point_biz_ids 引用存在（除非 "*"）
			if len(tgt.PointBizIDs) == 0 {
				ve.Errs = append(ve.Errs, fmt.Errorf("%s point_biz_ids 不能为空", tLoc))
			}
			for _, pb := range tgt.PointBizIDs {
				if pb == "*" {
					continue
				}
				if !allPoints[pb] {
					ve.Errs = append(ve.Errs, fmt.Errorf("%s point_biz_id 引用不存在: %s", tLoc, pb))
				}
			}
		}
	}
}
