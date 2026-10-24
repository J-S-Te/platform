#!/usr/bin/env bash
set -Eeuo pipefail

source_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/verify-readiness-test.XXXXXX")"
trap 'rm -r -- "$test_root"' EXIT
mkdir -p "$test_root/deploy/bin" "$test_root/deploy/manifests" "$test_root/deploy/packages"
mkdir -p "$test_root/checks"
cp "$source_dir/bin/deploy.sh" "$source_dir/bin/compose-scope.sh" "$source_dir/bin/subsystem-runtime.py" "$source_dir/bin/start-enabled.sh" "$source_dir/bin/project-background.sh" "$source_dir/bin/provisioner-config-refresh.sh" "$source_dir/bin/offline-configure.sh" "$source_dir/bin/offline-package-metadata.sh" "$source_dir/bin/public-transport.sh" "$test_root/deploy/bin/"
cp "$source_dir/docker-compose.yml" "$test_root/deploy/"
cp -R "$source_dir/subsystems.d" "$test_root/deploy/"
: > "$test_root/deploy/.env"
: > "$test_root/deploy/.release.env"
chmod 600 "$test_root/deploy/.env" "$test_root/deploy/.release.env"
source "$test_root/deploy/bin/deploy.sh"

fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
fixture_image_id="sha256:$(printf '%064d' 1)"
fixture_config="$(printf '{"services":{"project-api":{"image":"registry.invalid/fixture@%s"},"project-sla-notifier":{"image":"registry.invalid/fixture@%s"},"project-mysql":{"image":"registry.invalid/fixture@%s"},"customer-presale-worker":{"image":"registry.invalid/fixture@%s"}}}' "$fixture_image_id" "$fixture_image_id" "$fixture_image_id" "$fixture_image_id")"
compose() {
  case "${1:-}" in
    ps)
      if [[ "${2:-}" == -q ]]; then printf 'container-%s\n' "$3"; else printf 'mock compose status\n'; fi
      ;;
    logs) printf 'mock logs for %s\n' "${@: -1}" ;;
    *) return 0 ;;
  esac
}
docker() {
  [[ "$1" == inspect ]] || return 0
  local id="$2" format="$4" service="${2#container-}"
  local count_file="$test_root/checks/$service" count=0
  [[ ! -f "$count_file" ]] || read -r count < "$count_file"
  if [[ "$format" == *'.State.Status'* ]]; then
    printf '%s\n' "$((count + 1))" > "$count_file"
    printf 'running\n'
  elif [[ "$format" == *'.State.Health'* ]]; then
    if [[ "$mode" == delayed && "$count" -le 1 ]]; then printf 'starting\n';
    elif [[ "$mode" == timeout ]]; then printf 'starting\n';
    else printf 'healthy\n'; fi
  elif [[ "$format" == *'.RestartCount'* ]]; then
    printf '0\n'
  fi
}
sleep() { SECONDS=$((SECONDS + ${1:-1})); }

mode=delayed
UIP_HEALTH_WAIT_SECONDS=10
wait_for_required_service_health file-gateway-mysql file-gateway platform-api > "$test_root/delayed.log"
grep -q '必要服务均已健康' "$test_root/delayed.log" || fail 'transient starting state was not retried'

mode=timeout
rm -f "$test_root/checks/"*
UIP_HEALTH_WAIT_SECONDS=4
if wait_for_required_service_health platform-api > "$test_root/timeout.log" 2>&1; then
  fail 'unhealthy service passed readiness gate'
fi
grep -q '等待必要服务健康超过 4 秒' "$test_root/timeout.log" || fail 'timeout error absent'
grep -q 'mock logs for platform-api' "$test_root/timeout.log" || fail 'timeout diagnostics absent'

UIP_HEALTH_WAIT_SECONDS=0
if (wait_for_required_service_health platform-api) > "$test_root/invalid-timeout.log" 2>&1; then
  fail 'invalid timeout accepted'
fi
grep -q '必须是 1 到 3600 之间的整数秒' "$test_root/invalid-timeout.log" || fail 'invalid timeout message absent'

verify_body="$(sed -n '/^verify() {$/,/^}$/p' "$source_dir/bin/deploy.sh")"
for core_service in platform-api platform-worker file-gateway keycloak temporal subsystem-provisioner frontend; do
  grep -Fq "$core_service" <<<"$verify_body" || fail "core verify gate omits $core_service"
done

# 历史阶段结果仅是工作流元数据。必须重新检查当前项目数据库、API 和通知器状态；即使旧阶段辅助函数报告 VERIFIED，通知器退化也必须验证失败。
require_initialized() { return 0; }
scope_require() { return 0; }
scope_services() { printf '%s\n' project-api project-sla-notifier project-mysql; }
runtime_ready() { return 0; }
subsystem_phase() { printf 'VERIFIED\n'; }
wait_for_required_service_health() { return 0; }
env_get() {
  case "${2:-}" in
    PLATFORM_API_PORT) printf '18080\n' ;;
    *) printf 'false\n' ;;
  esac
}
PUBLIC_PLATFORM_ORIGIN='http://platform.test'
PUBLIC_KEYCLOAK_ISSUER='http://keycloak.test/realms/basic-platform'
compose() {
  case "${1:-}" in
    config) printf '%s\n' "$fixture_config" ;;
    ps)
      if [[ "${2:-}" == -q ]]; then printf 'container-%s\n' "$3"; else printf 'mock compose status\n'; fi
      ;;
    exec) return 0 ;;
    *) return 0 ;;
  esac
}
docker() {
  if [[ "${1:-}" == container && "${2:-}" == inspect ]]; then return 1; fi
  if [[ "${1:-}" == image && "${2:-}" == inspect ]]; then printf '%s\n' "$fixture_image_id"; return 0; fi
  [[ "${1:-}" == inspect ]] || return 0
  local service="${2#container-}" format="${4:-}"
  if [[ "$format" == *'.State.Status'* ]]; then
    [[ "$service" == project-sla-notifier ]] && printf 'exited\n' || printf 'running\n'
  elif [[ "$format" == *'.State.Health'* ]]; then
    [[ "$service" == project-sla-notifier ]] && printf 'unhealthy\n' || printf 'healthy\n'
  elif [[ "$format" == *'.RestartCount'* ]]; then
    printf '2\n'
  elif [[ "$format" == *'.Image'* ]]; then
    printf '%s\n' "$fixture_image_id"
  fi
}
curl() {
  local argument
  for argument in "$@"; do
    if [[ "$argument" == --write-out ]]; then printf '401'; return 0; fi
  done
  return 0
}
if (verify project) >"$test_root/degraded-project.log" 2>&1; then
  fail 'historical VERIFIED allowed a currently degraded project worker'
fi
grep -Fq 'service=project-sla-notifier' "$test_root/degraded-project.log" ||
  fail 'degraded project worker evidence absent'
grep -Fq 'status=exited' "$test_root/degraded-project.log" ||
  fail 'degraded project worker status evidence absent'

# 没有健康检查的长期运行 worker 需要有界稳定性采样，仅观察到瞬时运行状态不足以判定成功。
restart_counter="$test_root/restart-counter"
: >"$restart_counter"
compose() {
  if [[ "${1:-}" == config ]]; then printf '%s\n' "$fixture_config"; return 0; fi
  [[ "${1:-}" == ps && "${2:-}" == -q ]] || return 0
  printf 'container-%s\n' "$3"
}
docker() {
  if [[ "${1:-}" == image && "${2:-}" == inspect ]]; then printf '%s\n' "$fixture_image_id"; return 0; fi
  [[ "${1:-}" == inspect ]] || return 0
  local format="${4:-}" count
  if [[ "$format" == *'.State.Status'* ]]; then
    printf 'running\n'
  elif [[ "$format" == *'.State.Health'* ]]; then
    printf 'none\n'
  elif [[ "$format" == *'.RestartCount'* ]]; then
    count="$(wc -l <"$restart_counter" | tr -d ' ')"
    printf 'sample\n' >>"$restart_counter"
    printf '%s\n' "$count"
  elif [[ "$format" == *'.Image'* ]]; then
    printf '%s\n' "$fixture_image_id"
  fi
}
UIP_VERIFY_STABILITY_SECONDS=1
if verify_current_services customer-presale-worker >"$test_root/restart-loop.log" 2>&1; then
  fail 'restart-looping worker without healthcheck passed stability verification'
fi
grep -Fq 'restarts=0->1' "$test_root/restart-loop.log" || fail 'restart stability evidence absent'

echo 'verification readiness retry and timeout tests passed'
