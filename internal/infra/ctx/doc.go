// Package ctx 上下文传播约定（Phase 1.9）。
//
// ## 规则
//
// 1. **业务路径**（collect/forward/write worker）必须传递 ctx：
//   - worker 启动时从外部 ctx 派生子 ctx
//   - 每次外部 IO（Read/Write/Probe/Send）必须传 ctx 或 ctx 派生的 child ctx
//   - 严禁在 worker 内用 context.Background() 替代 ctx
//
// 2. **Snapshot 路径**（registry.Snapshot）用 context.Background() 是允许的：
//   - Snapshot 是 web dashboard 主动调用，无 caller ctx
//   - 用 WithTimeout(1500ms / 3s) 防止拖死
//
// 3. **启动初始化**（db.ExecContext 跑 PRAGMA 等）可以用 context.Background()：
//   - 仅 startup 阶段，无 caller ctx
//   - 但若 startup ctx 已有（如 main 收到 SIGINT），应优先用 startup ctx
//
// 4. **测试代码**（_test.go）可以用 context.Background()：
//   - 测试无业务约束
//   - 用 t.Context() 优先（Go 1.24+）
//
// ## 检测
//
// 业务代码（`internal/app/` + `internal/infra/` 排除 `protocol/registry.go` 和 `web/web.go` 启动阶段）
// 若出现 `context.TODO()` 或 `context.Background()`，视为需审计。
package ctx
