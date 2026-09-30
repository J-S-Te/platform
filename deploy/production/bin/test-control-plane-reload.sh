#!/usr/bin/env bash
set -Eeuo pipefail

bin_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/uip-control-plane-reload.XXXXXX")"
trap 'rm -rf -- "$test_root"' EXIT

fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
pass() { printf 'PASS: %s\n' "$*"; }

deploy_dir="$test_root/deploy"
release_file="$deploy_dir/.release.env"
compose_file="$deploy_dir/docker-compose.yml"
profiles_dir="$deploy_dir/subsystems.d"
mkdir -p "$profiles_dir" "$deploy_dir/runtime"
printf 'PLATFORM_IMAGE=image@sha256:test\n' >"$release_file"
printf 'services: {}\n' >"$compose_file"
printf 'schema_version: 1\napplication: contract\n' >"$profiles_dir/contract_management-prod.yaml"
printf 'schema_version: 1\napplication: data_analysis\n' >"$profiles_dir/data-analysis-prod.yaml"

# shellcheck source=provisioner-config-refresh.sh
source "$bin_dir/provisioner-config-refresh.sh"

calls="$test_root/calls"
api_manifest_matches=true
api_health=healthy
provisioner_running=true
platform_api_running=true

docker() {
  local rendered="$*"
  case "$1" in
    ps)
      if [[ "$rendered" == *'com.docker.compose.service=subsystem-provisioner'* && "$provisioner_running" == true ]]; then
        printf 'old-agent\n'
      elif [[ "$rendered" == *'com.docker.compose.service=platform-api'* && "$platform_api_running" == true ]]; then
        printf 'old-api\n'
      fi
      ;;
    inspect)
      if [[ "${*: -1}" == new-api ]]; then
        printf '%s\n' "$api_health"
      else
        printf 'healthy\n'
      fi
      ;;
    exec)
      if [[ "$rendered" == *'SUBSYSTEM_PRODUCTION_RELEASE_ENV_PATH'* ]]; then
        sha256sum "$release_file" "$compose_file"
      elif [[ "$rendered" == *'SUBSYSTEM_PRODUCTION_PROFILES_DIR'* ]]; then
        if [[ "$2" == new-api && "$api_manifest_matches" != true ]]; then
          printf '%064d  data-analysis-prod.yaml\n' 9
        else
          production_profiles_host_manifest
        fi
      elif [[ "$2" == new-api && "$3" == test && "$4" == -S ]]; then
        return 0
      else
        printf 'unexpected docker exec: %s\n' "$rendered" >&2
        return 97
      fi
      ;;
    *)
      printf 'unexpected Docker operation: %s\n' "$rendered" >&2
      return 97
      ;;
  esac
}

compose() {
  printf '%s\n' "$*" >>"$calls"
  if [[ "$1" == ps && "$2" == -q ]]; then
    case "$3" in
      subsystem-provisioner) printf 'new-agent\n' ;;
      platform-api) printf 'new-api\n' ;;
    esac
  fi
}

refresh_subsystem_control_plane_config >"$test_root/reload.out"
expected_order="$test_root/expected-order"
cat >"$expected_order" <<'EOF'
config --quiet
stop --timeout 60 platform-api
run --rm --no-deps subsystem-provisioner-socket-init
up -d --force-recreate --wait --wait-timeout 60 --no-deps subsystem-provisioner
ps -q subsystem-provisioner
up -d --force-recreate --wait --wait-timeout 120 --no-deps platform-api
ps -q platform-api
EOF
cmp -s "$expected_order" "$calls" || {
  diff -u "$expected_order" "$calls" >&2 || true
  fail 'control-plane services were not recreated in the safe order'
}
grep -Fq 'subsystems.d 集合摘要一致' "$test_root/reload.out" ||
  fail 'successful reload did not report the shared profile-set digest'
pass 'platform-api is stopped, Agent is recreated first, then API is recreated and verified'

api_manifest_matches=false
if refresh_subsystem_control_plane_config >"$test_root/drift.out" 2>&1; then
  fail 'platform-api profile-set drift was accepted'
fi
grep -Fq '生产子系统清单集合不一致' "$test_root/drift.out" ||
  fail 'profile-set drift did not produce an actionable diagnostic'
api_manifest_matches=true
pass 'host, Agent and API profile-set digests are a release gate'

api_health=unhealthy
if refresh_subsystem_control_plane_config >"$test_root/unhealthy.out" 2>&1; then
  fail 'unhealthy recreated platform-api was accepted'
fi
grep -Fq 'platform-api 重建后健康状态异常' "$test_root/unhealthy.out" ||
  fail 'platform-api health failure was not reported'
api_health=healthy
pass 'platform-api health is required after paired reload'

provisioner_running=false
platform_api_running=false
: >"$calls"
refresh_subsystem_control_plane_config >"$test_root/stopped.out"
grep -Fq '均未运行' "$test_root/stopped.out" ||
  fail 'stopped control plane was not handled as a static-validation case'
[[ "$(cat "$calls")" == 'config --quiet' ]] ||
  fail 'static validation unexpectedly started a stopped control plane'
pass 'a not-yet-deployed control plane is validated without being started'

# A previous failed reload can intentionally leave platform-api stopped. The
# same command must be able to repair that fail-closed partial state.
provisioner_running=true
platform_api_running=false
: >"$calls"
refresh_subsystem_control_plane_config >"$test_root/partial.out"
grep -Fq '只有一个服务在运行' "$test_root/partial.out" ||
  fail 'partial control-plane state was not reported'
if grep -Fq 'stop --timeout 60 platform-api' "$calls"; then
  fail 'reload attempted to stop an already absent platform-api'
fi
grep -Fq 'up -d --force-recreate --wait --wait-timeout 120 --no-deps platform-api' "$calls" ||
  fail 'reload did not recover the absent platform-api'
pass 'paired reload repairs a fail-closed partial control-plane state'

# Verify the public deploy.sh marker gate in an isolated asset directory. The
# gate executes before mutating subcommands, while help remains available.
fixture="$test_root/fixture"
mkdir -p "$fixture/bin" "$fixture/runtime"
for file in deploy.sh compose-scope.sh start-enabled.sh provisioner-config-refresh.sh \
  offline-package-metadata.sh offline-configure.sh public-transport.sh; do
  cp "$bin_dir/$file" "$fixture/bin/$file"
done
printf 'services: {}\n' >"$fixture/docker-compose.yml"
: >"$fixture/runtime/.control-plane-reload-required"
"$fixture/bin/deploy.sh" help >/dev/null || fail 'help was blocked by the reload marker'
if "$fixture/bin/deploy.sh" prepare contract >"$test_root/gate.out" 2>&1; then
  fail 'mutating prepare command crossed the reload-required marker'
fi
grep -Fq '仅允许只读诊断' "$test_root/gate.out" ||
  fail 'reload marker gate did not explain the fail-closed state'
pass 'reload-required marker blocks mutating CLI commands but preserves diagnostics'

# 运行中替换部署资产后，所有会重建/启动控制面的入口都必须先过同一道门禁；
# 只重建一侧就会回到 "production subsystem manifest drift detected"。
clearance_root="$test_root/clearance"
mkdir -p "$clearance_root/runtime"
deploy_dir="$clearance_root"
require_control_plane_reload_clearance >/dev/null || fail 'clearance gate blocked a clean deployment'
: >"$clearance_root/runtime/.control-plane-reload-required"
if require_control_plane_reload_clearance >"$test_root/clearance.out" 2>&1; then
  fail 'a pending control-plane reload marker did not block the caller'
fi
grep -Fq 'reload-control-plane' "$test_root/clearance.out" ||
  fail 'blocked caller was not told how to clear the marker'
rm -f "$clearance_root/runtime/.control-plane-reload-required"
ln -s /dev/null "$clearance_root/runtime/.control-plane-reload-required"
if require_control_plane_reload_clearance >/dev/null 2>&1; then
  fail 'a symlinked reload marker was accepted'
fi
pass 'shared clearance gate blocks mutating callers until the marker is cleared'

for entry in deploy-service.sh start-enabled.sh; do
  grep -Fq 'require_control_plane_reload_clearance' "$bin_dir/$entry" ||
    fail "$entry does not enforce the control-plane reload clearance"
done
grep -Fq 'require_control_plane_reload_clearance || return 1' "$bin_dir/provisioner-config-refresh.sh" ||
  fail 'single-sided provisioner refresh does not enforce the reload clearance'
pass 'deploy-service, start-enabled and single-sided refresh all enforce the gate'

printf 'Control-plane reload regression tests passed\n'
