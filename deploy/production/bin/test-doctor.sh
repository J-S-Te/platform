#!/usr/bin/env bash
# doctor 只读自检的替身回归：不连接真实 Docker、不修改主机、不启动或重启任何容器。
set -Eeuo pipefail

source "$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)/deploy.sh"

test_root="$(mktemp -d "${TMPDIR:-/tmp}/doctor-test.XXXXXX")"
trap 'rm -r -- "$test_root"' EXIT

deploy_dir="$test_root/deploy"
runtime_file="$deploy_dir/.env"
release_file="$deploy_dir/.release.env"
manifest_dir="$deploy_dir/manifests"
package_dir="$deploy_dir/packages"

fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

hex() { printf '%064d' "${1:-1}"; }
immutable() { printf '127.0.0.1:5000/uip/%s@sha256:%s' "$1" "$(hex "$2")"; }

# ---------------------------------------------------------------------------
# 替身：compose/public_transport 不连接 Docker；docker 只实现 doctor 读取所需的只读操作，
# 任何 restart/create/run/rm 都会被记录并返回 97，用于证明 doctor 确实只读。
# ---------------------------------------------------------------------------
mock_provisioner=true
mock_container_path='/opt/unified-identity-platform/.release.env'
mock_container_release=''
mock_cat_available=true
mock_sha256sum_available=true
docker_mutations="$test_root/docker-mutations.log"

public_transport_prepare() { :; }
compose() { return 0; }
docker() {
  case "$1" in
    ps)
      [[ "$2" == -q ]] || return 97
      [[ "$mock_provisioner" == true ]] && printf 'provisioner-id\n'
      return 0
      ;;
    inspect)
      [[ "$2" == -f ]] || return 97
      case "$3" in
        '{{.Name}}') printf '/uip-subsystem-provisioner\n' ;;
        *'Config.Env'*)
          printf 'SUBSYSTEM_PRODUCTION_HOST_DEPLOY_ROOT=%s\n' "${mock_container_path%/.release.env}"
          printf 'SUBSYSTEM_PRODUCTION_RELEASE_ENV_PATH=%s\n' "$mock_container_path"
          ;;
        *'Mounts'*) printf '%s|%s\n' "$release_file" "$mock_container_path" ;;
        *'StartedAt'*) printf '2025-01-01T00:00:00Z\n' ;;
        *) return 97 ;;
      esac
      return 0
      ;;
    exec)
      if [[ "$3" == cat ]]; then
        [[ "$mock_cat_available" == true ]] || return 127
        printf '%s\n' "$mock_container_release"
        return 0
      fi
      if [[ "$3" == test && "$4" == -f ]]; then return 0; fi
      if [[ "$3" == sha256sum ]]; then
        [[ "$mock_sha256sum_available" == true ]] || return 127
        printf '%s  %s\n' "$(printf '%s\n' "$mock_container_release" | sha256sum | awk '{print $1}')" "$4"
        return 0
      fi
      return 97
      ;;
    container) return 1 ;;
    restart|create|run|rm|start|stop|kill)
      printf 'MUTATION: docker %s\n' "$*" >> "$docker_mutations"
      return 97
      ;;
    *) return 97 ;;
  esac
}

# ---------------------------------------------------------------------------
# 夹具：默认全绿
# ---------------------------------------------------------------------------
write_release() {
  {
    printf 'FRONTEND_IMAGE=%s\n' "$(immutable frontend 1)"
    printf 'PLATFORM_IMAGE=%s\n' "$(immutable platform-backend 2)"
    printf 'CONTRACT_IMAGE=%s\n' "$(immutable contract-backend 3)"
    printf 'CUSTOMER_CRM_IMAGE=%s\n' "$(immutable customer-opportunity-backend 4)"
    printf 'CUSTOMER_PORTAL_IMAGE=%s\n' "$(immutable customer-portal-backend 5)"
    printf 'PROJECT_IMAGE=%s\n' "$(immutable project-backend 6)"
    printf 'SETTLEMENT_IMAGE=%s\n' "$(immutable settlement-backend 7)"
    printf 'DATA_ANALYSIS_IMAGE=%s\n' "$(immutable data-analysis-backend 8)"
    printf 'DATA_ANALYSIS_DASHBOARD_API_IMAGE=%s\n' "$(immutable data-analysis-backend 8)"
    printf 'DATA_ANALYSIS_AGGREGATION_WORKER_IMAGE=%s\n' "$(immutable data-analysis-backend 8)"
    printf 'DATA_ANALYSIS_ALERT_WORKER_IMAGE=%s\n' "$(immutable data-analysis-backend 8)"
    printf 'DATA_ANALYSIS_MIGRATE_IMAGE=%s\n' "$(immutable data-analysis-backend 8)"
  } > "$release_file"
  chmod 600 "$release_file"
}

write_runtime() {
  cat > "$deploy_dir/runtime/contract.env" <<'EOF'
PLATFORM_APPLICATION_CODE=contract_management
PLATFORM_ENVIRONMENT_CODE=prod
OIDC_ISSUER=http://keycloak:8080/realms/basic-platform
OIDC_CLIENT_ID=contract-client
OIDC_CLIENT_SECRET=contract-secret
PLATFORM_AUTHORIZATION_CATALOG_CLIENT_ID=catalog-client
PLATFORM_AUTHORIZATION_CATALOG_CLIENT_SECRET=catalog-secret
PLATFORM_AUDIT_CLIENT_ID=audit-client
PLATFORM_AUDIT_CLIENT_SECRET=audit-secret
FILE_GATEWAY_CLIENT_ID=file-client
FILE_GATEWAY_CLIENT_SECRET=file-secret
OIDC_SESSION_ENCRYPTION_KEY_BASE64=c2Vzc2lvbi1rZXk=
EOF
  chmod 600 "$deploy_dir/runtime/contract.env"
}

# 容器内看到的 .release.env 是宿主机文件的镜像；改过宿主机文件后必须显式重同步，
# 这正是坑 3 的两种状态：同步代表容器已重绑，未同步代表容器还绑旧 inode。
sync_container_release() { mock_container_release="$(cat "$release_file")"; }

reset_green() {
  rm -rf "$deploy_dir"
  mkdir -p "$manifest_dir" "$package_dir" "$deploy_dir/runtime" "$deploy_dir/subsystems.d"
  printf 'OFFLINE_CONFIGURATION_COMPLETE=true\nPUBLIC_ACCESS_MODE=ip\nPUBLIC_PLATFORM_HOST=192.0.2.20\nPUBLIC_SSO_HOST=192.0.2.20\nPUBLIC_HTTP_PORT=8081\nKEYCLOAK_HTTP_PORT=18090\n' > "$runtime_file"
  chmod 600 "$runtime_file"
  write_release
  printf 'COMPONENT=contract\nVERSION=test\nIMAGE_REF=%s\nIMAGE_DIGEST=%s\nPACKAGE_SHA256=%s\n' \
    "$(immutable contract-backend 3)" "$(hex 3)" "$(hex 9)" > "$manifest_dir/contract-test.imported"
  chmod 600 "$manifest_dir/contract-test.imported"
  printf 'fixture package, never sent to Docker\n' > "$package_dir/contract-backend-test-linux-amd64.tar.gz"
  (cd "$package_dir" && sha256sum contract-backend-test-linux-amd64.tar.gz > contract-backend-test-linux-amd64.tar.gz.sha256)
  write_runtime
  mock_provisioner=true
  mock_container_path='/opt/unified-identity-platform/.release.env'
  mock_cat_available=true
  mock_sha256sum_available=true
  sync_container_release
  rm -f "$docker_mutations"
}

doctor_log="$test_root/doctor.log"
doctor_rc=0
run_doctor() {
  doctor_rc=0
  doctor "$@" >"$doctor_log" 2>&1 || doctor_rc=$?
}
expect_rc() { [[ "$doctor_rc" -eq "$1" ]] || { cat "$doctor_log" >&2; fail "expected exit $1, got $doctor_rc"; }; }
expect_log() { grep -qF -- "$1" "$doctor_log" || { cat "$doctor_log" >&2; fail "missing doctor output: $1"; }; }
reject_log() { if grep -qF -- "$1" "$doctor_log"; then cat "$doctor_log" >&2; fail "unexpected doctor output: $1"; fi; }
snapshot() { find "$deploy_dir" -type f -exec sha256sum {} + | LC_ALL=C sort; }

# ---------------------------------------------------------------------------
# 1. 全绿：所有检查通过、且 doctor 未改动任何文件、未触发任何容器变更
# ---------------------------------------------------------------------------
reset_green
before="$(snapshot)"
run_doctor contract
expect_rc 0
expect_log '[OK] .env 存在且权限为 0600'
expect_log '[OK] .release.env 存在且权限为 0600'
expect_log '摘要校验通过'
expect_log '已登记不可变 digest，与导入记录一致'
expect_log '内容一致'
expect_log '未发现占位值或空值'
expect_log '当前阶段：IMPORTED'
expect_log '未发现阻断问题。'
reject_log '[警告]'
reject_log '[失败]'
after="$(snapshot)"
[[ "$before" == "$after" ]] || fail 'doctor modified files in the green scenario'
[[ ! -e "$docker_mutations" ]] || { cat "$docker_mutations" >&2; fail 'doctor issued a container mutation'; }

# 不带子系统参数时也必须全绿，并且不显示阶段。
run_doctor
expect_rc 0
expect_log '未发现阻断问题。'
reject_log '[失败]'
reject_log '当前阶段：'

# data-analysis 是多镜像包：每个生产键必须与导入记录中的同名字段比较，
# 不能把 Worker/Migrate 错误地与 dashboard API 的 IMAGE_REF 比较。
reset_green
dashboard_ref="$(immutable data-analysis-dashboard-api 8)"
aggregation_ref="$(immutable data-analysis-aggregation-worker 9)"
alert_ref="$(immutable data-analysis-alert-worker 10)"
migrate_ref="$(immutable data-analysis-production-migrate 11)"
env_set "$release_file" DATA_ANALYSIS_IMAGE "$dashboard_ref"
env_set "$release_file" DATA_ANALYSIS_DASHBOARD_API_IMAGE "$dashboard_ref"
env_set "$release_file" DATA_ANALYSIS_AGGREGATION_WORKER_IMAGE "$aggregation_ref"
env_set "$release_file" DATA_ANALYSIS_ALERT_WORKER_IMAGE "$alert_ref"
env_set "$release_file" DATA_ANALYSIS_MIGRATE_IMAGE "$migrate_ref"
{
  printf 'COMPONENT=data-analysis\nVERSION=test\n'
  printf 'IMAGE_REF=%s\n' "$dashboard_ref"
  printf 'DATA_ANALYSIS_DASHBOARD_API_IMAGE=%s\n' "$dashboard_ref"
  printf 'DATA_ANALYSIS_AGGREGATION_WORKER_IMAGE=%s\n' "$aggregation_ref"
  printf 'DATA_ANALYSIS_ALERT_WORKER_IMAGE=%s\n' "$alert_ref"
  printf 'DATA_ANALYSIS_MIGRATE_IMAGE=%s\n' "$migrate_ref"
  printf 'IMAGE_DIGEST=%s\nPACKAGE_SHA256=%s\n' "$(hex 8)" "$(hex 12)"
} > "$manifest_dir/data-analysis-test.imported"
chmod 600 "$manifest_dir/data-analysis-test.imported"
sync_container_release
run_doctor
expect_rc 0
expect_log 'data-analysis：DATA_ANALYSIS_IMAGE DATA_ANALYSIS_DASHBOARD_API_IMAGE DATA_ANALYSIS_AGGREGATION_WORKER_IMAGE DATA_ANALYSIS_ALERT_WORKER_IMAGE DATA_ANALYSIS_MIGRATE_IMAGE 已登记不可变 digest，与导入记录一致'
reject_log '[失败]'

# 任一专用键被篡改时必须精确指出对应清单字段并失败。
env_set "$release_file" DATA_ANALYSIS_AGGREGATION_WORKER_IMAGE "$(immutable data-analysis-aggregation-worker 13)"
sync_container_release
run_doctor
expect_rc 1
expect_log '[失败] DATA_ANALYSIS_AGGREGATION_WORKER_IMAGE 与 data-analysis-test.imported 的 DATA_ANALYSIS_AGGREGATION_WORKER_IMAGE 不一致'
expect_log '存在阻断问题'

# ---------------------------------------------------------------------------
# 2. 伴随文件缺失 -> [失败] 且非 0 退出
# ---------------------------------------------------------------------------
reset_green
rm -f "$package_dir/contract-backend-test-linux-amd64.tar.gz.sha256"
run_doctor contract
expect_rc 1
expect_log '[失败] 缺少校验伴随文件'
expect_log 'sha256sum contract-backend-test-linux-amd64.tar.gz'
expect_log '存在阻断问题'

# 摘要不匹配也必须失败。
reset_green
printf 'tampered\n' > "$package_dir/contract-backend-test-linux-amd64.tar.gz"
run_doctor contract
expect_rc 1
expect_log '摘要或伴随文件绑定失败'

# ---------------------------------------------------------------------------
# 3. .release.env 仍是占位值（只 import 未 prepare）-> 仅警告，0 退出
# ---------------------------------------------------------------------------
reset_green
sed -i 's#^CONTRACT_IMAGE=.*#CONTRACT_IMAGE=uip-package/contract-backend:pending#' "$release_file"
sync_container_release
run_doctor contract
expect_rc 0
expect_log '[警告] contract 已 import，但 CONTRACT_IMAGE 仍是占位值'
expect_log 'prepare contract'
expect_log '[警告] .release.env 仍有 1 个镜像键是占位值'
expect_log 'CONTRACT_IMAGE=uip-package/contract-backend:pending'
expect_log '未发现阻断问题，但有'
reject_log '[失败]'

# ---------------------------------------------------------------------------
# 4. 容器内 .release.env 与宿主机不一致（坑 3）-> [失败] 且建议 docker restart
# ---------------------------------------------------------------------------
reset_green
printf '# .release.env 已被原子替换\n' >> "$release_file"
# 故意不调用 sync_container_release：容器仍绑旧 inode。
run_doctor contract
expect_rc 1
expect_log '[失败] 容器 uip-subsystem-provisioner 仍绑定旧的 .release.env'
expect_log 'docker restart uip-subsystem-provisioner'

# Agent 未运行时不得误报，也不得尝试重启。
reset_green
mock_provisioner=false
run_doctor contract
expect_rc 0
expect_log 'subsystem-provisioner 未运行'
reject_log 'docker restart'
[[ ! -e "$docker_mutations" ]] || fail 'doctor touched containers when the agent is stopped'

# 镜像缺少 cat 时必须退化为 sha256sum，而不是误报不一致。
reset_green
mock_cat_available=false
run_doctor contract
expect_rc 0
expect_log '容器 uip-subsystem-provisioner 内读取到的 .release.env 与宿主机内容一致'
reject_log '[失败]'

# 容器内完全无法读取时只警告，并仍给出 docker restart 建议，不得直接判失败。
reset_green
mock_cat_available=false
mock_sha256sum_available=false
run_doctor contract
expect_rc 0
expect_log '[警告] 无法读取容器 uip-subsystem-provisioner'
expect_log 'docker restart uip-subsystem-provisioner'

# ---------------------------------------------------------------------------
# 5. runtime 存在占位键（坑 4）-> 仅警告，按文件分组列出键
# ---------------------------------------------------------------------------
reset_green
{
  printf 'OIDC_ISSUER=PENDING_ONBOARDING\n'
  printf 'OIDC_CLIENT_ID=REPLACE_WITH_CONTRACT_CLIENT_ID\n'
  printf 'OIDC_CLIENT_SECRET=\n'
  printf 'PLATFORM_AUDIT_CLIENT_ID=audit-client\n'
} > "$deploy_dir/runtime/contract.env"
chmod 600 "$deploy_dir/runtime/contract.env"
run_doctor contract
expect_rc 0
expect_log '[警告] contract.env 有 3 个键仍是占位值或空值'
expect_log 'OIDC_ISSUER=PENDING_ONBOARDING'
expect_log 'OIDC_CLIENT_ID=REPLACE_WITH_CONTRACT_CLIENT_ID'
expect_log 'OIDC_CLIENT_SECRET='
expect_log '不代表文件权限问题'
expect_log '未发现阻断问题，但有'
reject_log '[失败]'
reject_log 'PLATFORM_AUDIT_CLIENT_ID'

# ---------------------------------------------------------------------------
# 6. 子系统参数与配置文件错误
# ---------------------------------------------------------------------------
reset_green
rm -f "$manifest_dir/contract-test.imported"
run_doctor contract
expect_rc 1
expect_log '[失败] contract 尚未 import'
expect_log 'import packages/contract-*-linux-amd64.tar.gz'

reset_green
run_doctor not-a-subsystem
expect_rc 1
expect_log '[失败] 不支持的子系统：not-a-subsystem'

reset_green
chmod 644 "$runtime_file"
run_doctor
expect_rc 1
expect_log '[失败] .env 权限为 644，必须为 0600'

reset_green
rm -f "$runtime_file"
run_doctor
expect_rc 1
expect_log '[失败] .env 不存在'

# .release.env 缺失时既要明确失败，也不能泄漏 awk 的低层报错。
reset_green
rm -f "$release_file"
run_doctor
expect_rc 1
expect_log '[失败] .release.env 不存在'
reject_log "can't open file"

echo 'doctor read-only environment self-check tests passed'
