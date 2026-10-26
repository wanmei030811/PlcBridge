// mockinflux 是一个最小的 InfluxDB V1 替身，用于本地验证 Target 转发与探活。
//
// 用法：
//
//	mockinflux [addr]     默认 127.0.0.1:8086
//
// 支持：
//   - GET  /ping     → 204 No Content（探活）
//   - POST /write    → 204 No Content（写入）
//   - GET  /query    → 200 空结果集
package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
)

func main() {
	addr := "127.0.0.1:8086"
	if len(os.Args) > 1 {
		addr = os.Args[1]
	}
	var writes atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/ping", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/write", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		n := writes.Add(1)
		fmt.Printf("[%d] write db=%s bytes=%d\n", n, r.URL.Query().Get("db"), len(body))
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/query", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"statement_id":0,"series":[]}]}`))
	})
	srv := &http.Server{Addr: addr, Handler: mux}
	fmt.Printf("mock InfluxDB 已监听 %s（Ctrl+C 退出）\n", addr)
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "监听失败: %v\n", err)
			os.Exit(1)
		}
	}()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	_ = srv.Close()
}
