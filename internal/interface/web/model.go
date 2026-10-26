package web

import (
	"github.com/plcbridge/plcbridge/internal/infra/timefmt"
)

// 所有时间字段在 DTO 里同时提供：
//   - 时间戳字段（CreatedAtMs / UpdatedAtMs *int64）：直接来自 SQLite（存的就是 ms）
//   - 人读字段（CreatedAt / UpdatedAt timefmt.BJTMillis）：JSON 序列化为北京时间字符串

// ChannelDTO 是 REST API 用 Channel 数据传输对象。
type ChannelDTO struct {
	ID                     int64             `json:"id"`
	ChannelBizID           string            `json:"channel_biz_id"`
	Role                   string            `json:"role"` // "collect" | "forward"
	Type                   string            `json:"type"` // 协议类型 s7 / opcua / mqtt / influxdb / modbus ...
	Address                string            `json:"address"`
	Config                 string            `json:"config"`
	HeartbeatAddress       string            `json:"heartbeat_address"`
	HeartbeatMaxIntervalMs int               `json:"heartbeat_max_interval_ms"`
	HeartbeatEnabled       bool              `json:"heartbeat_enabled"`
	HeartbeatDebounceMs    int               `json:"heartbeat_debounce_ms"`
	QueueSize              int               `json:"queue_size"`
	TimeoutMs              int               `json:"timeout_ms"`
	CreatedAtMs            int64             `json:"-"`
	UpdatedAtMs            int64             `json:"-"`
	CreatedAt              timefmt.BJTMillis `json:"created_at"`
	UpdatedAt              timefmt.BJTMillis `json:"updated_at"`
}

// DeviceDTO
type DeviceDTO struct {
	ID          int64             `json:"id"`
	DeviceBizID string            `json:"device_biz_id"`
	Name        string            `json:"name"`
	Enabled     bool              `json:"enabled"`
	CreatedAtMs int64             `json:"-"`
	UpdatedAtMs int64             `json:"-"`
	CreatedAt   timefmt.BJTMillis `json:"created_at"`
	UpdatedAt   timefmt.BJTMillis `json:"updated_at"`
	Sources     []SourceDTO       `json:"sources,omitempty"`
	Targets     []TargetDTO       `json:"targets,omitempty"`
}

// SourceDTO
type SourceDTO struct {
	ID                int64             `json:"id"`
	SourceBizID       string            `json:"source_biz_id"`
	DeviceID          int64             `json:"device_id"`
	DeviceBizID       string            `json:"device_biz_id,omitempty"`
	ChannelBizID      string            `json:"channel_biz_id"`
	CollectEnabled    bool              `json:"collect_enabled"`
	WriteEnabled      bool              `json:"write_enabled"`
	CollectIntervalMs int               `json:"collect_interval_ms"`
	TimeoutMs         int               `json:"timeout_ms"`
	CreatedAtMs       int64             `json:"-"`
	UpdatedAtMs       int64             `json:"-"`
	CreatedAt         timefmt.BJTMillis `json:"created_at"`
	UpdatedAt         timefmt.BJTMillis `json:"updated_at"`
	Points            []PointDTO        `json:"points,omitempty"`
}

// PointDTO
type PointDTO struct {
	ID             int64             `json:"id"`
	PointBizID     string            `json:"point_biz_id"`
	SourceID       int64             `json:"source_id"`
	Name           string            `json:"name"`
	Address        string            `json:"address"`
	DataType       string            `json:"data_type"`
	CollectEnabled bool              `json:"collect_enabled"`
	WriteEnabled   bool              `json:"write_enabled"`
	StringLength   int               `json:"string_length"` // 仅 data_type=="string" 时使用
	CreatedAtMs    int64             `json:"-"`
	UpdatedAtMs    int64             `json:"-"`
	CreatedAt      timefmt.BJTMillis `json:"created_at"`
	UpdatedAt      timefmt.BJTMillis `json:"updated_at"`
}

// TargetDTO
type TargetDTO struct {
	ID                  int64             `json:"id"`
	TargetBizID         string            `json:"target_biz_id"`
	DeviceID            int64             `json:"device_id"`
	DeviceBizID         string            `json:"device_biz_id,omitempty"`
	ForwardChannelBizID string            `json:"forward_channel_biz_id"`
	Topic               string            `json:"topic"`
	Format              string            `json:"format"`
	CreatedAtMs         int64             `json:"-"`
	UpdatedAtMs         int64             `json:"-"`
	CreatedAt           timefmt.BJTMillis `json:"created_at"`
	UpdatedAt           timefmt.BJTMillis `json:"updated_at"`
	PointBizIDs         []string          `json:"point_biz_ids,omitempty"`
}

// TokenDTO 内部 store 类型（包含明文 token），不应序列化给客户端
type TokenDTO struct {
	ID          int64              `json:"-"`
	Token       string             `json:"-"`
	Name        string             `json:"-"`
	CreatedAtMs int64              `json:"-"`
	RevokedMs   int64              `json:"-"`
	CreatedAt   timefmt.BJTMillis  `json:"-"`
	RevokedAt   *timefmt.BJTMillis `json:"-"`
}

// TokenView 列表展示视图，不含 token 明文
type TokenView struct {
	ID        int64              `json:"id"`
	Name      string             `json:"name"`
	CreatedAt timefmt.BJTMillis  `json:"created_at"`
	RevokedAt *timefmt.BJTMillis `json:"revoked_at,omitempty"`
}

// TokenCreated 创建响应（一次性返回明文 token）
type TokenCreated struct {
	ID    int64  `json:"id"`
	Token string `json:"token"`
	Name  string `json:"name"`
}

// AccountDTO（仅 /me 返回）
type AccountDTO struct {
	Username string `json:"username"`
}

// DashboardDTO 仪表盘聚合
type DashboardDTO struct {
	Channels        ChannelStats   `json:"channels"`
	Devices         DeviceStats    `json:"devices"`
	Sources         SourceStats    `json:"sources"`
	Points          PointStats     `json:"points"`
	Targets         TargetStats    `json:"targets"`
	TokensActive    int            `json:"tokens_active"`
	CollectLastHour CollectStat    `json:"collect_last_hour"`
	TargetsLastHour TargetSendStat `json:"targets_last_hour"`
	WriteLastHour   WriteStat      `json:"write_last_hour"`
	StorageBytes    int64          `json:"storage_bytes"`
}

type ChannelStats struct {
	Total   int `json:"total"`
	Collect int `json:"collect"`
	Forward int `json:"forward"`
}
type DeviceStats struct {
	Total   int `json:"total"`
	Enabled int `json:"enabled"`
}
type SourceStats struct {
	Total     int `json:"total"`
	CollectOn int `json:"collect_on"`
	WriteOn   int `json:"write_on"`
}
type PointStats struct {
	Total     int `json:"total"`
	CollectOn int `json:"collect_on"`
	WriteOn   int `json:"write_on"`
}
type TargetStats struct {
	Total int `json:"total"`
}
type CollectStat struct {
	TotalErrors int `json:"total_errors"`
}
type TargetSendStat struct {
	TotalErrors int `json:"total_errors"`
}
type WriteStat struct {
	TotalErrors int `json:"total_errors"`
}

// applyTimes 把 DTO 的 ms 时间戳填充到 BJTMillis 字段。
func applyTimes(c *ChannelDTO) {
	c.CreatedAt = timefmt.BJTMillis(c.CreatedAtMs)
	c.UpdatedAt = timefmt.BJTMillis(c.UpdatedAtMs)
}
func applyTimesD(d *DeviceDTO) {
	d.CreatedAt = timefmt.BJTMillis(d.CreatedAtMs)
	d.UpdatedAt = timefmt.BJTMillis(d.UpdatedAtMs)
	for i := range d.Sources {
		applyTimesS(&d.Sources[i])
	}
	for i := range d.Targets {
		applyTimesT(&d.Targets[i])
	}
}
func applyTimesS(s *SourceDTO) {
	s.CreatedAt = timefmt.BJTMillis(s.CreatedAtMs)
	s.UpdatedAt = timefmt.BJTMillis(s.UpdatedAtMs)
	for i := range s.Points {
		applyTimesP(&s.Points[i])
	}
}
func applyTimesP(p *PointDTO) {
	p.CreatedAt = timefmt.BJTMillis(p.CreatedAtMs)
	p.UpdatedAt = timefmt.BJTMillis(p.UpdatedAtMs)
}
func applyTimesT(t *TargetDTO) {
	t.CreatedAt = timefmt.BJTMillis(t.CreatedAtMs)
	t.UpdatedAt = timefmt.BJTMillis(t.UpdatedAtMs)
}

func applyTimesTok(t *TokenDTO) {
	// TokenDTO.RevokedAt 由 store 直接从 SQL NullInt64 转换填充；这里只占位避免外部调用 panic。
	_ = t
}
