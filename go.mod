module github.com/plcbridge/plcbridge

go 1.24.0

require (
	github.com/eclipse/paho.golang v0.23.0
	github.com/eclipse/paho.mqtt.golang v1.5.1
	github.com/fsnotify/fsnotify v1.7.0
	github.com/gin-gonic/gin v1.9.1
	github.com/golang-jwt/jwt/v5 v5.2.0
	github.com/google/uuid v1.6.0
	github.com/gopcua/opcua v0.9.0
	github.com/grid-x/modbus v1.5.1
	github.com/influxdata/influxdb-client-go/v2 v2.14.0
	github.com/mattn/go-sqlite3 v1.14.16
	github.com/prometheus/client_golang v1.19.0
	github.com/robinson/gos7 v0.0.0-20260924061028-c54e650665ba
	github.com/rs/zerolog v1.32.0
	golang.org/x/crypto v0.42.0
	modernc.org/sqlite v1.28.0
)

require (
	github.com/apapsch/go-jsonmerge/v2 v2.0.0 // indirect
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/bytedance/sonic v1.10.0-rc3 // indirect
	github.com/cespare/xxhash/v2 v2.2.0 // indirect
	github.com/chenzhuoyu/base64x v0.0.0-20230717121745-296ad89f973d // indirect
	github.com/chenzhuoyu/iasm v0.9.0 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/gabriel-vasile/mimetype v1.4.2 // indirect
	github.com/gin-contrib/sse v0.1.0 // indirect
	github.com/go-playground/locales v0.14.1 // indirect
	github.com/go-playground/universal-translator v0.18.1 // indirect
	github.com/go-playground/validator/v10 v10.14.1 // indirect
	github.com/goccy/go-json v0.10.2 // indirect
	github.com/gorilla/websocket v1.5.3 // indirect
	github.com/grid-x/serial v0.0.0-20211107191517-583c7356b3aa // indirect
	github.com/influxdata/line-protocol v0.0.0-20200327222509-2487e7298839 // indirect
	github.com/json-iterator/go v1.1.12 // indirect
	github.com/kballard/go-shellquote v0.0.0-20180428030007-95032a82bc51 // indirect
	github.com/klauspost/cpuid/v2 v2.2.5 // indirect
	github.com/leodido/go-urn v1.2.4 // indirect
	github.com/mattn/go-colorable v0.1.13 // indirect
	github.com/mattn/go-isatty v0.0.19 // indirect
	github.com/modern-go/concurrent v0.0.0-20180306012644-bacd9c7ef1dd // indirect
	github.com/modern-go/reflect2 v1.0.2 // indirect
	github.com/oapi-codegen/runtime v1.0.0 // indirect
	github.com/pelletier/go-toml/v2 v2.0.9 // indirect
	github.com/prometheus/client_model v0.5.0 // indirect
	github.com/prometheus/common v0.48.0 // indirect
	github.com/prometheus/procfs v0.12.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/twitchyliquid64/golang-asm v0.15.1 // indirect
	github.com/ugorji/go/codec v1.2.11 // indirect
	golang.org/x/arch v0.4.0 // indirect
	golang.org/x/mod v0.27.0 // indirect
	golang.org/x/net v0.44.0 // indirect
	golang.org/x/sync v0.17.0 // indirect
	golang.org/x/sys v0.36.0 // indirect
	golang.org/x/text v0.29.0 // indirect
	golang.org/x/tools v0.36.0 // indirect
	google.golang.org/protobuf v1.33.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
	lukechampine.com/uint128 v1.2.0 // indirect
	modernc.org/cc/v3 v3.40.0 // indirect
	modernc.org/ccgo/v3 v3.16.13 // indirect
	modernc.org/libc v1.29.0 // indirect
	modernc.org/mathutil v1.6.0 // indirect
	modernc.org/memory v1.7.2 // indirect
	modernc.org/opt v0.1.3 // indirect
	modernc.org/strutil v1.1.3 // indirect
	modernc.org/token v1.0.1 // indirect
)

// === MVP 实际依赖 ===
//
// fsnotify/fsnotify       跨平台文件监听（Linux inotify / Windows ReadDirectoryChangesW）
// gin-gonic/gin            HTTP 路由（Web API + 监控指标）
// google/uuid              trace_id 生成
// gopcua/opcua             OPC UA 客户端（github.com/gopcua/opcua，纯 Go，TCP 二进制协议）
// eclipse/paho.golang      MQTT v5 客户端 + autopaho 自动重连（github.com/eclipse/paho.golang）
// eclipse/paho.mqtt.golang MQTT v3.1.1 客户端（github.com/eclipse/paho.mqtt.golang，v3.x broker 兼容）
// prometheus/client_golang 监控指标
// rs/zerolog               结构化日志
// modernc.org/sqlite       纯 Go SQLite（无需 CGo，跨平台编译简单）
// golang-jwt/jwt/v5        Token 鉴权（Web 调 plcbridge 的 Bearer Token）
//
// === 仅用于接口占位的间接依赖 ===
// （实际 MVP 不集成，通过 ProtocolPlugin / TargetPlugin 接口注册 stub 实现）
// - robinson/go-s7         （S7 协议，stub 由接口对齐，真实实现时启用）
