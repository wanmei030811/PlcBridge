// Package api 承载机器可读的 REST API 契约（OpenAPI 3.0.3）。
//
// 为什么单独建一个 Go 包而不是让 internal/infra/swagger 直接 embed：
//   - openapi.yaml 放在仓库根的 api/ 目录（对外可读性好，CI / 文档工具都按这个路径找）
//   - 但 go:embed 只能嵌入**当前包目录及其子目录**下的文件，不能跨目录向上取
//   - 所以由本包做唯一 embed 点，internal/infra/swagger 再从这里引用，
//     保证"spec 只有一份、且一定跟着二进制走"。
package api

import _ "embed"

// OpenAPIYAML 是 OpenAPI 3.0.3 契约文档（原文）。
//
// 为什么内嵌而不是运行时读文件：
//
//	运行时读文件要求部署时把 api/openapi.yaml 放到 exe 同级目录，
//	换个部署方式（容器 / systemd / 交叉编译）就 404，Swagger UI 白屏。
//	内嵌后二进制自包含，/api/openapi.yaml 恒可用。
//
//go:embed openapi.yaml
var OpenAPIYAML []byte
