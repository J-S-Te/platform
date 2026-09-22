#!/usr/bin/env bash
set -Eeuo pipefail

# Re-entrant host-side driver for the database-backed transport Saga. Run it
# after changing PUBLIC_HTTPS_ENABLED. A downgrade intentionally returns while
# DISABLING_HTTPS is draining; run the same command again after the reported
# deadline to remove 443 and its certificate mounts.
script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
deploy_dir="$(cd -- "$script_dir/.." && pwd)"
runtime_file="${BASIC_PLATFORM_RUNTIME_ENV_FILE:-$deploy_dir/.env}"
release_file="${BASIC_PLATFORM_RELEASE_ENV_FILE:-$deploy_dir/.release.env}"
compose_file="$deploy_dir/compose.yaml"

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
  local mode="$1" host="$2" port
  if [[ "$mode" == "HTTPS" ]]; then
    port="${PUBLIC_HTTPS_PORT:-443}"
    public_transport_origin https "$host" "$port"
  else
    port="${PUBLIC_HTTP_PORT:-80}"
    public_transport_origin http "$host" "$port"
  fi
}

# Target preflight is performed first. HTTP does not inspect certificate paths.
unset PUBLIC_TRANSPORT_STATE
public_transport_prepare "$deploy_dir" "$runtime_file" "$deploy_dir/compose.https.yaml" "$deploy_dir/compose.drain.yaml"
configured_transition_state="$PUBLIC_TRANSPORT_STATE"
desired_mode=HTTP
[[ "$PUBLIC_HTTPS_ENABLED" == "true" ]] && desired_mode=HTTPS
target_platform_origin="$PUBLIC_PLATFORM_ORIGIN"
target_sso_origin="$PUBLIC_SSO_ORIGIN"

status_json="$(coordinator status 2>/dev/null || true)"
if [[ -z "$status_json" ]]; then
  current_mode="$(current_container_mode)"
  current_platform_origin="$(origin_for_mode "$current_mode" "$PUBLIC_PLATFORM_HOST")"
  current_sso_origin="$(origin_for_mode "$current_mode" "$PUBLIC_SSO_HOST")"
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
      public_transport_prepare "$deploy_dir" "$runtime_file" "$deploy_dir/compose.https.yaml" "$deploy_dir/compose.drain.yaml"
      stable_command=(docker compose --project-directory "$deploy_dir" --file "$compose_file" --env-file "$runtime_file" --env-file "$release_file")
      public_transport_compose_args stable_command
      "${stable_command[@]}" up -d frontend
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
  # Re-run the idempotent external preparation on every resume. This closes
  # the crash window between the database dual-callback transaction and the
  # Keycloak Admin API update.
  status_json="$(coordinator prepare --target-mode "$desired_mode" --platform-origin "$target_platform_origin" --sso-origin "$target_sso_origin")"
  transition_id="$(jq -r '.TransitionID' <<<"$status_json")"
  persist_transport_state "$state"
fi

# Re-derive Compose overlays from the persisted state. During downgrade this
# validates and mounts the still-required certificate even though the target
# PUBLIC_HTTPS_ENABLED value is false.
export PUBLIC_TRANSPORT_STATE="$state"
public_transport_prepare "$deploy_dir" "$runtime_file" "$deploy_dir/compose.https.yaml" "$deploy_dir/compose.drain.yaml"
command=(docker compose --project-directory "$deploy_dir" --file "$compose_file" --env-file "$runtime_file" --env-file "$release_file")
public_transport_compose_args command
if [[ "$state" == "ENABLING_HTTPS" ]]; then
  # Bring up TLS first while the old HTTP-configured API/Worker keep running.
  # Starting target-configured workers before the database commit would make
  # their HTTPS transport gate reject the still-HTTP Environment records.
  "${command[@]}" up -d --wait frontend
  "${command[@]}" exec -T frontend nginx -t
else
  # Downgrade services must issue non-Secure replacement cookies throughout
  # the drain, while the temporary frontend overlay keeps 443 alive. Keep
  # background workers on the old runtime until the database callback commit;
  # otherwise their startup reconciliation would collapse Keycloak's required
  # dual callback set back to the source-only callback.
  drain_targets=(frontend)
  while IFS= read -r running_service; do
    case "$running_service" in
      platform-api|contract-api|customer-api|portal-api|project-api|settlement-api|dashboard-api|keycloak)
        drain_targets+=("$running_service")
        ;;
    esac
  done < <("${command[@]}" ps --services --status running)
  mapfile -t drain_targets < <(printf '%s\n' "${drain_targets[@]}" | awk '!seen[$0]++')
  "${command[@]}" up -d "${drain_targets[@]}"
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

# Final stable-mode recreation removes the temporary dual-protocol overlay.
unset PUBLIC_TRANSPORT_STATE
public_transport_prepare "$deploy_dir" "$runtime_file" "$deploy_dir/compose.https.yaml" "$deploy_dir/compose.drain.yaml"
final_command=(docker compose --project-directory "$deploy_dir" --file "$compose_file" --env-file "$runtime_file" --env-file "$release_file")
public_transport_compose_args final_command
"${final_command[@]}" up -d
echo "公开传输切换完成：${desired_mode}"
