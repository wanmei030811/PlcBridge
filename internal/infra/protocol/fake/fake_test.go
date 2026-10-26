// Plan2 主题 A 3.5：fake 协议验收测试。
//
// 本文件是"加新协议零改动"承诺的**端到端证明**：
//  1. 验证 protocol.AllInfo() 含 fake（注册表自注册）
//  2. 验证 protocol.Get("fake") 拿得到 plugin
//  3. 验证 CreateChannel → BatchRead 跑通
//  4. 验证 GET /api/v1/protocols HTTP 接口含 fake
//
// **关键**: 本测试文件**不修改** main.go、protocol/protocol.go、handler、前端。
// 加新协议 = 加一个子包 + 1 行 import 即可。
package fake

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/plcbridge/plcbridge/internal/infra/protocol"
	"github.com/plcbridge/plcbridge/internal/platform"
)

// TestFake_RegisteredInAllInfo 验证 fake 在 AllInfo() 中出现。
func TestFake_RegisteredInAllInfo(t *testing.T) {
	infos := protocol.AllInfo()

	var fakeInfo *protocol.ProtocolInfo
	for i := range infos {
		if infos[i].Name == "fake" {
			fakeInfo = &infos[i]
			break
		}
	}
	if fakeInfo == nil {
		t.Fatalf("AllInfo() 缺 fake（plugin init() 未触发？）")
	}
	if fakeInfo.Role != "collect" {
		t.Errorf("fake.role = %q, want \"collect\"", fakeInfo.Role)
	}
	if fakeInfo.Version == "" {
		t.Errorf("fake.version is empty")
	}
	if len(fakeInfo.ConfigSchema) == 0 {
		t.Errorf("fake.config_schema is empty")
	}
	// Capabilities 字段必须存在（Plan2 主题 A 3.6 契约）
	if fakeInfo.Capabilities.SupportsBatchRead || fakeInfo.Capabilities.RequiresRackSlot {
		t.Errorf("fake capabilities 应为全零值: %+v", fakeInfo.Capabilities)
	}
	// ConfigSchema 必须是合法 JSON
	var schema map[string]any
	if err := json.Unmarshal(fakeInfo.ConfigSchema, &schema); err != nil {
		t.Errorf("fake.config_schema 非法 JSON: %v", err)
	}
}

// TestFake_GetReturnsPlugin 验证 protocol.Get("fake") 返回 plugin。
func TestFake_GetReturnsPlugin(t *testing.T) {
	p, err := protocol.Get("fake")
	if err != nil {
		t.Fatalf("Get(\"fake\") failed: %v", err)
	}
	if p.Name() != "fake" {
		t.Errorf("plugin.Name() = %q, want \"fake\"", p.Name())
	}
}

// TestFake_CreateChannel_BatchRead 验证完整业务流。
//
// 流程：plugin → CreateChannel → BatchRead → 拿到 counter 递增值。
func TestFake_CreateChannel_BatchRead(t *testing.T) {
	p, err := protocol.Get("fake")
	if err != nil {
		t.Fatalf("Get(\"fake\") failed: %v", err)
	}

	ch := &platform.Channel{
		ChannelBizID: "fake-test-1",
		Role:         "collect",
		Type:         "fake",
		Address:      "memory://", // 故意占位，fake 不连网
		Config:       json.RawMessage(`{"start": 100, "step": 5}`),
	}
	cli, err := p.CreateChannel(ch)
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	if err := cli.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	points := []*platform.Point{
		{PointBizID: "p1", DataType: "int32", CollectEnabled: true},
		{PointBizID: "p2", DataType: "int32", CollectEnabled: true},
	}
	result, err := cli.BatchRead(context.Background(), points)
	if err != nil {
		t.Fatalf("BatchRead: %v", err)
	}
	if len(result) != 2 {
		t.Errorf("BatchRead 返回 %d 个值, want 2", len(result))
	}
	// 起始 100，step 5，2 个点位：p1 拿 105, p2 拿 110
	v1, ok := result["p1"]
	if !ok {
		t.Fatalf("result 缺 p1")
	}
	if v1.Int64 != 105 {
		t.Errorf("p1 = %d, want 105 (start 100 + step 5)", v1.Int64)
	}
	if v1.Quality != "good" {
		t.Errorf("p1.quality = %q, want \"good\"", v1.Quality)
	}
	v2, ok := result["p2"]
	if !ok {
		t.Fatalf("result 缺 p2")
	}
	if v2.Int64 != 110 {
		t.Errorf("p2 = %d, want 110 (start 100 + 2*step 5)", v2.Int64)
	}

	// Stats 含 connection_state + counter
	stats := cli.Stats()
	if stats["connection_state"] != "Connected" {
		t.Errorf("stats.connection_state = %v, want \"Connected\"", stats["connection_state"])
	}
	if _, ok := stats["counter"]; !ok {
		t.Errorf("stats 缺 counter 字段")
	}
}

// TestFake_CreateChannel_RejectsForwardRole 验证 fake 仅支持 collect。
func TestFake_CreateChannel_RejectsForwardRole(t *testing.T) {
	p, err := protocol.Get("fake")
	if err != nil {
		t.Fatalf("Get(\"fake\") failed: %v", err)
	}
	ch := &platform.Channel{
		ChannelBizID: "fake-test-2",
		Role:         "forward", // ← 故意
		Type:         "fake",
		Address:      "memory://",
	}
	if _, err := p.CreateChannel(ch); err == nil {
		t.Errorf("CreateChannel(role=forward) 应报错")
	}
}

// TestFake_HTTPListProtocolsContainsFake 端到端：JSON 序列化烟雾测试。
//
// 完整 HTTP 端到端（routes() + ServeHTTP）在 internal/infra/api/protocols_test.go
// 覆盖（含 fake 断言）；本测试专注验证 ProtocolInfo JSON 序列化形状正确。
func TestFake_HTTPListProtocolsContainsFake(t *testing.T) {
	infos := protocol.AllInfo()
	var fakeInfo *protocol.ProtocolInfo
	for i := range infos {
		if infos[i].Name == "fake" {
			fakeInfo = &infos[i]
			break
		}
	}
	if fakeInfo == nil {
		t.Fatalf("AllInfo() 不含 fake（HTTP /api/v1/protocols 也不会含）")
	}

	b, err := json.Marshal(fakeInfo)
	if err != nil {
		t.Fatalf("ProtocolInfo Marshal 失败: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("ProtocolInfo Unmarshal 失败: %v", err)
	}
	if decoded["name"] != "fake" {
		t.Errorf("JSON.name = %v, want \"fake\"", decoded["name"])
	}
	if decoded["role"] != "collect" {
		t.Errorf("JSON.role = %v, want \"collect\"", decoded["role"])
	}
	if _, ok := decoded["config_schema"]; !ok {
		t.Errorf("JSON 缺 config_schema 字段")
	}
	if _, ok := decoded["capabilities"]; !ok {
		t.Errorf("JSON 缺 capabilities 字段（Plan2 3.6 契约）")
	}
}
