# 构建 API、Worker 与数据库迁移二进制文件，保持运行镜像最小化。
# build context: platform/（基础平台后端项目根），因此可直接 COPY 源码与 scripts/...
FROM golang:1.26.4-alpine AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ ./cmd/
COPY internal/ ./internal/
COPY migrations/ ./migrations/

RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' -o /out/api ./cmd/api \
    && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' -o /out/file-gateway ./cmd/file-gateway \
    && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' -o /out/worker ./cmd/worker \
    && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' -o /out/migrate ./cmd/migrate \
    && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' -o /out/bootstrap-admin ./cmd/bootstrap-admin \
    && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' -o /out/subsystem-provisioner ./cmd/subsystem-provisioner

# API 运行容器不挂载 Docker Socket；同一镜像中的部署助手由独立服务运行。
# 部署助手需要 Docker CLI/Compose 与 bash 来执行经过白名单约束的本地编排和网关脚本。
FROM alpine:3.21

RUN apk add --no-cache bash ca-certificates curl docker-cli docker-cli-compose jq openssl tzdata util-linux wget

WORKDIR /app

COPY --from=builder /out/api ./api
COPY --from=builder /out/file-gateway ./file-gateway
COPY --from=builder /out/worker ./worker
COPY --from=builder /out/migrate ./migrate
COPY --from=builder /out/bootstrap-admin ./bootstrap-admin
COPY --from=builder /out/subsystem-provisioner ./subsystem-provisioner
COPY docker-entrypoint.sh /usr/local/bin/basic-platform-entrypoint
COPY scripts/sync-contract-catalog.sh /usr/local/bin/sync-contract-catalog.sh
COPY scripts/sync-settlement-catalog.sh /usr/local/bin/sync-settlement-catalog.sh

RUN chmod +x /usr/local/bin/basic-platform-entrypoint /usr/local/bin/sync-contract-catalog.sh /usr/local/bin/sync-settlement-catalog.sh

ENTRYPOINT ["/usr/local/bin/basic-platform-entrypoint"]
CMD ["./api"]
