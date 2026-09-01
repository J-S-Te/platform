#!/usr/bin/env bash
set -Eeuo pipefail

# 每次发布只接受带 sha256 digest 的阿里云镜像。版本指针写入 .release.env，运行密钥仍留在
# 独立 .env 中，避免发布产物或历史镜像记录携带数据库、OAuth 等敏感配置。

usage() {
  echo "usage: $0 {frontend|platform|contract|project} <acr-host>/<namespace>/<image>@sha256:<64-hex-digest>" >&2
  echo "       $0 data-analysis <dashboard-api@sha256:...> <aggregation-worker@sha256:...> <alert-worker@sha256:...> <production-migrate@sha256:...>" >&2
  exit 2
}

service="$1"
if [[ "$service" == "data-analysis" ]]; then
  [[ $# -eq 5 ]] || usage
  data_analysis_dashboard_image="$2"
  data_analysis_aggregation_image="$3"
  data_analysis_alert_image="$4"
  data_analysis_migrate_image="$5"
  image_ref="$data_analysis_dashboard_image"
else
  [[ $# -eq 2 ]] || usage
  image_ref="$2"
fi
case "$service" in
  frontend) image_key=FRONTEND_IMAGE ;;
  platform) image_key=PLATFORM_IMAGE ;;
  contract) image_key=CONTRACT_IMAGE ;;
  project) image_key=PROJECT_IMAGE ;;
  data-analysis)
    image_key=DATA_ANALYSIS_DASHBOARD_API_IMAGE
    data_analysis_image_keys=(DATA_ANALYSIS_DASHBOARD_API_IMAGE DATA_ANALYSIS_AGGREGATION_WORKER_IMAGE DATA_ANALYSIS_ALERT_WORKER_IMAGE DATA_ANALYSIS_MIGRATE_IMAGE)
    data_analysis_image_refs=("$data_analysis_dashboard_image" "$data_analysis_aggregation_image" "$data_analysis_alert_image" "$data_analysis_migrate_image")
    ;;
  *) usage ;;
esac

acr_enterprise_or_new_personal='^[a-z0-9.-]+\.cr\.aliyuncs\.com/[a-z0-9._/-]+@sha256:[a-f0-9]{64}$'
acr_legacy_personal='^registry(-vpc)?\.[a-z0-9-]+\.aliyuncs\.com/[a-z0-9._/-]+@sha256:[a-f0-9]{64}$'
if [[ "$service" == "data-analysis" ]]; then
  for data_analysis_image in "${data_analysis_image_refs[@]}"; do
    if [[ ! "$data_analysis_image" =~ $acr_enterprise_or_new_personal && ! "$data_analysis_image" =~ $acr_legacy_personal ]]; then
      echo "拒绝可变或格式错误的数据看板镜像引用：$data_analysis_image" >&2
      exit 2
    fi
  done
else
  if [[ ! "$image_ref" =~ $acr_enterprise_or_new_personal && ! "$image_ref" =~ $acr_legacy_personal ]]; then
    echo "拒绝可变或格式错误的镜像引用：$image_ref" >&2
    exit 2
  fi
fi

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
deploy_dir="$(cd -- "$script_dir/.." && pwd)"
release_file="$deploy_dir/.release.env"
runtime_file="$deploy_dir/.env"
contract_runtime_file="$deploy_dir/runtime/contract.env"
contract_runtime_template="$deploy_dir/subsystem-templates/contract.env.example"
project_runtime_file="$deploy_dir/runtime/project.env"
project_runtime_template="$deploy_dir/subsystem-templates/project.env.example"
data_analysis_runtime_file="$deploy_dir/runtime/data-analysis.env"
data_analysis_runtime_template="$deploy_dir/subsystem-templates/data-analysis.env.example"
compose_file="$deploy_dir/compose.yaml"
frontend_compose_file="$deploy_dir/compose.frontend.yaml"
profiles_dir="$deploy_dir/subsystems.d"
export CONTRACT_RUNTIME_ENV_FILE="$contract_runtime_file"
export PROJECT_RUNTIME_ENV_FILE="$project_runtime_file"
export DATA_ANALYSIS_RUNTIME_ENV_FILE="$data_analysis_runtime_file"

for command_name in docker curl gzip flock awk mktemp install stat df ln; do
  command -v "$command_name" >/dev/null || {
    echo "缺少命令：$command_name" >&2
    exit 1
  }
done
relax_runtime_perm_check="${DEPLOY_RELAX_RUNTIME_PERM_CHECK:-false}"
# CI/CD 发布项目服务时必须确认运行凭据已经生成并完成真实启动。默认关闭，
# 以保留首次接入时由平台先暂存不可变镜像的兼容行为；项目 workflow 会显式开启。
fail_if_runtime_not_ready="${DEPLOY_FAIL_IF_RUNTIME_NOT_READY:-false}"
docker compose version >/dev/null
[[ -f "$runtime_file" ]] || { echo "缺少 $runtime_file" >&2; exit 1; }
[[ -f "$release_file" ]] || { echo "缺少 $release_file" >&2; exit 1; }
[[ -f "$compose_file" ]] || { echo "缺少 $compose_file" >&2; exit 1; }
[[ -d "$profiles_dir" ]] || { echo "缺少生产子系统审核清单目录：$profiles_dir" >&2; exit 1; }
compgen -G "$profiles_dir/*.yaml" >/dev/null || { echo "生产子系统审核清单目录中没有 YAML 文件" >&2; exit 1; }

release_permission_error() {
  local current_user current_group owner mode
  current_user="$(id -un 2>/dev/null || printf unknown)"
  current_group="$(id -gn 2>/dev/null || printf unknown)"
  owner="$(stat -c '%U:%G' "$release_file" 2>/dev/null || printf unknown)"
  mode="$(stat -c '%a' "$release_file" 2>/dev/null || printf unknown)"
  echo "发布配置权限不足：$release_file（当前用户=${current_user}:${current_group}，文件属主=${owner}，权限=${mode}）" >&2
  echo "请使用 root 执行：chown ${current_user}:${current_group} $release_file && chmod 600 $release_file" >&2
  exit 1
}

runtime_permission_error() {
  local target="$1" current_user current_group owner mode
  current_user="$(id -un 2>/dev/null || printf unknown)"
  current_group="$(id -gn 2>/dev/null || printf unknown)"
  owner="$(stat -c '%U:%G' "$target" 2>/dev/null || printf unknown)"
  mode="$(stat -c '%a' "$target" 2>/dev/null || printf unknown)"
  echo "运行配置权限不足：$target（当前用户=${current_user}:${current_group}，文件属主=${owner}，权限=${mode}）" >&2
  echo "请使用 root 执行：chown ${current_user}:${current_group} $target && chmod 600 $target" >&2
  exit 1
}

# .release.env 存放不可变镜像版本指针，发布会在同目录创建临时文件并原子替换；
# 因此 CI 部署用户必须可读该文件且可写部署根目录。避免手工 root 发布后只留下模糊的 cp 权限错误。
[[ -r "$release_file" && -w "$deploy_dir" ]] || release_permission_error

ensure_runtime_file_mode_0600() {
  local target="$1" temporary
  [[ -r "$target" && -w "$deploy_dir/runtime" ]] || runtime_permission_error "$target"
  if chmod 600 "$target" 2>/dev/null; then
    return 0
  fi

  if [[ "$relax_runtime_perm_check" == "true" ]]; then
    if [[ -r "$target" ]]; then
      echo "跳过运行配置权限收紧（仅发布前端时允许）：$target" >&2
      return 0
    fi

    echo "无法读取运行配置文件：$target；请检查权限后重试" >&2
    return 1
  fi

  temporary="$(mktemp "$deploy_dir/runtime/.runtime-permissions.XXXXXX")" || {
    echo "无法创建运行配置权限修复临时文件：$target；请由文件属主或 root 执行部署" >&2
    exit 1
  }
  if ! install -m 600 "$target" "$temporary" 2>/dev/null || ! mv -f "$temporary" "$target"; then
    rm -f "$temporary"
    echo "无法将运行配置权限收紧为 0600：$target；请检查文件属主、runtime 目录写权限，或使用 root 执行部署" >&2
    exit 1
  fi
}

prepare_runtime_file() {
  local target="$1" template="$2" label="$3"
  install -d -m 700 "$deploy_dir/runtime"
  if [[ ! -f "$target" ]]; then
    [[ -f "$template" ]] || {
      echo "缺少 $target，且没有可用于初始化的 $template" >&2
      exit 1
    }
    install -m 600 "$template" "$target"
    echo "已初始化 $target；${label}接入前仍需由平台写入运行凭据"
  fi
  [[ ! -L "$target" ]] || {
    echo "拒绝符号链接运行配置：$target" >&2
    exit 1
  }
  ensure_runtime_file_mode_0600 "$target"
  if [[ "$relax_runtime_perm_check" != "true" ]]; then
    [[ "$(stat -c '%a' "$target")" == "600" ]] || {
      echo "运行配置权限无法收紧为 0600：$target" >&2
      exit 1
    }
  fi
}

# 只准备本次真正发布的子系统配置。frontend/platform 发布不得创建、改属主或
# 改权限 contract/project runtime，避免无关发布被历史 root-owned 密钥文件阻断。
case "$service" in
  contract) prepare_runtime_file "$contract_runtime_file" "$contract_runtime_template" "合同服务" ;;
  project) prepare_runtime_file "$project_runtime_file" "$project_runtime_template" "项目管理服务" ;;
  data-analysis) prepare_runtime_file "$data_analysis_runtime_file" "$data_analysis_runtime_template" "数据看板服务" ;;
esac

install -d -m 700 "$deploy_dir/runtime"
mkdir -p "$deploy_dir/backups/releases"
[[ -w "$deploy_dir/backups/releases" ]] || {
  echo "发布备份目录不可写：$deploy_dir/backups/releases；请将其属主调整为当前 CI 部署用户" >&2
  exit 1
}
exec 9>"$deploy_dir/runtime/.deploy.lock"
# 锁覆盖“备份—迁移—切镜像—健康检查—回退”完整窗口，防止并发发布互相覆盖 .release.env。
flock -w 900 9 || {
  echo "等待其他发布任务超时" >&2
  exit 1
}

compose() {
  docker compose \
    --project-directory "$deploy_dir" \
    --file "$compose_file" \
    --env-file "$runtime_file" \
    --env-file "$release_file" \
    "$@"
}

frontend_compose() {
  [[ -f "$frontend_compose_file" ]] || {
    echo "缺少前端独立发布清单：$frontend_compose_file" >&2
    return 1
  }
  docker compose \
    --project-directory "$deploy_dir" \
    --file "$frontend_compose_file" \
    --env-file "$runtime_file" \
    --env-file "$release_file" \
    "$@"
}

env_value() {
  local key="$1"
  awk -F= -v key="$key" '
    $0 !~ /^[[:space:]]*#/ && $1 == key {
      sub(/^[^=]*=/, "")
      print
      exit
    }
  ' "$runtime_file"
}

contract_env_value() {
  local key="$1"
  awk -F= -v key="$key" '
    $0 !~ /^[[:space:]]*#/ && $1 == key {
      sub(/^[^=]*=/, "")
      print
      exit
    }
  ' "$contract_runtime_file"
}

project_env_value() {
  local key="$1"
  awk -F= -v key="$key" '
    $0 !~ /^[[:space:]]*#/ && $1 == key {
      sub(/^[^=]*=/, "")
      print
      exit
    }
  ' "$project_runtime_file"
}

data_analysis_env_value() {
  local key="$1"
  awk -F= -v key="$key" '
    $0 !~ /^[[:space:]]*#/ && $1 == key {
      sub(/^[^=]*=/, "")
      print
      exit
    }
  ' "$data_analysis_runtime_file"
}

require_project_runtime_value() {
  local key="$1"
  local value
  value="$(project_env_value "$key")"
  if [[ -z "$value" || "$value" == REPLACE_WITH_* || "$value" == PENDING_* ]]; then
    echo "项目管理运行配置缺失或仍为占位值：$key" >&2
    return 1
  fi
}

project_runtime_ready() {
  local key value
  for key in     OIDC_CLIENT_ID     OIDC_CLIENT_SECRET     OIDC_TENANT_ID     OIDC_SESSION_ENCRYPTION_KEY_BASE64     PLATFORM_AUTHORIZATION_CATALOG_APPLICATION_ID     PLATFORM_AUTHORIZATION_CATALOG_CLIENT_ID     PLATFORM_AUTHORIZATION_CATALOG_CLIENT_SECRET; do
    value="$(project_env_value "$key")"
    if [[ -z "$value" || "$value" == REPLACE_WITH_* || "$value" == PENDING_* ]]; then
      return 1
    fi
  done
  [[ "$(project_env_value PLATFORM_AUTHORIZATION_CATALOG_SYNC_ENABLED)" == "true" ]]
}

data_analysis_runtime_ready() {
  local key value
  for key in \
    OIDC_CLIENT_ID \
    OIDC_CLIENT_SECRET \
    OIDC_TENANT_ID \
    OIDC_CODEC_KEY \
    PLATFORM_AUTHORIZATION_CATALOG_APPLICATION_ID \
    PLATFORM_AUTHORIZATION_CATALOG_CLIENT_ID \
    PLATFORM_AUTHORIZATION_CATALOG_CLIENT_SECRET \
    PLATFORM_AUDIT_CLIENT_ID \
    PLATFORM_AUDIT_CLIENT_SECRET \
    METABASE_EMBEDDING_SECRET; do
    value="$(data_analysis_env_value "$key")"
    if [[ -z "$value" || "$value" == REPLACE_WITH_* || "$value" == PENDING_* ]]; then
      return 1
    fi
  done
  [[ "$(data_analysis_env_value PLATFORM_AUTHORIZATION_CATALOG_SYNC_ENABLED)" == "true" ]] || return 1
  require_runtime_value DASHBOARD_MYSQL_PASSWORD || return 1
  require_runtime_value DASHBOARD_MYSQL_ROOT_PASSWORD
}

require_runtime_value() {
  local key="$1"
  local value
  value="$(env_value "$key")"
  if [[ -z "$value" || "$value" == REPLACE_WITH_* || "$value" == PENDING_* ]]; then
    echo "运行配置缺失或仍为占位值：$key" >&2
    return 1
  fi
}

require_contract_runtime_value() {
  local key="$1"
  local value
  value="$(contract_env_value "$key")"
  if [[ -z "$value" || "$value" == REPLACE_WITH_* || "$value" == PENDING_* ]]; then
    echo "合同运行配置缺失或仍为占位值：$key" >&2
    return 1
  fi
}

contract_runtime_ready() {
  local key value
  for key in \
    OIDC_CLIENT_ID \
    OIDC_CLIENT_SECRET \
    OIDC_TENANT_ID \
    OIDC_SESSION_ENCRYPTION_KEY_BASE64 \
    PLATFORM_AUTHORIZATION_CATALOG_APPLICATION_ID \
    PLATFORM_AUTHORIZATION_CATALOG_CLIENT_ID \
    PLATFORM_AUTHORIZATION_CATALOG_CLIENT_SECRET \
    PLATFORM_AUDIT_CLIENT_ID \
    PLATFORM_AUDIT_CLIENT_SECRET; do
    value="$(contract_env_value "$key")"
    if [[ -z "$value" || "$value" == REPLACE_WITH_* || "$value" == PENDING_* ]]; then
      return 1
    fi
  done
  [[ "$(contract_env_value PLATFORM_AUTHORIZATION_CATALOG_SYNC_ENABLED)" == "true" ]]
}

port_value() {
  local key="$1"
  local fallback="$2"
  local value
  value="$(env_value "$key")"
  if [[ "$value" =~ ^[0-9]+$ ]] && ((value >= 1 && value <= 65535)); then
    echo "$value"
  else
    echo "$fallback"
  fi
}

wait_for_health() {
  local url="$1"
  local attempts=60
  echo "等待健康检查：$url"
  for ((i = 1; i <= attempts; i++)); do
    if curl --fail --silent --show-error --max-time 3 "$url" >/dev/null 2>&1; then
      echo "健康检查通过"
      return 0
    fi
    sleep 2
  done
  echo "健康检查超时：$url" >&2
  return 1
}

verify_service_image() {
  local compose_kind="$1" service_name="$2" expected_image="$3" container_id actual_image
  if [[ "$compose_kind" == "frontend" ]]; then
    container_id="$(frontend_compose ps -q "$service_name" 2>/dev/null || true)"
  else
    container_id="$(compose ps -q "$service_name" 2>/dev/null || true)"
  fi
  [[ -n "$container_id" ]] || {
    echo "未找到已启动的 $service_name 容器，无法确认镜像是否生效" >&2
    return 1
  }
  actual_image="$(docker inspect "$container_id" --format '{{.Config.Image}}' 2>/dev/null || true)"
  [[ "$actual_image" == "$expected_image" ]] || {
    echo "$service_name 镜像未生效：期望=$expected_image，实际=$actual_image" >&2
    return 1
  }
  echo "$service_name 已运行目标不可变镜像：$expected_image"
}

dump_subsystem_provisioner_debug() {
  local container_id
  container_id="$(compose ps -q subsystem-provisioner 2>/dev/null || true)"
  if [[ -n "$container_id" ]]; then
    echo "---- subsystem-provisioner 容器状态 ----"
    docker inspect "$container_id" --format '{{json .State}}' || true
    echo "---- subsystem-provisioner 最近日志 ----"
    compose logs --no-color --tail 200 subsystem-provisioner || true
    echo "---- subsystem-provisioner compose ps ----"
    compose ps subsystem-provisioner || true
  else
    echo "未获取到 subsystem-provisioner 容器 ID" >&2
  fi
}

backup_database() {
  local mysql_service="$1"
  local database="$2"
  local output="$deploy_dir/backups/${service}-${release_id}.sql.gz"
  local temporary
  temporary="$(mktemp "$deploy_dir/backups/.${service}-${release_id}.XXXXXX.sql.gz")"
  chmod 600 "$temporary"
  echo "备份数据库到 $output"
  # single-transaction 为 InnoDB 提供一致性快照，备份管道任一环节失败都会因 pipefail 中止发布。
  if ! compose exec -T "$mysql_service" sh -c \
    'exec mysqldump --single-transaction --routines --triggers -uroot -p"$MYSQL_ROOT_PASSWORD" "$1"' \
    _ "$database" \
    | gzip -9 >"$temporary"; then
    rm -f "$temporary"
    echo "数据库备份失败，已清理未完成备份文件" >&2
    return 1
  fi
  mv -f "$temporary" "$output"
}

require_backup_space() {
  local available_kib minimum_kib
  minimum_kib="${BASIC_PLATFORM_MIN_FREE_KIB:-262144}"
  [[ "$minimum_kib" =~ ^[0-9]+$ ]] || {
    echo "BASIC_PLATFORM_MIN_FREE_KIB 必须是非负整数 KiB" >&2
    return 1
  }
  available_kib="$(df -Pk "$deploy_dir" | awk 'NR == 2 { print $4 }')"
  [[ "$available_kib" =~ ^[0-9]+$ ]] || {
    echo "无法读取 $deploy_dir 的可用磁盘空间" >&2
    return 1
  }
  if ((available_kib < minimum_kib)); then
    echo "发布前磁盘空间不足：${available_kib} KiB 可用，至少需要 ${minimum_kib} KiB；请先清理 backups/releases、Docker 无用层或扩容磁盘" >&2
    return 1
  fi
}

deploy_platform() {
  compose up -d --wait --wait-timeout 180 platform-mysql || return
  backup_database platform-mysql basic_platform || return
  compose --profile release run --rm platform-migrate ./migrate || return
  # 平台 API 只通过共享 Unix Socket 调用生产接入 Agent。先让同一平台镜像中的
  # Agent 健康，再切 API，避免新旧协议短暂不一致或页面误报 Agent 未启用。
  # Agent 需要强制重建以重载 subsystems.d 清单（无 HTTP 流量，秒级恢复）；
  # API 与 Worker 必须使用同一不可变镜像；Worker 独立运行，不能依赖 API
  # 入口脚本的双进程模式，否则 Keycloak 用户投影可能没有消费者。
  if ! compose up -d --force-recreate --wait --wait-timeout 60 --no-deps subsystem-provisioner; then
    echo "subsystem-provisioner 健康启动失败" >&2
    dump_subsystem_provisioner_debug
    return 1
  fi
  compose up -d --force-recreate --no-deps platform-api platform-worker || return
  wait_for_health "http://127.0.0.1:$(port_value PLATFORM_API_PORT 18080)/readyz" || {
    # 兜底：新镜像首启异常时强制重建一次并再次等待，避免平台 API 停留在宕机状态。
    echo "platform-api 未通过健康检查，强制重建一次后重试" >&2
    compose up -d --force-recreate --no-deps platform-api platform-worker || return
    wait_for_health "http://127.0.0.1:$(port_value PLATFORM_API_PORT 18080)/readyz" || return
  }
  verify_service_image compose platform-api "$image_ref" || return 1
  verify_service_image compose platform-worker "$image_ref" || return 1
}

deploy_contract() {
  # 子系统只有在浏览器 OIDC 与机器目录发布凭据全部就绪后才能启动，避免容器进入 401 重启循环。
  require_contract_runtime_value OIDC_CLIENT_ID || return
  require_contract_runtime_value OIDC_CLIENT_SECRET || return
  require_contract_runtime_value OIDC_TENANT_ID || return
  require_contract_runtime_value PLATFORM_AUTHORIZATION_CATALOG_APPLICATION_ID || return
  require_contract_runtime_value PLATFORM_AUTHORIZATION_CATALOG_CLIENT_ID || return
  require_contract_runtime_value PLATFORM_AUTHORIZATION_CATALOG_CLIENT_SECRET || return
  if [[ "$(contract_env_value PLATFORM_AUTHORIZATION_CATALOG_SYNC_ENABLED)" != "true" ]]; then
    echo "PLATFORM_AUTHORIZATION_CATALOG_SYNC_ENABLED 必须为 true" >&2
    return 1
  fi
  compose up -d --wait --wait-timeout 240 contract-mysql temporal || return
  backup_database contract-mysql contract_management || return
  # 迁移命令由 compose.yaml 固定；非零退出会在替换 API 镜像前终止发布。
  compose --profile release run --rm contract-migrate || return
  compose up -d --force-recreate --no-deps --wait --wait-timeout 120 contract-api || return
  if ! wait_for_health "http://127.0.0.1:$(port_value CONTRACT_API_PORT 18081)/healthz"; then
    echo "---- contract-api 最近日志 ----" >&2
    compose logs --no-color --tail 120 contract-api >&2 || true
    return 1
  fi
  verify_service_image compose contract-api "$image_ref" || return 1
}

deploy_project() {
  # 子系统只有在浏览器 OIDC 与机器目录发布凭据全部就绪后才能启动，避免容器进入 401 重启循环。
  require_project_runtime_value OIDC_CLIENT_ID || return
  require_project_runtime_value OIDC_CLIENT_SECRET || return
  require_project_runtime_value OIDC_TENANT_ID || return
  require_project_runtime_value PLATFORM_AUTHORIZATION_CATALOG_APPLICATION_ID || return
  require_project_runtime_value PLATFORM_AUTHORIZATION_CATALOG_CLIENT_ID || return
  require_project_runtime_value PLATFORM_AUTHORIZATION_CATALOG_CLIENT_SECRET || return
  if [[ "$(project_env_value PLATFORM_AUTHORIZATION_CATALOG_SYNC_ENABLED)" != "true" ]]; then
    echo "PLATFORM_AUTHORIZATION_CATALOG_SYNC_ENABLED 必须为 true" >&2
    return 1
  fi
  compose up -d --wait --wait-timeout 240 project-mysql temporal || return
  backup_database project-mysql project_management || return
  # 迁移命令由 compose.yaml 固定；非零退出会在替换 API 镜像前终止发布。
  compose --profile project-release run --rm project-migrate || return
  compose up -d --force-recreate --no-deps --wait --wait-timeout 120 project-api || return
  if ! wait_for_health "http://127.0.0.1:$(port_value PROJECT_API_PORT 18085)/healthz"; then
    echo "---- project-api 最近日志 ----" >&2
    compose logs --no-color --tail 120 project-api >&2 || true
    return 1
  fi
  verify_service_image compose project-api "$image_ref" || return 1
}

deploy_data_analysis() {
  # data_analysis 由 API、两个常驻 Worker 和一次性迁移镜像组成，四个镜像必须
  # 独立校验，不能用单一 tag 或只校验 dashboard-api 代替。
  compose up -d --wait --wait-timeout 240 data-analysis-mysql || return
  backup_database data-analysis-mysql dashboard_aggregation || return
  compose --profile data-analysis-release run --rm data-analysis-migrate || return
  compose up -d --force-recreate --no-deps --wait --wait-timeout 120 \
    data-analysis-api data-analysis-aggregation-worker data-analysis-alert-worker || return
  if ! wait_for_health "http://127.0.0.1:$(port_value DATA_ANALYSIS_API_PORT 18086)/data_analysis/readyz"; then
    echo "---- data-analysis-api 最近日志 ----" >&2
    compose logs --no-color --tail 120 data-analysis-api >&2 || true
    return 1
  fi
  verify_service_image compose data-analysis-api "${data_analysis_image_refs[0]}" || return 1
  verify_service_image compose data-analysis-aggregation-worker "${data_analysis_image_refs[1]}" || return 1
  verify_service_image compose data-analysis-alert-worker "${data_analysis_image_refs[2]}" || return 1
  docker image inspect "${data_analysis_image_refs[3]}" >/dev/null || {
    echo "迁移镜像未成功拉取：${data_analysis_image_refs[3]}" >&2
    return 1
  }
}

deploy_frontend() {
  frontend_compose up -d --force-recreate --no-deps --wait --wait-timeout 120 frontend || return
  wait_for_health "http://127.0.0.1:$(port_value FRONTEND_PORT 18082)/"
  verify_service_image frontend frontend "$image_ref"
}

rollback_runtime() {
  case "$service" in
    frontend) frontend_compose up -d --force-recreate --no-deps frontend ;;
    platform)
      if ! compose up -d --force-recreate --wait --wait-timeout 60 --no-deps subsystem-provisioner; then
        echo "回滚时 subsystem-provisioner 重建失败，继续尝试重建 platform-api" >&2
        dump_subsystem_provisioner_debug
      fi
      compose up -d --force-recreate --no-deps platform-api platform-worker
      ;;
    contract) compose up -d --force-recreate --no-deps contract-api ;;
    project) compose up -d --force-recreate --no-deps project-api ;;
    data-analysis)
      compose up -d --force-recreate --no-deps \
        data-analysis-api data-analysis-aggregation-worker data-analysis-alert-worker
      ;;
  esac
}

require_backup_space || exit 1
release_id="$(date -u +%Y%m%dT%H%M%SZ)"
previous_release="$(mktemp "$deploy_dir/.release.env.previous.XXXXXX")"
next_release="$(mktemp "$deploy_dir/.release.env.next.XXXXXX")"
chmod 600 "$previous_release" "$next_release"
# 同一文件系统内使用硬链接保存旧版本指针，不额外消耗数据块；磁盘接近满时仍可原子回退。
rm -f "$previous_release"
ln "$release_file" "$previous_release"
# 每次发布保留旧版本指针快照，但不复制包含运行密钥的 .env。
ln "$release_file" "$deploy_dir/backups/releases/${release_id}.env"

if [[ "$service" == "data-analysis" ]]; then
  awk -F= \
    -v k1="${data_analysis_image_keys[0]}" -v v1="${data_analysis_image_refs[0]}" \
    -v k2="${data_analysis_image_keys[1]}" -v v2="${data_analysis_image_refs[1]}" \
    -v k3="${data_analysis_image_keys[2]}" -v v3="${data_analysis_image_refs[2]}" \
    -v k4="${data_analysis_image_keys[3]}" -v v4="${data_analysis_image_refs[3]}" '
      BEGIN { found1 = found2 = found3 = found4 = 0 }
      $1 == k1 { print k1 "=" v1; found1 = 1; next }
      $1 == k2 { print k2 "=" v2; found2 = 1; next }
      $1 == k3 { print k3 "=" v3; found3 = 1; next }
      $1 == k4 { print k4 "=" v4; found4 = 1; next }
      { print }
      END {
        if (!found1) print k1 "=" v1
        if (!found2) print k2 "=" v2
        if (!found3) print k3 "=" v3
        if (!found4) print k4 "=" v4
      }
    ' "$release_file" >"$next_release"
else
  awk -F= -v key="$image_key" -v value="$image_ref" '
    BEGIN { found = 0 }
    $1 == key {
      print key "=" value
      found = 1
      next
    }
    { print }
    END {
      if (!found) print key "=" value
    }
  ' "$release_file" >"$next_release"
fi
mv "$next_release" "$release_file"
chmod 600 "$release_file"

if [[ "$service" == "data-analysis" ]]; then
  for data_analysis_image in "${data_analysis_image_refs[@]}"; do
    echo "拉取不可变数据看板镜像：$data_analysis_image"
    if ! docker pull "$data_analysis_image"; then
      mv -f "$previous_release" "$release_file"
      rm -f "$previous_release"
      echo "镜像拉取失败，发布配置已恢复" >&2
      exit 1
    fi
  done
else
  echo "拉取不可变镜像：$image_ref"
  if ! docker pull "$image_ref"; then
    mv -f "$previous_release" "$release_file"
    rm -f "$previous_release"
    echo "镜像拉取失败，发布配置已恢复" >&2
    exit 1
  fi
fi
if [[ "$service" == "frontend" ]]; then
  compose_config_ok=false
  frontend_compose config --quiet && compose_config_ok=true
else
  compose_config_ok=false
  compose config --quiet && compose_config_ok=true
fi
if [[ "$compose_config_ok" != "true" ]]; then
  mv -f "$previous_release" "$release_file"
  rm -f "$previous_release"
  echo "Compose 校验失败，发布配置已恢复" >&2
  exit 1
fi

# 首次上线时子系统镜像会先于浏览器接入发布。此时 OIDC 与机器凭据尚未生成，不能启动
# 子系统 API，但必须保留不可变 digest，供生产 Agent 在页面接入时迁移并启动。
if [[ "$service" == "contract" ]] && ! contract_runtime_ready; then
  rm -f "$previous_release"
  echo "合同镜像已安全暂存：$image_ref"
  echo "运行凭据尚未生成，请登录基础平台的“应用接入”页面完成 contract_management/prod 接入。"
  exit 0
fi
if [[ "$service" == "project" ]] && ! project_runtime_ready; then
  if [[ "$fail_if_runtime_not_ready" == "true" ]]; then
    echo "项目管理运行配置未完成，拒绝将仅暂存报告为发布成功" >&2
    echo "请先登录基础平台的“应用接入”页面完成 project_management/prod 接入，再重新运行 CI/CD" >&2
    exit 1
  fi
  rm -f "$previous_release"
  echo "项目管理镜像已安全暂存：$image_ref"
  echo "运行凭据尚未生成，请登录基础平台的“应用接入”页面完成 project_management/prod 接入。"
  exit 0
fi
if [[ "$service" == "data-analysis" ]] && ! data_analysis_runtime_ready; then
  rm -f "$previous_release"
  echo "数据看板镜像已安全暂存：${data_analysis_image_refs[*]}"
  echo "运行凭据尚未生成，请完成 data_analysis/prod 接入并补齐 dashboard 数据库凭据。"
  exit 0
fi

echo "开始发布 $service"
# 服务标识允许使用 data-analysis 这类连字符名称，但 Bash 函数名只能使用
# 标识符字符；统一转换后再动态调用，避免执行 deploy_data-analysis 这样的外部命令。
deploy_function="deploy_${service//-/_}"
if "$deploy_function"; then
  rm -f "$previous_release"
  echo "$service 发布成功：$image_ref"
  exit 0
fi

echo "发布失败，恢复上一镜像；已执行的数据库迁移不会反向回滚" >&2
# 应用镜像可回退，数据库迁移不可自动逆转；迁移必须保持前后版本兼容或由人工执行恢复方案。
mv -f "$previous_release" "$release_file"
rm -f "$previous_release"
rollback_runtime || true
exit 1
