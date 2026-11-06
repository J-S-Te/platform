#!/usr/bin/env bash
set -Eeuo pipefail

source_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/verify-readiness-test.XXXXXX")"
trap 'rm -r -- "$test_root"' EXIT
mkdir -p "$test_root/deploy/bin" "$test_root/deploy/manifests" "$test_root/deploy/packages"
mkdir -p "$test_root/checks"
cp "$source_dir/bin/deploy.sh" "$source_dir/bin/compose-scope.sh" "$source_dir/bin/subsystem-runtime.py" "$source_dir/bin/start-enabled.sh" "$source_dir/bin/project-background.sh" "$source_dir/bin/provisioner-config-refresh.sh" "$source_dir/bin/offline-configure.sh" "$source_dir/bin/offline-package-metadata.sh" "$source_dir/bin/public-transport.sh" "$test_root/deploy/bin/"
cp "$source_dir"/bin/deploy-{common,import,subsystem,license,doctor,package,status,verify,menu,registry}.sh "$test_root/deploy/bin/"
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
# 超时诊断与 verify_current_services 同一口径：真实等待时长 + 耗尽的是哪一层预算。
grep -Fq '等待服务健康收敛超过 4 秒：待收敛服务=platform-api（该服务已等待 4 秒；耗尽预算=单服务窗口；单服务窗口=4 秒；总预算=600 秒）' \
  "$test_root/timeout.log" || fail 'timeout error absent'
grep -q 'mock logs for platform-api' "$test_root/timeout.log" || fail 'timeout diagnostics absent'

UIP_HEALTH_WAIT_SECONDS=0
if (wait_for_required_service_health platform-api) > "$test_root/invalid-timeout.log" 2>&1; then
  fail 'invalid timeout accepted'
fi
grep -q '必须是 1 到 3600 之间的整数秒' "$test_root/invalid-timeout.log" || fail 'invalid timeout message absent'
# verify_current_services 现在复用同一时限校验，恢复合法值以免污染后续用例。
UIP_HEALTH_WAIT_SECONDS=10

# ---------------------------------------------------------------------------
# 缺陷 9（P2）必要服务健康门禁的收敛饥饿：必须与 verify_current_services 共用
# 「单服务窗口 UIP_HEALTH_WAIT_SECONDS + 全体总预算 UIP_HEALTH_TOTAL_SECONDS」。
#
# 旧实现让全部必要服务共用一个截止时间（deadline=SECONDS+180，并行轮询），于是慢但会健康的
# 后续服务被前面的服务耗掉预算后按共享截止时间判失败，并报出它从未真正等待过的时长。
# 下面用虚拟时钟固定两件事：A 吃掉大部分单服务窗口后 B 仍拿到自己的窗口并通过；总预算更紧时
# 必须按总预算失败，且诊断写明该服务真实等待时长和耗尽的是哪一层预算。
required_clock="$test_root/required-clock"
: >"$required_clock"
required_compose() {
  case "${1:-}" in
    ps)
      if [[ "${2:-}" == -q ]]; then printf 'container-%s\n' "$3"; else printf 'mock compose status\n'; fi
      ;;
    logs) printf 'mock logs for %s\n' "${@: -1}" ;;
    *) return 0 ;;
  esac
}
# 绝对时刻制的健康替身：svc-alpha 自 SECONDS>=86 起健康，svc-beta 自 SECONDS>=202 起才健康
# （真实场景：B 依赖 A，A 收敛后 B 才开始恢复）。alpha_healthy_at/beta_healthy_at 用来制造
# “永久未收敛”以便验证总预算才是更紧的约束。
required_docker() {
  if [[ "${1:-}" == image && "${2:-}" == inspect ]]; then printf '%s\n' "$fixture_image_id"; return 0; fi
  [[ "${1:-}" == inspect ]] || return 0
  local service="${2#container-}" format="${4:-}" healthy_at
  case "$format" in
    *'.State.Status'*) printf 'running\n' ;;
    *'.State.Health'*)
      printf '%s\n' "$service" >>"$required_clock"
      case "$service" in
        svc-alpha) healthy_at="${alpha_healthy_at:-86}" ;;
        *) healthy_at="${beta_healthy_at:-202}" ;;
      esac
      if ((SECONDS >= healthy_at)); then printf 'healthy\n'; else printf 'starting\n'; fi
      ;;
    *'.RestartCount'*) printf '0\n' ;;
    *'.Image'*) printf '%s\n' "$fixture_image_id" ;;
  esac
}
compose() { required_compose "$@"; }
docker() { required_docker "$@"; }

# 用例 A：A 用掉 86 秒才收敛，B 之后仍必须在自己的窗口内收敛（旧共享截止时间会误判失败）。
alpha_healthy_at=86
beta_healthy_at=202
UIP_HEALTH_WAIT_SECONDS=180
UIP_HEALTH_TOTAL_SECONDS=600
SECONDS=0
wait_for_required_service_health svc-alpha svc-beta >"$test_root/required-per-service-window.log" 2>&1 ||
  fail "required-service gate starved the second service: $(cat "$test_root/required-per-service-window.log")"
grep -Fq '必要服务均已健康。' "$test_root/required-per-service-window.log" ||
  fail 'required-service success message absent'
grep -Fq '[验收证据] service=svc-alpha container=container-svc-alpha status=running health=healthy restarts=0' \
  "$test_root/required-per-service-window.log" || fail 'required-service first evidence line changed'
grep -Fq '[验收证据] service=svc-beta container=container-svc-beta status=running health=healthy restarts=0' \
  "$test_root/required-per-service-window.log" || fail 'required-service second evidence line changed'
((SECONDS >= 202)) ||
  fail "virtual clock did not reach the second service's own convergence (SECONDS=$SECONDS)"
((SECONDS > 180)) ||
  fail 'required-service case does not exercise a convergence later than the old shared deadline'
# B 在 A 收敛之后才第一次被探测，说明它的窗口确实是自己的（而不是共享截止时间）。
[[ $(sort -u "$required_clock" | tr '\n' ' ') == 'svc-alpha svc-beta ' ]] ||
  fail 'unexpected service probe set in required-service case'

# 用例 B：总预算更紧时必须按总预算失败，并写明该服务真实等待时长与耗尽的那一层预算。
alpha_healthy_at=86
beta_healthy_at=100000
UIP_HEALTH_WAIT_SECONDS=180
UIP_HEALTH_TOTAL_SECONDS=120
SECONDS=0
if wait_for_required_service_health svc-alpha svc-beta >"$test_root/required-total-budget.log" 2>&1; then
  fail 'required-service total budget exhaustion still passed the health gate'
fi
grep -Fq '待收敛服务=svc-beta' "$test_root/required-total-budget.log" ||
  fail 'required-service total budget case must fail on the second service'
grep -Fq '耗尽预算=总预算；单服务窗口=180 秒；总预算=120 秒' "$test_root/required-total-budget.log" ||
  fail 'required-service binding budget layer absent from the diagnostic'
# A 在 86 秒收敛，120 秒总预算截止 => B 实际只等了 34 秒，诊断不得复述 180 秒。
# 虚拟时钟骑在 bash SECONDS 上，被抢占时会多跳 1 秒；断言结构而非精确秒数，避免 CI 假红。
grep -Eq '超过 3[45] 秒：待收敛服务=svc-beta（该服务已等待 3[45] 秒' "$test_root/required-total-budget.log" ||
  fail 'required-service diagnostic reports a false wait duration'
grep -Fq 'mock logs for svc-beta' "$test_root/required-total-budget.log" ||
  fail 'required-service timeout diagnostics absent'
((SECONDS >= 120 && SECONDS <= 140)) ||
  fail "required-service total budget was not the binding constraint (SECONDS=$SECONDS)"

# 预算在进入等待时就已耗尽（总预算 1 秒）：仍必须做一次真实的 2 秒采样，并按实测时长报告，
# 不能 0 秒轮询就判失败，也不能复述单服务窗口的 180 秒。
: >"$required_clock"
UIP_HEALTH_WAIT_SECONDS=180
UIP_HEALTH_TOTAL_SECONDS=1
SECONDS=0
if wait_for_required_service_health svc-beta >"$test_root/required-exhausted-budget.log" 2>&1; then
  fail 'already exhausted budget still reported a converged service'
fi
[[ -s "$required_clock" ]] || fail 'service was never probed when the budget was already exhausted'
grep -Eq '超过 [0-9]+ 秒：待收敛服务=svc-beta（该服务已等待 [0-9]+ 秒；耗尽预算=总预算；单服务窗口=180 秒；总预算=1 秒）' \
  "$test_root/required-exhausted-budget.log" ||
  fail 'exhausted budget did not keep a real 2-second sample'

# 必要服务门禁同样必须校验总预算非法值（不能静默退化成默认值继续等待）。
UIP_HEALTH_WAIT_SECONDS=10
UIP_HEALTH_TOTAL_SECONDS=0
if (wait_for_required_service_health svc-alpha) >"$test_root/required-invalid-total-budget.log" 2>&1; then
  fail 'invalid required-service total budget accepted'
fi
grep -q '总预算必须是 1 到 7200 之间的整数秒' "$test_root/required-invalid-total-budget.log" ||
  fail 'invalid required-service total budget message absent'
UIP_HEALTH_TOTAL_SECONDS=600
UIP_HEALTH_WAIT_SECONDS=10

verify_body="$(declare -f verify)"
# 六个核心服务必须无条件在验收门禁里；temporal 只能条件出现，否则“仅平台/仅结算/仅数据看板”
# 安装会永远等待一个 deploy-service.sh 根本不会启动的容器。
for core_service in platform-api platform-worker file-gateway keycloak subsystem-provisioner frontend; do
  grep -Fq "$core_service" <<<"$verify_body" || fail "core verify gate omits $core_service"
done
grep -Fq 'temporal_consumers_enabled' <<<"$verify_body" ||
  fail 'core verify gate does not consult temporal_consumers_enabled'
grep -Fq 'temporal' <<<"$verify_body" || fail 'core verify gate never includes temporal'

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
# curl 替身：--write-out 用于读取 HTTP 状态码。gateway_proxy_code 供既有用例使用，
# keycloak_public_code 供“公网 issuer 404 不阻断验收”的用例使用（不设时回退 401）。
curl() {
  local argument
  for argument in "$@"; do
    if [[ "$argument" == --write-out ]]; then
      case " $* " in
        *keycloak.test*) printf '%s' "${keycloak_public_code:-200}" ;;
        *) printf '%s' "${gateway_proxy_code:-401}" ;;
      esac
      return 0
    fi
  done
  case " $* " in
    *keycloak.test*) return "${keycloak_public_exit:-0}" ;;
  esac
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

# 现场 P0 回归：--resume 强制重建后健康必然先为 starting，verify_current_services 必须等待收敛而不是单次采样失败。
convergence_samples="$test_root/convergence-samples"
: >"$convergence_samples"
compose() {
  case "${1:-}" in
    config) printf '%s\n' "$fixture_config" ;;
    ps)
      if [[ "${2:-}" == -q ]]; then printf 'container-%s\n' "$3"; else printf 'mock compose status\n'; fi
      ;;
    logs) printf 'mock logs for %s\n' "${@: -1}" ;;
    *) return 0 ;;
  esac
}
docker() {
  if [[ "${1:-}" == image && "${2:-}" == inspect ]]; then printf '%s\n' "$fixture_image_id"; return 0; fi
  [[ "${1:-}" == inspect ]] || return 0
  local format="${4:-}" count=0
  case "$format" in
    *'.State.Status'*) printf 'running\n' ;;
    *'.State.Health'*)
      count="$(wc -l <"$convergence_samples" | tr -d ' ')"
      printf 'sample\n' >>"$convergence_samples"
      if ((count >= 2)); then printf 'healthy\n'; else printf 'starting\n'; fi
      ;;
    *'.RestartCount'*) printf '0\n' ;;
    *'.Image'*) printf '%s\n' "$fixture_image_id" ;;
  esac
}
UIP_HEALTH_WAIT_SECONDS=10
UIP_VERIFY_STABILITY_SECONDS=1
verify_current_services customer-presale-worker >"$test_root/health-convergence.log" 2>&1 ||
  fail 'transient starting health did not converge to healthy'
grep -Fq '[验收证据]' "$test_root/health-convergence.log" || fail 'converged health evidence absent'
grep -Fq 'health=healthy' "$test_root/health-convergence.log" || fail 'converged health state absent'

# 永久 starting 必须在配置时限内失败，并输出待收敛服务与容器日志证据。
docker() {
  if [[ "${1:-}" == image && "${2:-}" == inspect ]]; then printf '%s\n' "$fixture_image_id"; return 0; fi
  [[ "${1:-}" == inspect ]] || return 0
  local format="${4:-}"
  case "$format" in
    *'.State.Status'*) printf 'running\n' ;;
    *'.State.Health'*) printf 'starting\n' ;;
    *'.RestartCount'*) printf '0\n' ;;
    *'.Image'*) printf '%s\n' "$fixture_image_id" ;;
  esac
}
UIP_HEALTH_WAIT_SECONDS=4
if verify_current_services customer-presale-worker >"$test_root/health-wait-timeout.log" 2>&1; then
  fail 'permanently starting service passed health convergence'
fi
grep -Fq '超过 4 秒' "$test_root/health-wait-timeout.log" || fail 'health convergence timeout text absent'
grep -Fq 'service=customer-presale-worker' "$test_root/health-wait-timeout.log" || fail 'health convergence pending service absent'
grep -Fq 'mock logs for customer-presale-worker' "$test_root/health-wait-timeout.log" || fail 'health convergence diagnostics absent'
UIP_HEALTH_WAIT_SECONDS=0
if (verify_current_services customer-presale-worker) >"$test_root/invalid-verify-health-wait.log" 2>&1; then
  fail 'invalid verify health wait accepted'
fi
grep -q '必须是 1 到 3600 之间的整数秒' "$test_root/invalid-verify-health-wait.log" ||
  fail 'invalid verify health wait message absent'
UIP_HEALTH_WAIT_SECONDS=10

# 总预算被前面的服务耗尽后，后面的服务仍必须被真实探测至少一次，并如实报告它自己
# 等待了多久、是哪一层预算耗尽；旧实现会 0 秒轮询却打印“超过 180 秒”（假时长且没有采样）。
fairness_samples="$test_root/fairness-samples"
: >"$fairness_samples"
docker() {
  if [[ "${1:-}" == image && "${2:-}" == inspect ]]; then printf '%s\n' "$fixture_image_id"; return 0; fi
  [[ "${1:-}" == inspect ]] || return 0
  local format="${4:-}"
  case "$format" in
    *'.State.Status'*) printf 'running\n' ;;
    *'.State.Health'*) printf 'sample\n' >>"$fairness_samples"; printf 'starting\n' ;;
    *'.RestartCount'*) printf '0\n' ;;
    *'.Image'*) printf '%s\n' "$fixture_image_id" ;;
  esac
}
if wait_for_current_service_convergence customer-presale-worker 180 "$((SECONDS - 30))" 600 \
  >"$test_root/fairness.log" 2>&1; then
  fail 'exhausted total budget still reported a converged service'
fi
(( $(wc -l <"$fairness_samples") >= 1 )) ||
  fail 'service was never probed after the total budget was exhausted'
grep -Fq '待收敛服务=customer-presale-worker' "$test_root/fairness.log" ||
  fail 'per-service fairness pending service absent'
grep -Fq '耗尽预算=总预算；单服务窗口=180 秒；总预算=600 秒' "$test_root/fairness.log" ||
  fail 'exhausted budget class, per-service window and total budget absent'
grep -Fq '（该服务已等待 2 秒' "$test_root/fairness.log" ||
  fail 'per-service elapsed wait absent'
# “超过 X 秒”必须是该服务自己真实的等待时长，不能复用别处消耗掉的预算数字。
grep -Eq '超过 [0-9]+ 秒：待收敛服务=customer-presale-worker（该服务已等待 [0-9]+ 秒' "$test_root/fairness.log" ||
  fail 'fairness message reports a false duration'

# 瞬时 docker inspect 失败会让 health 为空；这既不能算已收敛，也不能变成 health=unknown 的
# 发布失败，必须继续在收敛窗口内轮询。
empty_health_samples="$test_root/empty-health-samples"
: >"$empty_health_samples"
docker() {
  if [[ "${1:-}" == image && "${2:-}" == inspect ]]; then printf '%s\n' "$fixture_image_id"; return 0; fi
  [[ "${1:-}" == inspect ]] || return 0
  local format="${4:-}" count=0
  case "$format" in
    *'.State.Status'*) printf 'running\n' ;;
    *'.State.Health'*)
      count="$(wc -l <"$empty_health_samples" | tr -d ' ')"
      printf 'sample\n' >>"$empty_health_samples"
      # 前两次 inspect 失败（空 health），第三次起恢复 healthy。
      if ((count >= 2)); then printf 'healthy\n'; fi
      ;;
    *'.RestartCount'*) printf '0\n' ;;
    *'.Image'*) printf '%s\n' "$fixture_image_id" ;;
  esac
}
UIP_HEALTH_WAIT_SECONDS=10
UIP_VERIFY_STABILITY_SECONDS=1
verify_current_services customer-presale-worker >"$test_root/empty-health-retry.log" 2>&1 ||
  fail 'transient empty health from docker inspect was not retried'
grep -Fq 'health=healthy' "$test_root/empty-health-retry.log" ||
  fail 'empty-health retry evidence absent'

# 收敛后再次读取仍是空 health => 明确报“无法读取健康状态”，不得落成 health=unknown。
empty_health_final="$test_root/empty-health-final"
: >"$empty_health_final"
docker() {
  if [[ "${1:-}" == image && "${2:-}" == inspect ]]; then printf '%s\n' "$fixture_image_id"; return 0; fi
  [[ "${1:-}" == inspect ]] || return 0
  local format="${4:-}" count=0
  case "$format" in
    *'.State.Status'*) printf 'running\n' ;;
    *'.State.Health'*)
      count="$(wc -l <"$empty_health_final" | tr -d ' ')"
      printf 'sample\n' >>"$empty_health_final"
      # 第 1 次空（进入收敛等待）、第 2 次 healthy（收敛成功）、第 3 次空（复读失败）。
      if ((count == 1)); then printf 'healthy\n'; fi
      ;;
    *'.RestartCount'*) printf '0\n' ;;
    *'.Image'*) printf '%s\n' "$fixture_image_id" ;;
  esac
}
if verify_current_services customer-presale-worker >"$test_root/empty-health-final.log" 2>&1; then
  fail 'unreadable health after convergence passed verification'
fi
grep -Fq '无法读取健康状态（docker inspect 失败）' "$test_root/empty-health-final.log" ||
  fail 'explicit unreadable-health diagnostic absent'
if grep -Fq 'health=unknown' "$test_root/empty-health-final.log"; then
  fail 'unreadable health still fell through to health=unknown'
fi
UIP_VERIFY_STABILITY_SECONDS=1

# 稳定性窗口内 RestartCount 新增属于真实不稳定；窗口开始前的历史重启不判失败。
restart_growth="$test_root/restart-growth"
: >"$restart_growth"
compose() {
  if [[ "${1:-}" == config ]]; then printf '%s\n' "$fixture_config"; return 0; fi
  [[ "${1:-}" == ps && "${2:-}" == -q ]] || return 0
  printf 'container-%s\n' "$3"
}
docker() {
  if [[ "${1:-}" == image && "${2:-}" == inspect ]]; then printf '%s\n' "$fixture_image_id"; return 0; fi
  [[ "${1:-}" == inspect ]] || return 0
  local format="${4:-}" count
  case "$format" in
    *'.State.Status'*) printf 'running\n' ;;
    *'.State.Health'*) printf 'healthy\n' ;;
    *'.RestartCount'*)
      count="$(wc -l <"$restart_growth" | tr -d ' ')"
      printf 'sample\n' >>"$restart_growth"
      printf '%s\n' "$count"
      ;;
    *'.Image'*) printf '%s\n' "$fixture_image_id" ;;
  esac
}
UIP_VERIFY_STABILITY_SECONDS=1
if verify_current_services customer-presale-worker >"$test_root/restart-growth.log" 2>&1; then
  fail 'restart growing inside stability window passed verification'
fi
grep -Fq 'restarts=0->1' "$test_root/restart-growth.log" || fail 'stability window restart growth evidence absent'
grep -Fq 'stability=1s' "$test_root/restart-growth.log" || fail 'stability window seconds absent'

# 外部依赖造成的既有重启是历史事实，不得单独判失败；证据仍需显式记录策略。
docker() {
  if [[ "${1:-}" == image && "${2:-}" == inspect ]]; then printf '%s\n' "$fixture_image_id"; return 0; fi
  [[ "${1:-}" == inspect ]] || return 0
  local format="${4:-}"
  case "$format" in
    *'.State.Status'*) printf 'running\n' ;;
    *'.State.Health'*) printf 'healthy\n' ;;
    *'.RestartCount'*) printf '1\n' ;;
    *'.Image'*) printf '%s\n' "$fixture_image_id" ;;
  esac
}
verify_current_services customer-presale-worker >"$test_root/historical-restart.log" 2>&1 ||
  fail 'historical restart was treated as instability'
grep -Fq 'restarts=1（历史重启，验收窗口内未新增）' "$test_root/historical-restart.log" ||
  fail 'historical restart policy evidence absent'

# ---------------------------------------------------------------------------
# Y1（P2）收敛饥饿：单服务窗口 + 全体总预算。
#
# 旧实现让全体服务共用一个截止时间（verify_current_services 里的 deadline=SECONDS+180），
# 于是“慢但会健康”的后续服务被前面服务消耗掉的预算判失败——用虚拟时钟实测：A≈86s 收敛、
# B≈204s 才健康，旧算法在 180s 的共享截止时间上直接把 B 判失败（一次误判发布阻塞）。
# 下面用虚拟时钟（已有 `sleep` 替身推进 SECONDS）固定住“每个服务都有自己的有界窗口”，
# 同时用第二个用例证明总预算仍是硬约束、且诊断能说清是哪一层预算耗尽。
fairness_config="$(printf '{"services":{"svc-alpha":{"image":"registry.invalid/alpha@%s"},"svc-beta":{"image":"registry.invalid/beta@%s"}}}' \
  "$fixture_image_id" "$fixture_image_id")"
convergence_clock="$test_root/convergence-clock"
mkdir -p "$convergence_clock"
convergence_compose() {
  case "${1:-}" in
    config) printf '%s\n' "$fairness_config" ;;
    ps)
      if [[ "${2:-}" == -q ]]; then printf 'container-%s\n' "$3"; else printf 'mock compose status\n'; fi
      ;;
    logs) printf 'mock logs for %s\n' "${@: -1}" ;;
    *) return 0 ;;
  esac
}
convergence_docker() {
  if [[ "${1:-}" == image && "${2:-}" == inspect ]]; then printf '%s\n' "$fixture_image_id"; return 0; fi
  [[ "${1:-}" == inspect ]] || return 0
  local service="${2#container-}" format="${4:-}" since_file threshold since
  since_file="$convergence_clock/$service"
  case "$format" in
    *'.State.Status'*) printf 'running\n' ;;
    *'.State.Health'*)
      # 每个服务从“自己第一次被探测”开始计时：alpha 需要 86 秒、beta 需要 116 秒自己的窗口。
      [[ -f "$since_file" ]] || printf '%s' "$SECONDS" >"$since_file"
      read -r since <"$since_file"
      case "$service" in
        svc-alpha) threshold=86 ;;
        *) threshold=116 ;;
      esac
      if ((SECONDS - since >= threshold)); then printf 'healthy\n'; else printf 'starting\n'; fi
      ;;
    *'.RestartCount'*) printf '0\n' ;;
    *'.Image'*) printf '%s\n' "$fixture_image_id" ;;
  esac
}
compose() { convergence_compose "$@"; }
docker() { convergence_docker "$@"; }

# 用例 1：A 消耗 86 秒，B 之后仍必须在自己的窗口内收敛（旧算法会因共享截止时间失败）。
rm -f "$convergence_clock/svc-alpha" "$convergence_clock/svc-beta"
UIP_HEALTH_WAIT_SECONDS=180
UIP_HEALTH_TOTAL_SECONDS=600
UIP_VERIFY_STABILITY_SECONDS=1
SECONDS=0
verify_current_services svc-alpha svc-beta >"$test_root/per-service-window.log" 2>&1 ||
  fail "per-service window starved the second service: $(cat "$test_root/per-service-window.log")"
grep -Fq '[验收证据] service=svc-alpha' "$test_root/per-service-window.log" ||
  fail 'first service convergence evidence absent'
grep -Fq '[验收证据] service=svc-beta' "$test_root/per-service-window.log" ||
  fail 'second service convergence evidence absent'
((SECONDS >= 200)) ||
  fail "virtual clock did not reach the second service's own convergence (SECONDS=$SECONDS)"
# 旧实现全体共用 180 秒截止时间：上面的真实收敛时刻晚于 180 秒，说明旧算法必然误判。
((SECONDS > 180)) ||
  fail 'case does not exercise a convergence later than the old shared deadline'
# 每个服务都真正被采样（B 自己等了约 116 秒），不是“0 秒轮询后报超时”。
[[ -s "$convergence_clock/svc-beta" ]] || fail 'second service was never probed'

# 用例 2：总预算是更紧的约束时必须按总预算失败，并说明是哪一层预算耗尽。
rm -f "$convergence_clock/svc-alpha" "$convergence_clock/svc-beta"
UIP_HEALTH_WAIT_SECONDS=180
UIP_HEALTH_TOTAL_SECONDS=120
SECONDS=0
if verify_current_services svc-alpha svc-beta >"$test_root/total-budget.log" 2>&1; then
  fail 'total budget exhaustion still passed verification'
fi
grep -Fq '待收敛服务=svc-beta' "$test_root/total-budget.log" ||
  fail 'total budget case must fail on the second service'
grep -Fq '耗尽预算=总预算；单服务窗口=180 秒；总预算=120 秒' "$test_root/total-budget.log" ||
  fail 'total budget binding and both budgets absent from the diagnostic'
((SECONDS <= 140)) ||
  fail "total budget was not enforced as the binding constraint (SECONDS=$SECONDS)"

# 总预算参数校验：非法值必须给出明确中文消息（不能退化成默认值继续跑）。
UIP_HEALTH_WAIT_SECONDS=10
UIP_HEALTH_TOTAL_SECONDS=0
if (verify_current_services customer-presale-worker) >"$test_root/invalid-total-budget.log" 2>&1; then
  fail 'invalid total health budget accepted'
fi
grep -q '总预算必须是 1 到 7200 之间的整数秒' "$test_root/invalid-total-budget.log" ||
  fail 'invalid total budget message absent'
UIP_HEALTH_TOTAL_SECONDS=600
UIP_HEALTH_WAIT_SECONDS=10
UIP_VERIFY_STABILITY_SECONDS=1

# ---------------------------------------------------------------------------
# Y4（P2，跨 lane 接口）：deploy-service.sh 在数据库依赖刷新失败时写
# <deploy_dir>/runtime/database-refresh-failed.json 并在之后重部署成功时删除。
# 平台发布当时已继续，所以 status()/verify() 必须显著提示 + 给出补救动作，但不能据此判失败。
status_body="$(declare -f status)"
grep -Fq 'report_database_refresh_failures' <<<"$status_body" ||
  fail 'status() 没有展示数据库依赖刷新失败标记'
verify_body="$(declare -f verify)"
grep -Fq 'report_database_refresh_failures' <<<"$verify_body" ||
  fail 'verify() 没有展示数据库依赖刷新失败标记'

mkdir -p "$test_root/deploy/runtime"
refresh_marker="$test_root/deploy/runtime/database-refresh-failed.json"
printf '%s' '{"schema":"uip.database-refresh-failed/v1","failures":[{"database":"platform_mysql","service":"platform-api","reason":"migrate 000130 失败","at":"2026-05-01T00:00:00Z"},{"database":"contract_mysql","service":"contract-api","reason":"连接超时","at":"2026-05-01T00:01:00Z"}]}' >"$refresh_marker"
report_database_refresh_failures >"$test_root/refresh-marker.log" 2>&1
for expected in platform_mysql platform-api contract_mysql contract-api 'migrate 000130 失败' '2026-05-01T00:00:00Z'; do
  grep -Fq "$expected" "$test_root/refresh-marker.log" || fail "refresh marker report omits $expected"
done
grep -Fq '在对应子系统发布阶段重新部署该服务；平台发布当时已继续' "$test_root/refresh-marker.log" ||
  fail 'refresh marker report omits the remediation action'

# 标记缺失时完全静默（不能每次验收都打印噪音）。
rm -f "$refresh_marker"
report_database_refresh_failures >"$test_root/refresh-marker-absent.log" 2>&1
[[ ! -s "$test_root/refresh-marker-absent.log" ]] || fail 'absent refresh marker produced output'

# 标记损坏/不是普通文件时只降级提示，绝不中断验收（advisory 约定）。
printf 'not-json' >"$refresh_marker"
if ! report_database_refresh_failures >"$test_root/refresh-marker-corrupt.log" 2>&1; then
  fail 'corrupt refresh marker made verification fail'
fi
grep -Fq '无法解析' "$test_root/refresh-marker-corrupt.log" ||
  fail 'corrupt refresh marker warning absent'
rm -f "$refresh_marker"
ln -s "$test_root/nowhere" "$refresh_marker"
if ! report_database_refresh_failures >"$test_root/refresh-marker-symlink.log" 2>&1; then
  fail 'symlinked refresh marker made verification fail'
fi
grep -Fq '不是可读的普通文件' "$test_root/refresh-marker-symlink.log" ||
  fail 'symlinked refresh marker warning absent'
rm -f "$refresh_marker"

# 公网 issuer 的“realm 未创建（404）”与“Keycloak/入口真实故障”必须区分：前者不阻断验收，
# 后者失败关闭。裸 curl --fail 把两者混成一个无诊断失败，正是现场同类误判的来源。
keycloak_public_code=404
if ! (verify) >"$test_root/verify-keycloak-404.log" 2>&1; then
  fail 'tenant realm 404 不应阻断 verify'
fi
grep -Fq 'tenant realm 尚未创建' "$test_root/verify-keycloak-404.log" ||
  fail 'tenant realm 404 告警文案缺失'
keycloak_public_code=503
if (verify) >"$test_root/verify-keycloak-503.log" 2>&1; then
  fail 'keycloak 公网 5xx 必须阻断 verify'
fi
grep -Fq 'Keycloak 公网 OIDC 发现文档不可用' "$test_root/verify-keycloak-503.log" ||
  fail 'keycloak 公网故障诊断缺失'
# 真实 curl 在连不上时 --write-out 仍会输出 000 并返回非零；这里用 000 表示“无可用 HTTP 状态”。
keycloak_public_code=000
keycloak_public_exit=7
if (verify) >"$test_root/verify-keycloak-refused.log" 2>&1; then
  fail '公网入口连接失败必须阻断 verify'
fi
grep -Fq 'HTTP 000' "$test_root/verify-keycloak-refused.log" ||
  fail '公网入口连接失败的诊断缺失'
keycloak_public_code=404
keycloak_public_exit=0

echo 'verification readiness retry and timeout tests passed'
