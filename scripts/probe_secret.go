//go:build ignore

// probe.go：部署冒烟检查 —— 验证 core 与 web 是否共享同一个 API 密钥。
//
// 用法：
//   go run ./scripts/probe_secret.go <configDir> <coreAPIAddr>
//
// 检查两件事：
//  1. 用 <configDir>/api.secret 签发的 token 能访问 core 的受保护端点（→ 两进程同密钥）。
//  2. 用错误密钥签发的 token 被拒（→ 鉴权确实生效，不是形同虚设）。

package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Println("usage: probe_secret <configDir> <coreAPIAddr>")
		os.Exit(2)
	}
	configDir := os.Args[1]
	coreAddr := os.Args[2]

	raw, err := os.ReadFile(filepath.Join(configDir, "api.secret"))
	if err != nil {
		fmt.Printf("FAIL: 读取 api.secret 失败: %v\n", err)
		os.Exit(1)
	}
	secret := strings.TrimSpace(string(raw))
	fmt.Printf("secret file len=%d\n", len(secret))

	// 注意：必须挑一个**真正受保护**的路由。
	// core 有意公开 /health、/metrics、/runtime/channels、/api/v1/protocols
	// （只含 schema/状态，无敏感数据），拿它们测鉴权会误判成"鉴权失效"。
	// /api/v1/channels/runtime 在 auth.Middleware() 保护的分组里。
	url := "http://" + coreAddr + "/api/v1/channels/runtime"

	// 1. 正确密钥 → 应 200。
	code, body := call(url, secret)
	fmt.Printf("correct key -> %d %s\n", code, truncate(body, 120))
	if code != http.StatusOK {
		fmt.Println("FAIL: 正确密钥被拒 —— core 与 web 可能未共享密钥")
		os.Exit(1)
	}

	// 2. 错误密钥 → 应 401。
	code, body = call(url, "wrong-key-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	fmt.Printf("wrong key   -> %d %s\n", code, truncate(body, 120))
	if code != http.StatusUnauthorized {
		fmt.Println("FAIL: 错误密钥未被拒 —— 鉴权失效")
		os.Exit(1)
	}

	fmt.Println("PASS: core 接受共享密钥签发的 token，且拒绝错误密钥")
}

func call(url, secret string) (int, string) {
	claims := jwt.MapClaims{
		"sub": "probe",
		"iat": time.Now().Unix(),
		"exp": time.Now().Add(5 * time.Minute).Unix(),
	}
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(secret))
	if err != nil {
		return 0, "sign error: " + err.Error()
	}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return 0, "request error: " + err.Error()
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return 0, "do error: " + err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
