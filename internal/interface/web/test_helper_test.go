package web

import "strconv"

// itoa 小工具，给测试用（避免拉入 strconv import）。
func itoa(n int64) string { return strconv.FormatInt(n, 10) }
