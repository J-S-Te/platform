#!/usr/bin/env bash
set -Eeuo pipefail

usage() {
  echo "usage: $0 {frontend|platform|contract} <acr-host>/<namespace>/<image>@sha256:<64-hex-digest>" >&2
  exit 2
}

[[ $# -eq 2 ]] || usage
service="$1"
image_ref="$2"
case "$service" in
  frontend) image_key=FRONTEND_IMAGE ;;
  platform) image_key=PLATFORM_IMAGE ;;
  contract) image_key=CONTRACT_IMAGE ;;
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
compose_file="$deploy_dir/compose.yaml"

for command_name in docker curl gzip flock awk mktemp; do
  command -v "$command_name" >/dev/null || {
    echo "缺少命令：$command_name" >&2
    exit 1
  }
done
docker compose version >/dev/null
[[ -f "$runtime_file" ]] || { echo "缺少 $runtime_file" >&2; exit 1; }
[[ -f "$release_file" ]] || { echo "缺少 $release_file" >&2; exit 1; }
[[ -f "$compose_file" ]] || { echo "缺少 $compose_file" >&2; exit 1; }

mkdir -p "$deploy_dir/backups/releases"
exec 9>"$deploy_dir/.deploy.lock"
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

require_runtime_value() {
  local key="$1"
  local value
  value="$(env_value "$key")"
  if [[ -z "$value" || "$value" == REPLACE_WITH_* || "$value" == PENDING_* ]]; then
    echo "运行配置缺失或仍为占位值：$key" >&2
    return 1
  fi
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

backup_database() {
  local mysql_service="$1"
  local database="$2"
  local output="$deploy_dir/backups/${service}-${release_id}.sql.gz"
  echo "备份数据库到 $output"
  compose exec -T "$mysql_service" sh -c \
    'exec mysqldump --single-transaction --routines --triggers -uroot -p"$MYSQL_ROOT_PASSWORD" "$1"' \
    _ "$database" \
    | gzip -9 >"$output"
}

deploy_platform() {
  compose up -d --wait --wait-timeout 180 platform-mysql || return
  backup_database platform-mysql basic_platform || return
  compose --profile release run --rm platform-migrate ./migrate || return
  compose up -d --no-deps platform-api || return
  wait_for_health "http://127.0.0.1:$(port_value PLATFORM_API_PORT 18080)/readyz"
}

deploy_contract() {
  require_runtime_value OIDC_CLIENT_ID || return
  require_runtime_value OIDC_CLIENT_SECRET || return
  require_runtime_value OIDC_TENANT_ID || return
  require_runtime_value PLATFORM_AUTHORIZATION_CATALOG_APPLICATION_ID || return
  require_runtime_value PLATFORM_AUTHORIZATION_CATALOG_CLIENT_ID || return
  require_runtime_value PLATFORM_AUTHORIZATION_CATALOG_CLIENT_SECRET || return
  if [[ "$(env_value PLATFORM_AUTHORIZATION_CATALOG_SYNC_ENABLED)" != "true" ]]; then
    echo "PLATFORM_AUTHORIZATION_CATALOG_SYNC_ENABLED 必须为 true" >&2
    return 1
  fi
  compose up -d --wait --wait-timeout 240 contract-mysql temporal || return
  backup_database contract-mysql contract_management || return
  # The migration command is owned by compose.yaml. A non-zero migration exit stops
  # the release before the API image is replaced.
  compose --profile release run --rm contract-migrate || return
  compose up -d --no-deps contract-api || return
  wait_for_health "http://127.0.0.1:$(port_value CONTRACT_API_PORT 18081)/healthz"
}

deploy_frontend() {
  compose up -d --no-deps frontend || return
  wait_for_health "http://127.0.0.1:$(port_value FRONTEND_PORT 18082)/"
}

rollback_runtime() {
  case "$service" in
    frontend) compose up -d --no-deps frontend ;;
    platform) compose up -d --no-deps platform-api ;;
    contract) compose up -d --no-deps contract-api ;;
  esac
}

release_id="$(date -u +%Y%m%dT%H%M%SZ)"
previous_release="$(mktemp "$deploy_dir/.release.env.previous.XXXXXX")"
next_release="$(mktemp "$deploy_dir/.release.env.next.XXXXXX")"
chmod 600 "$previous_release" "$next_release"
cp "$release_file" "$previous_release"
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
if ! docker pull "$image_ref" || ! compose config --quiet; then
  cp "$previous_release" "$release_file"
  rm -f "$previous_release"
  echo "镜像拉取或 Compose 校验失败，发布配置已恢复" >&2
  exit 1
fi

echo "开始发布 $service"
if "deploy_${service}"; then
  rm -f "$previous_release"
  echo "$service 发布成功：$image_ref"
  exit 0
fi

echo "发布失败，恢复上一镜像；已执行的数据库迁移不会反向回滚" >&2
cp "$previous_release" "$release_file"
rm -f "$previous_release"
rollback_runtime || true
exit 1
