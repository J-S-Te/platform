#!/usr/bin/env bash
# 部署期才能算出的公开传输值（PUBLIC_PLATFORM_ORIGIN / PUBLIC_SSO_ORIGIN / Secure Cookie /
# Keycloak HTTPS 要求 / 允许非回环 HTTP 会话）必须持久化到部署目录的运行配置（.env）：
# 生产编排用 Compose 必填语法（:?）要求它们存在，任何绕过 deploy.sh 的
# `docker compose --env-file .env --env-file .release.env up -d` 只读 .env。
# 现场事故：这些值只在部署脚本进程内 export，未持久化，于是 compose 静默取到
# http://localhost:8081 与 Secure Cookie=true，登录 401 死循环且没有任何报错。
#
# 本测试只用临时目录：不连接 Docker、不读写任何真实部署目录。
set -Eeuo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=public-transport.sh
source "$script_dir/public-transport.sh"

test_root="$(mktemp -d "${TMPDIR:-/tmp}/public-transport-persist.XXXXXX")"
trap 'rm -r -- "$test_root"' EXIT
runtime_env="$test_root/.env"

fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

write_config() {
  {
    printf 'PUBLIC_HTTPS_ENABLED=%s\n' "${1:-false}"
    printf '# 非托管注释行必须原样保留\n'
    printf 'PUBLIC_PLATFORM_HOST=platform.example.com\n'
    printf 'PUBLIC_SSO_HOST=sso.example.com\n'
    printf 'PUBLIC_HTTP_PORT=8081\n'
    printf 'PUBLIC_HTTPS_PORT=443\n'
    printf 'PUBLIC_SSO_HTTP_PORT=18090\n'
    printf 'PUBLIC_SSO_HTTPS_PORT=8443\n'
    printf 'PUBLIC_TLS_CERTIFICATE_PATH=%s\n' "${2:-}"
    printf 'PUBLIC_TLS_PRIVATE_KEY_PATH=%s\n' "${3:-}"
    printf 'SSO_TLS_CERTIFICATE_PATH=\n'
    printf 'SSO_TLS_PRIVATE_KEY_PATH=\n'
    printf 'KEYCLOAK_REALM=basic-platform\n'
    printf 'MYSQL_PASSWORD=fixture-not-a-secret\n'
  } >"$runtime_env"
  chmod 600 "$runtime_env"
  unset PUBLIC_TRANSPORT_STATE
}

expect_value() { grep -Fxq -- "$1" "$runtime_env" || fail "持久化后的运行配置缺少：$1"; }
expect_single_key() { [[ "$(grep -c "^$1=" "$runtime_env")" == 1 ]] || fail "运行配置里 $1 不是恰好一条"; }

# ---------------------------------------------------------------------------
# 1. HTTP 部署：解析值必须全部落盘，且保留权限与非托管键/注释
# ---------------------------------------------------------------------------
write_config false
public_transport_prepare "$test_root" "$runtime_env"
before_mode="$(stat -c '%a' "$runtime_env")"
public_transport_persist "$runtime_env" || fail '合法的 HTTP 传输配置被拒绝持久化'
expect_value 'PUBLIC_PLATFORM_ORIGIN=http://platform.example.com:8081'
expect_value 'PUBLIC_SSO_ORIGIN=http://sso.example.com:18090'
expect_value 'PUBLIC_KEYCLOAK_ISSUER=http://sso.example.com:18090/realms/basic-platform'
expect_value 'PUBLIC_TRANSPORT_COOKIE_SECURE=false'
expect_value 'PUBLIC_TRANSPORT_ALLOW_INSECURE_HTTP=true'
expect_value 'PUBLIC_TRANSPORT_KEYCLOAK_REQUIRE_HTTPS=false'
grep -Fxq -- '# 非托管注释行必须原样保留' "$runtime_env" || fail '持久化丢失了非托管注释行'
expect_value 'MYSQL_PASSWORD=fixture-not-a-secret'
[[ "$(stat -c '%a' "$runtime_env")" == "$before_mode" ]] || fail '持久化改变了运行配置权限'

# ---------------------------------------------------------------------------
# 2. 幂等：值未变化时不得改写文件（.env 是容器单文件 bind mount，换 inode 会制造漂移）
# ---------------------------------------------------------------------------
before_inode="$(stat -c '%i' "$runtime_env")"
before_digest="$(sha256sum "$runtime_env" | awk '{print $1}')"
public_transport_persist "$runtime_env" || fail '幂等持久化失败'
[[ "$(stat -c '%i' "$runtime_env")" == "$before_inode" ]] || fail '已经正确的运行配置被重写（inode 变化）'
[[ "$(sha256sum "$runtime_env" | awk '{print $1}')" == "$before_digest" ]] || fail '已经正确的运行配置内容被改写'

# ---------------------------------------------------------------------------
# 3. 切换 HTTPS：解析值必须整体更新，不能残留旧值或重复键
# ---------------------------------------------------------------------------
# 证书用配置文件生成：-addext 对 SAN 列表的解析在不同 openssl 版本/发行版上不一致，
# 配置文件写法在目标环境（1.1.1 / 3.x）都可复现。
cat >"$test_root/openssl-san.cnf" <<'CNF'
[req]
distinguished_name = distinguished
x509_extensions = extensions
prompt = no
[distinguished]
CN = platform.example.com
[extensions]
subjectAltName = DNS:platform.example.com,DNS:sso.example.com
CNF
openssl req -x509 -newkey rsa:2048 -nodes -days 1 -config "$test_root/openssl-san.cnf" \
  -keyout "$test_root/tls.key" -out "$test_root/tls.crt" >/dev/null 2>&1 \
  || fail '无法生成测试证书'
write_config true tls.crt tls.key
public_transport_prepare "$test_root" "$runtime_env"
public_transport_persist "$runtime_env" || fail '合法的 HTTPS 传输配置被拒绝持久化'
expect_value 'PUBLIC_PLATFORM_ORIGIN=https://platform.example.com'
expect_value 'PUBLIC_TRANSPORT_COOKIE_SECURE=true'
expect_value 'PUBLIC_TRANSPORT_ALLOW_INSECURE_HTTP=false'
expect_value 'PUBLIC_TRANSPORT_KEYCLOAK_REQUIRE_HTTPS=true'
expect_single_key PUBLIC_PLATFORM_ORIGIN
expect_single_key PUBLIC_TRANSPORT_COOKIE_SECURE

# ---------------------------------------------------------------------------
# 4. 失败关闭：符号链接目标与未解析值都必须拒绝，且不得改动文件
# ---------------------------------------------------------------------------
guard_digest="$(sha256sum "$runtime_env" | awk '{print $1}')"
ln -s "$runtime_env" "$test_root/runtime.link"
if public_transport_persist "$test_root/runtime.link" 2>/dev/null; then
  fail '持久化跟随了符号链接运行配置'
fi
if (
  unset PUBLIC_PLATFORM_ORIGIN PUBLIC_SSO_ORIGIN PUBLIC_KEYCLOAK_ISSUER PUBLIC_FRONTEND_SSO_HOST \
    PUBLIC_TRANSPORT_COOKIE_SECURE PUBLIC_TRANSPORT_ALLOW_INSECURE_HTTP \
    PUBLIC_TRANSPORT_KEYCLOAK_REQUIRE_HTTPS
  public_transport_persist "$runtime_env" 2>/dev/null
); then
  fail '持久化接受了未解析的传输配置（会写入空值/默认值）'
fi
if public_transport_persist "$test_root/missing.env" 2>/dev/null; then
  fail '持久化接受了不存在的运行配置文件'
fi
[[ "$(sha256sum "$runtime_env" | awk '{print $1}')" == "$guard_digest" ]] \
  || fail '被拒绝的持久化仍然改动了运行配置'

echo 'public transport persistence tests passed'
