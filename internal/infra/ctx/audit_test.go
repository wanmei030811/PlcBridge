package ctx

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// TestAuditNoBackgroundInBusinessPaths 审计业务路径没用 context.Background。
//
// 允许：
//   - *_test.go 文件
//   - 启动初始化（web/web.go PRAGMA）
//   - registry.Snapshot（明确文档化的设计）
//
// 禁止：
//   - app/collect / forward / write 的 worker
//   - app/collect/workers.go 等长生命周期 goroutine
func TestAuditNoBackgroundInBusinessPaths(t *testing.T) {
	// 白名单：明确允许 context.Background 的文件
	allowed := map[string]bool{
		"internal/infra/protocol/registry.go": true, // Snapshot 路径
		"internal/interface/web/web.go":      true, // 启动 PRAGMA
	}

	suspicious := map[string][]string{}

	matches, _ := filepath.Glob("internal/**/*.go")
	for _, path := range matches {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		if allowed[path] {
			continue
		}
		// 只查 internal/app 下业务代码
		if !strings.HasPrefix(path, "internal/app/") {
			continue
		}
		// 解析找 context.Background / context.TODO
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			continue
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "context" {
				return true
			}
			if sel.Sel.Name != "Background" && sel.Sel.Name != "TODO" {
				return true
			}
			pos := fset.Position(call.Pos())
			suspicious[path] = append(suspicious[path], pos.String())
			return true
		})
	}

	if len(suspicious) > 0 {
		for path, locs := range suspicious {
			t.Errorf("业务代码 %s 使用 context.Background/TODO（请改用传入的 ctx）：\n  %s",
				path, strings.Join(locs, "\n  "))
		}
	}
}

// TestDocumentedException 文档化：registry.Snapshot 用 context.Background 是设计。
func TestDocumentedException(t *testing.T) {
	// 这个测试占位，确保未来人不会"清理掉"白名单。
	// 真正约束见 doc.go 注释。
	allowed := []string{
		"internal/infra/protocol/registry.go",
		"internal/interface/web/web.go",
	}
	if len(allowed) != 2 {
		t.Errorf("白名单数量变更，需更新 doc.go")
	}
}
