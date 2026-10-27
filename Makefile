# qqbot 构建入口
#
# 常用：
#   make build      当前平台构建到 bin/
#   make release    交叉编译 linux/amd64 + linux/arm64 到 dist/
#   make test       跑测试（含端到端联调）
#   make lint       格式检查 + vet
#   make mock       起本地假协议端 + 假模型（开发联调用）
#   make clean      清理产物
#
# 前端不需要构建：web/dist 下是手写并内嵌的静态文件，
# go build 时会通过 embed 打进二进制，所以这里没有 npm 步骤。

SHELL := /bin/bash
BINARY := qqbot
PKG := github.com/vex/qqbot

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -s -w \
	-X $(PKG)/internal/version.Version=$(VERSION) \
	-X $(PKG)/internal/version.Commit=$(COMMIT) \
	-X $(PKG)/internal/version.BuildDate=$(DATE)

GOFLAGS := -trimpath

# 本地联调根目录，可用 QQBOT_ROOT 覆盖
DEVROOT ?= .devroot

.PHONY: all build release test test-race vet lint tidy mock dev clean

all: build

## build: 构建当前平台二进制
build:
	CGO_ENABLED=0 go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o bin/$(BINARY) ./cmd/qqbot
	CGO_ENABLED=0 go build $(GOFLAGS) -o bin/mockonebot ./cmd/mockonebot

## release: 交叉编译 linux/amd64 与 linux/arm64
release:
	@mkdir -p dist
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o dist/$(BINARY)-linux-amd64 ./cmd/qqbot
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o dist/$(BINARY)-linux-arm64 ./cmd/qqbot
	@cd dist && sha256sum $(BINARY)-linux-* > checksums.txt && cat checksums.txt

## test: 运行测试（含 mock 端到端联调）
test:
	go test ./...

## test-race: 竞态检测。需要 CGO 与 gcc，本地没装时跳过
test-race:
	@if command -v gcc >/dev/null 2>&1; then \
		CGO_ENABLED=1 go test -race ./...; \
	else \
		echo "跳过：未找到 gcc（竞态检测需要 cgo）。改用 make test-remote。"; \
	fi

## test-remote: 同步到服务器跑完整测试（含 -race）。需 QQBOT_SSH_HOST
test-remote:
	./scripts/remote-test.sh

## vet: 静态检查
vet:
	go vet ./...

## lint: 格式与静态检查
lint: vet
	@out=$$(gofmt -l . | grep -v '^web/'); if [ -n "$$out" ]; then echo "以下文件未格式化:"; echo "$$out"; exit 1; fi

## tidy: 整理依赖
tidy:
	go mod tidy

## mock: 起假协议端 + 假模型 + 控制面板
mock: build
	./bin/mockonebot -db $(DEVROOT)/data/qqbot/qqbot.db

## dev: 用本地根目录跑起机器人（另开一个终端先跑 make mock）
dev: build
	QQBOT_ROOT=$(abspath $(DEVROOT)) ./bin/$(BINARY) run

## clean: 清理产物
# 不要删 web/dist：它是源码，不是构建产物
clean:
	rm -rf bin dist
