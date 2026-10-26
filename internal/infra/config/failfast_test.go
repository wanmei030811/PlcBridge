// Plan2 主题 D 6.1：启动 fail-fast 校验测试。
//
// 验证：LoadWithRetry / TryLoad 在配置错误（缺字段、跨表引用错）时立即返回 error，
// 不掩盖、不继续启动。
package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestTryLoad_FailFast_OnInvalidConfig 验证非法配置立即报错。
func TestTryLoad_FailFast_OnInvalidConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")

	// 故意写跨表引用错的 config：source 引用不存在的 channel。
	// 注：Device 没有任何 Source 本身是合法的（新建的设备允许先不配点），
	// 所以这里用"source 引用不存在的 channel"来触发 fail-fast。
	badJSON := `{
		"version": 1,
		"channels": [],
		"devices": [
			{
				"device_biz_id": "dev-orphan",
				"name": "P",
				"enabled": true,
				"sources": [
					{"source_biz_id": "src-1", "channel_biz_id": "no-such-channel", "collect_enabled": true}
				]
			}
		]
	}`
	if err := os.WriteFile(cfgPath, []byte(badJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	store := NewStore(dir)
	_, err := store.TryLoad()
	if err == nil {
		t.Fatal("应 fail-fast 返回 error")
	}
	// 不应崩溃到 panic，错误应可读
	t.Logf("got expected error: %v", err)
}

// TestTryLoad_FailFast_OnMalformedJSON 验证 JSON 格式错。
func TestTryLoad_FailFast_OnMalformedJSON(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{not valid json`), 0o644); err != nil {
		t.Fatal(err)
	}
	store := NewStore(dir)
	if _, err := store.TryLoad(); err == nil {
		t.Fatal("坏 JSON 应立即报错")
	}
}

// TestTryLoad_FailFast_OnUnknownProtocol 验证 channel.type 未知立即报错。
func TestTryLoad_FailFast_OnUnknownProtocol(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	// 故意写未注册的 type = "made-up"
	badJSON := `{
		"version": 1,
		"channels": [
			{"id": 1, "channel_biz_id": "ch-bad", "role": "collect", "type": "made-up", "address": "x"}
		],
		"devices": [],
		"sources": [],
		"points": [],
		"targets": []
	}`
	if err := os.WriteFile(cfgPath, []byte(badJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	store := NewStore(dir)
	_, err := store.TryLoad()
	if err == nil {
		t.Fatal("未注册 type 应 fail-fast")
	}
}
