// Plan2 主题 A 3.3: AllInfo() 单元测试。
//
// 目的：
//  1. 验证 AllInfo() 返回所有已注册 plugin 的元信息
//  2. 验证 role 字段恒为 "collect"
//  3. 验证 ConfigSchema 是合法 JSON
//  4. 验证返回顺序按 name 字典序
//  5. 验证 Unregister 后 AllInfo 立即反映
package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"testing"

	"github.com/plcbridge/plcbridge/internal/platform"
)

// TestAllInfo_ReturnsAllRegistered 验证已注册 plugin 的元信息结构。
//
// 注意：本测试依赖插件注册。由于 protocol 包的 _test 文件无法 import plugins 包
// （循环依赖），这里仅验证 AllInfo() 调用成功 + 结构正确。
// subpackage 测试（s7_test.go / modbus_tcp_test.go / opcua_test.go）通过 AddProtocol/RemoveProtocol
// 已覆盖注册场景。
func TestAllInfo_ReturnsAllRegistered(t *testing.T) {
	infos := AllInfo()
	// 已知 MVP 有 3 个协议；测试验证结构正确即可（数量可能因环境不同而异）
	if len(infos) == 0 {
		t.Skip("protocol registry is empty (plugins not imported in this test context)")
	}
	if len(infos) < 3 {
		t.Logf("AllInfo() len = %d (expected >= 3 for MVP protocols)", len(infos))
	}
	gotNames := make([]string, 0, len(infos))
	for _, info := range infos {
		gotNames = append(gotNames, info.Name)
	}
	expectedNames := []string{"modbus_tcp", "opcua", "s7"} // 字典序
	for i, want := range expectedNames {
		if i >= len(gotNames) {
			break
		}
		if gotNames[i] != want {
			t.Errorf("AllInfo()[%d].Name = %q, want %q (排序错误或漏列)", i, gotNames[i], want)
		}
	}
}

// TestAllInfo_RoleAlwaysCollect 验证 role 字段恒为 "collect"。
func TestAllInfo_RoleAlwaysCollect(t *testing.T) {
	for _, info := range AllInfo() {
		if info.Role != "collect" {
			t.Errorf("AllInfo() entry %q: role = %q, want \"collect\"", info.Name, info.Role)
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

// TestAllInfo_ConfigSchemaValidJSON 验证 ConfigSchema 是合法 JSON Schema 对象。
func TestAllInfo_ConfigSchemaValidJSON(t *testing.T) {
	for _, info := range AllInfo() {
		var schema map[string]any
		if err := json.Unmarshal(info.ConfigSchema, &schema); err != nil {
			t.Errorf("AllInfo() entry %q: config_schema is not valid JSON: %v", info.Name, err)
			continue
		}
		// 必须是 object 类型（JSON Schema 顶级）
		if schema["type"] != "object" {
			t.Errorf("AllInfo() entry %q: config_schema.type = %v, want \"object\"", info.Name, schema["type"])
		}
		// 必须有 properties
		if _, ok := schema["properties"]; !ok {
			t.Errorf("AllInfo() entry %q: config_schema missing \"properties\"", info.Name)
		}
	}
}

// TestAllInfo_CapabilitiesPerProtocol 验证 3 个 MVP 协议的能力声明正确（Plan2 主题 A 3.6）。
//
// 这是契约测试，未来加新协议时只要按 Capability 语义填即可。
func TestAllInfo_CapabilitiesPerProtocol(t *testing.T) {
	wantByName := map[string]Capability{
		"s7":         {SupportsBatchRead: true, SupportsHeartbeat: true, RequiresRackSlot: true},
		"modbus_tcp": {SupportsBatchRead: true, RequiresSlaveID: true},
		"opcua":      {SupportsBatchRead: true, SupportsHeartbeat: true},
	}
	for _, info := range AllInfo() {
		want, ok := wantByName[info.Name]
		if !ok {
			continue // fake 等非 MVP 协议跳过
		}
		if info.Capabilities != want {
			t.Errorf("AllInfo() entry %q: capabilities = %+v, want %+v", info.Name, info.Capabilities, want)
		}
	}
}

// TestAllInfo_CapabilitiesJSONTagSnakeCase 验证 capabilities 的 JSON 字段名是 snake_case。
//
// 契约（api/openapi.yaml 的 ProtocolInfo.capabilities + web/src/api/protocols.ts 的
// ProtocolCapability 都声明 snake_case）：Go 端必须输出 supports_batch_read 而不是
// "SupportsBatchRead"，否则前端读 supports_batch_read 永远是 undefined。
func TestAllInfo_CapabilitiesJSONTagSnakeCase(t *testing.T) {
	raw, err := json.Marshal(Capability{
		SupportsBatchRead: true,
		SupportsHeartbeat: true,
		RequiresRackSlot:  true,
		RequiresSlaveID:   true,
	})
	if err != nil {
		t.Fatalf("Marshal Capability 失败: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("Unmarshal Capability 失败: %v", err)
	}
	for _, key := range []string{
		"supports_batch_read",
		"supports_batch_write",
		"supports_heartbeat",
		"requires_rack_slot",
		"requires_slave_id",
	} {
		if _, ok := m[key]; !ok {
			t.Errorf("Capability JSON 缺 %q 字段，实际: %s", key, raw)
		}
	}
	// 不应出现 PascalCase 字段。
	for _, key := range []string{
		"SupportsBatchRead",
		"SupportsBatchWrite",
		"SupportsHeartbeat",
		"RequiresRackSlot",
		"RequiresSlaveID",
	} {
		if _, ok := m[key]; ok {
			t.Errorf("Capability JSON 不应出现 PascalCase 字段 %q，实际: %s", key, raw)
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
//
// 防御：防止未来有人加 cache layer 时漏掉 invalidate。
func TestAllInfo_ReflectsUnregister(t *testing.T) {
	// 注册临时 plugin
	tmpName := "allinfo_test_tmp"
	if err := Register(&fakePluginForTest{name: tmpName}); err != nil {
		// 已存在也无所谓，跳过
		t.Logf("Register tmp: %v (ignored)", err)
	}
	defer Unregister(tmpName) // 测试结束清理

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

// fakePluginForTest 用于 TestAllInfo_ReflectsUnregister 的最小 ProtocolPlugin 实现。
type fakePluginForTest struct {
	name string
}

func (f *fakePluginForTest) Name() string    { return f.name }
func (f *fakePluginForTest) Version() string { return "0.0.1-test" }
func (f *fakePluginForTest) ConfigSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{}}`)
}
func (f *fakePluginForTest) Capabilities() Capability          { return Capability{} }
func (f *fakePluginForTest) ParseConfig(_ []byte) (any, error) { return nil, nil }
func (f *fakePluginForTest) Init(_ context.Context) error      { return nil }
func (f *fakePluginForTest) Close() error                      { return nil }
func (f *fakePluginForTest) CreateChannel(_ *platform.Channel) (ProtocolChannel, error) {
	return nil, errors.New("fakePluginForTest not implemented")
}
