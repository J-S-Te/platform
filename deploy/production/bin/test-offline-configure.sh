#!/usr/bin/env bash
set -Eeuo pipefail
source_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/offline-configure-test.XXXXXX")"
trap 'rm -r -- "$test_root"' EXIT
mkdir -p "$test_root/deploy/bin" "$test_root/deploy/subsystem-templates" "$test_root/tmp"
cp "$source_dir"/bin/{deploy.sh,compose-scope.sh,start-enabled.sh,provisioner-config-refresh.sh,offline-configure.sh,offline-package-metadata.sh,public-transport.sh} "$test_root/deploy/bin/"
cp "$source_dir"/{.env.example,.release.env.example,docker-compose.yml} "$test_root/deploy/"
cp "$source_dir"/subsystem-templates/*.env.example "$test_root/deploy/subsystem-templates/"
export TEST_LOG="$test_root/commands.log" MOCK_FIREWALL=ufw MOCK_OCCUPIED="" MOCK_DEPLOYED=""
export TMPDIR="$test_root/tmp"
docker() {
  case "$*" in
    info) return 0;;
    *'config --services') printf '%s\n' platform-api frontend contract-api customer-api portal-api project-api settlement-api data-analysis-api; return 0;;
    "container inspect --format "*) return 1;;
    "container inspect uip-platform-api") [[ -n "$MOCK_DEPLOYED" ]]; return;;
    "container inspect "*) return 1;;
    "ps -a "*) [[ -z "$MOCK_DEPLOYED" ]] || printf 'uip-platform-api\n';;
    "ps --filter "*) return 0;;
    *) printf 'unexpected docker mutation: %s\n' "$*" >&2; return 97;;
  esac
  return 0
}
ss() { [[ -z "$MOCK_OCCUPIED" ]] || printf 'LISTEN 0 128 0.0.0.0:8081\n'; return 0; }
ufw() {
  if [[ "$1" == status ]]; then
    [[ "$MOCK_FIREWALL" == ufw ]] && echo 'Status: active' || echo 'Status: inactive'
  else printf 'ufw %s\n' "$*" >> "$TEST_LOG"; fi
}
firewall-cmd() {
  case "$*" in
    "--quiet --state") [[ "$MOCK_FIREWALL" == firewalld ]];;
    "--get-active-zones") printf '%s\n' "${MOCK_ZONES:-public}" '  interfaces: eth0';;
    "--get-zones") echo 'public internal';;
    *--query-port=*) return 1;;
    *) printf 'firewalld %s\n' "$*" >> "$TEST_LOG";;
  esac
}
export -f docker ss ufw firewall-cmd
deploy="$test_root/deploy/bin/deploy.sh"
run_config() { bash "$deploy" configure "$@" >"$test_root/result.log" 2>&1; }
fail() { echo "FAIL: $*" >&2; exit 1; }
prompt_ip="$(printf ' 192.0.2.20 \r\n' | bash -c 'source "$1"; configuration_prompt IP ""' _ "$deploy")"
[[ "$prompt_ip" == 192.0.2.20 ]] || fail 'IP prompt did not trim whitespace/CRLF'
value() { awk -F= -v key="$1" '$1==key {sub(/^[^=]*=/,"");print;exit}' "$test_root/deploy/.env"; }
set_value() {
  local key="$1" value="$2" output="$test_root/env.new"
  awk -F= -v key="$key" -v value="$value" '$1==key {$0=key "=" value;found=1} {print} END {if(!found) print key "=" value}' "$test_root/deploy/.env" > "$output"
  chmod 600 "$output"; mv "$output" "$test_root/deploy/.env"
}
printf ' 192.0.2.20 \r\n18080\n08081\n18090\nadmin\nAsia/Shanghai\nn\n' | run_config || { cat "$test_root/result.log"; fail 'initial configure with pasted whitespace/CRLF IP'; }
[[ "$(value PUBLIC_HTTP_PORT)" == 8081 ]] || fail 'port normalization'
[[ "$(value COMPOSE_PROJECT_NAME)" == uip ]] || fail 'project name'
[[ "$(value KEYCLOAK_PUBLIC_URL)" == http://192.0.2.20:18090 ]] || fail 'SSO port'
[[ "$(value PUBLIC_SSO_HTTP_PORT)" == 18090 ]] || fail 'managed SSO public port'
[[ "$(value OFFLINE_CONFIGURATION_COMPLETE)" == true ]] || fail 'completion'
(
  source "$deploy"
  public_transport_prepare "$deploy_dir"
  command docker compose --project-directory "$deploy_dir" --file "$deploy_dir/docker-compose.yml" --env-file "$runtime_file" --env-file "$release_file" config --format json > "$test_root/rendered.json"
  jq -e '.services.frontend.environment.PUBLIC_SSO_HOST == "sso.invalid" and .services.keycloak.environment.KC_HOSTNAME == "http://192.0.2.20:18090" and .services["platform-api"].environment.KEYCLOAK_PUBLIC_URL == "http://192.0.2.20:18090"' "$test_root/rendered.json" >/dev/null
) || fail 'IP Compose rendering'
keys="$(value PLATFORM_KEYS_DIR)"; [[ "$keys" == /* ]] || keys="$test_root/deploy/$keys"
[[ "$(stat -c %a "$keys/jwt-ed25519-public.pem")" == 644 ]] || fail 'public key mode'
[[ "$(stat -c %a "$keys/jwt-ed25519-private.pem")" == 600 ]] || fail 'private key mode'
[[ "$(stat -c %u "$(value FILE_GATEWAY_HOST_ROOT)")" == 10001 ]] || fail 'gateway owner'
grep -q 'ufw allow 8081/tcp' "$TEST_LOG" || fail 'frontend firewall'
grep -q 'ufw allow 18090/tcp' "$TEST_LOG" || fail 'SSO firewall'
if grep -q 'ufw allow 18080/tcp' "$TEST_LOG"; then fail 'private API exposed'; fi
secret="$(value MYSQL_PASSWORD)"; private_hash="$(sha256sum "$keys/jwt-ed25519-private.pem")"
set_value IAM_IMPORT_FILE_GATEWAY_CLIENT_ID retained-id
set_value IAM_IMPORT_FILE_GATEWAY_CLIENT_SECRET retained-secret
set_value OIDC_ROLE_CONFIG_HASH retained-hash
chmod 600 "$keys/jwt-ed25519-public.pem"
chown 0:0 "$(value FILE_GATEWAY_HOST_ROOT)"
run_config </dev/null || { cat "$test_root/result.log"; fail 'idempotent repair'; }
[[ "$(stat -c %a "$keys/jwt-ed25519-public.pem")" == 644 ]] || fail 'public key repair'
[[ "$(stat -c %u "$(value FILE_GATEWAY_HOST_ROOT)")" == 10001 ]] || fail 'owner repair'
[[ "$(value MYSQL_PASSWORD)" == "$secret" && "$(sha256sum "$keys/jwt-ed25519-private.pem")" == "$private_hash" ]] || fail 'secret rotation'
printf '\n\n\n\n\n\n\n' | run_config --force || { cat "$test_root/result.log"; fail 'force same settings'; }
[[ "$(value IAM_IMPORT_FILE_GATEWAY_CLIENT_SECRET)" == retained-secret && "$(value OIDC_ROLE_CONFIG_HASH)" == retained-hash ]] || fail 'force cleared credentials'
before="$(sha256sum "$test_root/deploy/.env")"
if printf '999.999.999.999\n' | run_config --force; then fail 'invalid IP accepted'; fi
[[ "$(sha256sum "$test_root/deploy/.env")" == "$before" ]] || fail 'failed config altered env'
if printf '192.0.2.20\n8081\n08081\n18090\nadmin\nAsia/Shanghai\nn\n' | run_config --force; then fail 'duplicate numeric ports accepted'; fi
export MOCK_OCCUPIED=1
if run_config </dev/null; then fail 'occupied port accepted'; fi
unset MOCK_OCCUPIED; export MOCK_OCCUPIED=""
export MOCK_DEPLOYED=1
if printf '192.0.2.21\n\n\n\n\n\n\n' | run_config --force; then fail 'unsafe live address change'; fi
export MOCK_DEPLOYED=""
export MOCK_FIREWALL=firewalld
run_config </dev/null || { cat "$test_root/result.log"; fail 'firewalld'; }
grep -q -- '--zone=public --permanent --add-port=8081/tcp' "$TEST_LOG" || fail 'permanent explicit zone'
grep -q -- '--zone=public --add-port=8081/tcp' "$TEST_LOG" || fail 'runtime explicit zone'
export MOCK_ZONES=$'public\ninternal'
if run_config </dev/null; then fail 'ambiguous firewall zone accepted'; fi
set_value FIREWALL_ZONE internal
run_config </dev/null || fail 'explicit zone override'
unset MOCK_ZONES
# 域名 HTTPS 重配保持证书和安全配置，并放行实际 HTTPS 端口。
openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj '/CN=platform.example.com' \
  -addext 'subjectAltName=DNS:platform.example.com,DNS:sso.example.com' \
  -keyout "$test_root/deploy/tls.key" -out "$test_root/deploy/tls.crt" >/dev/null 2>&1
set_value PUBLIC_ACCESS_MODE domain
set_value PUBLIC_PLATFORM_HOST platform.example.com
set_value PUBLIC_SSO_HOST sso.example.com
set_value PUBLIC_HTTPS_ENABLED true
set_value PUBLIC_HTTPS_PORT 8443
set_value PUBLIC_TLS_CERTIFICATE_PATH tls.crt
set_value PUBLIC_TLS_PRIVATE_KEY_PATH tls.key
set_value AUTH_SESSION_COOKIE_SECURE true
set_value KEYCLOAK_REQUIRE_HTTPS true
set_value APP_CORS_ALLOWED_ORIGINS https://custom.example.com
printf '\n\n\n' | run_config --force || { cat "$test_root/result.log"; fail 'HTTPS force'; }
[[ "$(value PUBLIC_HTTPS_ENABLED)" == true && "$(value AUTH_SESSION_COOKIE_SECURE)" == true && "$(value KEYCLOAK_REQUIRE_HTTPS)" == true ]] || fail 'HTTPS downgraded'
[[ "$(value APP_CORS_ALLOWED_ORIGINS)" == https://custom.example.com ]] || fail 'custom CORS overwritten'
[[ "$(value IAM_IMPORT_FILE_GATEWAY_CLIENT_SECRET)" == retained-secret ]] || fail 'domain force cleared credentials'
(
  source "$deploy"
  public_transport_prepare "$deploy_dir"
  command docker compose --project-directory "$deploy_dir" --file "$deploy_dir/docker-compose.yml" --env-file "$runtime_file" --env-file "$release_file" config --quiet
) || fail 'HTTPS Compose rendering'
grep -q -- '--zone=internal --permanent --add-port=8443/tcp' "$TEST_LOG" || fail 'HTTPS firewall missing'
set_value PUBLIC_HTTPS_PORT 18080
if run_config </dev/null; then fail 'HTTPS collision accepted'; fi
set_value PUBLIC_HTTPS_PORT 8443
# 有公钥却丢失私钥，必须要求恢复而非生成新密钥。
mv "$keys/jwt-ed25519-private.pem" "$keys/saved-private.pem"
if run_config </dev/null; then fail 'missing private key silently replaced'; fi
mv "$keys/saved-private.pem" "$keys/jwt-ed25519-private.pem"

# 直接调用纯校验函数；fixtures从实际模板派生，不运行容器。
source "$deploy"
for good in 192.0.2.1 0.0.0.0 ::1 2001:db8::1 ::ffff:192.0.2.1 1:2:3:4:5:6:7:8; do valid_ip "$good" || fail "valid IP rejected $good"; done
for bad in abc 999.1.2.3 1.2.3 2001:::1 :1::2 1::2: 192.000.2.1 1:2:3:4:5:6:7 1:2:3:4:5:6:7:8:9; do
  if valid_ip "$bad"; then fail "bad IP accepted $bad"; fi
done
for name in contract customer portal project settlement data-analysis; do
  awk '{gsub(/PENDING_ONBOARDING|REPLACE_WITH_[A-Za-z0-9_]+/,"test-value"); print}' "$source_dir/subsystem-templates/$name.env.example" > "$test_root/$name.env"
  runtime_ready "$test_root/$name.env" || fail "$name valid template rejected"
  printf '\nOPTIONAL_EMPTY=\n' >> "$test_root/$name.env"
  runtime_ready "$test_root/$name.env" || fail "$name optional empty rejected"
  sed '/^PLATFORM_AUTHORIZATION_CATALOG_CLIENT_SECRET=/d; /^PORTAL_AUTHORIZATION_CATALOG_CLIENT_SECRET=/d' "$test_root/$name.env" > "$test_root/next.env"
  mv "$test_root/next.env" "$test_root/$name.env"
  if runtime_ready "$test_root/$name.env" 2>/dev/null; then fail "$name missing required accepted"; fi
done
echo 'offline configure, firewall and runtime readiness tests passed'
