#!/usr/bin/env bash
set -Eeuo pipefail

# 数据库支撑的传输 Saga 的可重入宿主机驱动器，修改 PUBLIC_HTTPS_ENABLED 后执行。
# 降级会在 DISABLING_HTTPS 排空期间主动返回；到报告的截止时间后再次执行相同命令，移除 443 及证书挂载。
script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
deploy_dir="$(cd -- "$script_dir/.." && pwd)"
runtime_file="${BASIC_PLATFORM_RUNTIME_ENV_FILE:-$deploy_dir/.env}"
release_file="${BASIC_PLATFORM_RELEASE_ENV_FILE:-$deploy_dir/.release.env}"
compose_file="$deploy_dir/docker-compose.yml"

install -d -m 700 "$deploy_dir/runtime"
exec 9>"$deploy_dir/runtime/.deploy.lock"
flock -w 900 9 || { echo "等待其他发布任务超时" >&2; exit 1; }

# shellcheck source=public-transport.sh
source "$script_dir/public-transport.sh"

base_compose() {
  docker compose --project-directory "$deploy_dir" --file "$compose_file" \
    --env-file "$runtime_file" --env-file "$release_file" "$@"
}

coordinator() {
  base_compose run --rm --no-deps platform-api ./public-transport-coordinator "$@"
}

running_enabled_services() {
  local enabled running service
  # 使用可移植的仅服务查询。部分受支持 Compose 版本拒绝环境解析标志，
  # 旧版本则无法验证 no-interpolate 下保留默认表达式字面冒号的绑定挂载。
  enabled="$(base_compose config --services)" || return 1
  running="$(base_compose ps --services --status running)" || return 1
  while IFS= read -r service; do
    [[ -n "$service" ]] || continue
    if grep -Fxq -- "$service" <<<"$enabled"; then printf '%s\n' "$service"; fi
  done <<<"$running"
}

persist_transport_state() {
  local state="$1" temporary mode
  mode="$(stat -c '%a' "$runtime_file")"
  temporary="$(mktemp "${runtime_file}.transport.XXXXXX")"
  awk -F= '$1 != "PUBLIC_TRANSPORT_STATE" {print}' "$runtime_file" >"$temporary"
  if [[ -n "$state" ]]; then
    printf 'PUBLIC_TRANSPORT_STATE=%s\n' "$state" >>"$temporary"
  fi
  chmod "$mode" "$temporary"
  mv -f -- "$temporary" "$runtime_file"
}

current_container_mode() {
  local container value
  container="$(base_compose ps -q frontend 2>/dev/null || true)"
  [[ -n "$container" ]] || { printf 'HTTP'; return; }
  value="$(docker inspect --format '{{range .Config.Env}}{{println .}}{{end}}' "$container" 2>/dev/null \
    | awk -F= '$1 == "PUBLIC_HTTPS_ENABLED" {print $2; exit}')"
  [[ "$value" == "true" ]] && printf 'HTTPS' || printf 'HTTP'
}

origin_for_mode() {
  local mode="$1" host="$2" http_port="$3" https_port="$4" port
  if [[ "$mode" == "HTTPS" ]]; then
    port="$https_port"
    public_transport_origin https "$host" "$port"
  else
    port="$http_port"
    public_transport_origin http "$host" "$port"
  fi
}

# 先进行目标预检，HTTP 模式不检查证书路径。
unset PUBLIC_TRANSPORT_STATE
public_transport_prepare "$deploy_dir" "$runtime_file"
configured_transition_state="$PUBLIC_TRANSPORT_STATE"
desired_mode=HTTP
[[ "$PUBLIC_HTTPS_ENABLED" == "true" ]] && desired_mode=HTTPS
target_platform_origin="$PUBLIC_PLATFORM_ORIGIN"
target_sso_origin="$PUBLIC_SSO_ORIGIN"

status_json="$(coordinator status 2>/dev/null || true)"
if [[ -z "$status_json" ]]; then
  current_mode="$(current_container_mode)"
  current_platform_origin="$(origin_for_mode "$current_mode" "$PUBLIC_PLATFORM_HOST" "$PUBLIC_HTTP_PORT" "$PUBLIC_HTTPS_PORT")"
  current_sso_origin="$(origin_for_mode "$current_mode" "$PUBLIC_SSO_HOST" "$PUBLIC_SSO_HTTP_PORT" "$PUBLIC_SSO_HTTPS_PORT")"
  status_json="$(coordinator init --mode "$current_mode" --platform-origin "$current_platform_origin" --sso-origin "$current_sso_origin")"
fi

state="$(jq -r '.State' <<<"$status_json")"
active_mode="$(jq -r '.Mode' <<<"$status_json")"
transition_id="$(jq -r '.TransitionID // empty' <<<"$status_json")"
active_platform_origin="$(jq -r '.PlatformOrigin' <<<"$status_json")"
active_sso_origin="$(jq -r '.SSOOrigin' <<<"$status_json")"

if [[ "$state" == "HTTP" || "$state" == "HTTPS" ]]; then
  if [[ "$active_mode" == "$desired_mode" ]]; then
    [[ "$active_platform_origin" == "$target_platform_origin" && "$active_sso_origin" == "$target_sso_origin" ]] || {
      echo "当前模式相同但公开域名/端口已变化；请使用独立主机名迁移流程，不能混入协议切换" >&2
      exit 1
    }
    persist_transport_state ""
    if [[ -n "$configured_transition_state" ]]; then
      unset PUBLIC_TRANSPORT_STATE
      public_transport_prepare "$deploy_dir" "$runtime_file"
      stable_command=(docker compose --project-directory "$deploy_dir" --file "$compose_file" --env-file "$runtime_file" --env-file "$release_file")
      public_transport_compose_args stable_command
      public_transport_install_certificates
      "${stable_command[@]}" up -d --no-deps --wait --wait-timeout 120 frontend
    fi
    echo "公开传输已经稳定：${state}"
    exit 0
  fi
  prepare_args=(prepare --target-mode "$desired_mode" --platform-origin "$target_platform_origin" --sso-origin "$target_sso_origin")
  if [[ "$desired_mode" == "HTTPS" ]]; then
    certificate_fingerprint="$(openssl x509 -in "$PUBLIC_TLS_CERTIFICATE_RESOLVED" -noout -fingerprint -sha256 | cut -d= -f2 | tr -d :)"
    certificate_not_after="$(date -u -d "$(openssl x509 -in "$PUBLIC_TLS_CERTIFICATE_RESOLVED" -noout -enddate | cut -d= -f2-)" +%Y-%m-%dT%H:%M:%SZ)"
    prepare_args+=(--certificate-fingerprint "$certificate_fingerprint" --certificate-not-after "$certificate_not_after")
  else
    drain_grace="$(public_transport_env_value "$runtime_file" PUBLIC_HTTPS_DRAIN_GRACE)"
    drain_grace="${drain_grace:-5m}"
    prepare_args+=(--drain-grace "$drain_grace")
  fi
  status_json="$(coordinator "${prepare_args[@]}")"
  state="$(jq -r '.State' <<<"$status_json")"
  transition_id="$(jq -r '.TransitionID' <<<"$status_json")"
fi

if [[ "$state" == "ENABLING_HTTPS" || "$state" == "DISABLING_HTTPS" ]]; then
  # 每次恢复都重新执行幂等外部准备，关闭数据库双回调事务与 Keycloak Admin API 更新之间的崩溃窗口。
  status_json="$(coordinator prepare --target-mode "$desired_mode" --platform-origin "$target_platform_origin" --sso-origin "$target_sso_origin")"
  transition_id="$(jq -r '.TransitionID' <<<"$status_json")"
  persist_transport_state "$state"
fi

# 根据持久化状态重新导出网关环境。降级时即使目标 PUBLIC_HTTPS_ENABLED 为 false，也仍须验证和挂载必需证书。
export PUBLIC_TRANSPORT_STATE="$state"
public_transport_prepare "$deploy_dir" "$runtime_file"
command=(docker compose --project-directory "$deploy_dir" --file "$compose_file" --env-file "$runtime_file" --env-file "$release_file")
public_transport_compose_args command
public_transport_install_certificates
if [[ "$state" == "ENABLING_HTTPS" ]]; then
  # 先启用 TLS，同时让使用旧 HTTP 配置的 API 和 Worker 继续运行。若数据库提交前启动目标配置 worker，
  # 其 HTTPS 门禁会拒绝仍为 HTTP 的 Environment 记录。
  "${command[@]}" up -d --no-deps --wait --wait-timeout 120 frontend
  "${command[@]}" exec -T frontend nginx -t
else
  # 降级服务在整个排空期必须签发非 Secure 的替换 Cookie，临时前端覆盖配置则保持 443 可用。数据库回调提交前，
  # 后台 worker 保持旧运行配置，否则启动对账会将 Keycloak 必需的双回调集合缩为仅源回调。
  drain_targets=(frontend)
  while IFS= read -r running_service; do
    case "$running_service" in
      platform-api|contract-api|customer-api|portal-api|project-api|settlement-api|data-analysis-api|keycloak)
        drain_targets+=("$running_service")
        ;;
    esac
  done <<<"$(running_enabled_services)"
  mapfile -t drain_targets < <(printf '%s\n' "${drain_targets[@]}" | awk '!seen[$0]++')
  "${command[@]}" up -d --no-deps --wait --wait-timeout 120 "${drain_targets[@]}"
fi

if [[ "$state" == "DISABLING_HTTPS" ]]; then
  active_json="$(coordinator status)"
  drain_until="$(jq -r '.DrainUntil // empty' <<<"$active_json")"
  now_epoch="$(date -u +%s)"
  drain_epoch="$(date -u -d "$drain_until" +%s)"
  if (( now_epoch < drain_epoch )); then
    echo "HTTPS 会话正在排空；443 保留到 ${drain_until}。截止后再次执行本命令完成 HTTP 切换。"
    exit 0
  fi
fi

coordinator commit --transition-id "$transition_id"
persist_transport_state ""

# 仅切换已运行且已启用服务，绝不启动尚未接管的子系统。
unset PUBLIC_TRANSPORT_STATE
public_transport_prepare "$deploy_dir" "$runtime_file"
final_command=(docker compose --project-directory "$deploy_dir" --file "$compose_file" --env-file "$runtime_file" --env-file "$release_file")
public_transport_compose_args final_command
public_transport_install_certificates
running_services="$(running_enabled_services)"
mapfile -t final_targets < <(printf '%s\n' "$running_services" | sed '/^$/d')
if ((${#final_targets[@]})); then
  "${final_command[@]}" up -d --no-deps --wait --wait-timeout 120 "${final_targets[@]}"
fi
echo "公开传输切换完成：${desired_mode}"
