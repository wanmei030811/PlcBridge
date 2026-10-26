package mqtt

import (
	"encoding/json"
	"testing"

	"github.com/plcbridge/plcbridge/internal/platform"
)

// newTestChannelMQTT 构造一个 mqtt Channel，供 *_test.go 复用。
func newTestChannelMQTT(brokerURL string, cfg *MQTTConfig) *platform.Channel {
	raw, _ := json.Marshal(cfg)
	return &platform.Channel{
		ChannelBizID: "ch-mqtt-test",
		Type:         "mqtt",
		Role:         "forward",
		Address:      brokerURL,
		Config:       raw,
	}
}

func newTestTargetMQTT() *platform.Target {
	return &platform.Target{
		TargetBizID:         "tgt-mqtt-test",
		ForwardChannelBizID: "ch-mqtt-test",
		PointBizIDs:         []string{"pt-1"},
		Topic:               "plcbridge/test",
		Format:              "json",
	}
}

// makeMQTTTarget 走 plugin.CreateTarget 间接构造 *mqttTarget，供 *_test.go 使用。
func makeMQTTTarget(t *testing.T, brokerURL string, cfg *MQTTConfig) *mqttTarget {
	t.Helper()
	channel := newTestChannelMQTT(brokerURL, cfg)
	tg := newTestTargetMQTT()
	p := newPlugin()
	cli, err := p.CreateTarget(tg, channel)
	if err != nil {
		t.Fatalf("CreateTarget: %v", err)
	}
	tt, ok := cli.(*mqttTarget)
	if !ok {
		t.Fatalf("CreateTarget returned %T, want *mqttTarget", cli)
	}
	return tt
}
