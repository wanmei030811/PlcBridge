// gen_token.go: 生成 API 访问用 JWT token。
//
// 用法：
//
//	$env:PLCBRIDGE_API_SECRET = '<与部署相同的密钥>'
//	go run ./cmd/gen_token
//
// 输出一个有效期 1 小时的 HS256 Bearer Token，可用于调用 /api/v1/* 接口。
//
// 安全说明：
//
//	历史版本这里硬编码了 auth.DefaultSecret（源码公开的弱密钥），
//	生成出来的 token 任何人都能伪造 —— 等于没有鉴权。
//	现在改为读取与部署进程完全相同的密钥（auth.ServiceSecret），
//	密钥缺失时直接报错退出，绝不静默回退到弱默认值。
package main

import (
	"fmt"
	"os"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/plcbridge/plcbridge/internal/infra/auth"
)

func main() {
	// 检查密钥是否由环境变量提供。
	// 未提供时 auth.ServiceSecret() 会回退到 DefaultSecret —— 那是公开弱值，
	// 这里必须挡住这种情况，避免又签出一个"人人可伪造"的 token。
	if os.Getenv(auth.SecretEnv) == "" {
		fmt.Fprintf(os.Stderr,
			"错误: 未设置环境变量 %s，无法生成有效 token。\n"+
				"请先设置与部署进程相同的密钥，例如:\n"+
				"  $env:%s = '<你的密钥>'\n",
			auth.SecretEnv, auth.SecretEnv)
		os.Exit(1)
	}

	claims := jwt.MapClaims{
		"sub": "test-client",
		"iat": time.Now().Unix(),
		"exp": time.Now().Add(time.Hour).Unix(),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString(auth.ServiceSecret())
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	fmt.Println(signed)
}
