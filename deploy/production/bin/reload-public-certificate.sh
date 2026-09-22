#!/usr/bin/env bash
set -Eeuo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
deploy_dir="$(cd -- "$script_dir/.." && pwd)"
runtime_file="$deploy_dir/.env"
release_file="$deploy_dir/.release.env"
frontend_compose_file="$deploy_dir/compose.frontend.yaml"

# shellcheck source=public-transport.sh
source "$script_dir/public-transport.sh"
public_transport_prepare "$deploy_dir"
[[ "$PUBLIC_HTTPS_ENABLED" == "true" ]] || {
  echo "当前为 HTTP 模式，无需重新加载证书" >&2
  exit 2
}
[[ -f "$release_file" && -f "$frontend_compose_file" ]] || {
  echo "生产发布文件不完整" >&2
  exit 1
}

command=(docker compose
  --project-directory "$deploy_dir"
  --file "$frontend_compose_file"
  --env-file "$runtime_file"
  --env-file "$release_file")
public_transport_compose_args command

# 重新创建单个网关容器，让 Certbot 更新后的符号链接目标以新的只读 bind mount
# 进入容器。业务 API、Worker、数据库和 Keycloak 均不重启。
"${command[@]}" config --quiet
"${command[@]}" up -d --force-recreate --no-deps --wait --wait-timeout 120 frontend
curl --fail --silent --show-error --max-time 5 \
  --cacert "$PUBLIC_TLS_CERTIFICATE_RESOLVED" \
  --resolve "${PUBLIC_PLATFORM_HOST}:${PUBLIC_HTTPS_PORT}:127.0.0.1" \
  "https://${PUBLIC_PLATFORM_HOST}:${PUBLIC_HTTPS_PORT}/" >/dev/null
echo "HTTPS 证书已验证并重新加载"
