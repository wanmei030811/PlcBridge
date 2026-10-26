// 协议 / Target 元信息（Plan2 主题 A 3.4）。
//
// 数据来源：web 后端 /api/v1/protocols 代理 → plcbridge 主进程
// plcbridge 端协议元数据通过 protocol.AllInfo() / target.AllInfo() 返回（plugin init() 自注册）。
//
// 关键设计：
//   - 接口在前端启动时拉取一次（onMounted），缓存到 ref 供后续表单使用
//   - ConfigSchema 暂不用于动态表单（v0.1 仅用 name/role），但已在响应里准备好
//   - 拉取失败时降级为空数组 + 报警，不阻塞页面渲染
import { apiGet } from './client'

// 单个协议 / Target 的元信息。
// 与 Go 端 ProtocolInfo / TargetInfo 对齐。
export interface ProtocolCapability {
  supports_batch_read: boolean
  supports_batch_write: boolean
  supports_heartbeat: boolean
  requires_rack_slot: boolean
  requires_slave_id: boolean
  // 该 Target 是否支持历史数据查询（Go 端 target.Capability）。
  //
  // 只有 forward 角色的 Target 才有这个字段（collect 协议的 Capability 里没有），
  // 因此声明为可选。前端据此决定是否渲染「历史趋势」按钮。
  supports_history?: boolean
}

export interface ProtocolInfo {
  name: string         // 's7' / 'modbus_tcp' / 'opcua' / 'mqtt' / 'influxdb' / ...
  version: string      // plugin 自报版本
  role: 'collect' | 'forward'
  // JSON Schema（驱动 Web UI 表单生成）。
  // 暂未用，留作后续 Plan2 主题 C 5.2 改造 web/src/components/SchemaForm.vue。
  config_schema: Record<string, any>
  // 协议能力声明（Plan2 主题 A 3.6）：前端按 capabilities 决定是否显示 rack/slot 等必填项。
  capabilities: ProtocolCapability
}

export interface ProtocolsResp {
  collect: ProtocolInfo[]
  forward: ProtocolInfo[]
}

export function listProtocols() {
  // suppressGlobalError：协议元信息在页面挂载时拉取，失败时调用方降级为空数组
  // 并自行提示，不弹全局错误。
  return apiGet<ProtocolsResp>('/protocols', { suppressGlobalError: true })
}
