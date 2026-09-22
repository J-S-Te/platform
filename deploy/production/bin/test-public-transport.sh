#!/usr/bin/env bash
set -Eeuo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=public-transport.sh
source "${script_dir}/public-transport.sh"

test_root="$(mktemp -d "${TMPDIR:-/tmp}/public-transport-test.XXXXXX")"
trap 'rm -r -- "$test_root"' EXIT

compose_file="${test_root}/compose.https.yaml"
drain_compose_file="${test_root}/compose.drain.yaml"
touch "$compose_file"
touch "$drain_compose_file"

write_config() {
  local enabled="$1" certificate="${2:-}" private_key="${3:-}" state="${4:-}"
  {
    printf 'PUBLIC_HTTPS_ENABLED=%s\n' "$enabled"
    printf 'PUBLIC_TRANSPORT_STATE=%s\n' "$state"
    printf 'PUBLIC_PLATFORM_HOST=platform.example.com\n'
    printf 'PUBLIC_SSO_HOST=sso.example.com\n'
    printf 'PUBLIC_HTTP_PORT=8081\n'
    printf 'PUBLIC_HTTPS_PORT=443\n'
    printf 'PUBLIC_SSO_HTTP_PORT=18090\n'
    printf 'PUBLIC_SSO_HTTPS_PORT=8443\n'
    printf 'PUBLIC_TLS_CERTIFICATE_PATH=%s\n' "$certificate"
    printf 'PUBLIC_TLS_PRIVATE_KEY_PATH=%s\n' "$private_key"
    printf 'SSO_TLS_CERTIFICATE_PATH=\n'
    printf 'SSO_TLS_PRIVATE_KEY_PATH=\n'
    printf 'KEYCLOAK_REALM=basic-platform\n'
  } >"${test_root}/runtime.env"
}

write_config false /definitely/not/readable.crt /definitely/not/readable.key
public_transport_prepare "$test_root" "${test_root}/runtime.env" "$compose_file" "$drain_compose_file"
[[ "$PUBLIC_PLATFORM_ORIGIN" == "http://platform.example.com:8081" ]]
[[ "$PUBLIC_SSO_ORIGIN" == "http://sso.example.com:18090" ]]
[[ "$PUBLIC_TRANSPORT_COOKIE_SECURE" == "false" ]]

openssl req -x509 -newkey rsa:2048 -nodes -days 1 \
  -subj '/CN=platform.example.com' \
  -addext 'subjectAltName=DNS:platform.example.com,DNS:sso.example.com' \
  -keyout "${test_root}/tls.key" -out "${test_root}/tls.crt" >/dev/null 2>&1

write_config true tls.crt tls.key
public_transport_prepare "$test_root" "${test_root}/runtime.env" "$compose_file" "$drain_compose_file"
[[ "$PUBLIC_PLATFORM_ORIGIN" == "https://platform.example.com" ]]
[[ "$PUBLIC_SSO_ORIGIN" == "https://sso.example.com:8443" ]]
[[ "$PUBLIC_KEYCLOAK_ISSUER" == "https://sso.example.com:8443/realms/basic-platform" ]]
[[ "$PUBLIC_TRANSPORT_COOKIE_SECURE" == "true" ]]
[[ "$PUBLIC_TLS_CERTIFICATE_RESOLVED" == "${test_root}/tls.crt" ]]

write_config false tls.crt tls.key DISABLING_HTTPS
public_transport_prepare "$test_root" "${test_root}/runtime.env" "$compose_file" "$drain_compose_file"
[[ "$PUBLIC_PLATFORM_ORIGIN" == "http://platform.example.com:8081" ]]
[[ "$PUBLIC_SSO_ORIGIN" == "http://sso.example.com:18090" ]]
[[ "$PUBLIC_TRANSPORT_COOKIE_SECURE" == "false" ]]
[[ "$PUBLIC_TRANSPORT_COMPOSE_FILE" == "$drain_compose_file" ]]
[[ "$PUBLIC_TLS_CERTIFICATE_RESOLVED" == "${test_root}/tls.crt" ]]

openssl req -x509 -newkey rsa:2048 -nodes -days 1 \
  -subj '/CN=unrelated.example.com' \
  -addext 'subjectAltName=DNS:unrelated.example.com' \
  -keyout "${test_root}/other.key" -out "${test_root}/other.crt" >/dev/null 2>&1

write_config true tls.crt other.key
if public_transport_prepare "$test_root" "${test_root}/runtime.env" "$compose_file" "$drain_compose_file" 2>/dev/null; then
  echo "mismatched private key was accepted" >&2
  exit 1
fi

write_config true other.crt other.key
if public_transport_prepare "$test_root" "${test_root}/runtime.env" "$compose_file" "$drain_compose_file" 2>/dev/null; then
  echo "certificate with wrong SAN was accepted" >&2
  exit 1
fi

echo "public transport configuration tests passed"
