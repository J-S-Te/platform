#!/usr/bin/env bash
set -Eeuo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
deploy_dir="$(cd -- "$script_dir/.." && pwd)"
runtime_file="$deploy_dir/.env"
release_file="$deploy_dir/.release.env"
frontend_compose_file="$deploy_dir/docker-compose.yml"

exec 9>"$deploy_dir/runtime/.deploy.lock"
flock -w 120 9 || { echo '等待其他发布任务超时' >&2; exit 1; }

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
bind_address="$(public_transport_tls_bind_address "$runtime_file")"
case "$bind_address" in
  0.0.0.0) bind_address=127.0.0.1 ;;
  '[::]') bind_address='[::1]' ;;
esac
sso_bind_port="$PUBLIC_HTTPS_PORT"
if [[ "$PUBLIC_ACCESS_MODE" == ip ]]; then sso_bind_port="$PUBLIC_SSO_HTTPS_PORT"; fi

# 用途：读出新连接实际收到的叶证书 SHA-256 指纹。
# 参数：host 和 port 是公开主机和端口，使用本机绑定地址连接。
# 返回值：成功时输出指纹，连接或解析失败时返回非零。
served_fingerprint() {
  local host="$1" port="$2"
  local sni=(-servername "$host")
  if public_transport_is_ip "$host"; then sni=(-noservername); fi
  timeout 4 openssl s_client -connect "${bind_address}:${port}" "${sni[@]}" \
    </dev/null 2>/dev/null | openssl x509 -noout -fingerprint -sha256 2>/dev/null
}

# 用途：直连网关的发布绑定，使用原始 HTTPS 地址验证证书和路由。
# 参数：公开主机、公开端口、宿主发布端口、证书路径和入口探测 URL。
# 返回值：新证书已加载且 HTTP 探测通过时返回 0，否则返回非零。
verify_listener() {
  local host="$1" port="$2" published_port="$3" certificate="$4" url="$5" expected observed attempt
  if [[ "$host" == *:* && "$host" != \[*\] ]]; then host="[$host]"; fi
  expected="$(openssl x509 -in "$certificate" -noout -fingerprint -sha256)" || return 1
  for attempt in {1..5}; do
    if observed="$(served_fingerprint "$host" "$published_port")" \
      && [[ "$observed" == "$expected" ]]; then
      curl --noproxy '*' --fail --silent --show-error --max-time 5 \
        --cacert "$certificate" --connect-to "${host}:${port}:${bind_address}:${published_port}" "$url" \
        >/dev/null
      return
    fi
    sleep 1
  done
  echo "HTTPS 入口 ${host}:${port} 未加载预期证书" >&2
  return 1
}

# 用途：判断证书链、私钥和监听口是否都已经与证书源同步。
# 参数：label 是受管文件名前缀，另传公开主机、端口、证书链和私钥。
# 返回值：磁盘和对外证书一致时返回 0，否则返回非零。
listener_is_current() {
  local label="$1" host="$2" port="$3" certificate="$4" private_key="$5" expected observed
  cmp -s "$certificate" "$PUBLIC_TLS_MANAGED_DIR/${label}.crt" || return 1
  cmp -s "$private_key" "$PUBLIC_TLS_MANAGED_DIR/${label}.key" || return 1
  expected="$(openssl x509 -in "$certificate" -noout -fingerprint -sha256)" || return 1
  observed="$(served_fingerprint "$host" "$port")" || return 1
  [[ "$observed" == "$expected" ]]
}

if listener_is_current platform "$PUBLIC_PLATFORM_HOST" "$PUBLIC_HTTPS_PORT" \
  "$PUBLIC_TLS_CERTIFICATE_RESOLVED" "$PUBLIC_TLS_PRIVATE_KEY_RESOLVED"; then
  if listener_is_current sso "$PUBLIC_SSO_HOST" \
    "$sso_bind_port" "$SSO_TLS_CERTIFICATE_RESOLVED" "$SSO_TLS_PRIVATE_KEY_RESOLVED"; then
    echo 'HTTPS 证书已经同步'
    exit 0
  fi
fi

# 受管证书是目录挂载，替换目录内文件后仅需平滑重载容器内 Nginx。
public_transport_install_certificates
"${command[@]}" exec -T frontend nginx -t </dev/null
"${command[@]}" exec -T frontend nginx -s reload </dev/null
verify_listener "$PUBLIC_PLATFORM_HOST" "$PUBLIC_HTTPS_PORT" "$PUBLIC_HTTPS_PORT" \
  "$PUBLIC_TLS_CERTIFICATE_RESOLVED" "${PUBLIC_PLATFORM_ORIGIN}/"
verify_listener "$PUBLIC_SSO_HOST" "$PUBLIC_SSO_HTTPS_PORT" "$sso_bind_port" \
  "$SSO_TLS_CERTIFICATE_RESOLVED" "${PUBLIC_KEYCLOAK_ISSUER}/.well-known/openid-configuration"
echo 'HTTPS 证书已验证并重新加载'
