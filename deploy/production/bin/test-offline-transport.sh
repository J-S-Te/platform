#!/usr/bin/env bash
set -Eeuo pipefail
script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
source "$script_dir/public-transport.sh"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/offline-transport-test.XXXXXX")"
trap 'rm -r -- "$test_root"' EXIT
mkdir -p "$test_root/docker" "$test_root/gateway" "$test_root/data/platform/keys"

write_config() {
  printf 'PUBLIC_ACCESS_MODE=%s\nPUBLIC_HTTPS_ENABLED=%s\nPUBLIC_PLATFORM_HOST=%s\nPUBLIC_SSO_HOST=%s\nPUBLIC_HTTP_PORT=%s\nKEYCLOAK_HTTP_PORT=%s\nKEYCLOAK_REALM=basic-platform\n' \
    "${1:-}" "${2:-false}" "${3:-192.0.2.20}" "${4:-192.0.2.20}" "${5:-8081}" "${6:-18090}" >"$test_root/.env"
  unset PUBLIC_TRANSPORT_STATE
}
expect_reject() {
  if public_transport_prepare "$test_root" 2>/dev/null; then
    echo "invalid transport configuration accepted" >&2
    exit 1
  fi
}
write_config
public_transport_prepare "$test_root"
[[ "$PUBLIC_ACCESS_MODE" == ip && "$PUBLIC_PLATFORM_ORIGIN" == http://192.0.2.20:8081 ]]
[[ "$PUBLIC_SSO_ORIGIN" == http://192.0.2.20:18090 && "$PUBLIC_FRONTEND_SSO_HOST" == sso.invalid ]]
[[ "$PUBLIC_SSO_HOST" == 192.0.2.20 ]]
[[ "$PUBLIC_KEYCLOAK_ISSUER" == http://192.0.2.20:18090/realms/basic-platform ]]
write_config ip false 2001:db8::1 2001:db8::1 08081 18090
public_transport_prepare "$test_root"
[[ "$PUBLIC_PLATFORM_ORIGIN" == 'http://[2001:db8::1]:8081' ]]
[[ "$PUBLIC_SSO_ORIGIN" == 'http://[2001:db8::1]:18090' ]]
write_config domain false platform.example.com sso.example.com 80 18090
public_transport_prepare "$test_root"
[[ "$PUBLIC_SSO_ORIGIN" == http://sso.example.com && "$PUBLIC_FRONTEND_SSO_HOST" == sso.example.com ]]
write_config ip true
expect_reject
write_config domain false example.com example.com
expect_reject
write_config ip false 192.0.2.20 192.0.2.20 08081 8081
expect_reject
write_config ip false 999.999.999.999
expect_reject
for invalid in 0 65536 99999999999999999999999999999 1x; do
  if public_transport_valid_port "$invalid"; then echo "invalid port accepted: $invalid" >&2; exit 1; fi
done
for invalid in '2001:::1' '1:2:3:4:5:6:7' '[::1' 'example.com;' 'a..b' '999.2.3.4'; do
  if public_transport_valid_host "$invalid"; then echo "invalid host accepted: $invalid" >&2; exit 1; fi
done
for valid in ::1 2001:db8::1 ::ffff:192.0.2.20 '[2001:db8::1]' 1:2:3:4:5:6:7:8; do
  public_transport_is_ip "$valid" || { echo "valid IP rejected: $valid" >&2; exit 1; }
done
if KEY_MODE=600 deploy_platform 2>/dev/null; then echo 'unreadable public key accepted' >&2; exit 1; fi
echo 'offline public transport validation tests passed'
