// config 校验器单测（Phase 1.4）。
package config

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/plcbridge/plcbridge/internal/platform"
)

// TestValidate_NilConfig
func TestValidate_NilConfig(t *testing.T) {
	err := Validate(nil)
	if err == nil {
		t.Fatal("nil config 应返回错误")
	}
	if !strings.Contains(err.Error(), "nil") {
		t.Errorf("error msg 应包含 nil: %v", err)
	}
}

// TestValidate_EmptyConfig 空 cfg 不报错。
func TestValidate_EmptyConfig(t *testing.T) {
	err := Validate(&platform.Config{Version: 1})
	if err != nil {
		t.Errorf("空 cfg 应校验通过: %v", err)
	}
}

// TestValidate_Channel 各种 channel 字段错误。
func TestValidate_Channel(t *testing.T) {
	cases := []struct {
		name    string
		channel platform.Channel
		wantErr string
	}{
		{
			name:    "empty biz id",
			channel: platform.Channel{Role: "collect", Type: "s7", Address: "1.2.3.4:102"},
			wantErr: "channel_biz_id 不能为空",
		},
		{
			name:    "invalid role",
			channel: platform.Channel{ChannelBizID: "c1", Role: "wrong", Type: "s7", Address: "x"},
			wantErr: `role 非法`,
		},
		{
			name:    "invalid type",
			channel: platform.Channel{ChannelBizID: "c1", Role: "collect", Type: "fake_proto", Address: "x"},
			wantErr: `type 非法`,
		},
		{
			name:    "empty address",
			channel: platform.Channel{ChannelBizID: "c1", Role: "collect", Type: "s7", Address: ""},
			wantErr: "address 不能为空",
		},
		{
			name:    "heartbeat enabled but no address",
			channel: platform.Channel{ChannelBizID: "c1", Role: "collect", Type: "s7", Address: "x", HeartbeatEnabled: true},
			wantErr: "heartbeat_address 必填",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := &platform.Config{Version: 1, Channels: []*platform.Channel{&c.channel}}
			err := Validate(cfg)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", c.wantErr)
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("error msg: got %q, want contains %q", err.Error(), c.wantErr)
			}
		})
	}
}

// TestValidate_ChannelDuplicateBizID channel_biz_id 重复。
func TestValidate_ChannelDuplicateBizID(t *testing.T) {
	cfg := &platform.Config{
		Version: 1,
		Channels: []*platform.Channel{
			{ChannelBizID: "c1", Role: "collect", Type: "s7", Address: "x"},
			{ChannelBizID: "c1", Role: "forward", Type: "mqtt", Address: "y"},
		},
	}
	err := Validate(cfg)
	if err == nil || !strings.Contains(err.Error(), "channel_biz_id 重复") {
		t.Errorf("expected duplicate channel error, got %v", err)
	}
}

// TestValidate_SourceChannelReference source 引用的 channel 不存在。
func TestValidate_SourceChannelReference(t *testing.T) {
	cfg := &platform.Config{
		Version: 1,
		Channels: []*platform.Channel{
			{ChannelBizID: "c1", Role: "collect", Type: "s7", Address: "x"},
		},
		Devices: []*platform.Device{
			{
				DeviceBizID: "d1",
				Sources: []*platform.Source{
					{SourceBizID: "s1", ChannelBizID: "nonexistent", CollectEnabled: true},
				},
			},
		},
	}
	err := Validate(cfg)
	if err == nil || !strings.Contains(err.Error(), "channel_biz_id 引用不存在") {
		t.Errorf("expected channel ref error, got %v", err)
	}
}

// TestValidate_SourceForwardChannel source 引用 forward channel（角色不符）。
func TestValidate_SourceForwardChannel(t *testing.T) {
	cfg := &platform.Config{
		Version: 1,
		Channels: []*platform.Channel{
			{ChannelBizID: "c1", Role: "forward", Type: "mqtt", Address: "x"},
		},
		Devices: []*platform.Device{
			{
				DeviceBizID: "d1",
				Sources: []*platform.Source{
					{SourceBizID: "s1", ChannelBizID: "c1", CollectEnabled: true},
				},
			},
		},
	}
	err := Validate(cfg)
	if err == nil || !strings.Contains(err.Error(), "必须是 collect") {
		t.Errorf("expected role mismatch error, got %v", err)
	}
}

// TestValidate_SourceGlobalDuplicate source_biz_id 跨 device 重复。
func TestValidate_SourceGlobalDuplicate(t *testing.T) {
	cfg := &platform.Config{
		Version: 1,
		Channels: []*platform.Channel{
			{ChannelBizID: "c1", Role: "collect", Type: "s7", Address: "x"},
		},
		Devices: []*platform.Device{
			{DeviceBizID: "d1", Sources: []*platform.Source{{SourceBizID: "s1", ChannelBizID: "c1"}}},
			{DeviceBizID: "d2", Sources: []*platform.Source{{SourceBizID: "s1", ChannelBizID: "c1"}}},
		},
	}
	err := Validate(cfg)
	if err == nil || !strings.Contains(err.Error(), "source_biz_id 全局重复") {
		t.Errorf("expected source global duplicate error, got %v", err)
	}
}

// TestValidate_PointInvalidDataType
func TestValidate_PointInvalidDataType(t *testing.T) {
	cfg := &platform.Config{
		Version:  1,
		Channels: []*platform.Channel{{ChannelBizID: "c1", Role: "collect", Type: "s7", Address: "x"}},
		Devices: []*platform.Device{
			{DeviceBizID: "d1", Sources: []*platform.Source{{
				SourceBizID: "s1", ChannelBizID: "c1",
				Points: []*platform.Point{{PointBizID: "p1", DataType: "weird_type", Address: "DB1.DBD0"}},
			}}},
		},
	}
	err := Validate(cfg)
	if err == nil || !strings.Contains(err.Error(), "data_type 非法") {
		t.Errorf("expected data_type error, got %v", err)
	}
}

// TestValidate_PointEmptyAddress
func TestValidate_PointEmptyAddress(t *testing.T) {
	cfg := &platform.Config{
		Version:  1,
		Channels: []*platform.Channel{{ChannelBizID: "c1", Role: "collect", Type: "s7", Address: "x"}},
		Devices: []*platform.Device{
			{DeviceBizID: "d1", Sources: []*platform.Source{{
				SourceBizID: "s1", ChannelBizID: "c1",
				Points: []*platform.Point{{PointBizID: "p1", DataType: "int32", Address: ""}},
			}}},
		},
	}
	err := Validate(cfg)
	if err == nil || !strings.Contains(err.Error(), "address 不能为空") {
		t.Errorf("expected address error, got %v", err)
	}
}

// TestValidate_TargetForwardChannelMissing target 引用 forward channel 不存在。
func TestValidate_TargetForwardChannelMissing(t *testing.T) {
	cfg := &platform.Config{
		Version: 1,
		Channels: []*platform.Channel{
			{ChannelBizID: "c1", Role: "collect", Type: "s7", Address: "x"},
		},
		Devices: []*platform.Device{
			{DeviceBizID: "d1",
				Sources: []*platform.Source{{SourceBizID: "s1", ChannelBizID: "c1",
					Points: []*platform.Point{{PointBizID: "p1", Address: "DB1.DBD0", DataType: "int32"}}}},
				Targets: []*platform.Target{{TargetBizID: "t1", ForwardChannelBizID: "missing", PointBizIDs: []string{"*"}}},
			},
		},
	}
	err := Validate(cfg)
	if err == nil || !strings.Contains(err.Error(), "forward_channel_biz_id 引用不存在") {
		t.Errorf("expected forward channel ref error, got %v", err)
	}
}

// TestValidate_TargetPointNotFound target.point_biz_ids 引用不存在的点位。
func TestValidate_TargetPointNotFound(t *testing.T) {
	cfg := &platform.Config{
		Version: 1,
		Channels: []*platform.Channel{
			{ChannelBizID: "c1", Role: "collect", Type: "s7", Address: "x"},
			{ChannelBizID: "c2", Role: "forward", Type: "mqtt", Address: "y"},
		},
		Devices: []*platform.Device{
			{DeviceBizID: "d1",
				Sources: []*platform.Source{{SourceBizID: "s1", ChannelBizID: "c1",
					Points: []*platform.Point{{PointBizID: "p1", Address: "DB1.DBD0", DataType: "int32"}}}},
				Targets: []*platform.Target{{TargetBizID: "t1", ForwardChannelBizID: "c2", PointBizIDs: []string{"p_ghost"}}},
			},
		},
	}
	err := Validate(cfg)
	if err == nil || !strings.Contains(err.Error(), "point_biz_id 引用不存在") {
		t.Errorf("expected point not found error, got %v", err)
	}
}

// TestValidate_TargetEmptyPointIDs target.point_biz_ids 不能为空。
func TestValidate_TargetEmptyPointIDs(t *testing.T) {
	cfg := &platform.Config{
		Version: 1,
		Channels: []*platform.Channel{
			{ChannelBizID: "c1", Role: "collect", Type: "s7", Address: "x"},
			{ChannelBizID: "c2", Role: "forward", Type: "mqtt", Address: "y"},
		},
		Devices: []*platform.Device{{DeviceBizID: "d1",
			Sources: []*platform.Source{{SourceBizID: "s1", ChannelBizID: "c1"}},
			Targets: []*platform.Target{{TargetBizID: "t1", ForwardChannelBizID: "c2"}},
		}},
	}
	err := Validate(cfg)
	if err == nil || !strings.Contains(err.Error(), "point_biz_ids 不能为空") {
		t.Errorf("expected empty point_biz_ids error, got %v", err)
	}
}

// TestValidate_MultipleErrors 多个错误聚合返回。
func TestValidate_MultipleErrors(t *testing.T) {
	cfg := &platform.Config{
		Version: 1,
		Channels: []*platform.Channel{
			{ChannelBizID: "", Role: "wrong", Type: "fake", Address: ""},
			{ChannelBizID: "c1", Role: "collect", Type: "s7", Address: "x"},
		},
		Devices: []*platform.Device{
			{DeviceBizID: "d1",
				Sources: []*platform.Source{{
					SourceBizID: "", ChannelBizID: "missing_channel",
					Points: []*platform.Point{
						{PointBizID: "", DataType: "wrong_type", Address: ""},
					},
				}},
			},
		},
	}
	err := Validate(cfg)
	if err == nil {
		t.Fatal("expected error")
	}
	ve, ok := err.(*ValidationError)
	if !ok {
		t.Fatalf("expected *ValidationError, got %T", err)
	}
	// 至少应该报：channel[0] empty biz id + sources[0] empty biz id（聚合生效即可）
	if len(ve.Errors()) < 2 {
		t.Errorf("expected aggregated errors, got %d: %v", len(ve.Errors()), ve.Errors())
	}
	// 验证错误信息包含 channel_biz_id 提示
	if !strings.Contains(ve.Error(), "channel_biz_id 不能为空") {
		t.Errorf("Error() should mention empty biz id")
	}
}

// TestValidate_HappyPath 完整合法 cfg 通过校验。
func TestValidate_HappyPath(t *testing.T) {
	cfg := &platform.Config{
		Version: 1,
		Channels: []*platform.Channel{
			{ChannelBizID: "c1", Role: "collect", Type: "s7", Address: "1.2.3.4:102"},
			{ChannelBizID: "c2", Role: "forward", Type: "mqtt", Address: "tcp://broker:1883"},
		},
		Devices: []*platform.Device{
			{DeviceBizID: "d1",
				Sources: []*platform.Source{{
					SourceBizID: "s1", ChannelBizID: "c1", CollectEnabled: true,
					CollectIntervalMs: 1000, TimeoutMs: 300,
					Points: []*platform.Point{
						{PointBizID: "p1", DataType: "int32", Address: "DB1.DBD0"},
						{PointBizID: "p2", DataType: "float32", Address: "DB1.DBD4"},
					},
				}},
				Targets: []*platform.Target{{
					TargetBizID: "t1", ForwardChannelBizID: "c2",
					PointBizIDs: []string{"*"}, Topic: "plc/d1/all", Format: "json",
				}},
			},
		},
	}
	if err := Validate(cfg); err != nil {
		t.Errorf("happy path should pass, got %v", err)
	}
}

// TestValidationError_Unwrap errors.Is/Unwrap 工作。
func TestValidationError_Unwrap(t *testing.T) {
	ve := &ValidationError{Errs: []error{
		&platformNotFoundErr{msg: "not found"},
	}}
	if !strings.Contains(ve.Error(), "config 校验失败") {
		t.Errorf("Error() should format multi-errors")
	}
	if ve.Unwrap() == nil {
		t.Errorf("Unwrap should return first error")
	}
}

// 辅助：自定义 error 让 Unwrap 走通
type platformNotFoundErr struct{ msg string }

func (e *platformNotFoundErr) Error() string { return e.msg }

// TestValidate_AllDataTypesAccepted 验证所有合法 data_type 通过。
//
// Plan2 主题 A 3.1: 词汇表由 platform.AllDataTypes() 单一事实源接管。
func TestValidate_AllDataTypesAccepted(t *testing.T) {
	for _, dt := range platform.AllDataTypes() {
		cfg := &platform.Config{
			Version:  1,
			Channels: []*platform.Channel{{ChannelBizID: "c1", Role: "collect", Type: "s7", Address: "x"}},
			Devices: []*platform.Device{
				{DeviceBizID: "d1", Sources: []*platform.Source{{
					SourceBizID: "s1", ChannelBizID: "c1",
					Points: []*platform.Point{{PointBizID: "p1", DataType: dt, Address: "DB1.DBD0"}},
				}}},
			},
		}
		if err := Validate(cfg); err != nil {
			t.Errorf("data_type=%s should be accepted, got %v", dt, err)
		}
	}
}

// TestValidate_ViaJSON 通过 JSON unmarshal 也能被验证。
func TestValidate_ViaJSON(t *testing.T) {
	raw := `{
		"version": 1,
		"channels": [{"channel_biz_id":"c1","role":"collect","type":"s7","address":"1.2.3.4:102"}],
		"devices": [{"device_biz_id":"d1",
			"sources":[{"source_biz_id":"s1","channel_biz_id":"c1","points":[{"point_biz_id":"p1","data_type":"int32","address":"DB1.DBD0"}]}]
		}]
	}`
	var cfg platform.Config
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("JSON 解析失败: %v", err)
	}
	if err := Validate(&cfg); err != nil {
		t.Errorf("valid JSON cfg 应通过校验: %v", err)
	}
}
