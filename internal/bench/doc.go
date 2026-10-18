// Package bench 提供 plcbridge 的端到端压测工具：
//   - mockmodbus：多端口 Modbus TCP server，模拟 N 台 PLC 持有共享内存
//   - sinktarget：in-process target plugin，直接统计 Send 计数
//   - sampler：定期采样 runtime/registry 状态，输出 csv
//
// 用法见 cmd/bench/main.go。
package bench
