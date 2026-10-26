#!/usr/bin/env bash
# Shared deploy entry helpers. Sourced; no side effects beyond function definitions.

die() { printf '错误：%s\n' "$*" >&2; exit 1; }
need() { command -v "$1" >/dev/null || die "缺少所需命令：$1"; }
random_hex() { openssl rand -hex "${1:-32}"; }
random_b64() { openssl rand -base64 32 | tr -d '\n'; }

env_get() {
  local file="$1" key="$2"
  awk -F= -v key="$key" '$0 !~ /^[[:space:]]*#/ && $1 == key {sub(/^[^=]*=/, ""); print; exit}' "$file"
}

env_set() {
  local file="$1" key="$2" value="$3" temporary
  [[ "$value" != *$'\n'* ]] || die "$key 不允许包含换行符"
  temporary="$(mktemp "$deploy_dir/.env-update.XXXXXX")" || return 1
  awk -F= -v key="$key" -v value="$value" '
    BEGIN { found=0 }
    $0 !~ /^[[:space:]]*#/ && $1 == key { print key "=" value; found=1; next }
    { print }
    END { if (!found) print key "=" value }
  ' "$file" > "$temporary" || { rm -f -- "$temporary"; return 1; }
  chmod 600 "$temporary" || { rm -f -- "$temporary"; return 1; }
  mv -f "$temporary" "$file" || { rm -f -- "$temporary"; return 1; }
}

env_set_generated() {
  local file="$1" key="$2" value="$3" current
  current="$(env_get "$file" "$key")"
  if [[ -n "$current" && "$current" != REPLACE_WITH_* && "$current" != PENDING_* ]]; then
    return 0
  fi
  env_set "$file" "$key" "$value"
}

ensure_application_network() {
  local network_name="basic-platform-production"
  if docker network inspect "$network_name" >/dev/null 2>&1; then
    return 0
  fi
  docker network create \
    --driver bridge \
    --subnet 172.31.255.0/24 \
    --gateway 172.31.255.1 \
    "$network_name" >/dev/null || docker network inspect "$network_name" >/dev/null
}

compose() {
  ensure_application_network
  public_transport_prepare "$deploy_dir" "$runtime_file"
  local args=(--project-directory "$deploy_dir" --file "$compose_file")
  public_transport_compose_args args
  docker compose "${args[@]}" --env-file "$runtime_file" --env-file "$release_file" "$@"
}

compose_monitoring() {
  compose "$@"
}

manifest_value() {
  local file="$1" key="$2"
  awk -F= -v key="$key" '$1 == key && $0 ~ /^[A-Z_]+=[A-Za-z0-9.,_:/-]+$/ {sub(/^[^=]*=/, ""); print; exit}' "$file"
}

manifest_optional_unique_value() {
  awk -F= -v key="$2" '$1 == key {if (++seen > 1) exit 1; value=substr($0,index($0,"=")+1)} END {if(seen>1 || (seen==1 && value==""))exit 1; print value}' "$1"
}

verify_archive_sidecar() {
  local archive="${1:?archive required}" sidecar="${2:-$1.sha256}" expected actual
  [[ -f "$sidecar" && ! -L "$sidecar" ]] || die "镜像包校验文件缺失或不是普通文件：$sidecar"
  expected="$(awk -v name="$(basename -- "$archive")" '
    NF != 2 || NR != 1 || length($1) != 64 || tolower($1) ~ /[^0-9a-f]/ || $2 != name {bad=1}
    {digest=tolower($1)}
    END {if (bad || NR != 1) exit 1; print digest}
  ' "$sidecar")" || die "镜像包校验文件必须只包含当前包的一个规范摘要：$sidecar"
  actual="$(sha256sum "$archive" | awk '{print tolower($1)}')"
  [[ "$actual" == "$expected" ]] || die "镜像包 SHA256 校验失败：$archive"
  printf '%s\n' "$actual"
}

prepare_import_temp_root() {
  local temp_root="${UIP_IMPORT_TMPDIR:-$deploy_dir/runtime/import-tmp}"
  [[ ! -L "$temp_root" ]] || die "镜像导入临时目录不能是符号链接：$temp_root"
  if [[ -e "$temp_root" && ! -d "$temp_root" ]]; then
    die "镜像导入临时路径不是目录：$temp_root"
  fi
  mkdir -p -- "$temp_root" || die "无法创建镜像导入临时目录：$temp_root"
  chmod 700 "$temp_root" || die "无法收紧镜像导入临时目录权限：$temp_root"
  printf '%s\n' "$temp_root"
}

refresh_subsystem_provisioner() {
  refresh_subsystem_control_plane_config ||
    die '子系统控制面未能成对重载或验证当前发布配置；已拒绝继续发布'
}

reload_control_plane() {
  require_initialized
  need flock
  [[ ! -L "$deploy_dir/runtime/.deploy.lock" ]] || die '部署锁不能是符号链接'
  if [[ -L "$control_plane_reload_marker" ]]; then
    die "控制面重载标记不能是符号链接：$control_plane_reload_marker"
  fi
  if [[ -e "$control_plane_reload_marker" && ! -f "$control_plane_reload_marker" ]]; then
    die "控制面重载标记不是普通文件：$control_plane_reload_marker"
  fi
  exec 7>"$deploy_dir/runtime/.deploy.lock"
  flock -w 60 7 || die '平台 Agent 或其他发布任务正在运行；请等待其结束后重试控制面重载'
  if ! refresh_subsystem_control_plane_config; then
    printf '错误：子系统控制面成对重载或一致性验证失败。重载标记已保留；修复后重新执行：%s reload-control-plane\n' "$0" >&2
    flock -u 7
    exec 7>&-
    return 1
  fi
  if [[ -f "$control_plane_reload_marker" ]]; then
    rm -f -- "$control_plane_reload_marker" || {
      printf '错误：控制面已验证，但无法移除重载标记：%s\n' "$control_plane_reload_marker" >&2
      flock -u 7
      exec 7>&-
      return 1
    }
  fi
  printf '子系统控制面重载完成：Agent 与 platform-api 使用同一生产清单集合。\n'
  flock -u 7
  exec 7>&-
}


require_initialized() {
  [[ -f "$runtime_file" && -f "$release_file" ]] || die "请先执行：$0 configure"
  [[ "$(stat -c '%a' "$runtime_file")" == 600 && "$(stat -c '%a' "$release_file")" == 600 ]] || die "配置文件权限必须为 0600"
}

# 本脚本必须与同一资产包中的 docker-compose.yml、offline-configure.sh、public-transport.sh
# 放在一起才能运行。交付目录顶层的 server_up.sh 只是本脚本的中文副本，单独执行会在这里
# 被拦截并给出可操作的提示，而不是抛出难以理解的 source 错误。
# 这里只列 source 阶段真正依赖的文件；deploy-service.sh 仅 deploy/upgrade 需要，故意
# 不在此校验，以免破坏只复制部分脚本的 fixture 测试。
missing_siblings=()
for sibling in offline-configure.sh public-transport.sh; do
  [[ -f "$script_dir/$sibling" ]] || missing_siblings+=("$script_dir/$sibling")
done
[[ -f "$deploy_dir/docker-compose.yml" ]] || missing_siblings+=("$deploy_dir/docker-compose.yml")
if ((${#missing_siblings[@]} > 0)); then
  printf '错误：当前脚本不在完整的部署目录中，缺少以下文件：\n' >&2
  printf '  %s\n' "${missing_siblings[@]}" >&2
  printf '请先解压 deployment-assets-*.tar.gz 到部署目录，再执行：\n' >&2
  printf '  cd <部署目录> && sudo ./bin/deploy.sh\n' >&2
  printf '交付目录顶层的 server_up.sh 是本脚本的中文副本，仅供查看和审核，不能直接运行。\n' >&2
  exit 1
fi

# shellcheck source=offline-configure.sh
source "$script_dir/offline-configure.sh"
