// Package fake provides a fake protocol plugin for Plan2 topic A 3.5 acceptance testing.
//
// It validates the contract: "adding a new protocol = new subpackage + import to _all.go".
// The production main process does not import this package, so fake won't appear in production.
package fake

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"time"

	"github.com/plcbridge/plcbridge/internal/infra/protocol"
	"github.com/plcbridge/plcbridge/internal/platform"
)

// fakePlugin is the ProtocolPlugin implementation for the fake protocol.
type fakePlugin struct{}

func newFakePlugin() protocol.ProtocolPlugin { return &fakePlugin{} }

func (p *fakePlugin) Name() string    { return "fake" }
func (p *fakePlugin) Version() string { return "0.0.1-test" }

// Capabilities for the fake protocol: all capabilities disabled (used for minimal path testing).
func (p *fakePlugin) Capabilities() protocol.Capability {
	return protocol.Capability{}
}

// ConfigSchema returns the minimal JSON Schema for the fake protocol.
func (p *fakePlugin) ConfigSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"start": {"type": "integer", "default": 0, "description": "起始值"},
			"step":  {"type": "integer", "default": 1, "description": "每次 BatchRead 增量"}
		},
		"required": []
	}`)
}

// ParseConfig accepts any JSON and returns a map.
func (p *fakePlugin) ParseConfig(raw []byte) (any, error) {
	if len(raw) == 0 {
		return map[string]any{"start": 0, "step": 1}, nil
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (p *fakePlugin) Init(_ context.Context) error { return nil }
func (p *fakePlugin) Close() error                 { return nil }

func (p *fakePlugin) CreateChannel(ch *platform.Channel) (protocol.ProtocolChannel, error) {
	if ch.Role != "collect" {
		return nil, errFakeRole{role: ch.Role}
	}
	cfg, _ := p.ParseConfig(ch.Config)
	return newFakeChannel(ch.ChannelBizID, cfg.(map[string]any)), nil
}

// errFakeRole is the explicit error type for role mismatch.
type errFakeRole struct{ role string }

func (e errFakeRole) Error() string {
	return "fake 协议仅支持 role=collect (got " + e.role + ")"
}

// === fakeChannel ===

type fakeChannel struct {
	channelID string
	cfg       map[string]any

	counter  atomic.Int64
	closed   atomic.Bool
	lastUsed atomic.Int64
}

func newFakeChannel(channelID string, cfg map[string]any) *fakeChannel {
	c := &fakeChannel{
		channelID: channelID,
		cfg:       cfg,
	}
	// Initial counter = start
	if start, ok := cfg["start"].(float64); ok {
		c.counter.Store(int64(start))
	}
	return c
}

func (c *fakeChannel) Connect(_ context.Context) error   { return nil }
func (c *fakeChannel) Reconnect(_ context.Context) error { return nil }
func (c *fakeChannel) Disconnect() error                 { return nil }

func (c *fakeChannel) HealthCheck(_ context.Context) error {
	if c.closed.Load() {
		return errFakeClosed{channelID: c.channelID}
	}
	c.touch()
	return nil
}

func (c *fakeChannel) Read(_ context.Context, _ string, dataType string) (protocol.Value, error) {
	if c.closed.Load() {
		return protocol.Value{}, errFakeClosed{channelID: c.channelID}
	}
	v := c.counter.Add(1)
	c.touch()
	return protocol.Value{DataType: dataType, Int64: v, Float64: float64(v), Quality: "good"}, nil
}

func (c *fakeChannel) BatchRead(_ context.Context, points []*platform.Point) (map[string]protocol.Value, error) {
	if c.closed.Load() {
		return nil, errFakeClosed{channelID: c.channelID}
	}
	out := make(map[string]protocol.Value, len(points))
	step := int64(1)
	if s, ok := c.cfg["step"].(float64); ok {
		step = int64(s)
	}
	if step == 0 {
		step = 1
	}
	for _, p := range points {
		if !p.CollectEnabled {
			continue
		}
		v := c.counter.Add(step)
		out[p.PointBizID] = protocol.Value{
			DataType: p.DataType,
			Int64:    v,
			Float64:  float64(v),
			Quality:  "good",
		}
	}
	c.touch()
	return out, nil
}

func (c *fakeChannel) Write(_ context.Context, _ string, value any, dataType string) error {
	if c.closed.Load() {
		return errFakeClosed{channelID: c.channelID}
	}
	// Fake protocol echoes the written value: store value in counter (so Read can retrieve it next time)
	if n, ok := value.(int64); ok {
		c.counter.Store(n)
	} else if n, ok := value.(float64); ok {
		c.counter.Store(int64(n))
	}
	c.touch()
	_ = dataType
	return nil
}

func (c *fakeChannel) BatchWrite(_ context.Context, writes []protocol.WriteOp) error {
	for _, w := range writes {
		if err := c.Write(context.Background(), w.Address, w.Value, w.DataType); err != nil {
			return err
		}
	}
	return nil
}

func (c *fakeChannel) IsConnected() bool { return !c.closed.Load() }

func (c *fakeChannel) LastActiveAt() time.Time {
	ns := c.lastUsed.Load()
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns)
}

func (c *fakeChannel) LastError() string      { return "" }
func (c *fakeChannel) LastErrorAt() time.Time { return time.Time{} }

func (c *fakeChannel) ProbeActive(_ context.Context) error {
	if c.closed.Load() {
		return errFakeClosed{channelID: c.channelID}
	}
	c.touch()
	return nil
}

func (c *fakeChannel) Stats() map[string]any {
	state := "Disconnected"
	if c.IsConnected() {
		state = "Connected"
	}
	return map[string]any{
		"connection_state": state,
		"channel_id":       c.channelID,
		"counter":          c.counter.Load(),
		"config":           c.cfg,
	}
}

func (c *fakeChannel) touch() { c.lastUsed.Store(time.Now().UnixNano()) }

type errFakeClosed struct{ channelID string }

func (e errFakeClosed) Error() string { return "fake channel " + e.channelID + " 已关闭" }

// init registers the fake protocol (triggered by _all.go or test imports).
func init() {
	if err := protocol.Register(newFakePlugin()); err != nil {
		panic(err)
	}
}
