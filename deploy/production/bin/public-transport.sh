#!/usr/bin/env bash

# 本文件既可由发布脚本 source，也可直接执行 check/status。它只读取公开传输配置，
# 不 source 包含密码的 .env，避免配置内容被 shell 解释或意外打印。

public_transport_env_value() {
  local file="$1" key="$2"
  awk -F= -v key="$key" '
    $0 !~ /^[[:space:]]*#/ && $1 == key {
      sub(/^[^=]*=/, "")
      print
      exit
    }
  ' "$file"
}

public_transport_fail() {
  echo "公开传输配置错误：$*" >&2
  return 1
}

public_transport_valid_host() {
  local value="$1"
  [[ -n "$value" && "$value" != *://* && "$value" != */* && "$value" != *\?* && "$value" != *\#* && "$value" != *[[:space:]]* ]]
}

public_transport_valid_port() {
  local value="$1"
  [[ "$value" =~ ^[0-9]+$ ]] && (( value >= 1 && value <= 65535 ))
}

public_transport_is_ip() {
  local value="$1" part tail compact count=0
  if [[ "$value" == \[*\] ]]; then
    value="${value#[}"; value="${value%]}"
    [[ "$value" == *:* ]] || return 1
  elif [[ "$value" == *\[* || "$value" == *\]* ]]; then
    return 1
  fi
  if [[ "$value" == *:* ]]; then
    [[ "$value" != :* || "$value" == ::* ]] && [[ "$value" != *: || "$value" == *:: ]] || return 1
    if [[ "$value" == *.* ]]; then
      tail="${value##*:}"
      public_transport_is_ip "$tail" || return 1
      value="${value%:*}:0:0"
    fi
    [[ "$value" =~ ^[0-9A-Fa-f:]+$ && "$value" != *:::* ]] || return 1
    compact="${value/::/}"
    [[ "$compact" != *::* ]] || return 1
    tail="$value"
    while [[ "$tail" == *:* ]]; do
      part="${tail%%:*}"; tail="${tail#*:}"
      [[ -z "$part" || ${#part} -le 4 ]] || return 1
      [[ -z "$part" ]] || count=$((count + 1))
    done
    [[ ${#tail} -le 4 ]] || return 1
    [[ -z "$tail" ]] || count=$((count + 1))
    if [[ "$value" == *::* ]]; then
      (( count < 8 ))
    else
      [[ "$value" != :* && "$value" != *: ]] && (( count == 8 ))
    fi
  else
    [[ "$value" =~ ^[0-9]{1,3}(\.[0-9]{1,3}){3}$ ]] || return 1
    tail="$value"
    while :; do
      part="${tail%%.*}"
      [[ "$part" == 0 || "$part" != 0* ]] || return 1
      (( 10#$part <= 255 )) || return 1
      [[ "$tail" == *.* ]] || break
      tail="${tail#*.}"
    done
  fi
}

public_transport_origin() {
  local scheme="$1" host="$2" port="$3" default_port
  [[ "$scheme" == "https" ]] && default_port=443 || default_port=80
  if [[ "$port" == "$default_port" ]]; then
    printf '%s://%s' "$scheme" "$host"
  else
    printf '%s://%s:%s' "$scheme" "$host" "$port"
  fi
}

public_transport_real_file() {
  local path="$1" base_dir="${2:-$PWD}" resolved
  if [[ "$path" != /* ]]; then
    path="${base_dir}/${path}"
  fi
  [[ -n "$path" && -r "$path" ]] || return 1
  if command -v realpath >/dev/null 2>&1; then
    resolved="$(realpath -- "$path" 2>/dev/null)" || return 1
  else
    resolved="$(readlink -f -- "$path" 2>/dev/null)" || return 1
  fi
  [[ -n "$resolved" && -f "$resolved" && -r "$resolved" ]] || return 1
  printf '%s' "$resolved"
}

public_transport_certificate_key_hash() {
  openssl x509 -in "$1" -pubkey -noout 2>/dev/null \
    | openssl pkey -pubin -outform DER 2>/dev/null \
    | sha256sum | awk '{print $1}'
}

public_transport_private_key_hash() {
  openssl pkey -in "$1" -pubout -outform DER 2>/dev/null \
    | sha256sum | awk '{print $1}'
}

public_transport_validate_pair() {
  local label="$1" certificate="$2" private_key="$3" host="$4"
  openssl crl2pkcs7 -nocrl -certfile "$certificate" 2>/dev/null \
    | openssl pkcs7 -print_certs -noout >/dev/null 2>&1 \
    || public_transport_fail "${label}证书链无法解析" || return 1
  openssl x509 -in "$certificate" -noout -checkend 0 >/dev/null 2>&1 \
    || public_transport_fail "${label}证书已过期、尚未生效或无法解析" || return 1
  openssl x509 -in "$certificate" -noout -checkhost "$host" >/dev/null 2>&1 \
    || public_transport_fail "${label}证书不覆盖域名 ${host}" || return 1
  [[ "$(public_transport_certificate_key_hash "$certificate")" == "$(public_transport_private_key_hash "$private_key")" ]] \
    || public_transport_fail "${label}证书与私钥不匹配" || return 1
}

public_transport_prepare() {
  local deploy_dir="$1" runtime_file="${2:-$1/.env}" https_compose_file="${3:-$1/compose.https.yaml}" drain_compose_file="${4:-$1/compose.drain.yaml}"
  local enabled transition_state tls_required platform_host sso_host http_port https_port sso_http_port sso_https_port
  local platform_certificate platform_private_key sso_certificate sso_private_key

  [[ -f "$runtime_file" ]] || public_transport_fail "缺少 ${runtime_file}" || return 1

  enabled="$(public_transport_env_value "$runtime_file" PUBLIC_HTTPS_ENABLED)"
  enabled="${enabled:-false}"
  [[ "$enabled" == "true" || "$enabled" == "false" ]] \
    || public_transport_fail "PUBLIC_HTTPS_ENABLED 只能是 true 或 false" || return 1
  transition_state="${PUBLIC_TRANSPORT_STATE:-$(public_transport_env_value "$runtime_file" PUBLIC_TRANSPORT_STATE)}"
  case "$transition_state" in
    ""|HTTP|HTTPS|ENABLING_HTTPS|DISABLING_HTTPS) ;;
    *) public_transport_fail "PUBLIC_TRANSPORT_STATE 无效" || return 1 ;;
  esac
  tls_required=false
  if [[ "$enabled" == "true" || "$transition_state" == "DISABLING_HTTPS" ]]; then
    tls_required=true
  fi

  platform_host="$(public_transport_env_value "$runtime_file" PUBLIC_PLATFORM_HOST)"
  sso_host="$(public_transport_env_value "$runtime_file" PUBLIC_SSO_HOST)"
  platform_host="${platform_host:-localhost}"
  sso_host="${sso_host:-localhost}"
  public_transport_valid_host "$platform_host" \
    || public_transport_fail "PUBLIC_PLATFORM_HOST 必须是不含协议和路径的主机名" || return 1
  public_transport_valid_host "$sso_host" \
    || public_transport_fail "PUBLIC_SSO_HOST 必须是不含协议和路径的主机名" || return 1

  http_port="$(public_transport_env_value "$runtime_file" PUBLIC_HTTP_PORT)"
  https_port="$(public_transport_env_value "$runtime_file" PUBLIC_HTTPS_PORT)"
  http_port="${http_port:-8081}"
  https_port="${https_port:-443}"
  sso_http_port="$(public_transport_env_value "$runtime_file" PUBLIC_SSO_HTTP_PORT)"
  sso_https_port="$(public_transport_env_value "$runtime_file" PUBLIC_SSO_HTTPS_PORT)"
  sso_http_port="${sso_http_port:-$http_port}"
  sso_https_port="${sso_https_port:-$https_port}"
  public_transport_valid_port "$http_port" || public_transport_fail "PUBLIC_HTTP_PORT 无效" || return 1
  public_transport_valid_port "$https_port" || public_transport_fail "PUBLIC_HTTPS_PORT 无效" || return 1
  public_transport_valid_port "$sso_http_port" || public_transport_fail "PUBLIC_SSO_HTTP_PORT 无效" || return 1
  public_transport_valid_port "$sso_https_port" || public_transport_fail "PUBLIC_SSO_HTTPS_PORT 无效" || return 1

  export PUBLIC_HTTPS_ENABLED="$enabled"
  export PUBLIC_TRANSPORT_STATE="$transition_state"
  export PUBLIC_PLATFORM_HOST="$platform_host"
  export PUBLIC_SSO_HOST="$sso_host"
  export PUBLIC_HTTP_PORT="$http_port"
  export PUBLIC_HTTPS_PORT="$https_port"
  export PUBLIC_SSO_HTTP_PORT="$sso_http_port"
  export PUBLIC_SSO_HTTPS_PORT="$sso_https_port"

  PUBLIC_TRANSPORT_COMPOSE_FILE=""
  if [[ "$tls_required" == "true" ]]; then
    command -v openssl >/dev/null || public_transport_fail "HTTPS 模式需要 openssl" || return 1
    platform_certificate="$(public_transport_env_value "$runtime_file" PUBLIC_TLS_CERTIFICATE_PATH)"
    platform_private_key="$(public_transport_env_value "$runtime_file" PUBLIC_TLS_PRIVATE_KEY_PATH)"
    [[ -n "$platform_certificate" && -n "$platform_private_key" ]] \
      || public_transport_fail "HTTPS 模式必须同时配置平台证书和私钥路径" || return 1
    platform_certificate="$(public_transport_real_file "$platform_certificate" "$(dirname "$runtime_file")")" \
      || public_transport_fail "平台证书路径不可读或不是常规文件" || return 1
    platform_private_key="$(public_transport_real_file "$platform_private_key" "$(dirname "$runtime_file")")" \
      || public_transport_fail "平台私钥路径不可读或不是常规文件" || return 1

    sso_certificate="$(public_transport_env_value "$runtime_file" SSO_TLS_CERTIFICATE_PATH)"
    sso_private_key="$(public_transport_env_value "$runtime_file" SSO_TLS_PRIVATE_KEY_PATH)"
    if [[ -n "$sso_certificate" || -n "$sso_private_key" ]]; then
      [[ -n "$sso_certificate" && -n "$sso_private_key" ]] \
        || public_transport_fail "SSO 证书和私钥必须同时配置或同时留空" || return 1
      sso_certificate="$(public_transport_real_file "$sso_certificate" "$(dirname "$runtime_file")")" \
        || public_transport_fail "SSO 证书路径不可读或不是常规文件" || return 1
      sso_private_key="$(public_transport_real_file "$sso_private_key" "$(dirname "$runtime_file")")" \
        || public_transport_fail "SSO 私钥路径不可读或不是常规文件" || return 1
    else
      sso_certificate="$platform_certificate"
      sso_private_key="$platform_private_key"
    fi

    public_transport_validate_pair "平台" "$platform_certificate" "$platform_private_key" "$platform_host" || return 1
    public_transport_validate_pair "SSO" "$sso_certificate" "$sso_private_key" "$sso_host" || return 1

    export PUBLIC_TLS_CERTIFICATE_RESOLVED="$platform_certificate"
    export PUBLIC_TLS_PRIVATE_KEY_RESOLVED="$platform_private_key"
    export SSO_TLS_CERTIFICATE_RESOLVED="$sso_certificate"
    export SSO_TLS_PRIVATE_KEY_RESOLVED="$sso_private_key"
    if [[ "$transition_state" == "DISABLING_HTTPS" ]]; then
      [[ -f "$drain_compose_file" ]] || public_transport_fail "缺少排空 Compose 覆盖文件 ${drain_compose_file}" || return 1
      PUBLIC_TRANSPORT_COMPOSE_FILE="$drain_compose_file"
    else
      [[ -f "$https_compose_file" ]] || public_transport_fail "缺少 HTTPS Compose 覆盖文件 ${https_compose_file}" || return 1
      PUBLIC_TRANSPORT_COMPOSE_FILE="$https_compose_file"
    fi
  fi

  if [[ "$enabled" == "true" ]]; then
    PUBLIC_TRANSPORT_SCHEME=https
    PUBLIC_TRANSPORT_PORT="$https_port"
    PUBLIC_SSO_TRANSPORT_PORT="$sso_https_port"
    export PUBLIC_TRANSPORT_COOKIE_SECURE=true
    export PUBLIC_TRANSPORT_ALLOW_INSECURE_HTTP=false
    export PUBLIC_TRANSPORT_KEYCLOAK_REQUIRE_HTTPS=true
  else
    if [[ "$tls_required" != "true" ]]; then
      unset PUBLIC_TLS_CERTIFICATE_RESOLVED PUBLIC_TLS_PRIVATE_KEY_RESOLVED \
        SSO_TLS_CERTIFICATE_RESOLVED SSO_TLS_PRIVATE_KEY_RESOLVED
    fi
    PUBLIC_TRANSPORT_SCHEME=http
    PUBLIC_TRANSPORT_PORT="$http_port"
    PUBLIC_SSO_TRANSPORT_PORT="$sso_http_port"
    export PUBLIC_TRANSPORT_COOKIE_SECURE=false
    export PUBLIC_TRANSPORT_ALLOW_INSECURE_HTTP=true
    export PUBLIC_TRANSPORT_KEYCLOAK_REQUIRE_HTTPS=false
  fi

  export PUBLIC_TRANSPORT_COMPOSE_FILE
  export PUBLIC_PLATFORM_ORIGIN="$(public_transport_origin "$PUBLIC_TRANSPORT_SCHEME" "$platform_host" "$PUBLIC_TRANSPORT_PORT")"
  export PUBLIC_SSO_ORIGIN="$(public_transport_origin "$PUBLIC_TRANSPORT_SCHEME" "$sso_host" "$PUBLIC_SSO_TRANSPORT_PORT")"
  local realm
  realm="$(public_transport_env_value "$runtime_file" KEYCLOAK_REALM)"
  realm="${realm:-basic-platform}"
  export PUBLIC_KEYCLOAK_ISSUER="${PUBLIC_SSO_ORIGIN}/realms/${realm}"
}

public_transport_compose_args() {
  local -n target="$1"
  if [[ -n "${PUBLIC_TRANSPORT_COMPOSE_FILE:-}" ]]; then
    target+=(--file "$PUBLIC_TRANSPORT_COMPOSE_FILE")
  fi
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  set -Eeuo pipefail
  script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
  deploy_dir="$(cd -- "$script_dir/.." && pwd)"
  case "${1:-check}" in
    check)
      public_transport_prepare "$deploy_dir"
      echo "公开传输配置有效：mode=${PUBLIC_HTTPS_ENABLED} platform=${PUBLIC_PLATFORM_ORIGIN} sso=${PUBLIC_SSO_ORIGIN}"
      ;;
    *) echo "usage: $0 [check]" >&2; exit 2 ;;
  esac
fi
