#!/usr/bin/env bash
set -Eeuo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
platform_root="$(cd -- "$script_dir/../../.." && pwd)"
local_compose="$platform_root/compose.local.yaml"
production_compose="$platform_root/deploy/production/docker-compose.yml"
local_runner="$platform_root/scripts/docker-local.sh"
production_runner="$platform_root/deploy/production/bin/deploy-service.sh"
production_cli="$platform_root/deploy/production/bin/deploy.sh"
production_dockerfile="$platform_root/Dockerfile"
offline_builder="$platform_root/deploy/production/bin/build-offline-packages.sh"

contains() {
  local file="$1" pattern="$2" description="$3"
  grep -Fq -- "$pattern" "$file" || {
    printf 'missing %s in %s\n' "$description" "$file" >&2
    exit 1
  }
}

contains "$local_compose" 'image: basic-platform/file-gateway:local' 'local gateway dedicated image'
contains "$local_compose" 'target: file-gateway-runtime' 'local gateway uses dedicated runtime image'
contains "$local_compose" 'condition: service_healthy' 'local health-gated dependencies'
contains "$local_compose" 'file_gateway_mysql_local_data:/var/lib/mysql' 'local gateway database persistence'
contains "$local_compose" 'file_gateway_local_data:/app/data/file-gateway' 'local gateway object persistence'
contains "$local_runner" 'compose_run up -d --wait mysql file-gateway-mysql contract-mysql customer-mysql settlement-mysql temporal' 'default local startup of gateway database'
contains "$local_runner" 'compose_up_wait "统一文件网关" file-gateway' 'default local startup of gateway process'
contains "$local_runner" 'verify_local_file_gateway' 'local gateway runtime verification'
contains "$local_runner" '检测到已有 File Gateway MySQL 数据卷' 'protection against rotating credentials on a retained database volume'
contains "$production_compose" 'image: ${FILE_GATEWAY_IMAGE:?FILE_GATEWAY_IMAGE must be set}' 'production gateway immutable image reference'
contains "$production_dockerfile" 'AS file-gateway-runtime' 'dedicated file gateway image target'
contains "$offline_builder" 'uip-package/file-gateway:$version' 'file gateway included in common infrastructure package'
contains "$production_cli" 'FILE_GATEWAY_IMAGE_REF' 'common import records file gateway digest'
contains "$production_runner" 'File Gateway 未通过 /readyz 健康检查；拒绝继续启动平台 API' 'production API startup gate'
contains "$production_cli" 'platform-api 容器无法访问 http://file-gateway:8086/readyz' 'production Docker-network verification'
contains "$production_cli" 'File Gateway 代理到达受保护 API' 'production frontend proxy verification'

local_api_block="$(awk '/^  api:$/ { inside=1 } /^  subsystem-provisioner:$/ { inside=0 } inside' "$local_compose")"
local_frontend_block="$(awk '/^  frontend:$/ { inside=1 } /^volumes:$/ { inside=0 } inside' "$local_compose")"
production_api_block="$(awk '/^  platform-api:$/ { inside=1 } /^  platform-worker:$/ { inside=0 } inside' "$production_compose")"
[[ "$local_api_block" == *$'file-gateway:\n        condition: service_healthy'* ]] || { echo 'local api lacks healthy File Gateway dependency' >&2; exit 1; }
[[ "$local_frontend_block" == *$'file-gateway:\n        condition: service_healthy'* ]] || { echo 'local frontend lacks healthy File Gateway dependency' >&2; exit 1; }
[[ "$production_api_block" == *$'file-gateway:\n        condition: service_healthy'* ]] || { echo 'production platform-api lacks healthy File Gateway dependency' >&2; exit 1; }

local_db_line="$(grep -nF 'compose_run up -d --wait mysql file-gateway-mysql' "$local_runner" | head -n1 | cut -d: -f1)"
local_gateway_line="$(grep -nF 'compose_up_wait "统一文件网关" file-gateway' "$local_runner" | head -n1 | cut -d: -f1)"
local_api_line="$(grep -nF 'compose_up_wait "基础平台 API" subsystem-provisioner api' "$local_runner" | head -n1 | cut -d: -f1)"
[[ -n "$local_db_line" && -n "$local_gateway_line" && -n "$local_api_line" ]] || {
  echo 'could not determine local startup ordering' >&2
  exit 1
}
((local_db_line < local_gateway_line && local_gateway_line < local_api_line)) || {
  echo 'local platform API can start before File Gateway' >&2
  exit 1
}

echo 'File Gateway deployment wiring checks passed'
