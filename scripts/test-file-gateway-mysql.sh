#!/usr/bin/env bash
set -Eeuo pipefail

repo_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
name="file-gateway-mysql-test-${RANDOM}"
password="file_gateway_test_password"
port="${FILE_GATEWAY_TEST_PORT:-33316}"
cleanup() { docker rm -f "$name" >/dev/null 2>&1 || true; }
trap cleanup EXIT

docker run --detach --name "$name" -e MYSQL_ROOT_PASSWORD="$password" -e MYSQL_DATABASE=file_gateway_test -p "127.0.0.1:${port}:3306" mysql:8.4 >/dev/null
for _ in $(seq 1 60); do
  if docker exec "$name" mysqladmin ping -uroot -p"$password" --silent >/dev/null 2>&1; then break; fi
  sleep 1
done
docker exec "$name" mysqladmin ping -uroot -p"$password" --silent >/dev/null
for _ in $(seq 1 30); do
  if docker run --rm mysql:8.4 mysql -hhost.docker.internal -P"$port" -uroot -p"$password" -e 'SELECT 1' file_gateway_test >/dev/null 2>&1; then break; fi
  sleep 1
done
docker run --rm mysql:8.4 mysql -hhost.docker.internal -P"$port" -uroot -p"$password" -e 'SELECT 1' file_gateway_test >/dev/null
cd -- "$repo_dir"
FILE_GATEWAY_TEST_DSN="root:${password}@tcp(127.0.0.1:${port})/file_gateway_test?charset=utf8mb4&parseTime=true&loc=UTC" \
  GOCACHE="${GOCACHE:-/tmp/basic-platform-go-cache}" \
  go test -count=1 ./internal/platform/filetask/infrastructure
