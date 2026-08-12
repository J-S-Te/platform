#!/usr/bin/env bash
set -Eeuo pipefail

usage() {
  echo "usage: $0 <crm-image@sha256:digest> <portal-image@sha256:digest>" >&2
  exit 2
}

[[ $# -eq 2 ]] || usage
crm_image_ref="$1"
portal_image_ref="$2"
acr_enterprise_or_new_personal='^[a-z0-9.-]+\.cr\.aliyuncs\.com/[a-z0-9._/-]+@sha256:[a-f0-9]{64}$'
acr_legacy_personal='^registry(-vpc)?\.[a-z0-9-]+\.aliyuncs\.com/[a-z0-9._/-]+@sha256:[a-f0-9]{64}$'
for image_ref in "$crm_image_ref" "$portal_image_ref"; do
  if [[ ! "$image_ref" =~ $acr_enterprise_or_new_personal && ! "$image_ref" =~ $acr_legacy_personal ]]; then
    echo "拒绝可变或格式错误的镜像引用：$image_ref" >&2
    exit 2
  fi
done
[[ "$crm_image_ref" != "$portal_image_ref" ]] || {
  echo "CRM 与 Portal 必须使用各自构建目标的独立镜像摘要" >&2
  exit 2
}

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
deploy_dir="$(cd -- "$script_dir/.." && pwd)"
runtime_file="$deploy_dir/.env"
release_file="$deploy_dir/.release.env"
customer_runtime_file="$deploy_dir/runtime/customer.env"
portal_runtime_file="$deploy_dir/runtime/portal.env"
customer_runtime_template="$deploy_dir/subsystem-templates/customer.env.example"
portal_runtime_template="$deploy_dir/subsystem-templates/portal.env.example"
compose_file="$deploy_dir/compose.yaml"
profiles_dir="$deploy_dir/subsystems.d"
export CUSTOMER_RUNTIME_ENV_FILE="$customer_runtime_file"
export PORTAL_RUNTIME_ENV_FILE="$portal_runtime_file"
customer_worker_services=(
  customer-opportunity-alert-worker
  customer-owner-notification-worker
  customer-presale-alert-worker
  customer-presale-assignment-notification-worker
  customer-presale-progress-notification-worker
  customer-presale-worker
)

for command_name in docker curl gzip flock awk mktemp install stat; do
  command -v "$command_name" >/dev/null || {
    echo "缺少命令：$command_name" >&2
    exit 1
  }
done
docker compose version >/dev/null
for required_file in "$runtime_file" "$release_file" "$compose_file"; do
  [[ -f "$required_file" ]] || { echo "缺少 $required_file" >&2; exit 1; }
done
[[ -f "$profiles_dir/customer_and_opportunity-prod.yaml" && -f "$profiles_dir/customer_portal-prod.yaml" ]] || {
  echo "缺少 CRM/Portal 生产接入审核清单，请先发布最新 platform 生产资产" >&2
  exit 1
}
[[ "$(stat -c '%a' "$runtime_file")" == "600" ]] || { echo "运行配置权限必须为 0600：$runtime_file" >&2; exit 1; }
[[ "$(stat -c '%a' "$release_file")" == "600" ]] || { echo "发布配置权限必须为 0600：$release_file" >&2; exit 1; }

install -d -m 700 "$deploy_dir/runtime" "$deploy_dir/backups" "$deploy_dir/backups/releases"
initialize_runtime_file() {
  local target="$1" template="$2"
  if [[ ! -f "$target" ]]; then
    [[ -f "$template" ]] || { echo "缺少运行配置模板：$template" >&2; exit 1; }
    install -m 600 "$template" "$target"
    echo "已初始化 $target；基础平台应用接入会由 Agent 自动补齐受管字段和声明的业务密钥"
  fi
  [[ ! -L "$target" ]] || {
    echo "拒绝符号链接运行配置：$target" >&2
    exit 1
  }
  chmod 600 "$target" || {
    echo "无法将运行配置权限收紧为 0600：$target" >&2
    exit 1
  }
  [[ "$(stat -c '%a' "$target")" == "600" ]] || {
    echo "运行配置权限无法收紧为 0600：$target" >&2
    exit 1
  }
}
initialize_runtime_file "$customer_runtime_file" "$customer_runtime_template"
initialize_runtime_file "$portal_runtime_file" "$portal_runtime_template"

exec 9>"$deploy_dir/runtime/.deploy.lock"
flock -w 900 9 || { echo "等待其他发布任务超时" >&2; exit 1; }

compose() {
  docker compose \
    --project-directory "$deploy_dir" \
    --file "$compose_file" \
    --env-file "$runtime_file" \
    --env-file "$release_file" \
    --profile customer \
    "$@"
}

env_value_from() {
  local file="$1" key="$2"
  awk -F= -v key="$key" '
    $0 !~ /^[[:space:]]*#/ && $1 == key {
      sub(/^[^=]*=/, "")
      print
      exit
    }
  ' "$file"
}

usable_value() {
  local value="$1"
  [[ -n "$value" && "$value" != REPLACE_WITH_* && "$value" != PENDING_* ]]
}

require_value() {
  local file="$1" key="$2" value
  value="$(env_value_from "$file" "$key")"
  usable_value "$value" || { echo "运行配置待补齐：$file:$key" >&2; return 1; }
}

infrastructure_ready() {
  local key
  for key in CUSTOMER_MYSQL_PASSWORD CUSTOMER_MYSQL_ROOT_PASSWORD PORTAL_MYSQL_PASSWORD PORTAL_MYSQL_ROOT_PASSWORD; do
    require_value "$runtime_file" "$key" || return 1
  done
}

customer_runtime_ready() {
  local key
  for key in \
    APP_PUBLIC_ORIGIN OIDC_ISSUER OIDC_CLIENT_ID OIDC_CLIENT_SECRET OIDC_REDIRECT_URI OIDC_TENANT_ID OIDC_ROLE_CONFIG_HASH \
    MACHINE_TOKEN_ISSUER MACHINE_TOKEN_AUDIENCE MACHINE_TOKEN_PUBLIC_KEY_PATH \
    PLATFORM_BASE_URL PLATFORM_AUTHORIZATION_CATALOG_APPLICATION_ID PLATFORM_AUTHORIZATION_CATALOG_CLIENT_ID PLATFORM_AUTHORIZATION_CATALOG_CLIENT_SECRET \
    PLATFORM_APPLICATION_CODE PLATFORM_ENVIRONMENT_CODE PLATFORM_AUDIT_CLIENT_ID PLATFORM_AUDIT_CLIENT_SECRET \
    SENSITIVE_ENCRYPTION_KEY_BASE64 SENSITIVE_HMAC_KEY_BASE64 PORTAL_INVITE_PEPPER_BASE64; do
    require_value "$customer_runtime_file" "$key" || return 1
  done
  [[ "$(env_value_from "$customer_runtime_file" DEV_AUTH_ENABLED)" == "false" ]] || {
    echo "生产 CRM 必须设置 DEV_AUTH_ENABLED=false" >&2
    return 1
  }
  [[ "$(env_value_from "$customer_runtime_file" PLATFORM_AUTHORIZATION_CATALOG_SYNC_ENABLED)" == "true" ]] || {
    echo "生产 CRM 必须启用授权目录同步" >&2
    return 1
  }
}

portal_runtime_ready() {
  local key
  for key in \
    PORTAL_PUBLIC_ORIGIN PORTAL_OIDC_ISSUER PORTAL_OIDC_CLIENT_ID PORTAL_OIDC_CLIENT_SECRET PORTAL_OIDC_REDIRECT_URI PORTAL_OIDC_TENANT_ID PORTAL_ROLE_CONFIG_HASH \
    PORTAL_ACCOUNT_SECURITY_CENTER_URL PORTAL_MACHINE_TOKEN_ISSUER PORTAL_MACHINE_TOKEN_AUDIENCE PORTAL_MACHINE_TOKEN_PUBLIC_KEY_PATH \
    PORTAL_CRM_PROVISION_CLIENT_SUBJECT PORTAL_CRM_DISABLE_CLIENT_SUBJECT PORTAL_CRM_INVITE_BASE_URL PORTAL_CRM_INVITE_TOKEN_URL PORTAL_CRM_INVITE_CLIENT_ID PORTAL_CRM_INVITE_CLIENT_SECRET \
    PORTAL_ENCRYPTION_KEY_BASE64 PORTAL_REPORT_INGEST_DESCRIPTOR_KEY_BASE64 PORTAL_HMAC_KEY_BASE64 \
    PORTAL_PLATFORM_BASE_URL PORTAL_AUTHORIZATION_CATALOG_APPLICATION_ID PORTAL_AUTHORIZATION_CATALOG_CLIENT_ID PORTAL_AUTHORIZATION_CATALOG_CLIENT_SECRET \
    PLATFORM_APPLICATION_CODE PLATFORM_ENVIRONMENT_CODE PLATFORM_AUDIT_CLIENT_ID PLATFORM_AUDIT_CLIENT_SECRET; do
    require_value "$portal_runtime_file" "$key" || return 1
  done
  [[ "$(env_value_from "$portal_runtime_file" PORTAL_AUTHORIZATION_CATALOG_SYNC_ENABLED)" == "true" ]] || {
    echo "生产 Portal 必须启用授权目录同步" >&2
    return 1
  }
}

previous_release="$(mktemp "$deploy_dir/.release.env.previous.XXXXXX")"
next_release="$(mktemp "$deploy_dir/.release.env.next.XXXXXX")"
release_updated=false
release_committed=false
cleanup() {
  local exit_code=$?
  trap - EXIT INT TERM
  if [[ "$release_updated" == true && "$release_committed" != true && -f "$previous_release" ]]; then
    cp "$previous_release" "$release_file"
    chmod 600 "$release_file"
  fi
  rm -f "$previous_release" "$next_release"
  exit "$exit_code"
}
trap cleanup EXIT INT TERM
chmod 600 "$previous_release" "$next_release"
cp "$release_file" "$previous_release"
release_id="$(date -u +%Y%m%dT%H%M%SZ)"
cp "$release_file" "$deploy_dir/backups/releases/customer-${release_id}.env"
chmod 600 "$deploy_dir/backups/releases/customer-${release_id}.env"

awk -F= -v crm="$crm_image_ref" -v portal="$portal_image_ref" '
  BEGIN { crm_found=0; portal_found=0 }
  $1 == "CUSTOMER_CRM_IMAGE" { print "CUSTOMER_CRM_IMAGE=" crm; crm_found=1; next }
  $1 == "CUSTOMER_PORTAL_IMAGE" { print "CUSTOMER_PORTAL_IMAGE=" portal; portal_found=1; next }
  { print }
  END {
    if (!crm_found) print "CUSTOMER_CRM_IMAGE=" crm
    if (!portal_found) print "CUSTOMER_PORTAL_IMAGE=" portal
  }
' "$release_file" >"$next_release"
release_updated=true
mv "$next_release" "$release_file"
chmod 600 "$release_file"

restore_release() {
  cp "$previous_release" "$release_file"
  chmod 600 "$release_file"
  release_updated=false
}

echo "拉取 CRM 不可变镜像：$crm_image_ref"
if ! docker pull "$crm_image_ref"; then
  restore_release
  rm -f "$previous_release"
  echo "CRM 镜像拉取失败，发布配置已恢复" >&2
  exit 1
fi
echo "拉取 Portal 不可变镜像：$portal_image_ref"
if ! docker pull "$portal_image_ref" || ! compose config --quiet; then
  restore_release
  rm -f "$previous_release"
  echo "Portal 镜像拉取或 Compose 校验失败，发布配置已恢复" >&2
  exit 1
fi

# 角色/权限目录必须与实际运行的不可变 CRM 镜像完全一致。不要依赖人工维护的
# runtime/customer.env 旧值：从本次镜像中的 authz-catalog 读取兼容哈希，并通过
# Compose environment 覆盖传入 customer-api，随后由发布步骤同步到基础平台。
embedded_crm_hash="$(docker run --rm --entrypoint ./authz-catalog "$crm_image_ref" print crm 2>/dev/null \
  | awk -F= '$1 == "claims_role_config_hash" { print $2; exit }')"
if [[ ! "$embedded_crm_hash" =~ ^sha256:[a-f0-9]{64}$ ]]; then
  restore_release
  echo "无法从 CRM 不可变镜像读取有效授权目录哈希" >&2
  exit 1
fi
export OIDC_ROLE_CONFIG_HASH="$embedded_crm_hash"
echo "使用 CRM 镜像内嵌授权目录哈希：$OIDC_ROLE_CONFIG_HASH"

if ! infrastructure_ready || ! customer_runtime_ready || ! portal_runtime_ready; then
  release_committed=true
  echo "CRM/Portal 镜像已安全暂存；运行凭据尚未完整配置，未启动数据库迁移或业务服务。"
  echo "请先在基础平台应用接入页面完成或重试对应环境；Agent 会补齐 runtime 配置，随后重新运行同一发布命令。"
  exit 0
fi

backup_database() {
  local mysql_service="$1" database="$2" label="$3"
  local output="$deploy_dir/backups/${label}-${release_id}.sql.gz"
  local temporary
  temporary="$(mktemp "$deploy_dir/backups/.${label}-${release_id}.XXXXXX.sql.gz")"
  chmod 600 "$temporary"
  echo "备份数据库到 $output"
  if ! compose exec -T "$mysql_service" sh -c \
    'exec mysqldump --single-transaction --routines --triggers -uroot -p"$MYSQL_ROOT_PASSWORD" "$1"' \
    _ "$database" | gzip -9 >"$temporary"; then
    rm -f "$temporary"
    return 1
  fi
  mv "$temporary" "$output"
}

port_value() {
  local key="$1" fallback="$2" value
  value="$(env_value_from "$runtime_file" "$key")"
  if [[ "$value" =~ ^[0-9]+$ ]] && ((value >= 1 && value <= 65535)); then echo "$value"; else echo "$fallback"; fi
}

wait_for_health() {
  local url="$1"
  echo "等待健康检查：$url"
  for ((attempt=1; attempt<=60; attempt++)); do
    if curl --fail --silent --show-error --max-time 3 "$url" >/dev/null 2>&1; then
      echo "健康检查通过：$url"
      return 0
    fi
    sleep 2
  done
  echo "健康检查超时：$url" >&2
  return 1
}

wait_for_workers() {
  local attempt state service all_running
  for ((attempt=1; attempt<=30; attempt++)); do
    state="$(compose ps --status running --services)"
    all_running=true
    for service in "${customer_worker_services[@]}"; do
      case $'\n'"$state"$'\n' in
        *$'\n'"$service"$'\n'*) ;;
        *) all_running=false ;;
      esac
    done
    [[ "$all_running" == true ]] && return 0
    sleep 2
  done
  echo "CRM Worker 运行状态检查超时" >&2
  return 1
}

rollback_runtime() {
  restore_release
  local previous_crm previous_portal
  previous_crm="$(env_value_from "$release_file" CUSTOMER_CRM_IMAGE)"
  previous_portal="$(env_value_from "$release_file" CUSTOMER_PORTAL_IMAGE)"
  if [[ "$previous_crm" =~ $acr_enterprise_or_new_personal || "$previous_crm" =~ $acr_legacy_personal ]] && \
     [[ "$previous_portal" =~ $acr_enterprise_or_new_personal || "$previous_portal" =~ $acr_legacy_personal ]]; then
    compose up -d --no-deps customer-api portal-api "${customer_worker_services[@]}" || true
    return
  fi
  echo "没有可验证的上一版不可变 CRM/Portal 镜像，停止新 API，等待人工恢复" >&2
  compose stop customer-api portal-api "${customer_worker_services[@]}" || true
}

echo "启动 CRM/Portal 数据库与 Temporal"
if ! compose up -d --wait --wait-timeout 180 customer-mysql portal-mysql temporal; then
  restore_release
  rm -f "$previous_release"
  echo "数据库启动失败，发布配置已恢复" >&2
  exit 1
fi
backup_database customer-mysql customer_opportunity customer
backup_database portal-mysql customer_portal portal

echo "执行 CRM 语句级生产迁移"
if ! compose --profile customer-release run --rm customer-migrate; then
  restore_release
  rm -f "$previous_release"
  echo "CRM 迁移失败；数据库不会自动反向回滚，请按 RUNNING 检查点进行人工核验" >&2
  exit 1
fi
echo "执行 Portal 语句级生产迁移"
if ! compose --profile customer-release run --rm portal-migrate; then
  restore_release
  rm -f "$previous_release"
  echo "Portal 迁移失败；数据库不会自动反向回滚，请按 RUNNING 检查点进行人工核验" >&2
  exit 1
fi

echo "切换 CRM API"
if ! compose up -d --no-deps customer-api || \
   ! wait_for_health "http://127.0.0.1:$(port_value CUSTOMER_API_PORT 18083)/customer-opportunity/healthz"; then
  compose logs --tail 100 customer-api >&2 || true
  rollback_runtime
  rm -f "$previous_release"
  echo "CRM 发布失败，已恢复上一镜像；已执行的数据库迁移不会反向回滚" >&2
  exit 1
fi
echo "自动发布 CRM 授权目录（角色及有效角色数量策略）"
if ! compose --profile customer-release run --rm --no-deps customer-api ./authz-catalog publish crm; then
  compose logs --tail 100 customer-api >&2 || true
  rollback_runtime
  rm -f "$previous_release"
  echo "CRM 授权目录发布失败；已恢复运行时配置，未继续切换 Portal" >&2
  exit 1
fi

echo "切换 Portal API"
if ! compose up -d --no-deps portal-api || \
   ! wait_for_health "http://127.0.0.1:$(port_value PORTAL_API_PORT 18084)/customer-portal/healthz"; then
  compose logs --tail 100 portal-api >&2 || true
  rollback_runtime
  rm -f "$previous_release"
  echo "Portal 发布失败，已恢复上一镜像；已执行的数据库迁移不会反向回滚" >&2
  exit 1
fi

echo "切换 CRM 站内告警与通知投影 Worker"
if ! compose up -d --no-deps "${customer_worker_services[@]}" || ! wait_for_workers; then
  compose logs --tail 100 "${customer_worker_services[@]}" >&2 || true
  rollback_runtime
  rm -f "$previous_release"
  echo "CRM Worker 发布失败，已恢复上一镜像；已执行的数据库迁移不会反向回滚" >&2
  exit 1
fi

release_committed=true
compose ps customer-api portal-api "${customer_worker_services[@]}" customer-mysql portal-mysql
echo "CRM/Portal 发布成功：$crm_image_ref / $portal_image_ref"
