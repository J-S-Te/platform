#!/usr/bin/env bash
set -Eeuo pipefail

source "$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)/deploy.sh"

test_root="$(mktemp -d "${TMPDIR:-/tmp}/subsystem-state-test.XXXXXX")"
trap 'rm -r -- "$test_root"' EXIT
deploy_dir="$test_root/deploy"
runtime_file="$deploy_dir/.env"
release_file="$deploy_dir/.release.env"
manifest_dir="$deploy_dir/manifests"
compose_file="$deploy_dir/docker-compose.yml"
profiles_dir="$deploy_dir/subsystems.d"
mkdir -p "$manifest_dir" "$deploy_dir/runtime" "$deploy_dir/subsystems.d"
touch "$runtime_file" "$release_file" "$deploy_dir/runtime/.deploy.lock"
chmod 600 "$runtime_file" "$release_file"
printf 'services: {}\n' > "$compose_file"
printf 'schema_version: 1\n' > "$deploy_dir/subsystems.d/contract_management-prod.yaml"
printf 'schema_version: 1\n' > "$deploy_dir/subsystems.d/data-analysis-prod.yaml"

mock_candidate=false
mock_candidate_code=contract_management
mock_candidate_environment=prod
mock_service=false
mock_health=healthy
mock_removed=false
mock_provisioner=false
mock_platform_api=false

flock() { return 0; }
stat() { if [[ "$*" == '-c %a '* ]]; then printf '600\n'; else command stat "$@"; fi; }
public_transport_prepare() { :; }
scope_services() { printf '%s\n' platform-api frontend contract-api data-analysis-api; }
compose() {
  if [[ "$*" == 'ps -q contract-api' && "$mock_service" == true ]]; then
    printf 'contract-service-id\n'
  elif [[ "$*" == 'ps -q subsystem-provisioner' && "$mock_provisioner" == true ]]; then
    printf 'provisioner-id\n'
  elif [[ "$*" == 'ps -q platform-api' && "$mock_platform_api" == true ]]; then
    printf 'platform-api-id\n'
  elif [[ "$*" == 'run --rm --no-deps subsystem-provisioner-socket-init' || \
          "$*" == 'up -d --force-recreate --wait --wait-timeout 60 --no-deps subsystem-provisioner' || \
          "$*" == 'stop --timeout 60 platform-api' || \
          "$*" == 'up -d --force-recreate --wait --wait-timeout 120 --no-deps platform-api' ]]; then
    printf '%s\n' "$*" >> "$test_root/refresh-compose.log"
  fi
}
docker() {
  if [[ "$1" == container && "$2" == inspect ]]; then
    if [[ "$3" == -f ]]; then
      case "$4" in
        *application_code*) printf '%s\n' "$mock_candidate_code" ;;
        *environment*) printf '%s\n' "$mock_candidate_environment" ;;
        *) return 97 ;;
      esac
      [[ "$mock_candidate" == true ]]
      return
    fi
    [[ "$3" == uip-contract-candidate && "$mock_candidate" == true ]]
    return
  fi
  if [[ "$1" == inspect && "$2" == -f ]]; then
    if { [[ "$4" == provisioner-id && "$mock_provisioner" == true ]] || \
         [[ "$4" == platform-api-id && "$mock_platform_api" == true ]]; }; then
      case "$3" in
        *State.Health*) printf 'healthy\n' ;;
        *) return 97 ;;
      esac
      return
    fi
    [[ "$4" == contract-service-id && "$mock_service" == true ]] || return 1
    case "$3" in
      *State.Running*) printf 'true\n' ;;
      *State.Health*) printf '%s\n' "$mock_health" ;;
      *) return 97 ;;
    esac
    return
  fi
  if [[ "$1" == create ]]; then
    [[ " $* " == *' --label com.basic-platform.environment=prod '* ]] || return 96
    mock_candidate=true
    return
  fi
  if [[ "$1" == rm && "$2" == uip-contract-candidate ]]; then
    mock_candidate=false
    mock_removed=true
    return
  fi
  if [[ "$1" == ps && "$2" == -q ]]; then
    if [[ " $* " == *' label=com.docker.compose.service=subsystem-provisioner '* && "$mock_provisioner" == true ]]; then
      printf 'provisioner-id\n'
    elif [[ " $* " == *' label=com.docker.compose.service=platform-api '* && "$mock_platform_api" == true ]]; then
      printf 'platform-api-id\n'
    fi
    return 0
  fi
  if [[ "$1" == exec && "$2" == provisioner-id ]]; then
    if [[ "$*" == *'SUBSYSTEM_PRODUCTION_RELEASE_ENV_PATH'* ]]; then
      printf '%s  /opt/unified-identity-platform/.release.env\n' "$(sha256sum "$release_file" | awk '{print $1}')"
      printf '%s  /opt/unified-identity-platform/docker-compose.yml\n' "$(sha256sum "$compose_file" | awk '{print $1}')"
    else
      production_profiles_host_manifest
    fi
    return 0
  fi
  if [[ "$1" == exec && "$2" == platform-api-id ]]; then
    if [[ "$3" == test && "$4" == -S ]]; then
      return 0
    fi
    production_profiles_host_manifest
    return 0
  fi
  printf 'unexpected Docker operation: %s\n' "$*" >&2
  return 97
}

assert_phase() {
  local expected="$1" actual
  actual="$(subsystem_phase contract)"
  [[ "$actual" == "$expected" ]] || {
    printf 'expected phase %s, got %s\n' "$expected" "$actual" >&2
    exit 1
  }
}

assert_phase NOT_IMPORTED
printf 'COMPONENT=contract\nVERSION=test\nIMAGE_REF=127.0.0.1:5000/uip/contract@sha256:test\n' > "$manifest_dir/contract-test.imported"
assert_phase IMPORTED

prepare_subsystem contract > "$test_root/prepare.out"
[[ "$mock_candidate" == true ]]
grep -q 'WAITING_FOR_PLATFORM_ADOPTION' "$test_root/prepare.out"
grep -q 'contract_management/prod' "$test_root/prepare.out"
assert_phase WAITING_FOR_PLATFORM_ADOPTION

# .release.env 是单文件 bind mount 进 subsystem-provisioner 的：env_set 用 mv 原子替换后
# 宿主机 inode 变化，而容器仍绑旧 inode，会一直读到替换前的占位镜像值，平台页面因此反复报
# “production subsystem image must use an immutable digest”。写完后必须让 Agent 重新绑定。
#
# 由于上面那次 prepare 已经推进到 WAITING_FOR_PLATFORM_ADOPTION，这里直接验证刷新函数本身：
# 有 Agent 时必须 force-recreate 并验证两个单文件挂载，没有 Agent 时必须静默跳过。
mock_provisioner=true
mock_platform_api=true
refresh_subsystem_provisioner > "$test_root/refresh.out"
grep -q '^stop --timeout 60 platform-api$' "$test_root/refresh-compose.log" || {
  echo 'refresh did not stop platform-api before switching Agent manifests' >&2; exit 1
}
grep -q '^run --rm --no-deps subsystem-provisioner-socket-init$' "$test_root/refresh-compose.log" || {
  echo 'refresh did not initialize the subsystem provisioner socket' >&2; exit 1
}
grep -q '^up -d --force-recreate --wait --wait-timeout 60 --no-deps subsystem-provisioner$' "$test_root/refresh-compose.log" || {
  echo 'refresh did not recreate the subsystem provisioner' >&2; exit 1
}
grep -q '^up -d --force-recreate --wait --wait-timeout 120 --no-deps platform-api$' "$test_root/refresh-compose.log" || {
  echo 'refresh did not recreate platform-api' >&2; exit 1
}
grep -q 'subsystems.d 集合摘要一致' "$test_root/refresh.out" || {
  echo 'refresh did not report the shared production manifest digest' >&2; exit 1
}
mock_provisioner=false
mock_platform_api=false
refresh_subsystem_provisioner > /dev/null || {
  echo 'refresh failed when no provisioner container exists' >&2; exit 1
}
[[ "$(wc -l < "$test_root/refresh-compose.log")" -eq 4 ]] || { echo 'refresh recreated a control plane that does not exist' >&2; exit 1; }

if continue_subsystem contract > "$test_root/early.out" 2>&1; then
  echo 'continue unexpectedly crossed the adoption phase' >&2
  exit 1
fi
grep -q 'WAITING_FOR_PLATFORM_ADOPTION' "$test_root/early.out"
if grep -q '接入配置缺失' "$test_root/early.out"; then
  echo 'premature continue leaked low-level missing-key diagnostics' >&2
  exit 1
fi

mock_candidate_environment=dev
assert_phase INVALID_CANDIDATE
if (prepare_subsystem contract) >/dev/null 2>&1; then
  echo 'prepare accepted a dev candidate for the production target' >&2
  exit 1
fi
[[ "$mock_candidate" == true ]]
mock_candidate_environment=prod

cat > "$deploy_dir/runtime/contract.env" <<'EOF'
PLATFORM_APPLICATION_CODE=contract_management
PLATFORM_ENVIRONMENT_CODE=prod
PLATFORM_AUDIT_CLIENT_ID=audit-client
PLATFORM_AUDIT_CLIENT_SECRET=audit-secret
FILE_GATEWAY_BASE_URL=http://file-gateway:8080
FILE_GATEWAY_APPLICATION_ID=file-app
FILE_GATEWAY_CLIENT_ID=file-client
FILE_GATEWAY_CLIENT_SECRET=file-secret
PLATFORM_AUTHORIZATION_CONTEXT_URL=http://platform-api:8080/context
PLATFORM_AUTHORIZATION_CATALOG_APPLICATION_ID=catalog-app
PLATFORM_AUTHORIZATION_CATALOG_CLIENT_ID=catalog-client
PLATFORM_AUTHORIZATION_CATALOG_CLIENT_SECRET=catalog-secret
OIDC_ISSUER=http://keycloak:8080/realms/basic-platform
OIDC_CLIENT_ID=contract-client
OIDC_CLIENT_SECRET=contract-secret
OIDC_REDIRECT_URI=http://example.test/contract_management/auth/callback
OIDC_TENANT_ID=tenant
OIDC_SESSION_ENCRYPTION_KEY_BASE64=session-key
EOF
chmod 600 "$deploy_dir/runtime/contract.env"
assert_phase PROVISIONING_OR_FAILED

mock_service=true
assert_phase READY_TO_CONTINUE
continue_subsystem contract > "$test_root/continue.out"
[[ "$mock_candidate" == false && "$mock_removed" == true ]]
grep -q 'VERIFIED' "$test_root/continue.out"
assert_phase VERIFIED
subsystem_status contract > "$test_root/status.out"
grep -q '当前阶段：VERIFIED' "$test_root/status.out"

# The data-analysis application code is snake_case, but its delivered profile
# uses the component's hyphenated slug. prepare must resolve the real filename.
data_image="127.0.0.1:5000/uip/data-analysis@sha256:$(printf '%064d' 1)"
printf 'COMPONENT=data-analysis\nVERSION=test\nIMAGE_REF=%s\n' "$data_image" > "$manifest_dir/data-analysis-test.imported"
for data_key in DATA_ANALYSIS_DASHBOARD_API_IMAGE DATA_ANALYSIS_AGGREGATION_WORKER_IMAGE DATA_ANALYSIS_ALERT_WORKER_IMAGE DATA_ANALYSIS_MIGRATE_IMAGE; do
  printf '%s=%s\n' "$data_key" "$data_image" >> "$manifest_dir/data-analysis-test.imported"
done
prepare_subsystem data-analysis > "$test_root/data-analysis-prepare.out"
grep -q 'data_analysis/prod' "$test_root/data-analysis-prepare.out"
[[ "$(env_get "$release_file" DATA_ANALYSIS_IMAGE)" == "$data_image" ]]
[[ "$(env_get "$release_file" DATA_ANALYSIS_DASHBOARD_API_IMAGE)" == "$data_image" ]]

echo 'subsystem deployment state-machine tests passed'
