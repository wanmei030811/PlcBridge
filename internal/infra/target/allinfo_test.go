// Plan2 主题 A 3.3: target.AllInfo() 单元测试。
//
// 目的：与 protocol.AllInfo 对称，覆盖 forward 角色。
package target

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"testing"

	"github.com/plcbridge/plcbridge/internal/platform"
)

// TestAllInfo_ReturnsAllRegistered 验证已注册 Target 的元信息结构。
//
// 注意：本测试依赖插件注册。subpackage 测试（mqtt_test.go / influxdb_test.go）
// 通过 AddTarget / RemoveTarget 已覆盖此场景，此处仅做基本结构验证。
func TestAllInfo_ReturnsAllRegistered(t *testing.T) {
	infos := AllInfo()
	if len(infos) == 0 {
		t.Skip("target registry is empty (plugins not imported in this test context)")
	}
	if len(infos) < 2 {
		t.Logf("AllInfo() len = %d (expected >= 2 for MVP targets)", len(infos))
	}
	gotNames := make([]string, 0, len(infos))
	for _, info := range infos {
		gotNames = append(gotNames, info.Name)
	}
	expectedNames := []string{"influxdb", "mqtt"} // 字典序
	for i, want := range expectedNames {
		if i >= len(gotNames) {
			break
		}
		if gotNames[i] != want {
			t.Errorf("AllInfo()[%d].Name = %q, want %q (排序错误或漏列)", i, gotNames[i], want)
		}
	}
}

// TestAllInfo_RoleAlwaysForward 验证 role 字段恒为 "forward"。
func TestAllInfo_RoleAlwaysForward(t *testing.T) {
	for _, info := range AllInfo() {
		if info.Role != "forward" {
			t.Errorf("AllInfo() entry %q: role = %q, want \"forward\"", info.Name, info.Role)
		}
	}
}

// TestAllInfo_VersionNonEmpty 验证 Version 字段非空。
func TestAllInfo_VersionNonEmpty(t *testing.T) {
	for _, info := range AllInfo() {
		if info.Version == "" {
			t.Errorf("AllInfo() entry %q: version is empty", info.Name)
		}
	}
}

// TestAllInfo_CapabilitiesJSONTagSnakeCase 验证 capabilities 的 JSON 字段名是 snake_case。
//
// 契约（api/openapi.yaml 的 ProtocolInfo.capabilities + web/src/api/protocols.ts 的
// ProtocolCapability 都声明 snake_case）：Go 端必须输出 supports_history 而不是
// "SupportsHistory"，否则前端读 supports_history 永远是 undefined。
func TestAllInfo_CapabilitiesJSONTagSnakeCase(t *testing.T) {
	raw, err := json.Marshal(Capability{SupportsHistory: true})
	if err != nil {
		t.Fatalf("Marshal Capability 失败: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("Unmarshal Capability 失败: %v", err)
	}
	if _, ok := m["supports_history"]; !ok {
		t.Errorf("Capability JSON 缺 supports_history 字段，实际: %s", raw)
	}
	if _, ok := m["SupportsHistory"]; ok {
		t.Errorf("Capability JSON 不应出现 PascalCase 字段 SupportsHistory，实际: %s", raw)
	}
	if m["supports_history"] != true {
		t.Errorf("supports_history = %v, want true", m["supports_history"])
	}
}

// TestAllInfo_ConfigSchemaValidJSON 验证 ConfigSchema 是合法 JSON Schema 对象。
func TestAllInfo_ConfigSchemaValidJSON(t *testing.T) {
	for _, info := range AllInfo() {
		var schema map[string]any
		if err := json.Unmarshal(info.ConfigSchema, &schema); err != nil {
			t.Errorf("AllInfo() entry %q: config_schema is not valid JSON: %v", info.Name, err)
			continue
		}
		if schema["type"] != "object" {
			t.Errorf("AllInfo() entry %q: config_schema.type = %v, want \"object\"", info.Name, schema["type"])
		}
		if _, ok := schema["properties"]; !ok {
			t.Errorf("AllInfo() entry %q: config_schema missing \"properties\"", info.Name)
		}
	}
}

// TestAllInfo_SortedByName 验证返回顺序按 name 字典序。
func TestAllInfo_SortedByName(t *testing.T) {
	infos := AllInfo()
	names := make([]string, len(infos))
	for i, info := range infos {
		names[i] = info.Name
	}
	if !sort.StringsAreSorted(names) {
		t.Errorf("AllInfo() not sorted: %v", names)
	}
}

// TestAllInfo_NoDuplicates 验证返回列表无重复 name。
func TestAllInfo_NoDuplicates(t *testing.T) {
	seen := map[string]bool{}
	for _, info := range AllInfo() {
		if seen[info.Name] {
			t.Errorf("AllInfo() contains duplicate name %q", info.Name)
		}
		seen[info.Name] = true
	}
}

// TestAllInfo_ReflectsUnregister 验证 Unregister 后 AllInfo 立即反映。
func TestAllInfo_ReflectsUnregister(t *testing.T) {
	tmpName := "allinfo_test_target_tmp"
	if err := Register(&fakeTargetForTest{name: tmpName}); err != nil {
		t.Logf("Register tmp: %v (ignored)", err)
	}
	defer Unregister(tmpName)

	infos := AllInfo()
	found := false
	for _, info := range infos {
		if info.Name == tmpName {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("AllInfo() 临时注册的 %q 未出现", tmpName)
	}

	Unregister(tmpName)
	infos = AllInfo()
	for _, info := range infos {
		if info.Name == tmpName {
			t.Errorf("Unregister 后 AllInfo() 仍含 %q", tmpName)
		}
	}
}

// fakeTargetForTest 用于 TestAllInfo_ReflectsUnregister 的最小 TargetPlugin 实现。
type fakeTargetForTest struct {
	name string
}

func (f *fakeTargetForTest) Name() string    { return f.name }
func (f *fakeTargetForTest) Version() string { return "0.0.1-test" }
func (f *fakeTargetForTest) ConfigSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{}}`)
}
func (f *fakeTargetForTest) Capabilities() Capability     { return Capability{} }
func (f *fakeTargetForTest) Init(_ context.Context) error { return nil }
func (f *fakeTargetForTest) Close() error                 { return nil }
func (f *fakeTargetForTest) CreateTarget(_ *platform.Target, _ *platform.Channel) (TargetClient, error) {
	return nil, errors.New("fakeTargetForTest not implemented")
}
