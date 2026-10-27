# syntax=docker/dockerfile:1
#
# qqbot 运行镜像。
#
# 两段构建：构建段用 golang 镜像（服务器上不需要装 Go），运行段只留二进制。
# CGO_ENABLED=0 是刻意的：SQLite 用纯 Go 实现（modernc.org/sqlite），
# 交叉编译不需要 C 工具链，运行镜像也不需要 glibc。
#
# 前端是手写的静态文件，通过 //go:embed 打进二进制，
# 所以运行段不需要任何静态资源目录。

# ---------- 构建段 ----------
FROM golang:1.27-alpine AS build

# ARG 必须在使用它的 RUN 之前声明，否则 compose 传进来的值会被静默丢弃，
# 回落到默认的 proxy.golang.org——国内机器上这一步会直接超时。
ARG GOPROXY=https://proxy.golang.org,direct

# 纯 Go 构建：不引入 C 工具链，产出的二进制可以扔进任意 Linux
ENV CGO_ENABLED=0

WORKDIR /src

# 先只拷依赖清单，让 go mod download 这层能命中缓存：
# 改业务代码时不必重新下载依赖（1 核机器上这一步很贵）。
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT=none
ARG BUILD_DATE=unknown

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go build \
      -trimpath \
      -ldflags "-s -w \
        -X github.com/vex/qqbot/internal/version.Version=${VERSION} \
        -X github.com/vex/qqbot/internal/version.Commit=${COMMIT} \
        -X github.com/vex/qqbot/internal/version.BuildDate=${BUILD_DATE}" \
      -o /out/qqbot ./cmd/qqbot

# ---------- 运行段 ----------
FROM alpine:3.21

# ca-certificates：调 DeepSeek HTTPS 必需
# tzdata：日志与人格变量里的时间要按本地时区显示
# su-exec：不需要，容器直接以非 root 用户启动
RUN apk add --no-cache ca-certificates tzdata \
 && adduser -D -u 10001 -h /opt/qqbot -s /sbin/nologin qqbot \
 && mkdir -p /opt/qqbot/data /opt/qqbot/etc \
 && chown -R 10001:10001 /opt/qqbot

COPY --from=build /out/qqbot /usr/local/bin/qqbot

ENV TZ=Asia/Shanghai \
    QQBOT_ROOT=/opt/qqbot

WORKDIR /opt/qqbot
USER qqbot

EXPOSE 5333

# 容器只跑前台，生命周期交给 compose 管；
# 不要在这里加健康检查脚本——镜像里没有 curl，用 compose 的 healthcheck 走 wget。
ENTRYPOINT ["/usr/local/bin/qqbot"]
CMD ["run"]
