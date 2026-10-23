#!/usr/bin/env bash
# 由 deploy.sh 加载；首次接入始终由平台完成，重启只处理已经接入的应用。
start_enabled_helper_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=provisioner-config-refresh.sh
source "$start_enabled_helper_dir/provisioner-config-refresh.sh"
source "$start_enabled_helper_dir/project-background.sh"
unset start_enabled_helper_dir

portal_compensation_ready() {
  local key value
  scope_enabled customer-opportunity && runtime_ready "$deploy_dir/runtime/customer.env" false || return 1
  for key in PORTAL_INVITE_COMPENSATION_PORTAL_CLIENT_ID PORTAL_INVITE_COMPENSATION_PORTAL_CLIENT_SECRET PORTAL_INVITE_COMPENSATION_PLATFORM_CLIENT_ID PORTAL_INVITE_COMPENSATION_PLATFORM_CLIENT_SECRET; do
    value="$(env_get "$deploy_dir/runtime/customer.env" "$key")"
    [[ -n "$value" && "$value" != PENDING_* && "$value" != REPLACE_WITH_* ]] || return 1
  done
}

portal_report_worker_ready() {
  local file="${PORTAL_REPORT_WORKER_RUNTIME_ENV_FILE:-$deploy_dir/runtime/portal-report-worker.env}" key value
  [[ -f "$file" && ! -L "$file" ]] || return 1
  for key in PORTAL_REPORT_WORKER_MYSQL_DSN PORTAL_OIDC_TENANT_ID PORTAL_REPORT_STORE_ROOT PORTAL_REPORT_CONTENT_KEY_BASE64 PORTAL_REPORT_INGEST_DESCRIPTOR_KEY_BASE64 PORTAL_REPORT_SOURCE_BASE_URL PORTAL_REPORT_SOURCE_TOKEN_URL PORTAL_REPORT_SOURCE_CLIENT_ID PORTAL_REPORT_SOURCE_CLIENT_SECRET; do
    value="$(env_get "$file" "$key")"
    [[ -n "$value" && "$value" != PENDING_* && "$value" != REPLACE_WITH_* ]] || return 1
  done
}

publish_registered_customer() (
  local component="$1" image="$2" previous key schema catalog_hash build_id catalog max_roles snapshot deployment_lock_held=false
  require_initialized
  require_control_plane_reload_clearance || return 1
  scope_require "$component" || return
  [[ "$image" =~ ^[a-z0-9.-]+(:[0-9]+)?/[a-z0-9._/-]+@sha256:[a-f0-9]{64}$ ]] || die '镜像必须是不可变 sha256 digest'
  subsystem_metadata "$component"
  exec 7>"$deploy_dir/runtime/.deploy.lock"
  flock -w 30 7 || die '另一个发布或平台接入正在进行'
  deployment_lock_held=true
  runtime_ready "$subsystem_runtime" || die '请先在平台完成该子系统接入，不能通过发布脚本生成平台凭据'
  if [[ "$component" == customer-opportunity ]]; then schema=crm; key=OIDC_ROLE_CONFIG_HASH; else schema=portal; key=PORTAL_ROLE_CONFIG_HASH; fi
  docker pull "$image" || return
  catalog="$(docker run --rm --entrypoint ./authz-catalog "$image" print "$schema")" || return
  catalog_hash="$(awk -F= '$1=="claims_role_config_hash" {print $2; exit}' <<< "$catalog")"
  [[ "$catalog_hash" =~ ^sha256:[a-f0-9]{64}$ ]] || die '无法读取镜像授权目录哈希'
  if [[ "$component" == customer-opportunity ]]; then
    max_roles="$(awk -F= '$1=="max_effective_roles" {print $2; exit}' <<< "$catalog")"
    [[ "$max_roles" =~ ^[0-9]+$ ]] || die '无法读取 CRM 最大角色数'
  fi
  install -d -m 700 "$deploy_dir/backups/releases"
  "$script_dir/backup-all.sh" --deployment-lock-fd 7 || return
  snapshot="$(mktemp -d "$deploy_dir/backups/releases/customer-single.XXXXXX")"
  install -m 600 "$release_file" "$snapshot/release.env"
  install -m 600 "$subsystem_runtime" "$snapshot/runtime.env"
  env_set "$release_file" "$subsystem_key" "$image"
  env_set "$subsystem_runtime" "$key" "$catalog_hash"
  if [[ "$component" == customer-opportunity ]]; then
    build_id="$(customer_presale_build_id "$image")" || return
    env_set "$release_file" CUSTOMER_PRESALE_TEMPORAL_WORKER_BUILD_ID "$build_id"
    env_set "$subsystem_runtime" OIDC_MAX_EFFECTIVE_ROLES "$max_roles"
  fi
  if ! refresh_subsystem_provisioner_config; then
    install -m 600 "$snapshot/release.env" "$release_file"
    install -m 600 "$snapshot/runtime.env" "$subsystem_runtime"
    refresh_subsystem_provisioner_config || true
    printf 'Agent 配置刷新或一致性核验失败，镜像指针与运行配置已恢复。\n' >&2
    return 1
  fi
  if ! start_registered "$component"; then
    install -m 600 "$snapshot/release.env" "$release_file"
    install -m 600 "$snapshot/runtime.env" "$subsystem_runtime"
    refresh_subsystem_provisioner_config || \
      printf '警告：回滚后 Agent 配置视图刷新失败，请停止接入操作并人工核对。\n' >&2
    printf '发布失败，镜像指针已恢复；保留失败容器用于排查，请检查迁移兼容性后重试。\n' >&2
    return 1
  fi
)

start_registered() (
  local component="$1" service database migration current_id configured services item selected
  scope_require "$component" || return
  subsystem_metadata "$component"
  require_control_plane_reload_clearance || return 1
  if [[ "${deployment_lock_held:-false}" != true ]]; then
    exec 7>"$deploy_dir/runtime/.deploy.lock"
    flock -w 30 7 || die '另一个发布或平台接入正在进行'
    refresh_subsystem_provisioner_config || return 1
  fi
  runtime_ready "$subsystem_runtime" || return
  if [[ "$component" == customer-portal ]]; then
    portal_report_worker_ready || die '门户报告 Worker 配置缺失；请在平台完成受控更新后重试'
  fi
  [[ "$component" != data-analysis ]] || subsystem_key=DATA_ANALYSIS_DASHBOARD_API_IMAGE
  configured="$(env_get "$release_file" "$subsystem_key")"
  [[ "$configured" =~ @sha256:[a-f0-9]{64}$ ]] || die "$component 尚未登记不可变镜像"
  docker image inspect "$configured" >/dev/null || die "$component 的登记镜像不在本机；请先导入"
  docker container inspect "$subsystem_candidate" >/dev/null 2>&1 && die "$component 有平台接入候选，请先在平台完成接入和 continue"
  current_id="$(docker ps -aq --filter "label=com.docker.compose.project=$(scope_project)" --filter "label=com.docker.compose.service=$subsystem_service")"
  [[ -n "$current_id" ]] || die "$component 尚无平台创建的业务容器，请先在平台接入"
  selected="$(scope_runtime_services "$component")" || return
  local -a applications=()
  while read -r service; do
    [[ -n "$service" ]] || continue
    applications+=("$service")
  done <<< "$selected"
  ((${#applications[@]} > 0)) || die '生产清单未选择任何长期运行组件'
  case "$component" in
    customer-opportunity) database=customer-mysql; migration=customer-migrate ;;
    customer-portal) database=portal-mysql; migration=portal-migrate ;;
    *) database="$component-mysql"; migration="$component-migrate" ;;
  esac
  case "$component" in
    contract|project|customer-opportunity) compose up -d --wait --wait-timeout 240 temporal || return ;;
  esac
  compose up -d --no-deps --wait --wait-timeout 240 "$database" || return
  "$script_dir/backup-all.sh" --deployment-lock-fd 7 || return
  [[ "$component" != project ]] || project_background_prepare || return
  if [[ "$component" == data-analysis ]]; then
    compose run --no-deps data-analysis-metabase-init || return
  fi
  # 失败的一次性容器保留，docker ps -a / docker logs 可直接排查。
  compose run --no-deps "$migration" || return
  case "$component" in
    contract) compose run --no-deps contract-api ./authz-catalog publish || return ;;
    customer-opportunity) compose run --no-deps customer-api ./authz-catalog publish crm || return ;;
    customer-portal) compose run --no-deps portal-api ./authz-catalog publish portal || return ;;
    settlement) compose run --no-deps settlement-catalog-sync || return ;;
  esac
  compose up -d --no-deps --wait --wait-timeout 180 "${applications[@]}" || return
  [[ "$component" != project ]] || project_background_activate || return
  verify_current_services "${applications[@]}" || return
  printf '%s 已完成迁移和健康启动。\n' "$component"
)

start_enabled() {
  require_initialized
  public_transport_prepare "$deploy_dir" "$runtime_file"
  scope_services >/dev/null || return
  scope_report || return
  local component platform_image gateway_image frontend_image
  platform_image="$(env_get "$release_file" PLATFORM_IMAGE)"
  gateway_image="$(env_get "$release_file" FILE_GATEWAY_IMAGE)"
  frontend_image="$(env_get "$release_file" FRONTEND_IMAGE)"
  "$script_dir/deploy-service.sh" platform "$platform_image" "$gateway_image" || return
  "$script_dir/deploy-service.sh" frontend "$frontend_image" || return
  for component in contract project customer-opportunity customer-portal settlement data-analysis; do
    scope_enabled "$component" || continue
    subsystem_metadata "$component"
    if ! runtime_ready "$subsystem_runtime" false; then
      printf '%s 允许部署但未接入，等待平台生成运行凭据。\n' "$component"
      continue
    fi
    if docker container inspect "$subsystem_candidate" >/dev/null 2>&1; then
      printf '%s 正在平台接入阶段，请在平台完成接入后 continue。\n' "$component"
      continue
    fi
    start_registered "$component" || return
  done
  verify
}
