// s7mock 是一个最小化的 S7 ISO-on-TCP 服务端，用于本地联调 / 演示。
//
// 用法：
//
//	s7mock [addr]        默认 127.0.0.1:102
//
// 它只实现到"让 gos7 客户端认为连接是活的、并且能读回 0 数据"这个程度，
// **不是**完整的 S7 协议实现，不能用于验证业务数据正确性。
//
// 用途：本地没有真实 PLC 时，验证 plcbridge 的采集链路 / 断线重连 /
// Target 转发是否正常工作。
package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/plcbridge/plcbridge/internal/infra/protocol/s7/s7mock"
)

func main() {
	addr := "127.0.0.1:102"
	if len(os.Args) > 1 {
		addr = os.Args[1]
	}
	srv, err := s7mock.Listen(addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "启动 mock S7 服务端失败: %v\n", err)
		os.Exit(1)
	}
	srv.Logf = func(format string, args ...any) {
		fmt.Printf(format+"\n", args...)
	}
	fmt.Printf("mock S7 服务端已监听 %s（Ctrl+C 退出）\n", srv.Addr())

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	fmt.Println("正在关闭…")
	_ = srv.Close()
}
