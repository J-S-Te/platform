#!/usr/bin/env bash
set -Eeuo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
source_script="$script_dir/lifecycle.sh"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/uip-lifecycle-test.XXXXXX")"
trap 'rm -rf -- "$test_root"' EXIT

mkdir -p "$test_root/bin"
cat >"$test_root/bin/docker" <<'MOCK'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$DOCKER_CALL_LOG"
exit 0
MOCK
chmod +x "$test_root/bin/docker"

export PATH="$test_root/bin:$PATH"
export DOCKER_CALL_LOG="$test_root/docker.log"
: >"$DOCKER_CALL_LOG"

if bash "$source_script" remove contract --confirm WRONG >/dev/null 2>&1; then
  echo 'wrong remove confirmation unexpectedly succeeded' >&2
  exit 1
fi
[[ ! -s "$DOCKER_CALL_LOG" ]] || { echo 'wrong confirmation reached Docker' >&2; exit 1; }

if bash "$source_script" cleanup --confirm WRONG >/dev/null 2>&1; then
  echo 'wrong cleanup confirmation unexpectedly succeeded' >&2
  exit 1
fi
[[ ! -s "$DOCKER_CALL_LOG" ]] || { echo 'wrong cleanup confirmation reached Docker' >&2; exit 1; }
if bash "$source_script" destroy --confirm WRONG >/dev/null 2>&1; then
  echo 'wrong destroy confirmation unexpectedly succeeded' >&2
  exit 1
fi
[[ ! -s "$DOCKER_CALL_LOG" ]] || { echo 'wrong destroy confirmation reached Docker' >&2; exit 1; }

help_output="$(bash "$source_script" help)"
for action in 'repair <platform|frontend|子系统|all>' 'remove <子系统>' 'remove-all-applications' 'cleanup --confirm' 'purge --keep-data' 'destroy --confirm'; do
  [[ "$help_output" == *"$action"* ]] || { printf 'missing lifecycle help item: %s\n' "$action" >&2; exit 1; }
done

# 普通移除路径必须保留数据卷。完整销毁由独立命令隔离，要求检查备份，且仅移除带本项目标签的数据卷。
grep -Fq 'docker rm --force "$id"' "$source_script" || { echo 'container removal is not scoped by exact ID' >&2; exit 1; }
grep -Fq 'label=com.docker.compose.project=$project' "$source_script" || { echo 'Docker cleanup is not project-scoped' >&2; exit 1; }
grep -Fq 'service=platform-api' "$source_script" || { echo 'platform repair does not target the actual Compose service' >&2; exit 1; }
grep -Fq 'repair 不会替代首次部署或平台受控接入' "$source_script" || { echo 'subsystem repair can no longer distinguish repair from first deployment' >&2; exit 1; }
grep -Fq '== "$backup_started" ]] ||' "$source_script" || { echo 'destroy does not require a fresh system backup' >&2; exit 1; }
grep -Fq '"$deploy_dir/bin/backup-all.sh"' "$source_script" || { echo 'destroy does not create a verified backup first' >&2; exit 1; }
grep -Fq '"$deploy_dir/bin/backup-all.sh" --deployment-lock-fd 8' "$source_script" || { echo 'destroy backup does not inherit the held deployment lock' >&2; exit 1; }
grep -Fq 'docker volume rm "${volumes[@]}"' "$source_script" || { echo 'destroy does not remove only collected project volumes' >&2; exit 1; }
grep -Fq 'rm -rf -- "$deploy_dir"' "$source_script" || { echo 'purge no longer removes its guarded deployment root' >&2; exit 1; }
grep -Fq 'destroy --confirm DELETE_UIP_DATA' "$source_script" || { echo 'destroy confirmation is not isolated behind its dedicated token' >&2; exit 1; }

# 在隔离安装中验证真实生命周期和生产配置解析器。Docker、锁和平台 CLI 调用均使用明确替身，无法访问宿主机守护进程、数据库或现行部署。
fixture="$test_root/deployment"
mkdir -p "$fixture/bin" "$fixture/runtime" "$fixture/subsystems.d" "$test_root/profiles"
cp "$source_script" "$script_dir/compose-scope.sh" "$script_dir/subsystem-runtime.py" "$fixture/bin/"
# 生命周期编排的外部后台控制边界；协议及排空证据另由真实helper用例验证。
cat >"$fixture/bin/project-background.sh" <<'MOCK'
project_background_prepare() { printf 'BACKGROUND_PREPARE\n' >>"$ACTION_LOG"; [[ -z "${MOCK_BACKGROUND_FAIL:-}" ]]; }
project_background_activate() { printf 'BACKGROUND_ACTIVATE\n' >>"$ACTION_LOG"; [[ -z "${MOCK_BACKGROUND_FAIL:-}" ]]; }
project_background_teardown() { printf 'BACKGROUND_DRAIN\n' >>"$ACTION_LOG"; [[ -z "${MOCK_BACKGROUND_FAIL:-}" ]]; }
project_background_stopped() { printf 'BACKGROUND_STOPPED\n' >>"$ACTION_LOG"; }
MOCK
cp "$script_dir/../subsystems.d/"*-prod.yaml "$test_root/profiles/"
cp "$script_dir/../docker-compose.yml" "$fixture/docker-compose.yml"
awk '
  /^services:/ {services=1; next}
  services && /^[^[:space:]#]/ {services=0}
  services && /^  [a-z][a-z0-9-]*:$/ {gsub(/^[[:space:]]+|:$/, ""); print}
' "$fixture/docker-compose.yml" >"$test_root/all-services"
[[ -s "$test_root/all-services" ]] || { echo 'Compose fixture has no services' >&2; exit 1; }
printf 'COMPOSE_PROJECT_NAME=lifecycle-fixture\n' >"$fixture/.env"
for key in PLATFORM_IMAGE FRONTEND_IMAGE FILE_GATEWAY_IMAGE CUSTOMER_CRM_IMAGE CUSTOMER_PORTAL_IMAGE CONTRACT_IMAGE PROJECT_IMAGE SETTLEMENT_IMAGE DATA_ANALYSIS_DASHBOARD_API_IMAGE; do
  printf '%s=127.0.0.1:5000/uip/fixture@sha256:%064d\n' "$key" 0
done >"$test_root/release-pristine"
chmod 600 "$fixture/.env" "$test_root/release-pristine"

cat >"$fixture/bin/deploy.sh" <<'MOCK'
#!/usr/bin/env bash
printf 'VERIFY:%s\n' "$*" >>"$ACTION_LOG"
[[ "$1" == verify ]]
MOCK
cat >"$fixture/bin/deploy-service.sh" <<'MOCK'
#!/usr/bin/env bash
printf 'DEPLOY_SERVICE:%s\n' "$*" >>"$ACTION_LOG"
MOCK
cat >"$test_root/bin/stat" <<'MOCK'
#!/usr/bin/env bash
[[ "$1" == -c && "$2" == '%a' ]] || exit 90
python3 - "$3" <<'PY'
import os, stat, sys
print(format(stat.S_IMODE(os.stat(sys.argv[1]).st_mode), 'o'))
PY
MOCK
cat >"$test_root/bin/flock" <<'MOCK'
#!/usr/bin/env bash
printf 'LOCK:%s\n' "$*" >>"$ACTION_LOG"
[[ -z "${MOCK_LOCK_FAIL:-}" ]]
MOCK
cat >"$test_root/bin/curl" <<'MOCK'
#!/usr/bin/env bash
echo 'unexpected network call in lifecycle test' >&2
exit 90
MOCK
cat >"$test_root/bin/docker" <<'MOCK'
#!/usr/bin/env bash
set -Eeuo pipefail
printf '%s\n' "$*" >>"$DOCKER_CALL_LOG"
case "${1:-}" in
  compose)
    shift
    while (($#)); do
      case "$1" in
        --project-directory|--file|--env-file) shift 2 ;;
        config)
          [[ "${2:-}" == --services && -z "${MOCK_INVENTORY_FAIL:-}" ]] || exit 31
          cat "$MOCK_COMPOSE_SERVICES"
          exit 0 ;;
        up)
          printf 'UP:%s\n' "$*" >>"$ACTION_LOG"
          while (($#)) && [[ "$1" != --no-deps ]]; do shift; done
          [[ "${1:-}" == --no-deps ]] || exit 91
          shift
          for service in "$@"; do printf 'UP_SERVICE:%s\n' "$service" >>"$ACTION_LOG"; done
          exit 0 ;;
        *) exit 92 ;;
      esac
    done ;;
  container)
    [[ "${2:-}" == inspect ]] || exit 93
    [[ "${3:-}" == "${MOCK_CANDIDATE:-}" ]] ;;
  ps)
    [[ -z "${MOCK_PS_FAIL:-}" ]] || exit 32
    state="$MOCK_DEPLOYED_SERVICES"
    [[ "${2:-}" != -q ]] || state="$MOCK_RUNNING_SERVICES"
    service=''
    for argument in "$@"; do
      case "$argument" in label=com.docker.compose.service=*) service="${argument##*=}" ;; esac
    done
    [[ -n "$service" ]] || exit 94
    if grep -Fxq "$service" "$state"; then printf 'cid-%s\n' "$service"; fi ;;
  rm)
    [[ -z "${MOCK_REMOVE_FAIL:-}" ]] || exit 33
    [[ "$#" == 3 && "$2" == --force && "$3" == cid-* ]] || exit 95
    printf 'RM_SERVICE:%s\n' "${3#cid-}" >>"$ACTION_LOG" ;;
  *) echo "unexpected Docker call: $*" >&2; exit 96 ;;
esac
MOCK
chmod 755 "$fixture/bin/"*.sh "$test_root/bin/"*
export ACTION_LOG="$test_root/actions.log"
export MOCK_COMPOSE_SERVICES="$test_root/enabled-services"
export MOCK_DEPLOYED_SERVICES="$test_root/deployed-services"
export MOCK_RUNNING_SERVICES="$test_root/running-services"
export MOCK_CANDIDATE='' MOCK_PS_FAIL='' MOCK_INVENTORY_FAIL='' MOCK_LOCK_FAIL=''
evidence_dir="${UIP_LIFECYCLE_TEST_EVIDENCE_DIR:-}"
[[ -z "$evidence_dir" ]] || mkdir -p "$evidence_dir"

fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
assert_action() { grep -Fxq "$1" "$ACTION_LOG" || fail "missing action: $1"; }
assert_no_mutations() {
  if grep -Eq '^(UP:|RM_SERVICE:|DEPLOY_SERVICE:)' "$ACTION_LOG"; then
    cat "$ACTION_LOG" >&2
    fail "$case_name performed a mutation"
  fi
}
assert_no_removed() {
  if grep -Fxq "RM_SERVICE:$1" "$ACTION_LOG"; then fail "removed retained service $1"; fi
}
assert_services() {
  local kind="$1" actual expected; shift
  actual="$(sed -n "s/^${kind}_SERVICE://p" "$ACTION_LOG" | sort)"
  expected="$(printf '%s\n' "$@" | sort)"
  [[ "$actual" == "$expected" ]] || fail "$case_name has unexpected $kind services: $actual"
}
reset_case() {
  case_name="$1"
  : >"$ACTION_LOG"; : >"$DOCKER_CALL_LOG"
  cp "$test_root/profiles/"*.yaml "$fixture/subsystems.d/"
  cp "$test_root/all-services" "$MOCK_COMPOSE_SERVICES"
  cp "$test_root/all-services" "$MOCK_DEPLOYED_SERVICES"
  : >"$MOCK_RUNNING_SERVICES"
  cp "$test_root/release-pristine" "$fixture/.release.env"
  for name in customer portal contract project settlement data-analysis; do
    printf 'FIXTURE_CONFIG=ready\n' >"$fixture/runtime/$name.env"
  done
  for key in OIDC_CLIENT_ID OIDC_CLIENT_SECRET OIDC_TENANT_ID PLATFORM_AUTHORIZATION_CATALOG_APPLICATION_ID PLATFORM_AUTHORIZATION_CATALOG_CLIENT_ID PLATFORM_AUTHORIZATION_CATALOG_CLIENT_SECRET PLATFORM_APPLICATION_CODE PLATFORM_ENVIRONMENT_CODE PLATFORM_AUDIT_CLIENT_ID PLATFORM_AUDIT_CLIENT_SECRET PLATFORM_OWNER_DIRECTORY_CLIENT_ID PLATFORM_OWNER_DIRECTORY_CLIENT_SECRET; do
    printf '%s=fixture-value\n' "$key"
  done >"$fixture/runtime/customer.env"
  chmod 600 "$fixture/.release.env" "$fixture/runtime/"*.env
  export MOCK_CANDIDATE='' MOCK_PS_FAIL='' MOCK_INVENTORY_FAIL='' MOCK_LOCK_FAIL='' MOCK_BACKGROUND_FAIL='' MOCK_REMOVE_FAIL=''
}
run_case() {
  local expected="$1" status=0; shift
  bash "$fixture/bin/lifecycle.sh" "$@" >"$test_root/output.log" 2>&1 || status=$?
  if [[ -n "$evidence_dir" ]]; then
    cp "$test_root/output.log" "$evidence_dir/$case_name.output.log"
    cp "$DOCKER_CALL_LOG" "$evidence_dir/$case_name.docker.log"
    cp "$ACTION_LOG" "$evidence_dir/$case_name.actions.log"
  fi
  if [[ "$expected" == pass && "$status" != 0 ]] || [[ "$expected" == fail && "$status" == 0 ]]; then
    cat "$test_root/output.log" >&2
    fail "$case_name expected $expected, exit=$status"
  fi
  printf 'PASS: %s (expected=%s, exit=%s)\n' "$case_name" "$expected" "$status"
}

reset_case repair-contract
run_case pass repair contract
assert_services UP contract-api contract-worker
assert_action 'VERIFY:verify contract'
grep -Fq -- '--wait --wait-timeout 180 --no-deps contract-api contract-worker' "$ACTION_LOG" || fail 'repair lost health wait or --no-deps'

reset_case repair-crm
run_case pass repair customer-opportunity
assert_action 'UP_SERVICE:customer-contract-transfer-worker'
[[ "$(grep -c '^UP_SERVICE:' "$ACTION_LOG")" == 9 ]] || fail 'CRM repair is not the complete nine-process runtime'
if grep -Eq '^UP_SERVICE:.*(mysql|migrate|catalog-sync)$' "$ACTION_LOG"; then fail 'repair started a database/one-shot task'; fi

reset_case repair-project-background-not-drained
export MOCK_BACKGROUND_FAIL=1
run_case fail repair project
assert_action BACKGROUND_PREPARE
assert_no_mutations

reset_case repair-portal-with-crm
run_case pass repair customer-portal
assert_services UP portal-api portal-report-worker portal-invite-compensation-worker
for condition in missing-runtime incomplete-runtime disabled-dependency; do
  reset_case "repair-portal-$condition"
  case "$condition" in
    missing-runtime) rm "$fixture/runtime/customer.env" ;;
    incomplete-runtime) printf 'OIDC_CLIENT_SECRET=PENDING_ONBOARDING\n' >"$fixture/runtime/customer.env" ;;
    disabled-dependency) sed '/^customer-api$/d' "$test_root/all-services" >"$MOCK_COMPOSE_SERVICES" ;;
  esac
  run_case pass repair customer-portal
  assert_services UP portal-api portal-report-worker
done

reset_case remove-contract
printf 'project-api\n' >"$MOCK_RUNNING_SERVICES"
run_case pass remove contract --confirm REMOVE_contract_AFTER_RETIREMENT
assert_services RM contract-api contract-worker contract-migrate
assert_no_removed contract-mysql; assert_no_removed temporal

reset_case remove-crm
before_runtime="$(cksum "$fixture/runtime/customer.env")"
run_case pass remove customer-opportunity --confirm REMOVE_customer-opportunity_AFTER_RETIREMENT
assert_action 'RM_SERVICE:customer-contract-transfer-worker'
assert_action 'RM_SERVICE:customer-mysql'
[[ "$(grep -c '^RM_SERVICE:' "$ACTION_LOG")" == 11 ]] || fail 'CRM owned removal is incomplete or duplicated'
[[ "$before_runtime" == "$(cksum "$fixture/runtime/customer.env")" ]] || fail 'removal changed runtime credentials'

reset_case remove-disabled-portal-worker
rm "$fixture/runtime/customer.env"
sed '/^customer-api$/d; /^portal-invite-compensation-worker$/d' "$test_root/all-services" >"$MOCK_COMPOSE_SERVICES"
run_case pass remove customer-portal --confirm REMOVE_customer-portal_AFTER_RETIREMENT
assert_services RM portal-api portal-report-worker portal-invite-compensation-worker portal-migrate portal-mysql

reset_case remove-project-success
run_case pass remove project --confirm REMOVE_project_AFTER_RETIREMENT
assert_action BACKGROUND_STOPPED
[[ "$(grep -n '^BACKGROUND_STOPPED$' "$ACTION_LOG" | cut -d: -f1)" -gt "$(grep -n '^RM_SERVICE:project-sla-notifier$' "$ACTION_LOG" | cut -d: -f1)" ]] || fail 'stopped proof preceded removal'

reset_case remove-project-failed-removal
export MOCK_REMOVE_FAIL=1
run_case fail remove project --confirm REMOVE_project_AFTER_RETIREMENT
if grep -Fxq BACKGROUND_STOPPED "$ACTION_LOG"; then fail 'failed removal wrote stopped proof'; fi

reset_case remove-all
run_case pass remove-all-applications --confirm REMOVE_ALL_AFTER_RETIREMENT
for service in customer-contract-transfer-worker contract-worker portal-invite-compensation-worker data-analysis-metabase-init; do
  assert_action "RM_SERVICE:$service"
done
for service in customer-mysql portal-mysql project-mysql settlement-mysql data-analysis-mysql; do
  [[ "$(grep -Fxc "RM_SERVICE:$service" "$ACTION_LOG")" == 1 ]] || fail "exclusive database missing or duplicated: $service"
done
for service in contract-mysql temporal platform-api frontend; do assert_no_removed "$service"; done
[[ -f "$fixture/.env" && -f "$fixture/.release.env" && -f "$fixture/runtime/contract.env" ]] || fail 'remove-all deleted deployment configuration'
if grep -Eq '^(volume |.* --volumes|.* -v( |$))' "$DOCKER_CALL_LOG"; then fail 'removal requested volume deletion'; fi

# Exercise the real guard in a trimmed core-only deployment: no image and no owned project consumer.
cp "$fixture/bin/project-background.sh" "$test_root/background-double.sh"
cp "$script_dir/project-background.sh" "$fixture/bin/project-background.sh"
reset_case remove-all-core-only-real-guard
sed '/^PROJECT_IMAGE=/d' "$fixture/.release.env" >"$test_root/core-release"
cp "$test_root/core-release" "$fixture/.release.env"; chmod 600 "$fixture/.release.env"
sed '/^project-/d' "$test_root/all-services" >"$MOCK_DEPLOYED_SERVICES"
run_case pass remove-all-applications --confirm REMOVE_ALL_AFTER_RETIREMENT
assert_no_removed project-sla-notifier
assert_action RM_SERVICE:contract-api
cp "$test_root/background-double.sh" "$fixture/bin/project-background.sh"

for action in remove-project remove-all; do
  reset_case "$action-background-not-drained"
  export MOCK_BACKGROUND_FAIL=1
  if [[ "$action" == remove-project ]]; then
    run_case fail remove project --confirm REMOVE_project_AFTER_RETIREMENT
  else
    run_case fail remove-all-applications --confirm REMOVE_ALL_AFTER_RETIREMENT
  fi
  assert_action BACKGROUND_DRAIN
  assert_no_mutations
done

for action in repair remove; do
  reset_case "$action-invalid-profile"
  printf '\ncompose:\n' >>"$fixture/subsystems.d/contract_management-prod.yaml"
  if [[ "$action" == repair ]]; then run_case fail repair contract;
  else run_case fail remove contract --confirm REMOVE_contract_AFTER_RETIREMENT; fi
  assert_no_mutations
  grep -Fq '生产运行组件清单读取失败' "$test_root/output.log" || fail 'invalid profile failed before the real parser'
done
reset_case remove-all-invalid-last-profile
printf '\ncompose:\n' >>"$fixture/subsystems.d/data-analysis-prod.yaml"
run_case fail remove-all-applications --confirm REMOVE_ALL_AFTER_RETIREMENT
assert_no_mutations
reset_case repair-all-invalid-last-profile
printf '\ncompose:\n' >>"$fixture/subsystems.d/data-analysis-prod.yaml"
run_case fail repair all
assert_no_mutations

for guard in candidate credentials immutable adopted inventory docker-query lock; do
  reset_case "repair-guard-$guard"
  case "$guard" in
    candidate) export MOCK_CANDIDATE=uip-contract-candidate ;;
    credentials) printf 'OIDC_CLIENT_SECRET=PENDING_ONBOARDING\n' >"$fixture/runtime/contract.env" ;;
    immutable) sed 's/@sha256:[0-9a-f]*$/:latest/' "$test_root/release-pristine" >"$fixture/.release.env" ;;
    adopted) : >"$MOCK_DEPLOYED_SERVICES" ;;
    inventory) export MOCK_INVENTORY_FAIL=1 ;;
    docker-query) export MOCK_PS_FAIL=1 ;;
    lock) export MOCK_LOCK_FAIL=1 ;;
  esac
  run_case fail repair contract
  assert_no_mutations
done
for action in remove remove-all; do
  reset_case "$action-candidate"
  export MOCK_CANDIDATE=uip-contract-candidate
  if [[ "$action" == remove ]]; then run_case fail remove contract --confirm REMOVE_contract_AFTER_RETIREMENT;
  else run_case fail remove-all-applications --confirm REMOVE_ALL_AFTER_RETIREMENT; fi
  assert_no_mutations
done
reset_case remove-crm-with-live-portal
printf 'portal-api\n' >"$MOCK_RUNNING_SERVICES"
run_case fail remove customer-opportunity --confirm REMOVE_customer-opportunity_AFTER_RETIREMENT
assert_no_mutations
for action in remove remove-all; do
  reset_case "$action-docker-query-failure"
  export MOCK_PS_FAIL=1
  if [[ "$action" == remove ]]; then run_case fail remove customer-opportunity --confirm REMOVE_customer-opportunity_AFTER_RETIREMENT;
  else run_case fail remove-all-applications --confirm REMOVE_ALL_AFTER_RETIREMENT; fi
  assert_no_mutations
done

echo 'deployment lifecycle safety checks passed'
