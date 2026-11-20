#!/usr/bin/env bash
set -Eeuo pipefail
image="${UIP_FRONTEND_TEST_IMAGE:-uip-unified-compose-frontend-test:20260924}"
fixture="$(mktemp -d)"
network="uip-compose-qa-$(basename "$fixture" | tr '[:upper:].' '[:lower:]-')"
container=""
cleanup() {
  local failed=0
  if [[ -n "$container" ]]; then docker rm -f "$container" >/dev/null || failed=1; fi
  docker network rm "$network" >/dev/null || failed=1
  rm -rf -- "$fixture"
  return "$failed"
}
docker image inspect "$image" >/dev/null
docker network create --internal --label uip.test=unified-compose "$network" >/dev/null
trap cleanup EXIT
openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj /CN=localhost \
  -addext 'subjectAltName=DNS:localhost,DNS:sso.localhost' \
  -keyout "$fixture/platform.key" -out "$fixture/platform.crt" >/dev/null 2>&1
cp "$fixture/platform.key" "$fixture/sso.key"
cp "$fixture/platform.crt" "$fixture/sso.crt"
for mode in HTTP HTTPS DISABLING_HTTPS; do
  enabled=false; [[ "$mode" != HTTPS ]] || enabled=true
  container="$(docker run -d --network "$network" --label uip.test=unified-compose \
    -e PUBLIC_HTTPS_ENABLED="$enabled" -e PUBLIC_TRANSPORT_STATE="$mode" \
    -e PUBLIC_PLATFORM_HOST=localhost -e PUBLIC_SSO_HOST=sso.localhost \
    -v "$fixture:/run/tls:ro" "$image")"
  ready=false
  for ((attempt=0; attempt<30; attempt++)); do
    if docker exec "$container" curl -fsS --max-time 2 http://127.0.0.1:8088/ >/dev/null 2>&1; then ready=true; break; fi
    sleep 1
  done
  [[ "$ready" == true ]] || { docker logs "$container"; exit 1; }
  docker exec "$container" nginx -t
  if [[ "$mode" == HTTPS ]]; then
    [[ "$(docker exec "$container" curl -sS -o /dev/null -w '%{http_code}' http://127.0.0.1/)" == 308 ]]
  else
    docker exec "$container" curl -fsS --max-time 5 http://127.0.0.1/ >/dev/null
  fi
  if [[ "$mode" != HTTP ]]; then
    docker exec "$container" curl -fsS --max-time 5 --cacert /run/tls/platform.crt https://localhost/ >/dev/null
  fi
  status="$(docker exec "$container" curl -sS --max-time 10 -o /tmp/unavailable -w '%{http_code}' http://127.0.0.1:8088/contract_management/healthz)"
  [[ "$status" == 502 || "$status" == 503 || "$status" == 504 ]] || {
    echo "unexpected unavailable status: $status" >&2
    docker exec "$container" cat /tmp/unavailable
    exit 1
  }
  docker exec "$container" cat /tmp/unavailable | grep -q '未部署、已停用'
  docker rm -f "$container" >/dev/null
  container=""
  echo "PASS: $mode gateway starts without backends; missing subsystem returns unavailable"
done

openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj /CN=test \
  -addext 'subjectAltName=IP:192.0.2.20' \
  -keyout "$fixture/platform.key" -out "$fixture/platform.crt" >/dev/null 2>&1
cp "$fixture/platform.key" "$fixture/sso.key"
cp "$fixture/platform.crt" "$fixture/sso.crt"
container="$(docker run -d --network "$network" --label uip.test=unified-compose \
  -e PUBLIC_HTTPS_ENABLED=true -e PUBLIC_PLATFORM_HOST=192.0.2.20 -e PUBLIC_SSO_HOST=192.0.2.20 \
  -e PUBLIC_HTTPS_PORT=18081 -e PUBLIC_SSO_HTTPS_PORT=18445 \
  -v "$fixture:/run/tls:ro" "$image")"
ready=false
for ((attempt=0; attempt<15; attempt++)); do
  if docker exec "$container" curl -fsS --max-time 2 http://127.0.0.1:8088/ >/dev/null 2>&1; then
    ready=true
    break
  fi
  if [[ "$(docker inspect "$container" --format '{{.State.Running}}')" == false ]]; then break; fi
  sleep 1
done
[[ "$ready" == true ]] || { docker logs "$container"; exit 1; }
docker exec "$container" nginx -t
redirect="$(docker exec "$container" curl -sS -o /dev/null -w '%{redirect_url}' \
  -H 'Host: 192.0.2.20:8081' 'http://127.0.0.1/project_management/?from=http')"
[[ "$redirect" == 'https://192.0.2.20:18081/project_management/?from=http' ]]
docker exec "$container" curl -fsS --max-time 5 --cacert /run/tls/platform.crt \
  --connect-to 192.0.2.20:18081:127.0.0.1:443 https://192.0.2.20:18081/ >/dev/null
subsystem_redirect="$(docker exec "$container" curl -fsS --max-time 5 --cacert /run/tls/platform.crt \
  --connect-to 192.0.2.20:18081:127.0.0.1:443 -o /dev/null -w '%{redirect_url}' \
  https://192.0.2.20:18081/project_management)"
[[ "$subsystem_redirect" == 'https://192.0.2.20:18081/project_management/' ]] || {
  echo "子系统跳转脱离 HTTPS 入口：${subsystem_redirect}" >&2; exit 1
}
sso_admin_status="$(docker exec "$container" curl -sS --max-time 5 --cacert /run/tls/sso.crt \
  --connect-to 192.0.2.20:18445:127.0.0.1:444 -o /dev/null -w '%{http_code}' \
  https://192.0.2.20:18445/admin/)"
[[ "$sso_admin_status" == 404 ]]
echo 'PASS: IP HTTPS uses the configured redirect port and a separate restricted SSO TLS listener'
