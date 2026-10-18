# PlcBridge build / run helpers
#
# 重要约定：所有可执行产物统一输出到 bin/ 目录。
# 历史教训（2026-10-13）：曾有 `go build` 不带 -o 跑到根目录生成 plcbridge.exe / plcbridge-web.exe，
# 出现根目录和 bin/ 都有但只有 bin/ 真正被 supervisor 使用的混乱局面。
# 此 Makefile 把"输出到 bin/"作为唯一标准，避免再发生。
#
# 常用命令：
#   make help         查看所有命令
#   make build        编译 plcbridge + plcbridge-web 到 bin/
#   make web          只编译 web
#   make core         只编译 core
#   make run-web      启动 web（前台；测试用）
#   make clean        清掉 bin/ 下产物
#   make lint         跑项目级检查（根目录禁出现 plcbridge.exe / plcbridge-web.exe）

GO          ?= go
BIN         := bin
VERSION     := $(shell powershell -NoProfile -Command "(Get-Date -Format 'yyyyMMdd-HHmmss')" 2>/dev/null || date +%Y%m%d-%H%M%S)
LDFLAGS     := -X github.com/plcbridge/plcbridge/internal/interface/web.Version=v1.0.0-$(VERSION)

.DEFAULT_GOAL := help
.PHONY: help all build web core run-web run-core clean tidy vet test lint

## help: 显示所有命令
help:
	@powershell -NoProfile -Command "Get-Content Makefile | Select-String -Pattern '^[a-zA-Z][a-zA-Z_-]*:.*##' | ForEach-Object { $$line = $$_.Line; $$colon = $$line.IndexOf(':'); $$name = $$line.Substring(0,$$colon).Trim(); $$desc = $$line.Substring($$colon+1).Trim() -replace '^##\s*',''; Write-Host (\"  make {0,-12} {1}\" -f $$name,$$desc) }"

## all: build + lint
all: build lint

## build: 编译 web + core 到 bin/
build: $(BIN)
	@echo ">> building plcbridge-web -> $(BIN)/plcbridge-web.exe"
	$(GO) build $(LDFLAGS) -o $(BIN)/plcbridge-web.exe ./cmd/plcbridge-web
	@echo ">> building plcbridge -> $(BIN)/plcbridge.exe"
	$(GO) build -o $(BIN)/plcbridge.exe ./cmd/plcbridge
	@echo ">> done. binaries in $(BIN)/"

## web: 只编译 web
web: $(BIN)
	$(GO) build $(LDFLAGS) -o $(BIN)/plcbridge-web.exe ./cmd/plcbridge-web

## core: 只编译 core
core: $(BIN)
	$(GO) build -o $(BIN)/plcbridge.exe ./cmd/plcbridge

## run-web: 前台启动 web（测试用，会持有终端）
run-web: build
	./$(BIN)/plcbridge-web.exe --config F:\Project\PlcBridge\config --addr :8080

## run-core: 前台启动 core
run-core: build
	./$(BIN)/plcbridge.exe --config F:\Project\PlcBridge\config

## vet: 静态检查
vet:
	$(GO) vet ./...

## test: 跑测试
test:
	$(GO) test ./...

## lint: 跑项目级检查（根目录禁出现 plcbridge.exe/plcbridge-web.exe 等）
lint:
	@powershell -NoProfile -ExecutionPolicy Bypass -File scripts\check-no-root-exe.ps1

## tidy: 整理 go.mod
tidy:
	$(GO) mod tidy

## clean: 清掉 bin/ 下产物
clean:
	@if exist $(BIN) (powershell -NoProfile -Command "Get-ChildItem '$(BIN)' -Filter '*.exe' -ErrorAction SilentlyContinue | Remove-Item -Force"; rmdir $(BIN) 2>nul & echo "cleaned $(BIN)/")
	@echo ">> if root .\\plcbridge.exe or .\\plcbridge-web.exe reappear, run:"
	@echo "   powershell -Command \"Remove-Item -Force plcbridge.exe,plcbridge-web.exe -ErrorAction SilentlyContinue\""

$(BIN):
	mkdir $(BIN)
