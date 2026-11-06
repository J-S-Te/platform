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

# 平台发布证明的 Keycloak HTTP 门禁。容器状态/healthcheck 只能证明进程与端口还在，
# 2026-11-05 事故中 keycloak-db 重建后 Keycloak 仍接受 TCP 连接、却无法处理任何请求
# （Hikari 连接陈旧 + Infinispan clientscopes 缓存卡死），交付证明因此漏检。
#
# 探针必须直连 Keycloak 自己发布到宿主机上的端口，绝不能改用公开 issuer
# （PUBLIC_KEYCLOAK_ISSUER = PUBLIC_SSO_ORIGIN/realms/<realm>）：
# 公开入口由 frontend nginx 容器提供（frontend/nginx/gateway-*.conf.template 把 /realms 反代到
# keycloak，这些端口只由前端的 frontend 服务发布），而阶段 B（平台发布证明）先于阶段 C（发布前端）
# 执行。全新安装时公开入口还没有任何监听，走公开地址就会把“前端尚未部署”误判成“Keycloak 卡死”，
# 阶段 B 因此永久失败、--resume 只会反复重试；升级场景前端早已运行，才看不出这个问题。
# keycloak 一旦启动就同时具备 /health/ready 与已发布的宿主端口，所以回环探针在全新安装和
# 升级两种场景下都成立，也不依赖任何公开入口是否已经就绪。

# 按 docker compose 的 .env 语义归一化取值：去首尾空白、剥一层成对引号、去行内 `#` 注释。
# env_get 只做裸值读取；直接把它的结果拼进 URL，会把 `"0.0.0.0"`、`0.0.0.0 ` 或
# `0.0.0.0 # 备注` 变成 http://"0.0.0.0":18090/... 这类 curl 直接拒绝的地址，进而把一份
# 健康部署判成“Hikari 卡死”。compose 自己会正确归一化这几种写法（见 docker compose config），
# 所以门禁也必须以同样的语义解释 .env。
keycloak_trim() {
  local value="$1"
  value="${value#"${value%%[![:space:]]*}"}"
  value="${value%"${value##*[![:space:]]}"}"
  printf '%s' "$value"
}

keycloak_dotenv_value() {
  local value
  value="$(keycloak_trim "${1-}")"
  case "$value" in
    \"*)
      # 取到闭合双引号为止：`"0.0.0.0" # 备注` 与 `"0.0.0.0"` 都归一化为 0.0.0.0。
      value="${value#\"}"
      value="$(keycloak_trim "${value%\"*}")"
      ;;
    \'*)
      value="${value#\'}"
      value="$(keycloak_trim "${value%\'*}")"
      ;;
    \#*) value='' ;;
    *)
      # 未加引号：compose 视“空白 + #”之后为行内注释。
      case "$value" in
        *[[:space:]]'#'*) value="${value%%[[:space:]]'#'*}" ;;
      esac
      value="$(keycloak_trim "$value")"
      ;;
  esac
  printf '%s' "$value"
}

# 宿主地址的客户端形态：通配监听（空/0.0.0.0/::）换成回环，IPv6 字面量加方括号。
keycloak_host_literal() {
  local host="$1"
  case "$host" in
    ''|0.0.0.0|::|'[::]') host=127.0.0.1 ;;
  esac
  case "$host" in
    *:*)
      case "$host" in
        \[*\]) ;;
        *) host="[$host]" ;;
      esac
      ;;
  esac
  printf '%s' "$host"
}

# 从已解析的 compose 模型读取 keycloak 服务容器端口 8080 的宿主发布映射（host_ip<TAB>published）。
# compose 已完成 .env 归一化与端口展开，因此这是“实际发布出去的端点”的唯一权威来源；
# 读不到模型或没有该映射时返回非零，由调用方回退到归一化后的 .env 取值。
keycloak_published_endpoint() {
  local resolved="$1"
  [[ -n "$resolved" ]] || return 1
  python3 - "$resolved" <<'PY'
import json, sys
try:
    model = json.loads(sys.argv[1])
except ValueError:
    raise SystemExit(1)
if not isinstance(model, dict):
    raise SystemExit(1)
services = model.get("services")
service = services.get("keycloak") if isinstance(services, dict) else None
ports = service.get("ports") if isinstance(service, dict) else None
if not isinstance(ports, list):
    raise SystemExit(1)
for entry in ports:
    if not isinstance(entry, dict):
        continue
    try:
        target = int(entry.get("target"))
    except (TypeError, ValueError):
        continue
    if target != 8080:
        continue
    published = entry.get("published")
    if published is None:
        continue
    published = str(published).strip()
    if not published.isdigit():
        continue
    host_ip = entry.get("host_ip")
    host_ip = "" if host_ip is None else str(host_ip).strip()
    sys.stdout.write("%s\t%s\n" % (host_ip, published))
    raise SystemExit(0)
raise SystemExit(1)
PY
}

# 拆分“响应体 + --write-out 追加的 3 位状态码”，供调用方区分 404（realm 未创建）与其他失败。
keycloak_probe_body=''
keycloak_probe_code=''
keycloak_split_probe_output() {
  local raw="$1" last
  keycloak_probe_body="$raw"
  keycloak_probe_code=''
  [[ -n "$raw" ]] || return 0
  if [[ "$raw" == *$'\n'* ]]; then
    last="${raw##*$'\n'}"
    [[ "$last" =~ ^[0-9]{3}$ ]] || return 0
    keycloak_probe_code="$last"
    keycloak_probe_body="${raw%$'\n'*}"
  elif [[ "$raw" =~ ^[0-9]{3}$ ]]; then
    keycloak_probe_code="$raw"
    keycloak_probe_body=''
  fi
}

# “卡死”诊断：连接/超时/非 200 都走这里，措辞与 2026-11-05 事故一致。
keycloak_http_wedged_diagnostic() {
  local url="$1" detail="${2:-}"
  delivery_runtime_error "Keycloak 未提供可用的 OIDC 发现文档：$url${detail:+
$detail}
最可能的原因：Keycloak 仍接受该端口的 TCP 连接，但请求处理已被卡死（2026-11-05 事故同型：
keycloak-db 重建后连接池持有陈旧的 Hikari 连接，或 Infinispan clientscopes 缓存死锁）。
请检查 keycloak 与 keycloak-db 日志、确认数据库可达，必要时重启 keycloak 后重新执行本次交付核验。
本门禁直连 Keycloak 自己发布的宿主端口，不经过公开入口；公开入口是否可用不是本判据。"
}

require_keycloak_http_ready() {
  local resolved="${1:-}" mapping address port realm
  local master_url tenant_url document raw code attempt
  local -r realm_retry_attempts=6 realm_retry_seconds=5
  [[ -f "$runtime_file" && -r "$runtime_file" && ! -L "$runtime_file" ]] || {
    delivery_runtime_error "缺少可读的运行配置 $runtime_file，无法确定 Keycloak 探针地址，拒绝在缺少 HTTP 证据时通过发布核验"
    return 1
  }
  # 端点优先级：已解析 compose 模型的发布映射（compose 已按 .env 语义归一化）> 归一化后的 .env。
  if [[ -z "$resolved" ]]; then resolved="$(compose config --format json 2>/dev/null || true)"; fi
  mapping="$(keycloak_published_endpoint "$resolved" 2>/dev/null || true)"
  if [[ -n "$mapping" ]]; then
    address="${mapping%%$'\t'*}"
    port="${mapping##*$'\t'}"
    [[ "$port" =~ ^[0-9]+$ ]] && ((port >= 1 && port <= 65535)) || {
      delivery_runtime_error "当前审核编排中 keycloak 发布的宿主端口不合法：$port"
      return 1
    }
    address="$(keycloak_host_literal "$address")"
  else
    # 运行配置回退路径：同样的 .env 归一化，避免引号/行内注释/通配地址污染 URL。
    address="$(keycloak_host_literal "$(keycloak_dotenv_value "$(env_get "$runtime_file" KEYCLOAK_BIND_ADDRESS)")")"
    port="$(keycloak_dotenv_value "$(env_get "$runtime_file" KEYCLOAK_HTTP_PORT)")"
    port="${port:-18090}"
    if [[ ! "$port" =~ ^[0-9]+$ ]] || ((port < 1 || port > 65535)); then
      delivery_runtime_error "运行配置的 KEYCLOAK_HTTP_PORT 不是合法端口：$port"
      return 1
    fi
  fi
  realm="$(keycloak_dotenv_value "$(env_get "$runtime_file" KEYCLOAK_REALM)")"
  realm="${realm:-basic-platform}"
  if [[ ! "$realm" =~ ^[A-Za-z0-9][A-Za-z0-9._-]*$ ]]; then
    delivery_runtime_error "运行配置的 KEYCLOAK_REALM 不是合法 realm 名：$realm"
    return 1
  fi
  master_url="http://$address:$port/realms/master/.well-known/openid-configuration"
  tenant_url="http://$address:$port/realms/$realm/.well-known/openid-configuration"

  # (a) 硬性要求：Keycloak 真的在提供 HTTP 服务。master realm 由 Keycloak 自己创建，
  # 因此 200 + 含 issuer 的发现文档即可证明“进程还能处理请求”，卡死的 Keycloak 必然过不了。
  raw="$(curl --fail --silent --show-error --connect-timeout 5 --max-time 20 \
    --write-out '\n%{http_code}' "$master_url")" || {
    keycloak_http_wedged_diagnostic "$master_url" 'curl 未能完成请求（连接被拒绝、超时或返回非 2xx/3xx）'
    return 1
  }
  keycloak_split_probe_output "$raw"
  document="$keycloak_probe_body"
  code="$keycloak_probe_code"
  if [[ -n "$code" && "$code" != 200 ]]; then
    keycloak_http_wedged_diagnostic "$master_url" "master realm 的发现文档返回 HTTP $code（期望 200）"
    return 1
  fi
  jq -er '.issuer' <<< "$document" >/dev/null || {
    delivery_runtime_error "Keycloak OIDC 发现文档缺少 issuer 字段（$master_url），响应不是可用的 OIDC 元数据"
    return 1
  }

  # (b) 租户 realm：由 platform-worker 启动期的 bootstrap（EnsureKeycloakBroker）创建，
  # 而 platform-worker 的 healthcheck 只是 PID1 进程探针，所以“平台健康”必然早于 realm 创建；
  # KEYCLOAK_MANAGEMENT_ENABLED=false 或 bootstrap 慢/失败时 realm 会一直 404。
  # 404 => realm 未创建/名称不一致：有界重试后只警告不阻断（HTTP 服务已由 (a) 证明）。
  # 其他失败（拒绝连接/超时/5xx）= 卡死形态，保持与 (a) 相同的 fail-closed 诊断。
  for ((attempt = 1; attempt <= realm_retry_attempts; attempt++)); do
    raw="$(curl --silent --show-error --connect-timeout 5 --max-time 20 \
      --write-out '\n%{http_code}' "$tenant_url")" || {
      keycloak_http_wedged_diagnostic "$tenant_url" 'curl 未能完成请求（连接被拒绝或超时）'
      return 1
    }
    keycloak_split_probe_output "$raw"
    document="$keycloak_probe_body"
    code="$keycloak_probe_code"
    if [[ "$code" == 404 ]]; then
      ((attempt < realm_retry_attempts)) || break
      sleep "$realm_retry_seconds"
      continue
    fi
    if [[ -n "$code" && "$code" != 200 ]]; then
      keycloak_http_wedged_diagnostic "$tenant_url" \
        "realm $realm 的发现文档返回 HTTP $code（既不是 200 也不是 404），说明 Keycloak 可连接但没有正常提供该 realm 的 OIDC 元数据"
      return 1
    fi
    jq -er '.issuer' <<< "$document" >/dev/null || {
      delivery_runtime_error "Keycloak OIDC 发现文档缺少 issuer 字段（$tenant_url），响应不是可用的 OIDC 元数据"
      return 1
    }
    printf 'Keycloak OIDC 发现文档可用（issuer 已声明）：%s\n' "$tenant_url"
    return 0
  done
  printf 'Keycloak OIDC 发现文档可用（issuer 已声明）：%s\n' "$master_url"
  printf '警告：Keycloak 已在正常提供 HTTP 服务（master realm 发现文档 200 且含 issuer），但 realm %s 尚不存在（HTTP 404：realm 未创建或名称不一致）。\n' "$realm" >&2
  printf '      该租户 realm 由 platform-worker 启动期的 Keycloak bootstrap（EnsureKeycloakBroker）创建，\n' >&2
  printf '      所以 platform-worker 的 healthcheck 可以先于 realm 创建变健康；KEYCLOAK_MANAGEMENT_ENABLED=false\n' >&2
  printf '      时该 bootstrap 不会执行，realm 需要由运营流程显式创建。\n' >&2
  printf '      本次发布证明不因此失败（Keycloak 的 HTTP 服务已单独证明）；请在 platform-worker 启动/健康后\n' >&2
  printf '      重新核验 realm %s，并确认 KEYCLOAK_MANAGEMENT_ENABLED 与 KEYCLOAK_REALM 配置是否符合预期。\n' "$realm" >&2
  return 0
}

delivery_release_matches() (
  local component="${1:?component required}" archive="${2:?archive required}" common_archive="${3:-}"
  local facts common_facts image gateway proxy resolved selected service expected key provisioner api
  local -a platform_services=()
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
      # 共享 Temporal 只在确有消费子系统启用时才由 deploy-service.sh 启动（temporal_consumers_enabled）；
      # 未启用时它根本不存在，仍硬要求它健康会让“仅平台 / 仅结算 / 仅数据看板”安装无法通过
      # 自己的发布证明。其余服务保持无条件必需。
      platform_services=(platform-api platform-worker subsystem-provisioner file-gateway docker-socket-proxy
        platform-mysql keycloak-db contract-mysql file-gateway-mysql keycloak)
      if temporal_consumers_enabled; then platform_services+=(temporal); fi
      verify_current_services "${platform_services[@]}" || return 1
      # 健康探针只证明 Keycloak 端口还在；发布证明还必须证明它真的在提供 OIDC 元数据。
      # 这里把上面已解析的编排模型传进去：compose 已完成 .env 归一化与端口展开，是本门禁
      # 判定“实际发布出去的端点”的权威来源（也不必再执行一次只读 compose 解析）。
      require_keycloak_http_ready "$resolved" || return 1
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
