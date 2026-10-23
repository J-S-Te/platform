#!/usr/bin/env bash
set -Eeuo pipefail
source "$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)/compose-scope.sh"
profiles_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../subsystems.d" && pwd)"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/uip-scope.XXXXXX")"
trap 'rm -r -- "$test_root"' EXIT
deploy_dir="$test_root"; runtime_file="$test_root/.env"; release_file="$test_root/.release.env"
printf 'COMPOSE_PROJECT_NAME=existing-project\n' > "$runtime_file"
touch "$release_file"
enabled=$'platform-api\nfrontend\nportal-api'
docker() {
  case "$*" in
    *'config --services') printf '%s\n' "$enabled" ;;
    'ps -a '*) printf 'crm-id customer-api\nshared-db contract-mysql\n' ;;
    'ps -q '*customer-api) printf 'crm-id\n' ;;
    'ps -q '*contract-api) printf 'contract-api-id\n' ;;
    'ps -q '*contract-worker) printf 'contract-worker-id\n' ;;
    'ps -q '*) : ;;
    'stop --timeout 60 '*) printf '%s\n' "$*" >> "$test_root/stopped" ;;
    *) printf 'Unexpected command: %s\n' "$*" >&2; return 1 ;;
  esac
}
scope_enabled customer-portal
if scope_require customer-opportunity 2> "$test_root/error"; then exit 1; fi
grep -q '此系统未启用' "$test_root/error"
scope_report 2> "$test_root/report"
grep -q './bin/deploy.sh disable customer-opportunity' "$test_root/report"
scope_disable customer-opportunity > /dev/null
[[ "$(cat "$test_root/stopped")" == 'stop --timeout 60 crm-id' ]]
if scope_owned_services contract | grep -Eq 'contract-mysql|temporal'; then
  echo 'shared Temporal database was classified as an optional subsystem' >&2; exit 1
fi
scope_owned_services customer-portal | grep -Fxq portal-report-worker
scope_owned_services contract | grep -Fxq contract-worker
: > "$test_root/stopped"
scope_disable contract > /dev/null
grep -Fxq 'stop --timeout 60 contract-api-id' "$test_root/stopped"
grep -Fxq 'stop --timeout 60 contract-worker-id' "$test_root/stopped"
[[ "$(wc -l < "$test_root/stopped")" -eq 2 ]]
enabled=$'contract-api\ncontract-worker\ncontract-migrate\ncontract-mysql\ntemporal'
[[ "$(scope_runtime_services contract)" == $'contract-api\ncontract-worker' ]]
enabled=contract-api
if scope_runtime_services contract > /dev/null 2>&1; then
  echo 'Missing reviewed worker was accepted as a complete runtime' >&2; exit 1
fi
[[ "$(customer_presale_build_id "registry.invalid/app@sha256:$(printf '%064d' 1)")" == customer-presale-0000000000000000 ]]
if customer_presale_build_id registry.invalid/app:latest > /dev/null 2>&1; then exit 1; fi
if scope_disable platform 2>/dev/null; then exit 1; fi
echo 'Compose scope, disabled module rejection and exact-container stop tests passed'
