#!/usr/bin/env bash
set -Eeuo pipefail

repo_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
name="file-gateway-mysql-test-${RANDOM}"
# 安全（SEC-F6）：root 口令改为每次随机生成（openssl rand），
# 避免可预测口令配合固定回环端口（127.0.0.1:33316）被本机其它用户撞用。
command -v openssl >/dev/null || { echo "缺少 openssl，无法生成随机测试口令" >&2; exit 1; }
password="$(openssl rand -hex 16)"
port="${FILE_GATEWAY_TEST_PORT:-33316}"
cleanup() { docker rm -f "$name" >/dev/null 2>&1 || true; }
trap cleanup EXIT

# 安全（SEC-F6）：所有口令均以“按名转发”方式传入容器（-e 后不带值，取自本进程环境），
# 口令值不再进入 docker CLI 的 argv（/proc/*/cmdline）。
MYSQL_ROOT_PASSWORD="$password" docker run --detach --name "$name" -e MYSQL_ROOT_PASSWORD -e MYSQL_DATABASE=file_gateway_test -p "127.0.0.1:${port}:3306" mysql:8.4 >/dev/null
for _ in $(seq 1 60); do
  if MYSQL_PWD="$password" docker exec -e MYSQL_PWD "$name" mysqladmin ping -uroot --silent >/dev/null 2>&1; then break; fi
  sleep 1
done
MYSQL_PWD="$password" docker exec -e MYSQL_PWD "$name" mysqladmin ping -uroot --silent >/dev/null
for _ in $(seq 1 30); do
  if MYSQL_PWD="$password" docker run --rm -e MYSQL_PWD mysql:8.4 mysql -hhost.docker.internal -P"$port" -uroot -e 'SELECT 1' file_gateway_test >/dev/null 2>&1; then break; fi
  sleep 1
done
MYSQL_PWD="$password" docker run --rm -e MYSQL_PWD mysql:8.4 mysql -hhost.docker.internal -P"$port" -uroot -e 'SELECT 1' file_gateway_test >/dev/null
cd -- "$repo_dir"
# DSN 仅经环境变量交给 go test（不进入 argv）；口令为本次随机值，测试结束即失效。
FILE_GATEWAY_TEST_DSN="root:${password}@tcp(127.0.0.1:${port})/file_gateway_test?charset=utf8mb4&parseTime=true&loc=UTC" \
  GOCACHE="${GOCACHE:-/tmp/basic-platform-go-cache}" \
  go test -count=1 ./internal/platform/filetask/infrastructure
