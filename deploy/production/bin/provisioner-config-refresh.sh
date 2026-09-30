#!/usr/bin/env bash

# subsystem-provisioner receives .release.env and docker-compose.yml as single-file
# bind mounts. Both files are intentionally updated with rename(2), so a running
# container remains attached to the old inode until it is recreated. Both the
# provisioner and platform-api also cache subsystems.d at process startup. When
# deployment assets replace that directory, recreating only one process produces
# different manifest checksums and every controlled retry is correctly rejected as
# drift. Callers must hold runtime/.deploy.lock before invoking these helpers. The
# helpers deliberately do not acquire the lock themselves: deploy.sh,
# deploy-service.sh and start-enabled.sh protect the surrounding transaction.

# CI can install into a non-default root. Compose's bind-mount sources and both
# control-plane processes must resolve that same root before any reload attempt.
prepare_ci_deploy_root() (
  local runtime temporary configured profiles
  [[ "${deploy_dir:-}" == /* && "$deploy_dir" != / ]] || { echo '无效 CI 部署根目录' >&2; return 1; }
  runtime="$deploy_dir/.env"
  [[ -f "$runtime" && ! -L "$runtime" ]] || { echo 'CI 部署需要普通文件形式的 .env' >&2; return 1; }
  [[ ! -L "$deploy_dir/runtime" && ! -L "$deploy_dir/runtime/.deploy.lock" ]] || {
    echo 'CI 部署锁目录不能是符号链接' >&2; return 1;
  }
  install -d -m 700 "$deploy_dir/runtime"
  exec 8>"$deploy_dir/runtime/.deploy.lock"
  flock -w 900 8 || return 1
  configured="$(awk -F= '$1=="SUBSYSTEM_PRODUCTION_HOST_DEPLOY_ROOT" {count++; value=substr($0,index($0,"=")+1)} END {if(count>1)exit 1; print value}' "$runtime")" || {
    echo '重复的 SUBSYSTEM_PRODUCTION_HOST_DEPLOY_ROOT 配置' >&2; return 1;
  }
  profiles="$(awk -F= '$1=="SUBSYSTEM_PRODUCTION_PROFILES_DIR" {count++; value=substr($0,index($0,"=")+1)} END {if(count>1)exit 1; print value}' "$runtime")" || {
    echo '重复的 SUBSYSTEM_PRODUCTION_PROFILES_DIR 配置' >&2; return 1;
  }
  [[ -z "$configured" || "$configured" == "$deploy_dir" ]] || {
    echo 'SUBSYSTEM_PRODUCTION_HOST_DEPLOY_ROOT 与 DEPLOY_PATH 不一致；拒绝更改明确配置' >&2; return 1;
  }
  [[ -z "$profiles" || "$profiles" == "$deploy_dir/subsystems.d" ]] || {
    echo 'SUBSYSTEM_PRODUCTION_PROFILES_DIR 与 CI 安装清单目录不一致' >&2; return 1;
  }
  [[ -z "$configured" ]] || return 0
  temporary="$(mktemp "$deploy_dir/.env.ci-root.XXXXXX")" || return 1
  trap 'rm -f -- "$temporary"' EXIT
  CI_DEPLOY_ROOT="$deploy_dir" awk -F= '
    $1=="SUBSYSTEM_PRODUCTION_HOST_DEPLOY_ROOT" {next}
    {print}
    END {print "SUBSYSTEM_PRODUCTION_HOST_DEPLOY_ROOT=" ENVIRON["CI_DEPLOY_ROOT"]}
  ' "$runtime" >"$temporary" || return 1
  if [[ "$(id -u)" == 0 ]]; then
    chown --reference="$runtime" "$temporary" || return 1
  fi
  chmod 600 "$temporary" || return 1
  mv -f -- "$temporary" "$runtime" || return 1
  echo '已将 CI 部署根目录绑定到平台与 Agent 的 Compose 配置'
)

control_plane_reload_marker_path() {
  local root="${deploy_dir:-}"
  [[ -n "$root" ]] || { echo '错误：缺少部署目录，无法检查控制面重载门禁' >&2; return 1; }
  printf '%s/runtime/.control-plane-reload-required' "$root"
}

# 部署资产（subsystems.d、模板、compose）在控制面运行中被替换后，install-assets.sh 会
# 留下 runtime/.control-plane-reload-required。在成对重载成功前，任何会重建或启动控制面
# 的入口都必须拒绝：只重建一侧会让 platform-api 与 subsystem-provisioner 使用不同的生产
# 清单集合，受控采用/更新会被 manifest drift 保护正确拒绝，现场表现为"重试也没用"。
require_control_plane_reload_clearance() {
  local marker
  marker="$(control_plane_reload_marker_path)" || return 1
  if [[ -L "$marker" ]]; then
    echo "错误：控制面重载标记不能是符号链接：$marker" >&2
    return 1
  fi
  if [[ -e "$marker" && ! -f "$marker" ]]; then
    echo "错误：控制面重载标记不是普通文件：$marker" >&2
    return 1
  fi
  [[ -f "$marker" ]] || return 0
  echo '错误：部署资产已更新，但 subsystem-provisioner 与 platform-api 尚未成对重载。' >&2
  echo '为避免平台与 Agent 使用不同的生产清单，本次操作已拒绝。' >&2
  echo "请先执行：${deploy_dir}/bin/deploy.sh reload-control-plane" >&2
  return 1
}

subsystem_control_plane_container() {
  local service="${1:?compose service required}" container
  container="$(docker ps -q \
    --filter "label=com.docker.compose.project=${COMPOSE_PROJECT_NAME:-basic-platform-production}" \
    --filter "label=com.docker.compose.service=${service}" 2>/dev/null | head -n 1 || true)"
  if [[ -z "$container" ]]; then
    container="$(docker ps -q --filter "name=uip-${service}" 2>/dev/null | head -n 1 || true)"
  fi
  printf '%s\n' "$container"
}

production_profiles_host_manifest() {
  local directory="${profiles_dir:-${deploy_dir:?missing deploy dir}/subsystems.d}"
  local file name hash output=''
  local -a files=()

  [[ -d "$directory" && ! -L "$directory" ]] || {
    echo "生产子系统清单目录缺失、不是目录或是符号链接：$directory" >&2
    return 1
  }
  shopt -s nullglob
  files=("$directory"/*.yaml)
  shopt -u nullglob
  ((${#files[@]} > 0)) || {
    echo "生产子系统清单目录中没有 YAML：$directory" >&2
    return 1
  }
  for file in "${files[@]}"; do
    [[ -f "$file" && ! -L "$file" ]] || {
      echo "生产子系统清单必须是普通文件且不能是符号链接：$file" >&2
      return 1
    }
    name="${file##*/}"
    [[ "$name" =~ ^[A-Za-z0-9][A-Za-z0-9._-]*\.yaml$ ]] || {
      echo "生产子系统清单文件名不安全：$name" >&2
      return 1
    }
    hash="$(sha256sum "$file" | awk '{print $1}')" || return 1
    output+="${hash}  ${name}"$'\n'
  done
  printf '%s' "$output" | LC_ALL=C sort
}

production_profiles_container_manifest() {
  local container="${1:?container required}"
  docker exec "$container" sh -ec '
    directory="${SUBSYSTEM_PRODUCTION_PROFILES_DIR:?missing profiles directory}"
    [ -d "$directory" ] && [ ! -L "$directory" ] || exit 41
    found=false
    for file in "$directory"/*.yaml; do
      [ -f "$file" ] || continue
      [ ! -L "$file" ] || exit 42
      name=${file##*/}
      case "$name" in
        *[!A-Za-z0-9._-]*|.*) exit 43 ;;
        *.yaml) ;;
        *) exit 43 ;;
      esac
      found=true
      hash=$(sha256sum "$file" | awk "{print \$1}") || exit 44
      printf "%s  %s\n" "$hash" "$name"
    done
    [ "$found" = true ] || exit 45
  ' | LC_ALL=C sort
}

verify_subsystem_control_plane_profiles() {
  local provisioner="${1:?provisioner container required}"
  local platform_api="${2:?platform api container required}"
  local host_manifest provisioner_manifest api_manifest host_digest provisioner_digest api_digest

  host_manifest="$(production_profiles_host_manifest)" || return 1
  provisioner_manifest="$(production_profiles_container_manifest "$provisioner")" || {
    echo '无法读取 subsystem-provisioner 容器内的生产子系统清单集合' >&2
    return 1
  }
  api_manifest="$(production_profiles_container_manifest "$platform_api")" || {
    echo '无法读取 platform-api 容器内的生产子系统清单集合' >&2
    return 1
  }
  host_digest="$(printf '%s' "$host_manifest" | sha256sum | awk '{print $1}')"
  provisioner_digest="$(printf '%s' "$provisioner_manifest" | sha256sum | awk '{print $1}')"
  api_digest="$(printf '%s' "$api_manifest" | sha256sum | awk '{print $1}')"
  if [[ "$provisioner_manifest" != "$host_manifest" || "$api_manifest" != "$host_manifest" ]]; then
    echo '生产子系统清单集合不一致；拒绝开放平台接入操作' >&2
    echo "  subsystems.d: host=${host_digest} agent=${provisioner_digest} api=${api_digest}" >&2
    return 1
  fi
  echo "platform-api、subsystem-provisioner 与宿主机的 subsystems.d 集合摘要一致：${host_digest}"
}

verify_subsystem_provisioner_config_consistency() {
  local container="$1" host_release_hash host_compose_hash container_hashes
  local container_release_hash container_compose_hash health

  host_release_hash="$(sha256sum "$release_file" | awk '{print $1}')" || return 1
  host_compose_hash="$(sha256sum "$compose_file" | awk '{print $1}')" || return 1
  container_hashes="$(docker exec "$container" sh -ec '
    : "${SUBSYSTEM_PRODUCTION_RELEASE_ENV_PATH:?missing release env path}"
    : "${SUBSYSTEM_PRODUCTION_COMPOSE_FILE:?missing compose file path}"
    sha256sum "$SUBSYSTEM_PRODUCTION_RELEASE_ENV_PATH" "$SUBSYSTEM_PRODUCTION_COMPOSE_FILE"
  ')" || {
    echo '无法读取 subsystem-provisioner 容器内的发布配置摘要' >&2
    return 1
  }
  container_release_hash="$(awk 'NR == 1 {print $1}' <<<"$container_hashes")"
  container_compose_hash="$(awk 'NR == 2 {print $1}' <<<"$container_hashes")"
  if [[ "$container_release_hash" != "$host_release_hash" || "$container_compose_hash" != "$host_compose_hash" ]]; then
    echo 'subsystem-provisioner 配置视图与宿主机不一致；拒绝继续发布' >&2
    echo "  .release.env: host=${host_release_hash} agent=${container_release_hash:-missing}" >&2
    echo "  docker-compose.yml: host=${host_compose_hash} agent=${container_compose_hash:-missing}" >&2
    return 1
  fi
  health="$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' "$container" 2>/dev/null || true)"
  [[ "$health" == healthy ]] || {
    echo "subsystem-provisioner 重建后健康状态异常：${health:-missing}" >&2
    return 1
  }
  echo 'subsystem-provisioner 已绑定当前 .release.env 与 docker-compose.yml，摘要一致'
}

refresh_subsystem_provisioner_config() {
  local container
  # 单侧重载只适用于 .release.env 指针变化；资产被替换后必须走成对重载，
  # 否则只重建 Agent 会让 API 继续持有旧清单集合并触发 manifest drift。
  require_control_plane_reload_clearance || return 1
  command -v sha256sum >/dev/null || {
    echo '缺少命令：sha256sum，无法验证 Agent 配置一致性' >&2
    return 1
  }

  container="$(docker ps -q \
    --filter "label=com.docker.compose.project=${COMPOSE_PROJECT_NAME:-basic-platform-production}" \
    --filter 'label=com.docker.compose.service=subsystem-provisioner' 2>/dev/null | head -n 1 || true)"
  if [[ -z "$container" ]]; then
    container="$(docker ps -q --filter 'name=uip-subsystem-provisioner' 2>/dev/null | head -n 1 || true)"
  fi
  [[ -n "$container" ]] || return 0

  echo '重建 subsystem-provisioner，使原子替换后的发布/编排配置重新绑定'
  compose run --rm --no-deps subsystem-provisioner-socket-init || return 1
  compose up -d --force-recreate --wait --wait-timeout 60 --no-deps subsystem-provisioner || return 1
  container="$(compose ps -q subsystem-provisioner 2>/dev/null || true)"
  [[ -n "$container" ]] || {
    echo 'subsystem-provisioner 重建后未找到运行容器' >&2
    return 1
  }
  verify_subsystem_provisioner_config_consistency "$container"
}

# Recreate both processes that cache production manifests. platform-api is
# stopped first so it cannot submit a checksum from the old profile set while
# the provisioner is already serving the new set. The caller-held deployment
# lock prevents a concurrent Agent operation from crossing this reload window.
refresh_subsystem_control_plane_config() {
  local provisioner platform_api health
  command -v sha256sum >/dev/null || {
    echo '缺少命令：sha256sum，无法验证控制面配置一致性' >&2
    return 1
  }
  production_profiles_host_manifest >/dev/null || return 1
  compose config --quiet || {
    echo 'Compose 配置校验失败；拒绝重载子系统控制面' >&2
    return 1
  }

  provisioner="$(subsystem_control_plane_container subsystem-provisioner)"
  platform_api="$(subsystem_control_plane_container platform-api)"
  if [[ -z "$provisioner" && -z "$platform_api" ]]; then
    echo 'platform-api 与 subsystem-provisioner 均未运行；部署资产静态校验通过，无需重载容器'
    return 0
  fi
  if [[ -z "$provisioner" || -z "$platform_api" ]]; then
    echo '检测到子系统控制面只有一个服务在运行；本次重载将修复并成对重建两个服务'
    echo "  platform-api=${platform_api:-missing} subsystem-provisioner=${provisioner:-missing}"
  fi

  if [[ -n "$platform_api" ]]; then
    echo '暂停 platform-api，防止清单切换窗口接受旧摘要的接入请求'
    compose stop --timeout 60 platform-api || return 1
  fi
  echo '成对重载子系统控制面：先重建 Agent，再重建 platform-api'
  compose run --rm --no-deps subsystem-provisioner-socket-init || return 1
  compose up -d --force-recreate --wait --wait-timeout 60 --no-deps subsystem-provisioner || return 1
  provisioner="$(compose ps -q subsystem-provisioner 2>/dev/null || true)"
  [[ -n "$provisioner" ]] || {
    echo 'subsystem-provisioner 重建后未找到运行容器；platform-api 保持停止以防止错误接入' >&2
    return 1
  }
  verify_subsystem_provisioner_config_consistency "$provisioner" || return 1

  compose up -d --force-recreate --wait --wait-timeout 120 --no-deps platform-api || return 1
  platform_api="$(compose ps -q platform-api 2>/dev/null || true)"
  [[ -n "$platform_api" ]] || {
    echo 'platform-api 重建后未找到运行容器' >&2
    return 1
  }
  health="$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' "$platform_api" 2>/dev/null || true)"
  [[ "$health" == healthy ]] || {
    echo "platform-api 重建后健康状态异常：${health:-missing}" >&2
    return 1
  }
  docker exec "$platform_api" test -S /run/basic-platform-provisioner/provisioner.sock || {
    echo 'platform-api 无法访问 subsystem-provisioner Unix Socket' >&2
    return 1
  }
  verify_subsystem_control_plane_profiles "$provisioner" "$platform_api"
}
