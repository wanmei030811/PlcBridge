// gen_token.go: 生成测试 JWT token
package main

import (
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func main() {
	claims := jwt.MapClaims{
		"sub": "test-client",
		"exp": time.Now().Add(time.Hour).Unix(),
		"iat": time.Now().Unix(),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString([]byte("plcbridge-default-secret-change-me"))
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Println(signed)
}
