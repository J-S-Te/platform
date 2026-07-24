# syntax=docker/dockerfile:1

# 构建 API、Worker 与数据库迁移二进制文件，保持运行镜像最小化。
FROM golang:1.26.4-alpine AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . ./

RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' -o /out/api ./cmd/api \
    && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' -o /out/worker ./cmd/worker \
    && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' -o /out/migrate ./cmd/migrate

# 运行阶段只保留应用二进制文件、证书和生成 JWT 密钥需要的 openssl。
FROM alpine:3.21

RUN apk add --no-cache ca-certificates openssl tzdata

WORKDIR /app

COPY --from=builder /out/api ./api
COPY --from=builder /out/worker ./worker
COPY --from=builder /out/migrate ./migrate
COPY docker-entrypoint.sh /usr/local/bin/basic-platform-entrypoint

RUN chmod +x /usr/local/bin/basic-platform-entrypoint

ENTRYPOINT ["/usr/local/bin/basic-platform-entrypoint"]
CMD ["./api"]
