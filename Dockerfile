# syntax=docker/dockerfile:1
# 构建 API、Worker 与数据库迁移二进制文件，保持运行镜像最小化。
# build context: platform/（基础平台后端项目根），因此可直接 COPY 源码与 scripts/...
# An independent checkout uses the reviewed snapshot. A local workspace can
# override this stage with --build-context license_core=../license-core.
# Both inputs must match the tracked manifest; signing tools are never copied.
FROM scratch AS license_core
COPY third_party/license-core/go.mod third_party/license-core/clock.go third_party/license-core/diff.go third_party/license-core/license.go third_party/license-core/recovery.go /
COPY third_party/license-core/runtime/lock_unix.go third_party/license-core/runtime/snapshot.go third_party/license-core/runtime/state.go /runtime/

FROM golang:1.26.4-alpine AS license-core-verified

WORKDIR /src

COPY go.mod go.sum ./
COPY --from=license_core /go.mod /clock.go /diff.go /license.go /recovery.go ./third_party/license-core/
COPY --from=license_core /runtime/lock_unix.go /runtime/snapshot.go /runtime/state.go ./third_party/license-core/runtime/
COPY scripts/license-core-sync.sh scripts/license-core.sha256 ./scripts/
RUN sh scripts/license-core-sync.sh --check

FROM license-core-verified AS builder
ARG GOPROXY=https://goproxy.cn|https://proxy.golang.org|direct
ARG GOSUMDB=sum.golang.google.cn
ENV GOPROXY=${GOPROXY} \
    GOSUMDB=${GOSUMDB}
RUN set -eu; \
    for attempt in 1 2 3 4 5; do \
      if go mod download && go mod verify; then exit 0; fi; \
      echo "go module download failed (attempt ${attempt}/5)" >&2; \
      sleep $((attempt * 2)); \
    done; \
    exit 1

COPY cmd/ ./cmd/
COPY internal/ ./internal/
COPY migrations/ ./migrations/

# 镜像是离线交付的唯一数据库迁移载体。在生成任何可执行文件前校验
# 迁移连续性、已发布迁移 checksum 与历史种子退役规则，防止错误发布包进入服务器。
RUN CGO_ENABLED=0 go test ./migrations \
    && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' -o /out/api ./cmd/api \
    && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' -o /out/file-gateway ./cmd/file-gateway \
    && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' -o /out/file-inventory ./cmd/file-inventory \
    && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' -o /out/worker ./cmd/worker \
    && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' -o /out/migrate ./cmd/migrate \
    && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' -o /out/public-transport-coordinator ./cmd/public-transport-coordinator \
    && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' -o /out/bootstrap-admin ./cmd/bootstrap-admin \
    && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' -o /out/license-package ./cmd/license-package \
    && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' -o /out/license-install ./cmd/license-install \
    && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' -o /out/provision-iam-import-client ./cmd/provision-iam-import-client \
    && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' -o /out/subsystem-provisioner ./cmd/subsystem-provisioner

# File Gateway has an independent storage identity and is deployed as its own
# least-privilege image in both local Compose and production. Keep this target
# built from the same source and toolchain as the Platform image.
FROM alpine:3.21 AS file-gateway-runtime
RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -S -g 10001 filegateway \
    && adduser -S -D -H -u 10001 -G filegateway filegateway \
    && install -d -o filegateway -g filegateway -m 0750 /app/data/file-gateway
WORKDIR /app
COPY --from=builder /out/file-gateway ./file-gateway
USER filegateway
EXPOSE 8086
ENTRYPOINT ["./file-gateway"]

# API 运行容器不挂载 Docker Socket；同一镜像中的部署助手由独立服务运行。
# 部署助手需要 Docker CLI/Compose 与 bash 来执行经过白名单约束的本地编排和网关脚本。
FROM alpine:3.21

RUN apk add --no-cache bash ca-certificates curl docker-cli docker-cli-compose jq openssl su-exec tzdata util-linux wget \
    && addgroup -S -g 10001 filegateway \
    && adduser -S -D -H -u 10001 -G filegateway filegateway

WORKDIR /app

COPY --from=builder /out/api ./api
COPY --from=builder /out/file-gateway ./file-gateway
COPY --from=builder /out/file-inventory ./file-inventory
COPY --from=builder /out/worker ./worker
COPY --from=builder /out/migrate ./migrate
COPY --from=builder /out/public-transport-coordinator ./public-transport-coordinator
COPY --from=builder /out/bootstrap-admin ./bootstrap-admin
COPY --from=builder /out/license-package ./license-package
COPY --from=builder /out/license-install ./license-install
COPY --from=builder /out/provision-iam-import-client ./provision-iam-import-client
COPY --from=builder /out/subsystem-provisioner ./subsystem-provisioner
COPY docker-entrypoint.sh /usr/local/bin/basic-platform-entrypoint
COPY scripts/sync-contract-catalog.sh /usr/local/bin/sync-contract-catalog.sh
COPY scripts/sync-settlement-catalog.sh /usr/local/bin/sync-settlement-catalog.sh
COPY scripts/sync-project-catalog.sh /usr/local/bin/sync-project-catalog.sh

RUN chmod +x /usr/local/bin/basic-platform-entrypoint /usr/local/bin/sync-contract-catalog.sh /usr/local/bin/sync-settlement-catalog.sh /usr/local/bin/sync-project-catalog.sh

ENTRYPOINT ["/usr/local/bin/basic-platform-entrypoint"]
CMD ["./api"]
