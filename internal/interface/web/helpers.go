package web

import "time"

// timeNowMs 是 time.Now 的毫秒别名（避免 model.go / sqlstore.go 同时 import "time"）。
func timeNowMs() int64 {
	return time.Now().UnixMilli()
}
