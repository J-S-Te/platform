#!/usr/bin/env bash
# Deploy status/verification helpers. Sourced by deploy.sh.

status() {
  scope_report || return
  if [[ -f "$deploy_dir/runtime/first-failure.json" && ! -L "$deploy_dir/runtime/first-failure.json" ]]; then
    python3 - "$deploy_dir/runtime/first-failure.json" <<'PY' || printf '警告：首个失败记录无法解析\n' >&2
import json, sys
try:
    value = json.load(open(sys.argv[1], encoding="utf-8"))
except (OSError, ValueError):
    raise SystemExit(1)
if not isinstance(value, dict) or value.get("schema") != "uip.first-failure/v1":
    raise SystemExit(1)
print("首个发布失败：阶段=%s，原因=%s，时间=%s" % (value.get("stage", "未知"), value.get("reason", "未知"), value.get("at", "未知")))
PY
  fi
  if [[ -f "$deploy_dir/runtime/recovery-status.json" && ! -L "$deploy_dir/runtime/recovery-status.json" ]]; then
    python3 - "$deploy_dir/runtime/recovery-status.json" <<'PY' || printf '警告：最近恢复结果无法解析，请查看 %s\n' "$deploy_dir/runtime/recovery-status.json" >&2
import json, sys
try:
    value = json.load(open(sys.argv[1], encoding="utf-8"))
except (OSError, ValueError):
    raise SystemExit(1)
if not isinstance(value, dict) or value.get("schema") != "uip.recovery-status/v1":
    raise SystemExit(1)
labels = {"recovered": "已恢复并通过核验", "failed": "恢复失败，需人工处理", "not_applicable": "无需运行态回滚"}
migrations = {"not_started": "未开始", "attempted": "已尝试/结果未知", "completed": "已完成", "unknown": "未知"}
print("最近恢复结果：%s（服务：%s，迁移：%s，时间：%s）" % (
    labels.get(value.get("status"), str(value.get("status"))), value.get("service", "未知"),
    migrations.get(value.get("migration"), str(value.get("migration", "未知"))), value.get("at", "未知")))
if value.get("message"):
    print("说明：" + str(value["message"]))
PY
  fi
  if [[ -n "${1:-}" ]]; then
    subsystem_status "$1"
    return
  fi
  if [[ -f "$script_dir/deployment-session.py" && -d "${deploy_dir}.installation-state" ]]; then
    python3 "$script_dir/deployment-session.py" status --state-dir "${deploy_dir}.installation-state" --format human || true
    printf '\n'
  fi
  printf '容器名称\t所属子系统\t镜像\t运行状态\t健康状态\t重启次数\t端口\n'
  docker ps -aq --filter 'name=uip' | while read -r container; do
    [[ -n "$container" ]] || continue
    docker inspect --format '{{.Name}}\t{{index .Config.Labels "com.basic-platform.application_code"}}\t{{.Config.Image}}\t{{.State.Status}}\t{{if .State.Health}}{{.State.Health.Status}}{{else}}-{{end}}\t{{.RestartCount}}\t{{json .NetworkSettings.Ports}}' "$container"
  done | sed 's#^/##'
}

wait_for_required_service_health() {
  local timeout="${UIP_HEALTH_WAIT_SECONDS:-180}" deadline service container state health restarts pending
  [[ "$timeout" =~ ^[0-9]+$ ]] && ((timeout >= 1 && timeout <= 3600)) || { printf '健康等待时间必须是 1 到 3600 之间的整数秒\n' >&2; return 1; }
  deadline=$((SECONDS + timeout))
  while :; do
    pending=''
    for service in "$@"; do
      container="$(compose ps -q "$service")" || return
      if [[ -z "$container" ]]; then pending+=" $service"; continue; fi
      state="$(docker inspect "$container" --format '{{.State.Status}}')" || return
      health="$(docker inspect "$container" --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}')" || return
      [[ "$state" == running && "$health" == healthy ]] || pending+=" $service"
    done
    if [[ -z "$pending" ]]; then
      for service in "$@"; do
        container="$(compose ps -q "$service")" || return
        state="$(docker inspect "$container" --format '{{.State.Status}}')" || return
        health="$(docker inspect "$container" --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}')" || return
        restarts="$(docker inspect "$container" --format '{{.RestartCount}}')" || return
        [[ "$state" == running && "$health" == healthy ]] || {
          printf '核心服务在生成验收证据前状态发生变化：service=%s status=%s health=%s\n' \
            "$service" "${state:-unknown}" "${health:-unknown}" >&2
          return 1
        }
        [[ "$restarts" =~ ^[0-9]+$ ]] || {
          printf '无法读取核心服务 %s 的 RestartCount\n' "$service" >&2
          return 1
        }
        printf '[验收证据] service=%s container=%s status=running health=healthy restarts=%s\n' \
          "$service" "$container" "$restarts"
      done
      printf '必要服务均已健康。\n'
      return 0
    fi
    if ((SECONDS >= deadline)); then
      printf '等待必要服务健康超过 %s 秒：%s\n' "$timeout" "$pending" >&2
      for service in "$@"; do compose logs --tail 80 "$service" >&2 || true; done
      return 1
    fi
    sleep 2
  done
}

verify_current_services() {
  local stability_seconds="${UIP_VERIFY_STABILITY_SECONDS:-10}"
  local service container state health restarts current_container current_state current_health current_restarts index
  local expected_image expected_native actual_image actual_native resolved
  local -a stable_services=()
  local -a stable_containers=()
  local -a stable_restarts=()

  [[ "$stability_seconds" =~ ^[0-9]+$ ]] && ((stability_seconds >= 1 && stability_seconds <= 60)) || {
    printf '无探针 Worker 稳定观察时间必须是 1 到 60 之间的整数秒\n' >&2
    return 1
  }

  for service in "$@"; do
    container="$(compose ps -q "$service" 2>/dev/null || true)"
    if [[ -z "$container" ]]; then
      printf '[验收失败] service=%s container=missing；必需长期服务未运行\n' "$service" >&2
      return 1
    fi
    state="$(docker inspect "$container" --format '{{.State.Status}}' 2>/dev/null || true)"
    health="$(docker inspect "$container" --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' 2>/dev/null || true)"
    restarts="$(docker inspect "$container" --format '{{.RestartCount}}' 2>/dev/null || true)"
    if [[ "$state" != running ]]; then
      printf '[验收失败] service=%s container=%s status=%s health=%s restarts=%s\n' \
        "$service" "$container" "${state:-unknown}" "${health:-unknown}" "${restarts:-unknown}" >&2
      return 1
    fi
    resolved="$(compose config --format json 2>/dev/null)" || {
      printf '[验收失败] service=%s 无法解析当前审核编排镜像\n' "$service" >&2
      return 1
    }
    expected_image="$(jq -er --arg service "$service" '.services[$service].image | select(type == "string" and length > 0)' <<< "$resolved")" || return 1
    # 在此守护进程上比较原生 ID，绝不直接比较仓库清单 ID 和归档配置摘要，也不用可变标签作为证据。
    expected_native="$(docker image inspect "$expected_image" --format '{{.Id}}' 2>/dev/null)" || return 1
    actual_image="$(docker inspect "$container" --format '{{.Image}}' 2>/dev/null)" || return 1
    actual_native="$(docker image inspect "$actual_image" --format '{{.Id}}' 2>/dev/null)" || return 1
    if [[ ! "$expected_native" =~ ^sha256:[a-f0-9]{64}$ || "$expected_native" != "$actual_native" ]]; then
      printf '[验收失败] service=%s 实际镜像与当前审核编排不一致\n' "$service" >&2
      return 1
    fi
    [[ "$restarts" =~ ^[0-9]+$ ]] || {
      printf '[验收失败] service=%s container=%s 无法读取 RestartCount\n' "$service" "$container" >&2
      return 1
    }
    case "$health" in
      healthy)
        printf '[验收证据] service=%s container=%s status=running health=healthy restarts=%s\n' \
          "$service" "$container" "$restarts"
        ;;
      none)
        stable_services+=("$service")
        stable_containers+=("$container")
        stable_restarts+=("$restarts")
        ;;
      *)
        printf '[验收失败] service=%s container=%s status=running health=%s restarts=%s\n' \
          "$service" "$container" "${health:-unknown}" "$restarts" >&2
        return 1
        ;;
    esac
  done

  if ((${#stable_services[@]} > 0)); then
    printf '观察 %d 个无 healthcheck 的长期 Worker %s 秒，校验容器不替换且 RestartCount 不增加。\n' \
      "${#stable_services[@]}" "$stability_seconds"
    sleep "$stability_seconds"
    for ((index=0; index<${#stable_services[@]}; index++)); do
      service="${stable_services[$index]}"
      current_container="$(compose ps -q "$service" 2>/dev/null || true)"
      current_state="$(docker inspect "$current_container" --format '{{.State.Status}}' 2>/dev/null || true)"
      current_health="$(docker inspect "$current_container" --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' 2>/dev/null || true)"
      current_restarts="$(docker inspect "$current_container" --format '{{.RestartCount}}' 2>/dev/null || true)"
      if [[ "$current_container" != "${stable_containers[$index]}" || "$current_state" != running || "$current_health" != none || \
            "$current_restarts" != "${stable_restarts[$index]}" ]]; then
        printf '[验收失败] service=%s container=%s->%s status=%s health=%s restarts=%s->%s stability=%ss\n' \
          "$service" "${stable_containers[$index]}" "${current_container:-missing}" "${current_state:-unknown}" \
          "${current_health:-unknown}" "${stable_restarts[$index]}" "${current_restarts:-unknown}" "$stability_seconds" >&2
        return 1
      fi
      printf '[验收证据] service=%s container=%s status=running health=none restarts=%s stability=%ss\n' \
        "$service" "$current_container" "$current_restarts" "$stability_seconds"
    done
  fi
}

verify_controlled_deployment() {
  local component="$1"
  subsystem_metadata "$component"
  # 使用激活时同一个仅限 root 的维护进程解析持久化管理员，并读取正常受控状态处理器。
  compose run -T --rm --no-deps platform-api ./license-install verify-deployment \
    --application "$subsystem_app_code" --timeout 60s >/dev/null
}

require_runtime_image_bindings() {
  local selected="$1" service resolved image
  [[ -n "$selected" ]] || return 1
  resolved="$(compose config --format json 2>/dev/null)" || return 1
  while IFS= read -r service; do
    [[ -n "$service" ]] || continue
    image="$(jq -er --arg service "$service" '.services[$service].image | select(type == "string" and length > 0)' <<< "$resolved")" || return 1
    [[ "$image" =~ @sha256:[a-f0-9]{64}$ || "$service" == data-analysis-metabase ]] || {
      printf '[验收失败] service=%s 未绑定不可变发行镜像\n' "$service" >&2
      return 1
    }
  done <<< "$selected"
}

verify_selected_runtime() {
  local component="$1" selected service
  local -a services=()
  selected="$(scope_runtime_services "$component")" || return 1
  require_runtime_image_bindings "$selected" || return 1
  while IFS= read -r service; do [[ -z "$service" ]] || services+=("$service"); done <<< "$selected"
  verify_current_services "${services[@]}" || return 1
  [[ "$component" != project ]] || project_background_verify || return 1
  verify_controlled_deployment "$component" || {
    printf '[验收失败] %s 平台部署状态或清单核验尚未完成\n' "$component" >&2
    return 1
  }
}

verify_subsystem_current_health() {
  local component="$1" flag selected service
  local -a services=()
  subsystem_metadata "$component"
  runtime_ready "$subsystem_runtime" || {
    printf '错误：%s 当前 runtime 配置不完整，不能使用历史 VERIFIED 结果通过验收。\n' "$component" >&2
    return 1
  }
  if docker container inspect "$subsystem_candidate" >/dev/null 2>&1; then
    printf '错误：%s 仍存在未完成采用的候选容器 %s。\n' "$component" "$subsystem_candidate" >&2
    return 1
  fi
  selected="$(scope_runtime_services "$component")" || return 1
  require_runtime_image_bindings "$selected" || return 1
  while IFS= read -r service; do [[ -z "$service" ]] || services+=("$service"); done <<< "$selected"

  # 只列出长期服务；migrate/init/catalog-sync 是受控一次性任务，不能按常驻容器验收。
  case "$component" in
    contract)
      services+=(contract-mysql)
      [[ "$(env_get "$subsystem_runtime" CRM_REFERENCE_ENABLED)" != true ]] || services+=(customer-api)
      [[ "$(env_get "$subsystem_runtime" PROJECT_INTEGRATION_ENABLED)" != true ]] || services+=(project-api)
      ;;
    project) services+=(project-mysql) ;;
    customer-opportunity)
      services+=(customer-mysql)
      for flag in CONTRACT_VERIFICATION_ENABLED; do
        [[ "$(env_get "$subsystem_runtime" "$flag")" != true ]] || services+=(contract-api)
      done
      ;;
    customer-portal)
      services+=(portal-mysql file-gateway)
      if portal_compensation_ready; then
        services+=(customer-mysql customer-api)
        printf '[验收证据] Portal 邀请补偿凭据已启用，纳入补偿 Worker 及 CRM 跨系统依赖。\n'
      else
        printf '[验收证据] Portal 邀请补偿未满足既有启用条件，不要求补偿 Worker 常驻。\n'
      fi
      ;;
    settlement) services+=(settlement-mysql) ;;
    data-analysis)
      services+=(data-analysis-mysql contract-api project-api)
      ;;
    *) die "不支持的子系统：$component" ;;
  esac
  verify_current_services "${services[@]}" || return
  [[ "$component" != project ]] || project_background_verify || return
  verify_controlled_deployment "$component"
}

verify() {
  require_initialized
  local component="${1:-}"
  [[ -z "$component" ]] || scope_require "$component" || return
  wait_for_required_service_health \
    platform-api platform-worker file-gateway keycloak temporal subsystem-provisioner frontend || return
  compose ps
  local api_port
  api_port="$(env_get "$runtime_file" PLATFORM_API_PORT)"; api_port="${api_port:-18080}"
  curl --fail --silent --show-error --connect-timeout 5 --max-time 20 "http://127.0.0.1:$api_port/readyz" >/dev/null
  curl --fail --silent --show-error --connect-timeout 5 --max-time 20 "$PUBLIC_PLATFORM_ORIGIN/healthz" >/dev/null
  curl --fail --silent --show-error --connect-timeout 5 --max-time 20 "$PUBLIC_KEYCLOAK_ISSUER/.well-known/openid-configuration" >/dev/null
  compose exec -T platform-api wget -qO- http://file-gateway:8086/readyz >/dev/null || die 'platform-api 容器无法访问 http://file-gateway:8086/readyz'
  gateway_proxy_status="$(curl --silent --show-error --connect-timeout 5 --max-time 20 --output /dev/null --write-out '%{http_code}' "$PUBLIC_PLATFORM_ORIGIN/file-gateway/api/v2/upload-sessions")"
  [[ "$gateway_proxy_status" == 405 || "$gateway_proxy_status" == 401 ]] || die "File Gateway 代理未到达受保护 API（HTTP ${gateway_proxy_status}）"
  echo 'File Gateway 代理到达受保护 API；匿名请求被正确拒绝。'
  echo '基础平台核心服务健康检查已通过；不代表任何业务子系统已部署，也不代表外部客户端/安全组已验证。'
  if [[ -n "$component" ]]; then
    verify_subsystem_current_health "$component" || return
    printf '%s/prod 子系统当前数据库、API、Worker 与跨系统依赖验收已通过。\n' "$subsystem_app_code"
  else
    echo '如需验收业务子系统，请执行：deploy.sh verify <component>。'
  fi
}
