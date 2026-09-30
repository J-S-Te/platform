#!/usr/bin/env bash
# Safe operational lifecycle actions for the offline production deployment.
set -Eeuo pipefail
umask 077

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
deploy_dir="$(cd -- "$script_dir/.." && pwd -P)"
runtime_file="$deploy_dir/.env"
release_file="$deploy_dir/.release.env"
compose_file="$deploy_dir/docker-compose.yml"
source "$script_dir/compose-scope.sh"
command_name="${1:-help}"
shift || true

usage() {
  cat <<'EOF'
用法：./bin/lifecycle.sh <命令>

  repair <platform|frontend|子系统|all>
      按当前不可变镜像修复并重建目标容器；保留数据库/文件数据卷。
      子系统仅在平台已受控采用、runtime 凭据完整且没有候选容器时可修复。
  remove <子系统> --confirm REMOVE_<子系统>_AFTER_RETIREMENT
      停止并移除指定子系统及其专属数据库容器；保留全部数据卷和 runtime 凭据。
      执行前必须先在平台控制面退役对应 prod 环境。
  remove-all-applications --confirm REMOVE_ALL_AFTER_RETIREMENT
      移除所有子系统容器与专属数据库容器；保留基础平台、前端、全部数据卷和配置。
  cleanup --confirm CLEANUP_UIP_ENVIRONMENT
      停止并移除当前 Compose 项目的容器；保留数据库/文件卷、配置、镜像与备份。
  purge --keep-data --confirm PURGE_UIP_KEEP_DATA
      停止并移除容器，删除部署脚本/配置/包；保留 Docker 数据卷及部署备份，
      备份目录移至部署目录旁。不会删除 File Gateway 外置数据目录。
  destroy --confirm DELETE_UIP_DATA
      先执行并校验数据库备份，再删除本 Compose 项目的全部命名卷及部署目录；
      备份移至部署目录旁。File Gateway 外置对象目录默认保留。
  destroy --remove-file-gateway-files --confirm DELETE_UIP_DATA_AND_FILES
      同上，并删除配置指向的 File Gateway 外置对象目录。

确认字符串是防误操作门槛，不是数据删除权限提升。remove/cleanup/purge 不删除数据卷；
只有 destroy 显式删除带当前 Compose project 标签的卷，不执行全局 docker system prune。
EOF
}

die() { printf '错误：%s\n' "$*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || die "缺少命令：$1"; }

confirm_exact() {
  local expected="$1" supplied="${2:-}" prompt="${3:-继续将执行上述操作，输入确认字符串}"
  if [[ -z "$supplied" && -t 0 ]]; then
    printf '%s [%s]: ' "$prompt" "$expected" >&2
    IFS= read -r supplied || true
  fi
  [[ "$supplied" == "$expected" ]] || die "确认字符串不匹配；未执行任何操作。要求：$expected"
}

require_config() {
  [[ -f "$runtime_file" && ! -L "$runtime_file" ]] || die "运行配置不存在或不安全：$runtime_file"
  [[ -f "$release_file" && ! -L "$release_file" ]] || die "发布配置不存在或不安全：$release_file"
  [[ -f "$compose_file" && ! -L "$compose_file" ]] || die "Compose 配置不存在或不安全：$compose_file"
  [[ "$(stat -c '%a' "$runtime_file")" == 600 && "$(stat -c '%a' "$release_file")" == 600 ]] ||
    die '运行配置权限不正确，要求 .env 与 .release.env 均为 0600'
}

compose() {
  docker compose --project-directory "$deploy_dir" --file "$compose_file" \
    --env-file "$runtime_file" --env-file "$release_file" "$@"
}

compose_project_name() {
  local value
  value="$(awk -F= '$1=="COMPOSE_PROJECT_NAME" {sub(/^[^=]*=/, ""); print; exit}' "$runtime_file")"
  value="${value:-basic-platform-production}"
  [[ "$value" =~ ^[a-z0-9][a-z0-9_-]*$ ]] || die "COMPOSE_PROJECT_NAME 格式非法：$value"
  printf '%s' "$value"
}

remove_services_keep_volumes() {
  local project="$1" service ids id
  shift
  for service in "$@"; do
    ids="$(docker ps -aq --filter "label=com.docker.compose.project=$project" \
      --filter "label=com.docker.compose.service=$service")"
    [[ -n "$ids" ]] || continue
    while IFS= read -r id; do
      [[ -n "$id" ]] || continue
      docker rm --force "$id" >/dev/null
      printf '已移除容器：project=%s service=%s id=%s（保留挂载卷）\n' "$project" "$service" "$id"
    done <<< "$ids"
  done
}

acquire_lock() {
  need flock
  [[ ! -L "$deploy_dir/runtime/.deploy.lock" ]] || die '部署锁不能是符号链接'
  exec 8>"$deploy_dir/runtime/.deploy.lock"
  flock -w 30 8 || die '另一个部署/清理操作正在运行，请稍后重试'
}

# Output only the app/worker services owned by each deployment. Database services
# are appended separately; volumes remain because compose rm is never called with -v.
app_services() {
  case "$1" in
    customer-opportunity)
      printf '%s\n' customer-api customer-opportunity-alert-worker customer-owner-notification-worker \
        customer-presale-alert-worker customer-presale-assignment-notification-worker \
        customer-presale-progress-notification-worker customer-notification-delivery-worker customer-presale-worker ;;
    customer-portal) printf '%s\n' portal-api portal-invite-compensation-worker ;;
    contract) printf '%s\n' contract-api ;;
    project) printf '%s\n' project-api project-sla-notifier ;;
    settlement) printf '%s\n' settlement-api settlement-worker ;;
    data-analysis) printf '%s\n' data-analysis-api data-analysis-aggregation-worker data-analysis-alert-worker data-analysis-metabase ;;
    *) return 1 ;;
  esac
}

database_service() {
  case "$1" in
    customer-opportunity) printf customer-mysql ;;
    customer-portal) printf portal-mysql ;;
    contract) printf contract-mysql ;;
    project) printf project-mysql ;;
    settlement) printf settlement-mysql ;;
    data-analysis) printf data-analysis-mysql ;;
    *) return 1 ;;
  esac
}

runtime_file_for() {
  case "$1" in
    customer-opportunity) printf '%s/runtime/customer.env' "$deploy_dir" ;;
    customer-portal) printf '%s/runtime/portal.env' "$deploy_dir" ;;
    contract) printf '%s/runtime/contract.env' "$deploy_dir" ;;
    project) printf '%s/runtime/project.env' "$deploy_dir" ;;
    settlement) printf '%s/runtime/settlement.env' "$deploy_dir" ;;
    data-analysis) printf '%s/runtime/data-analysis.env' "$deploy_dir" ;;
    *) return 1 ;;
  esac
}

runtime_ready() {
  local file="$1"
  [[ -f "$file" && ! -L "$file" ]] || return 1
  ! awk -F= '
    /^[[:space:]]*#/ || /^[[:space:]]*$/ {next}
    {
      k=$1; v=substr($0,index($0,"=")+1)
      gsub(/^[[:space:]]+|[[:space:]]+$/, "", v)
      if (v == "" || v == "PENDING_ONBOARDING" || v ~ /^REPLACE_WITH_/) bad=1
    }
    END {exit bad ? 0 : 1}
  ' "$file"
}

release_image_ready() {
  local key="$1" image
  image="$(awk -F= -v k="$key" '$1==k {sub(/^[^=]*=/,""); print; exit}' "$release_file")"
  [[ "$image" =~ @sha256:[a-f0-9]{64}$ ]]
}

current_release_image() {
  local key="$1" image
  image="$(awk -F= -v k="$key" '$1==k {sub(/^[^=]*=/,""); print; exit}' "$release_file")"
  [[ "$image" =~ @sha256:[a-f0-9]{64}$ ]] || die "$key 当前不是不可变 digest；请先使用 deploy 发布已导入镜像，repair 不会替你升级"
  printf '%s' "$image"
}

repair_core() {
  local component="$1" key image project service
  project="$(compose_project_name)"
  case "$component" in
    platform) key=PLATFORM_IMAGE; service=platform-api ;;
    frontend) key=FRONTEND_IMAGE; service=frontend ;;
    *) die "不是基础组件：$component" ;;
  esac
  docker ps -aq --filter "label=com.docker.compose.project=$project" \
    --filter "label=com.docker.compose.service=$service" | grep -q . ||
    die "$component 尚无已部署容器；首次部署请执行 ./bin/deploy.sh deploy $component"
  image="$(current_release_image "$key")"
  printf 'repair 使用当前版本 %s；不会采用仅 import 尚未发布的新镜像。\n' "$image"
  if [[ "$component" == platform ]]; then
    "$deploy_dir/bin/deploy-service.sh" "$component" "$image" "$(current_release_image FILE_GATEWAY_IMAGE)"
  else
    "$deploy_dir/bin/deploy-service.sh" "$component" "$image"
  fi
  "$deploy_dir/bin/deploy.sh" verify
}

repair_subsystem() {
  local component="$1" services runtime project service found=false
  scope_require "$component" || return
  app_services "$component" >/dev/null || die "不支持的子系统：$component"
  project="$(compose_project_name)"
  runtime="$(runtime_file_for "$component")"
  runtime_ready "$runtime" || die "$component runtime 仍有缺失/占位值；先由平台控制面受控采用或重试，不能用猜测配置修复。"
  case "$component" in
    customer-opportunity) release_image_ready CUSTOMER_CRM_IMAGE || die 'CUSTOMER_CRM_IMAGE 尚非不可变 digest';;
    customer-portal) release_image_ready CUSTOMER_PORTAL_IMAGE || die 'CUSTOMER_PORTAL_IMAGE 尚非不可变 digest';;
    contract) release_image_ready CONTRACT_IMAGE || die 'CONTRACT_IMAGE 尚非不可变 digest';;
    project) release_image_ready PROJECT_IMAGE || die 'PROJECT_IMAGE 尚非不可变 digest';;
    settlement) release_image_ready SETTLEMENT_IMAGE || die 'SETTLEMENT_IMAGE 尚非不可变 digest';;
    data-analysis) release_image_ready DATA_ANALYSIS_DASHBOARD_API_IMAGE || die 'DATA_ANALYSIS_DASHBOARD_API_IMAGE 尚非不可变 digest';;
  esac
  local candidate="uip-${component}-candidate"
  docker container inspect "$candidate" >/dev/null 2>&1 && die "发现平台受控部署候选 ${candidate}；不要并行重建，请在平台页面点击重试并检查 Agent 日志。"
  while IFS= read -r service; do
    if [[ -n "$(docker ps -aq --filter "label=com.docker.compose.project=$project" \
      --filter "label=com.docker.compose.service=$service")" ]]; then
      found=true
      break
    fi
  done < <(app_services "$component")
  [[ "$found" == true ]] || die "$component 没有已部署的应用/Worker 容器；repair 不会替代首次部署或平台受控接入。"
  services="$(app_services "$component" | tr '\n' ' ')"
  printf '修复 %s：只重建应用/Worker 容器（--no-deps），不重启数据库，不删除数据卷。\n' "$component"
  # Current deployment contains one or more profile-scoped workers; explicitly naming
  # the approved service list activates those profiles without starting dependencies.
  # shellcheck disable=SC2086
  compose up -d --force-recreate --wait --wait-timeout "${UIP_HEALTH_WAIT_SECONDS:-180}" --no-deps $services
}

repair() {
  local target="${1:-}" subsystem
  require_config
  need docker; need curl
  case "$target" in
    platform)
      repair_core platform
      ;;
    frontend)
      repair_core frontend
      ;;
    customer-opportunity|customer-portal|contract|project|settlement|data-analysis)
      acquire_lock; repair_subsystem "$target"
      "$deploy_dir/bin/deploy.sh" verify "$target"
      ;;
    all)
      repair_core platform
      repair_core frontend
      for subsystem in customer-opportunity customer-portal contract project settlement data-analysis; do
        scope_enabled "$subsystem" || continue
        if runtime_ready "$(runtime_file_for "$subsystem")"; then
          if docker container inspect "uip-${subsystem}-candidate" >/dev/null 2>&1; then
            printf '[跳过] %s 存在平台受控候选；请在平台页面处理，不由 repair all 干预。\n' "$subsystem"
          else
            acquire_lock
            repair_subsystem "$subsystem"
            flock -u 8
            "$deploy_dir/bin/deploy.sh" verify "$subsystem"
          fi
        else
          printf '[跳过] %s 尚未完成平台受控采用/凭据下发。\n' "$subsystem"
        fi
      done
      ;;
    *) usage; exit 2 ;;
  esac
}

remove_subsystem() {
  local component="$1" token="$2" db services runtime project
  app_services "$component" >/dev/null || die "不支持的子系统：$component"
  confirm_exact "REMOVE_${component}_AFTER_RETIREMENT" "$token" \
    "确认平台控制面已退役 ${component}/prod，输入确认字符串"
  require_config; need docker; acquire_lock
  project="$(compose_project_name)"
  runtime="$(runtime_file_for "$component")"
  if docker container inspect "uip-${component}-candidate" >/dev/null 2>&1; then
    die '存在未完成的受控接入候选；先在平台完成退役/重试，拒绝擅自移除候选容器'
  fi
  if [[ "$component" == customer-opportunity ]] &&
    [[ -n "$(docker ps -q --filter "label=com.docker.compose.project=$project" --filter label=com.docker.compose.service=portal-api)" ]]; then
    die '客户门户仍依赖 customer-api；请先在平台退役并移除 customer-portal，再移除客户与商机系统'
  fi
  services="$(app_services "$component" | tr '\n' ' ')"
  db="$(database_service "$component")"
  # contract-mysql backs Temporal, which is shared by Contract and Project.
  # Keep shared dependencies while the other consumer still exists.
  if [[ "$component" == contract ]] && [[ -n "$(docker ps -q --filter "label=com.docker.compose.project=$project" --filter label=com.docker.compose.service=project-api)" ]]; then
    printf '[保留] project-api 仍在运行，contract-mysql/Temporal 是共享依赖。\n'
  elif [[ "$component" == project ]] && [[ -n "$(docker ps -q --filter "label=com.docker.compose.project=$project" --filter label=com.docker.compose.service=contract-api)" ]]; then
    printf '[保留] contract-api 仍在运行，Temporal 是共享依赖。\n'
  fi
  printf '将停止并移除 %s 的应用/Worker 与专属数据库容器；named volumes、runtime 配置均保留。\n' "$component"
  if [[ "$component" == contract ]]; then
    # Keep the shared contract DB; only remove the Contract API.
    # shellcheck disable=SC2086
    remove_services_keep_volumes "$project" $services
  elif [[ "$component" == project && -n "$(docker ps -q --filter "label=com.docker.compose.project=$project" --filter label=com.docker.compose.service=contract-api)" ]]; then
    # Temporal is managed with the shared Contract DB; remove only Project services.
    # shellcheck disable=SC2086
    remove_services_keep_volumes "$project" $services "$db"
  else
    # shellcheck disable=SC2086
    remove_services_keep_volumes "$project" $services "$db"
  fi
  printf '已移除 %s 容器；数据库卷及 runtime 凭据仍保留。平台环境需已在控制面退役。\n' "$component"
  [[ ! -e "$runtime" ]] || printf '保留运行配置：%s\n' "$runtime"
}

remove_all_applications() {
  local token="$1" component project; local -a services=()
  confirm_exact REMOVE_ALL_AFTER_RETIREMENT "$token" '确认所有子系统 prod 环境均已退役，输入确认字符串'
  require_config; need docker; acquire_lock
  project="$(compose_project_name)"
  for component in customer-opportunity customer-portal contract project settlement data-analysis; do
    while IFS= read -r service; do services+=("$service"); done < <(app_services "$component")
    [[ "$component" == contract ]] || services+=("$(database_service "$component")")
  done
  printf '将停止并移除所有已登记子系统的应用/Worker 与专属数据库容器；保留公共 Temporal、contract-mysql、所有数据卷、配置和基础平台。\n'
  remove_services_keep_volumes "$project" "${services[@]}"
}

cleanup_compose_stack() {
  local project; project="$(compose_project_name)"
  local registry_id="" registry_image="" registry_mounts=""
  if docker container inspect uip-offline-registry >/dev/null 2>&1; then
    registry_id="$(docker container inspect -f '{{.Id}}' uip-offline-registry)"
    registry_image="$(docker inspect -f '{{.Config.Image}}' "$registry_id")"
    registry_mounts="$(docker inspect -f '{{range .Mounts}}{{.Name}}={{.Destination}} {{end}}' "$registry_id")"
    [[ "$registry_image" == registry:2.8.3 && "$registry_mounts" == *uip-offline-registry-data=/var/lib/registry* ]] ||
      die 'uip-offline-registry 标记与预期不符；拒绝继续清理'
  fi
  printf '清理范围：当前 Compose 项目容器与非 external 网络；保留数据库、File Gateway、Temporal、监控数据卷及全部配置/备份。\n'
  local -a ids=() networks=()
  mapfile -t ids < <(docker ps -aq --filter "label=com.docker.compose.project=$project")
  if ((${#ids[@]} > 0)); then docker rm --force "${ids[@]}" >/dev/null; fi
  mapfile -t networks < <(docker network ls -q --filter "label=com.docker.compose.project=$project")
  if ((${#networks[@]} > 0)); then docker network rm "${networks[@]}" >/dev/null; fi
  # Registry is a script-owned standalone container, not part of docker-compose.yml. Remove
  # it only if its image and named cache volume match the known installation markers.
  if [[ -n "$registry_id" ]]; then
    docker rm --force "$registry_id" >/dev/null
    printf '已移除离线镜像 Registry 容器；缓存卷 uip-offline-registry-data 保留。\n'
  fi
}

cleanup_environment() {
  local token="$1"
  confirm_exact CLEANUP_UIP_ENVIRONMENT "$token" '停止并移除整套应用容器（保留数据卷/配置），输入确认字符串'
  require_config; need docker; acquire_lock
  cleanup_compose_stack
  printf '环境容器已清理；数据卷、配置、镜像和备份均保留，可通过原配置重新部署。\n'
}

destroy_environment() {
  local remove_gateway_files=false token="" project stamp backup_started backup_parent backup_archive
  local system_batch gateway_root gateway_canonical gateway_backup_root gateway_batch logical service volume
  local registry_id="" registry_image="" registry_mounts
  while (($#)); do
    case "$1" in
      --remove-file-gateway-files) remove_gateway_files=true; shift ;;
      --confirm) token="${2:-}"; shift 2 ;;
      *) die "未知 destroy 参数：$1" ;;
    esac
  done
  if [[ "$remove_gateway_files" == true ]]; then
    confirm_exact DELETE_UIP_DATA_AND_FILES "$token" '将删除数据库卷、配置及外置文件网关对象数据；请确认备份已完成，输入确认字符串'
  else
    confirm_exact DELETE_UIP_DATA "$token" '将删除数据库卷及部署配置；File Gateway 外置文件仍保留，输入确认字符串'
  fi
  require_config; need docker; need gzip; need sha256sum; acquire_lock
  [[ "$deploy_dir" == /opt/* && "${deploy_dir##*/}" == unified-identity-platform ]] ||
    die "destroy 仅允许标准安装路径 /opt/unified-identity-platform，当前路径为 $deploy_dir"
  [[ ! -L "$deploy_dir" && -f "$deploy_dir/bin/deploy.sh" && -f "$deploy_dir/docker-compose.yml" ]] ||
    die '部署目录身份校验失败；拒绝 destroy'
  project="$(compose_project_name)"
  if docker container inspect uip-offline-registry >/dev/null 2>&1; then
    registry_id="$(docker container inspect -f '{{.Id}}' uip-offline-registry)"
    registry_image="$(docker inspect -f '{{.Config.Image}}' "$registry_id")"
    registry_mounts="$(docker inspect -f '{{range .Mounts}}{{.Name}}={{.Destination}} {{end}}' "$registry_id")"
    [[ "$registry_image" == registry:2.8.3 && "$registry_mounts" == *uip-offline-registry-data=/var/lib/registry* ]] ||
      die 'Registry 容器标记与预期不符；拒绝 destroy'
  fi

  backup_started="$(date -u +%Y%m%dT%H%M%SZ)"
  printf '第一步：为正在运行的 MySQL 与 File Gateway 生成一致性备份。\n'
  "$deploy_dir/bin/backup-all.sh" --deployment-lock-fd 8
  backup_parent="$(dirname -- "$deploy_dir")"
  stamp="$(date -u +%Y%m%dT%H%M%SZ)"
  backup_archive="$backup_parent/unified-identity-platform-backups-$stamp"
  [[ ! -e "$backup_archive" ]] || die "备份归档目标已存在：$backup_archive"
  system_batch="$(find "$deploy_dir/backups/system" -mindepth 1 -maxdepth 1 -type d -name '20*' -print 2>/dev/null | sort | tail -n 1)"
  [[ -n "$system_batch" && -f "$system_batch/MANIFEST" && -f "$system_batch/SHA256SUMS" ]] ||
    die '未找到刚生成的 MySQL 系统备份；没有删除数据'
  [[ "${system_batch##*/}" > "$backup_started" || "${system_batch##*/}" == "$backup_started" ]] ||
    die 'MySQL 系统备份不是本次 destroy 刚生成的；没有删除数据'
  (cd -- "$system_batch" && sha256sum --check SHA256SUMS >/dev/null) || die 'MySQL 系统备份校验失败；没有删除数据'
  grep -q '^FORMAT=1$' "$system_batch/MANIFEST" || die 'MySQL 备份格式无效；没有删除数据'

  # Every present Compose-owned database volume must have a matching SQL dump.
  # A stopped database is not silently treated as empty just because backup-all skipped it.
  local -a db_volumes=(
    platform-mysql-data:platform-mysql keycloak-mysql-data:keycloak-db file-gateway-mysql-data:file-gateway-mysql
    customer-mysql-data:customer-mysql portal-mysql-data:portal-mysql contract-mysql-data:contract-mysql
    project-mysql-data:project-mysql settlement-mysql-data:settlement-mysql data-analysis-mysql-data:data-analysis-mysql
  )
  for logical in "${db_volumes[@]}"; do
    volume="${logical%%:*}"; service="${logical#*:}"
    if docker volume inspect "${project}_${volume}" >/dev/null 2>&1; then
      [[ -s "$system_batch/${service}.sql.gz" ]] || die "数据库卷 ${project}_${volume} 存在但备份中没有 ${service}.sql.gz；没有删除数据"
      gzip -t "$system_batch/${service}.sql.gz" || die "$service 备份 gzip 校验失败；没有删除数据"
    fi
  done

  gateway_root="$(awk -F= '$1=="FILE_GATEWAY_HOST_ROOT" {sub(/^[^=]*=/, ""); print; exit}' "$runtime_file")"
  gateway_root="${gateway_root:-$deploy_dir/data/file-gateway}"
  gateway_backup_root="$(awk -F= '$1=="FILE_GATEWAY_BACKUP_ROOT" {sub(/^[^=]*=/, ""); print; exit}' "$runtime_file")"
  gateway_backup_root="${gateway_backup_root:-$deploy_dir/backups/file-gateway}"
  if [[ -d "$gateway_root" ]]; then
    gateway_batch="$(find "$gateway_backup_root" -mindepth 1 -maxdepth 1 -type d -name '20*' -print 2>/dev/null | sort | tail -n 1)"
    [[ -n "$gateway_batch" && -f "$gateway_batch/files.tar.gz" && -f "$gateway_batch/database.sql.gz" && -f "$gateway_batch/SHA256SUMS" ]] ||
      die 'File Gateway 对象目录存在，但未找到数据库+对象文件的一致性备份；没有删除数据'
    [[ "${gateway_batch##*/}" > "$backup_started" || "${gateway_batch##*/}" == "$backup_started" ]] ||
      die 'File Gateway 备份不是本次 destroy 刚生成的；没有删除数据'
    (cd -- "$gateway_batch" && sha256sum --check SHA256SUMS >/dev/null && gzip -t files.tar.gz && gzip -t database.sql.gz) ||
      die 'File Gateway 备份校验失败；没有删除数据'
  fi

  if [[ "$remove_gateway_files" == true && -d "$gateway_root" ]]; then
    need realpath
    gateway_canonical="$(realpath -e -- "$gateway_root")"
    [[ ! -L "$gateway_root" && "$gateway_root" == /opt/*/data/file-gateway && "$gateway_canonical" == "$gateway_root" ]] ||
      die "File Gateway 外置目录路径不符合保护规则：${gateway_root}；拒绝删除"
    [[ "$(dirname -- "$gateway_root")" != /opt && "$gateway_root" != /opt && "$gateway_root" != / ]] ||
      die 'File Gateway 外置目录保护校验失败；拒绝删除'
  fi

  printf '备份校验通过。接下来将删除 Compose 项目 %s 的容器和命名数据卷。\n' "$project"
  [[ ! -d "$deploy_dir/backups" ]] || mv -- "$deploy_dir/backups" "$backup_archive"
  if [[ -d "$backup_archive" ]]; then chmod 700 "$backup_archive"; fi
  cleanup_compose_stack
  local -a volumes=()
  mapfile -t volumes < <(docker volume ls -q --filter "label=com.docker.compose.project=$project")
  if ((${#volumes[@]} > 0)); then docker volume rm "${volumes[@]}"; fi
  if [[ -n "$registry_id" ]]; then docker rm --force "$registry_id" >/dev/null 2>&1 || true; fi
  if [[ -n "$registry_id" ]] && docker volume inspect uip-offline-registry-data >/dev/null 2>&1; then
    docker volume rm uip-offline-registry-data >/dev/null
  fi
  if [[ "$remove_gateway_files" == true && -d "$gateway_root" ]]; then
    rm -rf -- "$gateway_root"
  fi
  rm -rf -- "$deploy_dir"
  printf '部署目录与指定数据卷已删除；系统备份保留于：%s\n' "$backup_archive"
  [[ "$remove_gateway_files" != false ]] || printf 'File Gateway 外置对象目录保留于：%s\n' "$gateway_root"
}

purge_configuration() {
  local keep_data=false token="" arg stamp archive_path parent base
  while (($#)); do
    case "$1" in
      --keep-data) keep_data=true; shift ;;
      --confirm) token="${2:-}"; shift 2 ;;
      *) die "未知 purge 参数：$1" ;;
    esac
  done
  [[ "$keep_data" == true ]] || die 'purge 仅提供显式保留数据库/文件数据的模式；需带 --keep-data'
  confirm_exact PURGE_UIP_KEEP_DATA "$token" '删除部署脚本、配置和安装包（保留 Docker 数据卷及备份），输入确认字符串'
  require_config; need docker; acquire_lock
  [[ "$deploy_dir" == /opt/* && "${deploy_dir##*/}" == unified-identity-platform ]] ||
    die "purge 仅允许标准安装路径 /opt/unified-identity-platform，当前路径为 $deploy_dir"
  [[ ! -L "$deploy_dir" && -f "$deploy_dir/bin/deploy.sh" && -f "$deploy_dir/docker-compose.yml" ]] ||
    die '部署目录身份校验失败；拒绝删除非本部署目录'
  parent="$(dirname -- "$deploy_dir")"; base="${deploy_dir##*/}"
  stamp="$(date -u +%Y%m%dT%H%M%SZ)"
  archive_path="$parent/${base}-backups-${stamp}"
  [[ ! -e "$archive_path" ]] || die "备份归档目标已存在：$archive_path"
  cleanup_compose_stack
  if [[ -d "$deploy_dir/backups" ]]; then
    [[ ! -L "$deploy_dir/backups" ]] || die '备份目录是符号链接；拒绝 purge'
    mv -- "$deploy_dir/backups" "$archive_path"
    chmod 700 "$archive_path"
    printf '已将备份保留在：%s\n' "$archive_path"
  fi
  # This exact, canonical, guarded path is the only recursive deletion target.
  rm -rf -- "$deploy_dir"
  printf '部署脚本与配置已清除；数据库/文件数据卷和外置 File Gateway 数据仍保留。\n'
}

case "$command_name" in
  help|-h|--help) usage ;;
  repair) repair "${1:-}" ;;
  remove)
    component="${1:-}"; token=""
    [[ -n "$component" ]] || die '用法：remove <子系统> --confirm REMOVE_<子系统>_AFTER_RETIREMENT'
    shift || true
    [[ "${1:-}" == --confirm ]] || die 'remove 必须显式带 --confirm <专用确认字符串>'
    token="${2:-}"
    remove_subsystem "$component" "$token"
    ;;
  remove-all-applications)
    [[ "${1:-}" == --confirm ]] || die '必须显式带 --confirm REMOVE_ALL_AFTER_RETIREMENT'
    remove_all_applications "${2:-}"
    ;;
  cleanup)
    [[ "${1:-}" == --confirm ]] || die '必须显式带 --confirm CLEANUP_UIP_ENVIRONMENT'
    cleanup_environment "${2:-}"
    ;;
  purge) purge_configuration "$@" ;;
  destroy) destroy_environment "$@" ;;
  *) usage >&2; die "未知生命周期命令：$command_name" ;;
esac
