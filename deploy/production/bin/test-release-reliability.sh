#!/usr/bin/env bash
set -Eeuo pipefail

bin_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
deploy_service="$bin_dir/deploy-service.sh"
start_enabled="$bin_dir/start-enabled.sh"
legacy_customer_deploy="$bin_dir/deploy-customer-opportunity.sh"
compose_file="$bin_dir/../docker-compose.yml"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/uip-release-reliability.XXXXXX")"
trap 'rm -rf -- "$test_root"' EXIT

fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
pass() { printf 'PASS: %s\n' "$*"; }

extract_function() {
  local name="$1" file="$2"
  awk -v signature="${name}() {" '
    $0 == signature {inside=1}
    inside {print}
    inside && $0 == "}" {exit}
  ' "$file"
}

# 4. A failed public HTTP probe must be the frontend deployment result even if
# the immutable image is already running.
eval "$(extract_function deploy_frontend "$deploy_service")"
deploy_dir="$test_root/deploy"
PUBLIC_HTTP_PORT=8081
image_ref='127.0.0.1:5000/uip/frontend@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'
public_transport_install_certificates() { return 0; }
frontend_compose() { return 0; }
wait_for_health() { return 1; }
verify_service_image() { return 0; }
if deploy_frontend; then
  fail 'frontend deployment accepted a failed public HTTP probe'
fi
pass 'frontend probe failure propagates as non-zero'

# 5. Runtime initialization/DSN writes must start only after the shared lock.
lock_line="$(grep -n 'flock -w 900 9' "$deploy_service" | head -n1 | cut -d: -f1)"
runtime_write_line="$(grep -n '^    prepare_runtime_file "\$contract_runtime_file"' "$deploy_service" | head -n1 | cut -d: -f1)"
[[ -n "$lock_line" && -n "$runtime_write_line" && "$lock_line" -lt "$runtime_write_line" ]] ||
  fail 'runtime DSN mutation is not protected by the deployment lock'
pass 'runtime DSN mutation occurs inside the deployment lock'

for oneoff in \
  'platform-migrate ./migrate' \
  contract-migrate \
  project-migrate \
  settlement-migrate \
  data-analysis-metabase-init \
  data-analysis-migrate; do
  grep -Fq "compose run --rm --no-deps $oneoff" "$deploy_service" ||
    fail "one-shot task does not remove its completed container: $oneoff"
done
pass 'all migration/init one-shot containers are removed after completion'

# The legacy two-image CRM/Portal entrypoint remains supported. It must use the
# same lock and Agent refresh contract as deploy-service.sh rather than becoming
# an unlocked compatibility bypass.
legacy_lock_line="$(grep -n 'flock -w 900 9' "$legacy_customer_deploy" | head -n1 | cut -d: -f1)"
legacy_runtime_write_line="$(grep -n '^initialize_runtime_file "\$customer_runtime_file"' "$legacy_customer_deploy" | head -n1 | cut -d: -f1)"
[[ -n "$legacy_lock_line" && -n "$legacy_runtime_write_line" && "$legacy_lock_line" -lt "$legacy_runtime_write_line" ]] ||
  fail 'legacy CRM/Portal runtime DSN mutation is not protected by the deployment lock'
legacy_commit_line="$(grep -n '^mv "\$next_release" "\$release_file"' "$legacy_customer_deploy" | head -n1 | cut -d: -f1)"
legacy_refresh_line="$(grep -n '^if ! refresh_subsystem_provisioner_config; then' "$legacy_customer_deploy" | tail -n1 | cut -d: -f1)"
[[ -n "$legacy_commit_line" && -n "$legacy_refresh_line" && "$legacy_commit_line" -lt "$legacy_refresh_line" ]] ||
  fail 'legacy CRM/Portal release commit does not refresh Agent config'
legacy_restore_body="$(extract_function restore_release "$legacy_customer_deploy")"
grep -Fq 'refresh_subsystem_provisioner_config' <<<"$legacy_restore_body" ||
  fail 'legacy CRM/Portal rollback does not refresh Agent config'
legacy_restore_root="$test_root/legacy-restore"
mkdir -p "$legacy_restore_root"
release_file="$legacy_restore_root/.release.env"
previous_release="$legacy_restore_root/.release.env.previous"
printf 'CUSTOMER_CRM_IMAGE=new\n' >"$release_file"
printf 'CUSTOMER_CRM_IMAGE=old\n' >"$previous_release"
release_updated=true
customer_runtime_updated=false
portal_runtime_updated=false
legacy_refresh_calls="$legacy_restore_root/refresh.calls"
restore_customer_runtime() { return 0; }
restore_portal_runtime() { return 0; }
refresh_subsystem_provisioner_config() { printf 'refresh\n' >>"$legacy_refresh_calls"; }
eval "$legacy_restore_body"
restore_release
grep -Fxq 'CUSTOMER_CRM_IMAGE=old' "$release_file" ||
  fail 'legacy CRM/Portal rollback did not restore the previous release pointer'
grep -Fxq 'refresh' "$legacy_refresh_calls" ||
  fail 'legacy CRM/Portal rollback did not execute Agent refresh'
pass 'legacy CRM/Portal entrypoint shares lock and refreshes Agent on commit/rollback'

# 6. Project deploy/rollback must treat API and notifier worker as one release.
grep -Fq 'verify_service_image compose project-sla-notifier "$image_ref"' "$deploy_service" ||
  fail 'project notifier image is not verified during deployment'
eval "$(extract_function rollback_ref_is_immutable "$deploy_service")"
eval "$(extract_function release_image_value "$deploy_service")"
eval "$(extract_function rollback_runtime "$deploy_service")"
release_file="$test_root/.release.env"
printf 'PROJECT_IMAGE=%s\n' '127.0.0.1:5000/uip/project@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb' >"$release_file"
service=project
project_calls="$test_root/project.calls"
compose() { printf 'compose %s\n' "$*" >>"$project_calls"; }
wait_for_health() { printf 'health %s\n' "$*" >>"$project_calls"; }
verify_service_image() { printf 'verify %s\n' "$*" >>"$project_calls"; }
port_value() { printf '18085\n'; }
rollback_runtime
grep -Fq 'project-api project-sla-notifier' "$project_calls" || fail 'project rollback omitted notifier worker'
grep -Fq 'verify compose project-api ' "$project_calls" || fail 'project API rollback image was not verified'
grep -Fq 'verify compose project-sla-notifier ' "$project_calls" || fail 'project notifier rollback image was not verified'
pass 'project API and worker roll back and verify together'

# A failed first installation has only mutable public placeholders in the
# previous release file.  It must restore the file but never ask Compose to
# pull those placeholders as a fake rollback on an offline host.
printf 'PLATFORM_IMAGE=ghcr.io/j-s-te/platform:main\nFILE_GATEWAY_IMAGE=ghcr.io/j-s-te/platform-file-gateway:main\n' >"$release_file"
service=platform
: >"$project_calls"
rollback_runtime 2>"$test_root/first-install-rollback.log"
[[ ! -s "$project_calls" ]] || fail 'first-install rollback attempted to start mutable public images'
grep -Fq '跳过运行态回滚' "$test_root/first-install-rollback.log" ||
  fail 'first-install rollback skip reason was not reported'
pass 'first-install failure never pulls mutable public placeholder images'

# 7. Refresh must recreate (not restart) Agent and compare both mounted files.
source "$bin_dir/provisioner-config-refresh.sh"
deploy_dir="$test_root/deploy"
release_file="$deploy_dir/.release.env"
compose_file="$deploy_dir/docker-compose.yml"
mkdir -p "$deploy_dir"
printf 'PLATFORM_IMAGE=image@sha256:test\n' >"$release_file"
printf 'services: {}\n' >"$compose_file"
refresh_calls="$test_root/refresh.calls"
agent_matches=true
docker() {
  case "$1" in
    ps) printf 'old-agent\n' ;;
    exec)
      if [[ "$agent_matches" == true ]]; then
        sha256sum "$release_file" "$compose_file"
      else
        printf '%064d  release\n%064d  compose\n' 1 2
      fi
      ;;
    inspect) printf 'healthy\n' ;;
    *) return 1 ;;
  esac
}
compose() {
  printf '%s\n' "$*" >>"$refresh_calls"
  [[ "$1" != ps ]] || printf 'new-agent\n'
}
refresh_subsystem_provisioner_config
grep -Fq 'run --rm --no-deps subsystem-provisioner-socket-init' "$refresh_calls" || fail 'Agent socket init was not run'
grep -Fq 'up -d --force-recreate --wait --wait-timeout 60 --no-deps subsystem-provisioner' "$refresh_calls" || fail 'Agent was not recreated'
agent_matches=false
if verify_subsystem_provisioner_config_consistency new-agent >/dev/null 2>&1; then
  fail 'Agent/host digest mismatch was accepted'
fi
grep -Fq 'refresh_subsystem_provisioner_config' "$start_enabled" || fail 'CRM/Portal release does not refresh Agent config'
pass 'Agent is recreated and host/container config digests are enforced'

# 8. Worker has a Compose health probe and the deploy path enforces a stable
# running/healthy restart count before success.
worker_block="$(awk '/^  platform-worker:$/ {inside=1} /^  frontend:$/ {inside=0} inside' "$compose_file")"
# The temporary compose fixture shadows compose_file above; read the production
# file explicitly for this assertion.
worker_block="$(awk '/^  platform-worker:$/ {inside=1} /^  frontend:$/ {inside=0} inside' "$bin_dir/../docker-compose.yml")"
grep -Fq 'healthcheck:' <<<"$worker_block" || fail 'platform worker has no Compose healthcheck'
grep -Fq '/app/worker' <<<"$worker_block" || fail 'platform worker healthcheck does not identify the worker process'
grep -Fq 'verify_service_stable platform-worker' "$deploy_service" || fail 'platform worker has no release stability gate'
pass 'platform worker health and restart stability are release gates'

# 9. A narrow Compose health window that fails the data-analysis dependency wait
# must fall back to waiting for the running API container instead of rolling back.
eval "$(extract_function deploy_data_analysis "$deploy_service")"
eval "$(extract_function wait_container_healthy "$deploy_service")"
compose() {
  printf 'compose %s\n' "$*" >>"$compose_calls"
  case "$*" in
    *"data-analysis-mysql"*) return 0 ;;
    *"metabase-init"*) return 0 ;;
    *"data-analysis-migrate"*) return 0 ;;
    *"--force-recreate --no-deps --wait --wait-timeout 120 data-analysis-api"* ) return 1 ;;
    *"data-analysis-aggregation-worker"* ) return 0 ;;
    *) return 0 ;;
  esac
}
backup_database() { return 0; }
wait_for_health() { return 0; }
verify_service_image() { return 0; }
docker() { :; }
image() { :; }
data_analysis_image_refs=('ref-api' 'ref-aggregation' 'ref-alert' 'ref-migrate')
compose_calls="$(mktemp)"
: >"$compose_calls"
wait_container_healthy() {
  printf 'fallback-wait %s\n' "$1" >>"$compose_calls"
  return 0
}
if deploy_data_analysis; then
  grep -Fq 'fallback-wait data-analysis-api' "$compose_calls" ||
    fail 'dependency-wait failure did not fall back to waiting for the API container'
  grep -Fq 'compose up -d --no-deps --wait --wait-timeout 120 data-analysis-aggregation-worker data-analysis-alert-worker data-analysis-metabase' "$compose_calls" ||
    fail 'workers were not relaunched after the fallback health wait'
  pass 'data-analysis dependency-wait failure falls back to container health wait'
else
  fail 'data-analysis deployment rolled back despite the fallback health wait succeeding'
fi
rm -f "$compose_calls"

printf 'Release reliability regression tests passed\n'
