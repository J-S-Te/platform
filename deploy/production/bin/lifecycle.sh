#!/usr/bin/env bash
# 离线生产部署的安全运维生命周期操作。
set -Eeuo pipefail
umask 077

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
deploy_dir="$(cd -- "$script_dir/.." && pwd -P)"
runtime_file="$deploy_dir/.env"
release_file="$deploy_dir/.release.env"
compose_file="$deploy_dir/docker-compose.yml"
source "$script_dir/compose-scope.sh"
source "$script_dir/project-background.sh"
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
      备份目录移至部署目录旁。部署目录内的 File Gateway 默认对象目录会先移到
      同一归档旁（data/file-gateway → <归档>/file-gateway）；配置指向部署目录
      之外的对象目录保持原位。
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

env_get() {
  awk -F= -v key="$2" '$0 !~ /^[[:space:]]*#/ && $1 == key {sub(/^[^=]*=/, ""); print; exit}' "$1"
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
      --filter "label=com.docker.compose.service=$service")" || return
    [[ -n "$ids" ]] || continue
    while IFS= read -r id; do
      [[ -n "$id" ]] || continue
      docker rm --force "$id" >/dev/null || return
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
      # 空值是合法的"可选集成未启用"：CRM 的可选 S3、数据看板的可选 Dashboard ID、
      # 未启用的结算集成客户端键在受控采用完成后本来就是空的。只有显式占位符才表示
      # 尚未完成凭据下发；否则 repair 会把已正确配置的子系统当成未接入而拒绝。
      if (v != "") has_value=1
      if (v == "PENDING_ONBOARDING" || v ~ /^REPLACE_WITH_/) bad=1
    }
    END {exit (bad || !has_value) ? 0 : 1}
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
  local component="$1" selected runtime project service ids found=false
  local -a services=()
  scope_require "$component" || return
  selected="$(scope_runtime_services "$component")" || return
  [[ -n "$selected" ]] || die "$component 没有选中的受控运行组件"
  while IFS= read -r service; do services+=("$service"); done <<< "$selected"
  project="$(compose_project_name)"
  runtime="$(runtime_file_for "$component")" || die "不支持的子系统：$component"
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
  for service in "${services[@]}"; do
    ids="$(docker ps -aq --filter "label=com.docker.compose.project=$project" \
      --filter "label=com.docker.compose.service=$service")" || return
    if [[ -n "$ids" ]]; then
      found=true
      break
    fi
  done
  [[ "$found" == true ]] || die "$component 没有已部署的应用/Worker 容器；repair 不会替代首次部署或平台受控接入。"
  printf '修复 %s：只重建应用/Worker 容器（--no-deps），不重启数据库，不删除数据卷。\n' "$component"
  [[ "$component" != project ]] || project_background_prepare || return
  # 当前部署包含受 profile 限定的 worker；显式列出批准服务可激活这些 profile，而不启动依赖。
  compose up -d --force-recreate --wait --wait-timeout "${UIP_HEALTH_WAIT_SECONDS:-180}" --no-deps "${services[@]}" || return
  [[ "$component" != project ]] || project_background_activate
}

repair() {
  local target="${1:-}" subsystem enabled service
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
      # 修复核心之前就验证全部已接管子系统的完整运行策略，防止后续配置损坏导致只完成部分修复。
      enabled="$(scope_services)" || return
      for subsystem in customer-opportunity customer-portal contract project settlement data-analysis; do
        service="$(scope_service_for "$subsystem")" || return
        grep -Fxq "$service" <<< "$enabled" || continue
        if runtime_ready "$(runtime_file_for "$subsystem")"; then
          scope_runtime_services "$subsystem" >/dev/null || return
        fi
      done
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
  local component="$1" token="$2" owned runtime project service running
  local -a services=()
  runtime="$(runtime_file_for "$component")" || die "不支持的子系统：$component"
  confirm_exact "REMOVE_${component}_AFTER_RETIREMENT" "$token" \
    "确认平台控制面已退役 ${component}/prod，输入确认字符串"
  require_config; need docker; acquire_lock
  project="$(compose_project_name)"
  # 归属范围包含之前选定的条件 worker 和一次性任务。先获取完整策略，解析失败绝不能进入移除流程。
  owned="$(scope_owned_services "$component")" || return
  [[ -n "$owned" ]] || die "$component 没有登记的受控组件"
  while IFS= read -r service; do services+=("$service"); done <<< "$owned"
  if docker container inspect "uip-${component}-candidate" >/dev/null 2>&1; then
    die '存在未完成的受控接入候选；先在平台完成退役/重试，拒绝擅自移除候选容器'
  fi
  if [[ "$component" == customer-opportunity ]]; then
    running="$(docker ps -q --filter "label=com.docker.compose.project=$project" --filter label=com.docker.compose.service=portal-api)" || return
    [[ -z "$running" ]] || die '客户门户仍依赖 customer-api；请先在平台退役并移除 customer-portal，再移除客户与商机系统'
  fi
  # contract-mysql 支撑合同与项目共用的 Temporal；其他消费者仍在时必须保留共享依赖。
  if [[ "$component" == contract ]]; then
    running="$(docker ps -q --filter "label=com.docker.compose.project=$project" --filter label=com.docker.compose.service=project-api)" || return
    [[ -z "$running" ]] || printf '[保留] project-api 仍在运行，contract-mysql/Temporal 是共享依赖。\n'
  elif [[ "$component" == project ]]; then
    running="$(docker ps -q --filter "label=com.docker.compose.project=$project" --filter label=com.docker.compose.service=contract-api)" || return
    [[ -z "$running" ]] || printf '[保留] contract-api 仍在运行，Temporal 是共享依赖。\n'
  fi
  printf '将停止并移除 %s 的应用/Worker 与专属数据库容器；named volumes、runtime 配置均保留。\n' "$component"
  [[ "$component" != project ]] || project_background_teardown || return
  remove_services_keep_volumes "$project" "${services[@]}" || return
  [[ "${component:-project}" != project ]] || project_background_stopped || return
  printf '已移除 %s 容器；数据库卷及 runtime 凭据仍保留。平台环境需已在控制面退役。\n' "$component"
  [[ ! -e "$runtime" ]] || printf '保留运行配置：%s\n' "$runtime"
}

remove_all_applications() {
  local token="$1" component project owned service; local -a services=()
  confirm_exact REMOVE_ALL_AFTER_RETIREMENT "$token" '确认所有子系统 prod 环境均已退役，输入确认字符串'
  require_config; need docker; acquire_lock
  project="$(compose_project_name)"
  for component in customer-opportunity customer-portal contract project settlement data-analysis; do
    owned="$(scope_owned_services "$component")" || return
    [[ -n "$owned" ]] || die "$component 没有登记的受控组件"
    while IFS= read -r service; do services+=("$service"); done <<< "$owned"
    if docker container inspect "uip-${component}-candidate" >/dev/null 2>&1; then
      die "${component} 存在未完成的受控接入候选；先在平台完成退役/重试，拒绝移除"
    fi
  done
  printf '将停止并移除所有已登记子系统的应用/Worker 与专属数据库容器；保留公共 Temporal、contract-mysql、所有数据卷、配置和基础平台。\n'
  project_background_teardown || return
  remove_services_keep_volumes "$project" "${services[@]}" || return
  project_background_stopped || return
}

cleanup_compose_stack() {
  local project; project="$(compose_project_name)"
  project_background_teardown || return
  local registry_id="" registry_image="" registry_mounts=""
  if docker container inspect uip-offline-registry >/dev/null 2>&1; then
    registry_id="$(docker container inspect -f '{{.Id}}' uip-offline-registry)"
    registry_image="$(docker inspect -f '{{.Config.Image}}' "$registry_id")"
    registry_mounts="$(docker inspect -f '{{range .Mounts}}{{.Name}}={{.Destination}} {{end}}' "$registry_id")"
    [[ "$registry_image" == registry:2.8.3 && "$registry_mounts" == *uip-offline-registry-data=/var/lib/registry* ]] ||
      die 'uip-offline-registry 标记与预期不符；拒绝继续清理'
  fi
  printf '清理范围：当前 Compose 项目容器与非 external 网络；保留数据库、File Gateway、Temporal、监控数据卷及全部配置/备份。\n'
  local inventory
  local -a ids=() networks=()
  inventory="$(docker ps -aq --filter "label=com.docker.compose.project=$project")" || return
  [[ -z "$inventory" ]] || mapfile -t ids <<< "$inventory"
  if ((${#ids[@]} > 0)); then docker rm --force "${ids[@]}" >/dev/null || return; fi
  project_background_stopped || return
  mapfile -t networks < <(docker network ls -q --filter "label=com.docker.compose.project=$project")
  if ((${#networks[@]} > 0)); then docker network rm "${networks[@]}" >/dev/null; fi
  # Registry 是脚本管理的独立容器，不属于 docker-compose.yml。只有镜像和具名缓存卷均符合已知安装标记时才能移除。
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
  # backup-all.sh 当前写 FORMAT=2；FORMAT=1 是旧批次布局。destroy 只接受 1 会让每次
  # 在正常备份之后仍然安全失败（无法销毁），因此这里接受工具链实际产出过的两种格式，
  # 其余校验（MANIFEST/SHA256SUMS/逐库 dump）保持不变。
  grep -Eq '^FORMAT=[12]$' "$system_batch/MANIFEST" || die 'MySQL 备份格式无效；没有删除数据'

  # 每个现存 Compose 数据库卷都必须对应 SQL 转储；不能因为 backup-all 跳过已停止数据库，就静默视其为空。
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

# purge_gateway_root <deploy_dir> 解析本部署实际使用的 File Gateway 对象目录。
# 默认值与 offline-configure.sh 保持一致：未配置 FILE_GATEWAY_HOST_ROOT 时是
# <deploy_dir>/data/file-gateway（位于部署目录内部）。
purge_gateway_root() {
  local target_deploy_dir="$1" configured=""
  if [[ -f "$target_deploy_dir/.env" ]]; then
    configured="$(awk -F= '$1=="FILE_GATEWAY_HOST_ROOT" {sub(/^[^=]*=/, ""); print; exit}' "$target_deploy_dir/.env")"
  fi
  configured="${configured%\"}"
  configured="${configured#\"}"
  if [[ -z "$configured" ]]; then
    printf '%s\n' "$target_deploy_dir/data/file-gateway"
  elif [[ "$configured" == ./* ]]; then
    printf '%s\n' "$target_deploy_dir/${configured#./}"
  else
    printf '%s\n' "$configured"
  fi
}

# purge_retain_gateway_objects <deploy_dir> <archive_path>
# purge 会 rm -rf 整个部署目录，而默认对象目录就在部署目录里：不先移出的话，
# 数据库命名卷虽然保留，对象字节已经丢失，却仍然打印"文件数据保留"。
# 配置指向部署目录之外时保持原位（rm -rf 也不会碰到它）。
purge_retain_gateway_objects() {
  local target_deploy_dir="$1" archive="$2" gateway_root="" link_target=""
  gateway_root="$(purge_gateway_root "$target_deploy_dir")"
  [[ -n "$gateway_root" ]] || return 0
  if [[ -L "$gateway_root" ]]; then
    link_target="$(readlink -- "$gateway_root" 2>/dev/null || printf '%s' "$gateway_root")"
    printf 'File Gateway 目录是符号链接，真实对象目录不随部署目录删除：%s\n' "$link_target"
    return 0
  fi
  [[ -d "$gateway_root" ]] || return 0
  case "$gateway_root" in
    "$target_deploy_dir"/*) ;;
    *)
      printf 'File Gateway 对象目录位于部署目录之外，保持原位：%s\n' "$gateway_root"
      return 0
      ;;
  esac
  install -d -m 700 "$archive"
  mv -- "$gateway_root" "$archive/file-gateway"
  printf '已将 File Gateway 对象目录保留在：%s\n' "$archive/file-gateway"
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
  # 默认对象目录在部署目录内部，必须先移出再删除部署目录，否则对象字节随 rm -rf 丢失。
  purge_retain_gateway_objects "$deploy_dir" "$archive_path"
  # 只有这个确切、规范且受保护的路径允许作为递归删除目标。
  rm -rf -- "$deploy_dir"
  printf '部署脚本与配置已清除；数据库数据卷与 File Gateway 对象数据仍保留。\n'
}

approval_reload() {
  # 内部发布继续流程复用继承的已打开锁，避免再次打开同一文件而与发布器死锁。
  [[ $# == 2 && "$1" == --deployment-lock-fd && "$2" =~ ^[0-9]+$ ]] || die 'approval-reload 仅接受发布器继承的部署锁'
  [[ "$(id -u)" == 0 ]] || die '批准控制面重载必须由 root 维护进程执行'
  require_config; need python3; need docker
  [[ -f "$script_dir/publish-runtime-approvals.py" && ! -L "$script_dir/publish-runtime-approvals.py" ]] || die '缺少批准发布事务校验器'
  python3 "$script_dir/publish-runtime-approvals.py" --deploy-root "$deploy_dir" --reload-check-fd "$2"
  profiles_dir="$deploy_dir/subsystems.d"
  COMPOSE_PROJECT_NAME="$(compose_project_name)"
  source "$script_dir/provisioner-config-refresh.sh"
  refresh_subsystem_control_plane_config
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
  approval-reload) approval_reload "$@" ;;
  *) usage >&2; die "未知生命周期命令：$command_name" ;;
esac
