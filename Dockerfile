# ---- build ----
# 注：未使用 `# syntax=docker/dockerfile:1` 与 cache mount。
# BuildKit 的 cache mount 需要拉取外部 frontend 镜像，在受限网络下会直接失败。
# 用「先 copy go.mod 再 download」的分层缓存即可获得等效的依赖缓存效果，且零外部依赖。
FROM golang:1.25-alpine AS builder

ARG TARGETOS=linux
ARG TARGETARCH
ARG VERSION=dev
# 默认走国内镜像并回退 direct；CI 中可通过 build-arg 覆盖为官方源
ARG GOPROXY=https://goproxy.cn,https://proxy.golang.org,direct

WORKDIR /src

# 依赖层单独缓存：源码改动不触发重新下载
COPY go.mod go.sum ./
RUN GOPROXY=${GOPROXY} go mod download

COPY . .

# 静态链接 + 裁剪符号表，产物可直接跑在 scratch/distroless
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath \
      -ldflags "-s -w -X github.com/unirate/gateway/internal/config.buildVersion=${VERSION}" \
      -o /out/gateway ./cmd/gateway && \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "-s -w" \
      -o /out/mockupstream ./cmd/mockupstream

# ---- runtime ----
FROM alpine:3.20

# ca-certificates: 上游走 HTTPS 时必需
# tzdata: 自然日/周窗口按业务时区对齐需要时区库
# curl: 容器健康检查
RUN apk add --no-cache ca-certificates tzdata curl && \
    addgroup -g 10001 -S app && \
    adduser -u 10001 -S app -G app

COPY --from=builder /out/gateway /usr/local/bin/gateway
COPY --from=builder /out/mockupstream /usr/local/bin/mockupstream

# SQLite 数据目录必须在镜像里预建并 chown：
# Docker 把空命名卷挂到镜像中已存在的路径时会继承该路径的 owner/mode，
# 挂到不存在的路径则新建为 root:root —— 后者会让 UID 10001 无法建库，
# 且失败点在 store.Open 建表时，不在启动早期，排查成本高。
RUN mkdir -p /var/lib/unirate && chown 10001:10001 /var/lib/unirate

# 非 root 运行
USER 10001:10001

ENV PROXY_ADDR=:8080 \
    OBS_ADDR=:9091 \
    ADMIN_ADDR=127.0.0.1:9090 \
    LOG_LEVEL=info

EXPOSE 8080 9091

# 存活探针只看进程，绝不检查 Redis —— 否则依赖抖动会触发无意义重启
HEALTHCHECK --interval=10s --timeout=3s --start-period=5s --retries=3 \
  CMD curl -fsS http://127.0.0.1:9091/live || exit 1

ENTRYPOINT ["/usr/local/bin/gateway"]
