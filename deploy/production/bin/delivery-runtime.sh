#!/usr/bin/env bash
# 由 deploy.sh 加载：会话记录不能替代当前证据。导入探针只检查；发布探针还使用现有仅限 root 的受控验证维护命令，
# 不能从 --status 调用。
delivery_runtime_helper="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)/delivery-runtime.py"

delivery_runtime_error() { printf '交付恢复核验失败：%s\n' "$*" >&2; return 1; }

delivery_import_facts() {
  [[ -f "$delivery_runtime_helper" && ! -L "$delivery_runtime_helper" ]] || {
    delivery_runtime_error '缺少安全的运行证明工具'; return 1;
  }
  python3 "$delivery_runtime_helper" --target "$deploy_dir" --component "$1" \
    --archive "$2" --registry "${registry_address:-127.0.0.1:5000}"
}

delivery_import_matches() {
  delivery_import_facts "${1:?component required}" "${2:?archive required}" >/dev/null
}

delivery_release_key_matches() {
  local key="$1" expected="$2" value
  [[ -f "$release_file" && ! -L "$release_file" && -n "$expected" ]] || return 1
  value="$(awk -F= -v key="$key" '
    $1 == key {count++; value=substr($0,index($0,"=")+1)}
    END {if (count != 1) exit 1; print value}
  ' "$release_file")" || return 1
  [[ "$value" == "$expected" ]] || delivery_runtime_error "$key 与本次包目标不一致"
}

# 与 deploy.sh 的 Compose 包装器不同，此处不得创建缺失网络；public_transport_prepare 仅验证并导出现有传输配置。
delivery_readonly_compose() {
  local -a delivery_args=(--project-directory "$deploy_dir" --file "$compose_file")
  public_transport_compose_args delivery_args || return 1
  docker compose "${delivery_args[@]}" --env-file "$runtime_file" --env-file "$release_file" "$@"
}

delivery_staged_matches() {
  local component="$1" facts="$2" image key build_id
  image="$(jq -er '.image_ref' <<< "$facts")" || return 1
  subsystem_metadata "$component" || return 1
  delivery_release_key_matches "$subsystem_key" "$image" || return 1
  if [[ "$component" == data-analysis ]]; then
    for key in DATA_ANALYSIS_DASHBOARD_API_IMAGE DATA_ANALYSIS_AGGREGATION_WORKER_IMAGE DATA_ANALYSIS_ALERT_WORKER_IMAGE DATA_ANALYSIS_MIGRATE_IMAGE; do
      image="$(jq -er --arg key "$key" '.release_bindings[$key]' <<< "$facts")" || return 1
      delivery_release_key_matches "$key" "$image" || return 1
    done
  elif [[ "$component" == customer-opportunity ]]; then
    build_id="$(customer_presale_build_id "$image")" || return 1
    delivery_release_key_matches CUSTOMER_PRESALE_TEMPORAL_WORKER_BUILD_ID "$build_id" || return 1
  fi
}

delivery_compose_image_matches() {
  local resolved="$1" service="$2" expected="$3"
  jq -e --arg service "$service" --arg expected "$expected" \
    '.services[$service].image == $expected' <<< "$resolved" >/dev/null ||
    delivery_runtime_error "$service 的实际审核编排未绑定本次目标镜像"
}

delivery_release_matches() (
  local component="${1:?component required}" archive="${2:?archive required}" common_archive="${3:-}"
  local facts common_facts image gateway proxy resolved selected service expected key provisioner api
  require_initialized || return 1
  [[ ! -e "$deploy_dir/runtime/.assets-install-transaction" && ! -L "$deploy_dir/runtime/.assets-install-transaction" &&
     ! -e "$deploy_dir/runtime/.control-plane-reload-required" && ! -L "$deploy_dir/runtime/.control-plane-reload-required" ]] || return 1
  facts="$(delivery_import_facts "$component" "$archive")" || return 1
  public_transport_prepare "$deploy_dir" "$runtime_file" || return 1
  # 现有验证函数的动态分发改用原生只读包装器，同时保留完整健康、镜像和 READY 门禁。
  compose() { delivery_readonly_compose "$@"; }
  resolved="$(compose config --format json)" || return 1
  image="$(jq -er '.image_ref' <<< "$facts")" || return 1
  case "$component" in
    platform)
      [[ -n "$common_archive" ]] || { delivery_runtime_error '平台核验必须显式绑定本次 common 包'; return 1; }
      common_facts="$(delivery_import_facts common "$common_archive")" || return 1
      gateway="$(jq -er '.file_gateway_image_ref' <<< "$common_facts")" || return 1
      proxy="$(jq -r '.docker_socket_proxy_image_ref // ""' <<< "$common_facts")" || return 1
      delivery_release_key_matches PLATFORM_IMAGE "$image" || return 1
      delivery_release_key_matches FILE_GATEWAY_IMAGE "$gateway" || return 1
      for service in platform-api platform-worker subsystem-provisioner; do
        delivery_compose_image_matches "$resolved" "$service" "$image" || return 1
      done
      delivery_compose_image_matches "$resolved" file-gateway "$gateway" || return 1
      if [[ -n "$proxy" ]]; then
        delivery_release_key_matches DOCKER_SOCKET_PROXY_IMAGE "$proxy" || return 1
        delivery_compose_image_matches "$resolved" docker-socket-proxy "$proxy" || return 1
      fi
      verify_current_services platform-api platform-worker subsystem-provisioner file-gateway docker-socket-proxy \
        platform-mysql keycloak-db contract-mysql file-gateway-mysql keycloak temporal || return 1
      provisioner="$(compose ps -q subsystem-provisioner)" || return 1
      api="$(compose ps -q platform-api)" || return 1
      [[ -n "$provisioner" && -n "$api" ]] || return 1
      verify_subsystem_provisioner_config_consistency "$provisioner" || return 1
      verify_subsystem_control_plane_profiles "$provisioner" "$api" || return 1
      ;;
    frontend)
      delivery_release_key_matches FRONTEND_IMAGE "$image" || return 1
      delivery_compose_image_matches "$resolved" frontend "$image" || return 1
      verify_current_services frontend || return 1
      ;;
    customer-opportunity|customer-portal|contract|project|settlement|data-analysis)
      delivery_staged_matches "$component" "$facts" || return 1
      selected="$(scope_runtime_services "$component")" || return 1
      [[ -n "$selected" ]] || return 1
      while IFS= read -r service; do
        expected="$image"
        if [[ "$component" == data-analysis ]]; then
          case "$service" in
            data-analysis-api) key=DATA_ANALYSIS_DASHBOARD_API_IMAGE ;;
            data-analysis-aggregation-worker) key=DATA_ANALYSIS_AGGREGATION_WORKER_IMAGE ;;
            data-analysis-alert-worker) key=DATA_ANALYSIS_ALERT_WORKER_IMAGE ;;
            data-analysis-metabase) key='' ; expected=metabase/metabase:v0.53.7 ;;
            *) delivery_runtime_error '数据看板 profile 含未知长期组件'; return 1 ;;
          esac
          [[ -z "$key" ]] || expected="$(jq -er --arg key "$key" '.release_bindings[$key]' <<< "$facts")" || return 1
        fi
        delivery_compose_image_matches "$resolved" "$service" "$expected" || return 1
      done <<< "$selected"
      if [[ "$component" == data-analysis ]]; then
        expected="$(jq -er '.release_bindings.DATA_ANALYSIS_MIGRATE_IMAGE' <<< "$facts")" || return 1
        delivery_compose_image_matches "$resolved" data-analysis-migrate "$expected" || return 1
      elif [[ "$component" == customer-opportunity ]]; then
        expected="$(customer_presale_build_id "$image")" || return 1
        jq -e --arg expected "$expected" '.services["customer-presale-worker"].environment.TEMPORAL_WORKER_BUILD_ID == $expected' \
          <<< "$resolved" >/dev/null || return 1
      fi
      verify_selected_runtime "$component" || return 1
      ;;
    *) delivery_runtime_error '该组件没有独立发行运行阶段'; return 1 ;;
  esac
)

delivery_candidate_matches() {
  local component="$1" facts="$2" candidate image version
  subsystem_metadata "$component" || return 1
  image="$(jq -er '.image_ref' <<< "$facts")" || return 1
  version="$(jq -er '.version' <<< "$facts")" || return 1
  candidate="$(docker container inspect "$subsystem_candidate" --format '{{json .}}')" || return 1
  jq -e --arg app "$subsystem_app_code" --arg service "$subsystem_service" --arg image "$image" --arg version "$version" \
    --argjson identities "$(jq -ce --arg image "$image" '.acceptable_ids[$image]' <<< "$facts")" '
      .Config.Image == $image and (.Image as $native | $identities | index($native) != null) and
      .Config.Labels["com.basic-platform.discovery"] == "v1" and
      .Config.Labels["com.basic-platform.application_code"] == $app and
      .Config.Labels["com.basic-platform.environment"] == "prod" and
      .Config.Labels["com.basic-platform.service_name"] == $service and
      .Config.Labels["com.basic-platform.service_role"] == "business" and
      .Config.Labels["com.basic-platform.version"] == $version
    ' <<< "$candidate" >/dev/null || delivery_runtime_error '既有候选的标签或镜像不属于本次交付，拒绝替换'
}

delivery_prepare_resume() (
  local component="${1:?component required}" archive="${2:?archive required}" facts phase
  require_initialized || return 1
  [[ ! -e "$deploy_dir/runtime/.assets-install-transaction" && ! -L "$deploy_dir/runtime/.assets-install-transaction" &&
     ! -e "$deploy_dir/runtime/.control-plane-reload-required" && ! -L "$deploy_dir/runtime/.control-plane-reload-required" ]] || return 1
  facts="$(delivery_import_facts "$component" "$archive")" || return 1
  subsystem_metadata "$component" || return 1
  public_transport_prepare "$deploy_dir" "$runtime_file" || return 1
  compose() { delivery_readonly_compose "$@"; }
  if docker container inspect "$subsystem_candidate" >/dev/null 2>&1; then
    delivery_candidate_matches "$component" "$facts" || return 1
    delivery_staged_matches "$component" "$facts" || return 1
  fi
  phase="$(subsystem_phase "$component")" || return 1
  case "$phase" in
    VERIFIED)
      delivery_release_matches "$component" "$archive" || return 1
      printf '%s/prod：VERIFIED，本次目标已通过完整运行核验。\n' "$subsystem_app_code"
      ;;
    READY_TO_CONTINUE)
      delivery_release_matches "$component" "$archive" || return 1
      printf '%s/prod：READY_TO_CONTINUE，等待操作员明确执行 continue；未自动删除候选。\n' "$subsystem_app_code"
      ;;
    WAITING_FOR_PLATFORM_ADOPTION|PROVISIONING_OR_FAILED)
      printf '%s/prod：%s，等待平台受控采用或重试；现有候选保留。\n' "$subsystem_app_code" "$phase"
      ;;
    IMPORTED)
      # 只有确认不存在的候选才可准备；不能为了让会话看似最新而删除未知候选或其他发布。
      if docker container inspect "$subsystem_candidate" >/dev/null 2>&1; then return 1; fi
      prepare_subsystem "$component" || return 1
      delivery_candidate_matches "$component" "$facts" || return 1
      delivery_staged_matches "$component" "$facts" || return 1
      printf '%s/prod：WAITING_FOR_PLATFORM_ADOPTION，本次目标候选已准备，等待平台受控采用。\n' "$subsystem_app_code"
      ;;
    *) delivery_runtime_error "$component 当前阶段 $phase 不允许恢复 prepare"; return 1 ;;
  esac
)
