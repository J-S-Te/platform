#!/usr/bin/env bash
set -Eeuo pipefail

# 每次发布只接受带 sha256 digest 的阿里云镜像。版本指针写入 .release.env，运行密钥仍留在
# 独立 .env 中，避免发布产物或历史镜像记录携带数据库、OAuth 等敏感配置。

usage() {
  echo "usage: $0 {frontend|platform|contract|project} <acr-host>/<namespace>/<image>@sha256:<64-hex-digest>" >&2
  exit 2
}

[[ $# -eq 2 ]] || usage
service="$1"
image_ref="$2"
case "$service" in
  frontend) image_key=FRONTEND_IMAGE ;;
  platform) image_key=PLATFORM_IMAGE ;;
  contract) image_key=CONTRACT_IMAGE ;;
  project) image_key=PROJECT_IMAGE ;;
  *) usage ;;
esac

acr_enterprise_or_new_personal='^[a-z0-9.-]+\.cr\.aliyuncs\.com/[a-z0-9._/-]+@sha256:[a-f0-9]{64}$'
acr_legacy_personal='^registry(-vpc)?\.[a-z0-9-]+\.aliyuncs\.com/[a-z0-9._/-]+@sha256:[a-f0-9]{64}$'
if [[ ! "$image_ref" =~ $acr_enterprise_or_new_personal && ! "$image_ref" =~ $acr_legacy_personal ]]; then
  echo "拒绝可变或格式错误的镜像引用：$image_ref" >&2
  exit 2
fi

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
deploy_dir="$(cd -- "$script_dir/.." && pwd)"
release_file="$deploy_dir/.release.env"
runtime_file="$deploy_dir/.env"
contract_runtime_file="$deploy_dir/runtime/contract.env"
contract_runtime_template="$deploy_dir/subsystem-templates/contract.env.example"
project_runtime_file="$deploy_dir/runtime/project.env"
project_runtime_template="$deploy_dir/subsystem-templates/project.env.example"
compose_file="$deploy_dir/compose.yaml"
frontend_compose_file="$deploy_dir/compose.frontend.yaml"
profiles_dir="$deploy_dir/subsystems.d"
export CONTRACT_RUNTIME_ENV_FILE="$contract_runtime_file"
export PROJECT_RUNTIME_ENV_FILE="$project_runtime_file"

for command_name in docker curl gzip flock awk mktemp install stat; do
  command -v "$command_name" >/dev/null || {
    echo "缺少命令：$command_name" >&2
    exit 1
  }
done
relax_runtime_perm_check="${DEPLOY_RELAX_RUNTIME_PERM_CHECK:-false}"
docker compose version >/dev/null
[[ -f "$runtime_file" ]] || { echo "缺少 $runtime_file" >&2; exit 1; }
[[ -f "$release_file" ]] || { echo "缺少 $release_file" >&2; exit 1; }
[[ -f "$compose_file" ]] || { echo "缺少 $compose_file" >&2; exit 1; }
[[ -d "$profiles_dir" ]] || { echo "缺少生产子系统审核清单目录：$profiles_dir" >&2; exit 1; }
compgen -G "$profiles_dir/*.yaml" >/dev/null || { echo "生产子系统审核清单目录中没有 YAML 文件" >&2; exit 1; }

ensure_runtime_file_mode_0600() {
  local target="$1" temporary
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
esac

install -d -m 700 "$deploy_dir/runtime"
mkdir -p "$deploy_dir/backups/releases"
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
  echo "备份数据库到 $output"
  # single-transaction 为 InnoDB 提供一致性快照，备份管道任一环节失败都会因 pipefail 中止发布。
  compose exec -T "$mysql_service" sh -c \
    'exec mysqldump --single-transaction --routines --triggers -uroot -p"$MYSQL_ROOT_PASSWORD" "$1"' \
    _ "$database" \
    | gzip -9 >"$output"
}

deploy_platform() {
  compose up -d --wait --wait-timeout 180 platform-mysql || return
  backup_database platform-mysql basic_platform || return
  compose --profile release run --rm platform-migrate ./migrate || return
  # 平台 API 只通过共享 Unix Socket 调用生产接入 Agent。先让同一平台镜像中的
  # Agent 健康，再切 API，避免新旧协议短暂不一致或页面误报 Agent 未启用。
  # Agent 需要强制重建以重载 subsystems.d 清单（无 HTTP 流量，秒级恢复）；
  # platform-api 只在镜像 digest 变化时由 compose 自动重建，不在此强制重建，
  # 避免每次部署都打断门户/接入操作造成 502。
  if ! compose up -d --force-recreate --wait --wait-timeout 60 --no-deps subsystem-provisioner; then
    echo "subsystem-provisioner 健康启动失败" >&2
    dump_subsystem_provisioner_debug
    return 1
  fi
  compose up -d --no-deps platform-api || return
  wait_for_health "http://127.0.0.1:$(port_value PLATFORM_API_PORT 18080)/readyz" || {
    # 兜底：新镜像首启异常时强制重建一次并再次等待，避免平台 API 停留在宕机状态。
    echo "platform-api 未通过健康检查，强制重建一次后重试" >&2
    compose up -d --force-recreate --no-deps platform-api || return
    wait_for_health "http://127.0.0.1:$(port_value PLATFORM_API_PORT 18080)/readyz" || return
  }
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
  compose up -d --no-deps contract-api || return
  if ! wait_for_health "http://127.0.0.1:$(port_value CONTRACT_API_PORT 18081)/healthz"; then
    echo "---- contract-api 最近日志 ----" >&2
    compose logs --no-color --tail 120 contract-api >&2 || true
    return 1
  fi
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
  compose up -d --no-deps project-api || return
  if ! wait_for_health "http://127.0.0.1:$(port_value PROJECT_API_PORT 18085)/healthz"; then
    echo "---- project-api 最近日志 ----" >&2
    compose logs --no-color --tail 120 project-api >&2 || true
    return 1
  fi
}

deploy_frontend() {
  frontend_compose up -d frontend || return
  wait_for_health "http://127.0.0.1:$(port_value FRONTEND_PORT 18082)/"
}

rollback_runtime() {
  case "$service" in
    frontend) frontend_compose up -d frontend ;;
    platform)
      if ! compose up -d --force-recreate --wait --wait-timeout 60 --no-deps subsystem-provisioner; then
        echo "回滚时 subsystem-provisioner 重建失败，继续尝试重建 platform-api" >&2
        dump_subsystem_provisioner_debug
      fi
      compose up -d --no-deps platform-api
      ;;
    contract) compose up -d --no-deps contract-api ;;
    project) compose up -d --no-deps project-api ;;
  esac
}

release_id="$(date -u +%Y%m%dT%H%M%SZ)"
previous_release="$(mktemp "$deploy_dir/.release.env.previous.XXXXXX")"
next_release="$(mktemp "$deploy_dir/.release.env.next.XXXXXX")"
chmod 600 "$previous_release" "$next_release"
# 临时版本文件与正式指针位于同一文件系统，mv 后不会暴露半写入的镜像 digest。
cp "$release_file" "$previous_release"
# 每次发布保留旧版本指针快照，但不复制包含运行密钥的 .env。
cp "$release_file" "$deploy_dir/backups/releases/${release_id}.env"

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
mv "$next_release" "$release_file"
chmod 600 "$release_file"

echo "拉取不可变镜像：$image_ref"
if ! docker pull "$image_ref"; then
  cp "$previous_release" "$release_file"
  rm -f "$previous_release"
  echo "镜像拉取失败，发布配置已恢复" >&2
  exit 1
fi
if [[ "$service" == "frontend" ]]; then
  compose_config_ok=false
  frontend_compose config --quiet && compose_config_ok=true
else
  compose_config_ok=false
  compose config --quiet && compose_config_ok=true
fi
if [[ "$compose_config_ok" != "true" ]]; then
  cp "$previous_release" "$release_file"
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
  rm -f "$previous_release"
  echo "项目管理镜像已安全暂存：$image_ref"
  echo "运行凭据尚未生成，请登录基础平台的“应用接入”页面完成 project_management/prod 接入。"
  exit 0
fi

echo "开始发布 $service"
if "deploy_${service}"; then
  rm -f "$previous_release"
  echo "$service 发布成功：$image_ref"
  exit 0
fi

echo "发布失败，恢复上一镜像；已执行的数据库迁移不会反向回滚" >&2
# 应用镜像可回退，数据库迁移不可自动逆转；迁移必须保持前后版本兼容或由人工执行恢复方案。
cp "$previous_release" "$release_file"
rm -f "$previous_release"
rollback_runtime || true
exit 1
