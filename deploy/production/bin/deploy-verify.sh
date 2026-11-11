#!/usr/bin/env bash
# Deploy status/verification helpers. Sourced by deploy.sh.

# 跨子系统接口：deploy-service.sh 发现“数据库依赖刷新失败”时写
# <deploy_dir>/runtime/database-refresh-failed.json（schema uip.database-refresh-failed/v1），
# 并在该服务之后重新部署成功时删除它。平台发布按设计在这种情形下继续（依赖数据库的迁移可能
# 已部分应用，重试或人工处理才安全），所以这里是**durable 提示标记**，与 first-failure.json /
# recovery-status.json 这类“阶段结果”语义不同：verify() 只做显著提示 + 给出补救动作，
# 不把它当作发布失败判据（否则一份历史标记会让 --resume 永久无法收敛，等于把已继续的发布
# 改判为失败）。文件缺失、损坏、不可读或不是普通文件时只降级提示，绝不中断验收。
report_database_refresh_failures() {
  local marker="$deploy_dir/runtime/database-refresh-failed.json"
  [[ -e "$marker" || -L "$marker" ]] || return 0
  if [[ -L "$marker" || ! -f "$marker" || ! -r "$marker" ]]; then
    printf '警告：检测到数据库依赖刷新失败标记，但它不是可读的普通文件：%s\n' "$marker" >&2
    printf '补救：在对应子系统发布阶段重新部署该服务；平台发布当时已继续。\n' >&2
    return 0
  fi
  python3 - "$marker" <<'PY' || printf '警告：数据库依赖刷新失败标记无法解析，请人工查看 %s\n' "$marker" >&2
import json, sys
try:
    with open(sys.argv[1], encoding="utf-8") as handle:
        value = json.load(handle)
except (OSError, ValueError):
    raise SystemExit(1)
failures = value.get("failures") if isinstance(value, dict) else None
if not isinstance(failures, list):
    raise SystemExit(1)
pairs = [item for item in failures if isinstance(item, dict)]
if not pairs:
    raise SystemExit(0)
print("警告：检测到数据库依赖刷新失败记录——平台发布当时已继续，相关依赖数据库的迁移可能未完成：")
for item in pairs:
    print("  · 数据库 %s → 服务 %s（原因：%s；记录时间：%s）" % (
        item.get("database", "未知"), item.get("service", "未知"),
        item.get("reason", "未记录"), item.get("at", "未知")))
print("补救：在对应子系统发布阶段重新部署该服务；平台发布当时已继续。")
print("说明：该标记由发布流程写入、在该服务重新部署成功后删除；本次验收不因此判定失败。")
PY
}

# 运行中 OIDC 消费者 → 部署声明的期望环境变量，每行“服务<TAB>变量<TAB>期望值”。
# 只列部署期才能算出、且写错就会让登录静默失败（或不安全）的键：回跳地址与 Secure Cookie 标志。
# 其余 OIDC 字段（client id/secret/issuer）仍由各子系统 runtime/*.env 负责。
public_transport_expected_oidc_env() {
  local origin="$1" secure="$2"
  printf 'platform-api\tAPP_PUBLIC_BASE_URL\t%s\n' "$origin"
  printf 'platform-api\tAUTH_SESSION_COOKIE_SECURE\t%s\n' "$secure"
  printf 'contract-api\tOIDC_REDIRECT_URI\t%s/contract_management/auth/callback\n' "$origin"
  printf 'contract-api\tOIDC_SESSION_COOKIE_SECURE\t%s\n' "$secure"
  printf 'customer-api\tOIDC_REDIRECT_URI\t%s/customer-opportunity/auth/callback\n' "$origin"
  printf 'customer-api\tOIDC_SESSION_COOKIE_SECURE\t%s\n' "$secure"
  printf 'portal-api\tPORTAL_OIDC_REDIRECT_URI\t%s/customer-portal/auth/callback\n' "$origin"
  printf 'portal-api\tPORTAL_SESSION_COOKIE_SECURE\t%s\n' "$secure"
  printf 'project-api\tOIDC_REDIRECT_URI\t%s/project_management/auth/callback\n' "$origin"
  printf 'project-api\tOIDC_SESSION_COOKIE_SECURE\t%s\n' "$secure"
  printf 'settlement-api\tOIDC_REDIRECT_URI\t%s/settlement/auth/callback\n' "$origin"
  printf 'settlement-api\tOIDC_SESSION_COOKIE_SECURE\t%s\n' "$secure"
  printf 'data-analysis-api\tOIDC_REDIRECT_URI\t%s/data_analysis/auth/callback\n' "$origin"
  printf 'data-analysis-api\tCOOKIE_SECURE\t%s\n' "$secure"
}

# 比对“运行中容器的生效环境变量”与“本次部署声明”。每行输出：
# 服务<TAB>变量<TAB>声明值<TAB>实际值（实际值缺失时写明 <未设置>）。
# 返回值：一致 0；发现漂移 1；无法解析部署声明（未导出 PUBLIC_*）2。
# 只读：仅 docker ps/inspect，不重启、不创建容器。
public_transport_runtime_drift() {
  local origin="${PUBLIC_PLATFORM_ORIGIN:-}" secure="${PUBLIC_TRANSPORT_COOKIE_SECURE:-}"
  if [[ -z "$origin" || -z "$secure" ]]; then
    printf '无法解析部署声明：PUBLIC_PLATFORM_ORIGIN=%s PUBLIC_TRANSPORT_COOKIE_SECURE=%s\n' \
      "${origin:-<未设置>}" "${secure:-<未设置>}"
    return 2
  fi
  local service key expected container environment actual drift=0
  while IFS=$'\t' read -r service key expected; do
    [[ -n "$service" ]] || continue
    container="$(docker ps -q --filter "label=com.docker.compose.service=${service}" 2>/dev/null || true)"
    container="${container%%$'\n'*}"
    [[ -n "$container" ]] || continue
    if ! environment="$(docker inspect -f '{{range .Config.Env}}{{println .}}{{end}}' "$container" 2>/dev/null)"; then
      printf '%s\t%s\t%s\t<无法读取容器 %s 的环境变量>\n' "$service" "$key" "$expected" "$container"
      drift=1
      continue
    fi
    actual="$(printf '%s\n' "$environment" |
      awk -v key="$key" 'index($0, key "=") == 1 { print substr($0, length(key) + 2); exit }')"
    if [[ "$actual" != "$expected" ]]; then
      printf '%s\t%s\t%s\t%s\n' "$service" "$key" "$expected" "${actual:-<未设置>}"
      drift=1
    fi
  done <<< "$(public_transport_expected_oidc_env "$origin" "$secure")"
  return "$drift"
}

# 验收门禁：运行中容器必须按当前部署声明签发回跳地址与 Secure Cookie。
# 现场事故形态是“健康检查全绿、登录却 401 死循环”，因此这一层必须在打印成功结论之前拦下。
verify_public_transport_consistency() {
  local drift_output='' drift_status=0
  drift_output="$(public_transport_runtime_drift)" || drift_status=$?
  case "$drift_status" in
    0)
      printf '[验收证据] 运行中 OIDC 消费者的回跳地址与 Secure Cookie 与部署声明一致：%s（cookie_secure=%s）\n' \
        "$PUBLIC_PLATFORM_ORIGIN" "$PUBLIC_TRANSPORT_COOKIE_SECURE"
      return 0
      ;;
    2)
      printf '[警告] 无法解析部署的公开传输声明，跳过运行态比对：%s\n' "$drift_output" >&2
      return 0
      ;;
  esac
  printf '[验收失败] 运行中容器的公开传输配置与部署声明不一致（服务/变量/声明值/实际值）：\n' >&2
  printf '%s\n' "$drift_output" >&2
  printf '原因：这些容器由缺失或过期的公开传输变量创建（典型是绕过 deploy.sh 的 compose up 静默取到 localhost 与 Secure Cookie=true）；健康检查不会发现，登录会 401/被踢回登录页。\n' >&2
  printf '修复：sudo %s configure --force 重新解析并持久化，再按组件重建容器：sudo %s deploy <component>\n' \
    "$0" "$0" >&2
  return 1
}

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
  report_database_refresh_failures
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
    docker inspect -f '{{.Name}}\t{{index .Config.Labels "com.basic-platform.application_code"}}\t{{.Config.Image}}\t{{.State.Status}}\t{{if .State.Health}}{{.State.Health.Status}}{{else}}-{{end}}\t{{.RestartCount}}\t{{json .NetworkSettings.Ports}}' "$container"
  done | sed 's#^/##'
}

# 健康收敛预算校验：单服务窗口 + 全体总预算。成功时把合法值回填到
# health_budget_window / health_budget_total，供所有“等服务健康收敛”的路径共用一套口径。
# 配置非法必须显式失败，不能静默退化成默认值继续等待（否则现场配错只会表现成“等很久”）。
read_health_budget() {
  health_budget_window="${UIP_HEALTH_WAIT_SECONDS:-180}"
  health_budget_total="${UIP_HEALTH_TOTAL_SECONDS:-600}"
  [[ "$health_budget_window" =~ ^[0-9]+$ ]] && ((health_budget_window >= 1 && health_budget_window <= 3600)) || {
    printf '健康等待时间必须是 1 到 3600 之间的整数秒\n' >&2
    return 1
  }
  [[ "$health_budget_total" =~ ^[0-9]+$ ]] && ((health_budget_total >= 1 && health_budget_total <= 7200)) || {
    printf '健康等待总预算必须是 1 到 7200 之间的整数秒\n' >&2
    return 1
  }
}

# 计算一个服务本轮收敛的生效截止时间：单服务窗口（从该服务自己第一次被探测时开始计时）
# 与全体总预算取较早者，因此“前一个服务消耗掉很久”不会把后一个服务挤成 0 秒窗口；
# 生效时长不足一次采样时仍强制保留一次真实的 2 秒采样，保证该服务至少被真实探测一次、
# 诊断里的等待时长是实测值。结果回填到 health_convergence_deadline 和
# health_convergence_binding（后者记录真正耗尽的那一层预算）。
resolve_health_convergence_deadline() {
  local started="$1" per_service_window="$2" total_deadline="$3"
  local service_deadline=$((started + per_service_window))
  if ((total_deadline <= service_deadline)); then
    health_convergence_deadline="$total_deadline"
    health_convergence_binding='总预算'
  else
    health_convergence_deadline="$service_deadline"
    health_convergence_binding='单服务窗口'
  fi
  ((health_convergence_deadline - started >= 2)) || health_convergence_deadline=$((started + 2))
}

# 收敛失败的统一诊断：真实等待时长 + 哪一层预算耗尽 + 该服务最近 80 行日志。
report_health_convergence_timeout() {
  local service="$1" elapsed="$2" per_service_window="$3" total_budget="$4" binding="$5"
  printf '等待服务健康收敛超过 %s 秒：待收敛服务=%s（该服务已等待 %s 秒；耗尽预算=%s；单服务窗口=%s 秒；总预算=%s 秒）\n' \
    "$elapsed" "$service" "$elapsed" "$binding" "$per_service_window" "$total_budget" >&2
  compose logs --tail 80 "$service" >&2 || true
}

# 必要服务的健康门禁同样用两层预算（与 verify_current_services 一致）：每个服务从自己第一次
# 被探测时开始计算单服务窗口，全体共享总预算。旧实现让所有服务共用一个截止时间，于是慢但会
# 健康的后续服务被前面的服务耗掉预算后误判失败，且报出它从未真正等待过的时长。
wait_for_required_service_health() {
  local service container state health restarts started total_deadline
  read_health_budget || return 1
  started=$SECONDS
  total_deadline=$((started + health_budget_total))
  for service in "$@"; do
    wait_for_current_service_convergence "$service" "$health_budget_window" "$total_deadline" "$health_budget_total" || return 1
  done
  for service in "$@"; do
    container="$(compose ps -q "$service")" || return
    state="$(docker inspect "$container" --format '{{.State.Status}}')" || return
    health="$(docker inspect "$container" --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}')" || return
    restarts="$(docker inspect "$container" --format '{{.RestartCount}}')" || return
    [[ "$state" == running && "$health" == healthy ]] || {
      if [[ -z "$health" || "$health" == unknown ]]; then
        printf '无法读取核心服务 %s 的健康状态（docker inspect 失败或编排未声明 healthcheck）：service=%s status=%s health=%s\n' \
          "$service" "$service" "${state:-unknown}" "${health:-unknown}" >&2
      elif [[ "$health" == none ]]; then
        printf '核心服务 %s 在编排中没有 healthcheck，无法据此生成验收证据：status=%s\n' \
          "$service" "${state:-unknown}" >&2
      else
        printf '核心服务在生成验收证据前状态发生变化：service=%s status=%s health=%s\n' \
          "$service" "${state:-unknown}" "${health:-unknown}" >&2
      fi
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
}

# 现场 P0：--resume 会强制重建 platform-api/platform-worker，容器健康必然先为 starting。
# 旧逻辑单次采样即判失败，导致每次 resume 的交付证明都失败；这里改为有界等待收敛。
#
# 收敛预算分两层，缺一不可（见 resolve_health_convergence_deadline）：
#   1) 单服务窗口：UIP_HEALTH_WAIT_SECONDS，从该服务自己被探测时开始计时（默认 180 秒）；
#   2) 全体总预算：UIP_HEALTH_TOTAL_SECONDS，从本轮核验开始时计时（默认 600 秒）。
# 生效截止时间取两者中较早的一个，因此“前一个服务消耗很久”不会把后一个服务挤成 0 秒窗口
# （旧实现同轮共用单个截止时间，会让本来只是慢但会健康的服务被误判失败），同时整轮核验的
# 总时长仍被总预算约束住。预算已耗尽时仍强制保留一次真实的 2 秒采样，因此总时长上界是
# “总预算 + 每个尚未收敛服务各一次 2 秒采样”，诊断里的等待时长是该服务真实测得的。
wait_for_current_service_convergence() {
  local service="$1" per_service_window="$2" total_deadline="$3" total_budget="$4"
  local container='' state=unknown health=unknown restarts=unknown
  local started effective_deadline elapsed binding
  started=$SECONDS
  resolve_health_convergence_deadline "$started" "$per_service_window" "$total_deadline"
  effective_deadline="$health_convergence_deadline"
  binding="$health_convergence_binding"
  while :; do
    container="$(compose ps -q "$service" 2>/dev/null || true)"
    if [[ -n "$container" ]]; then
      state="$(docker inspect "$container" --format '{{.State.Status}}' 2>/dev/null || true)"
      health="$(docker inspect "$container" --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' 2>/dev/null || true)"
      # 空 health 只表示 docker inspect 瞬时失败，不能当作已收敛；继续轮询，把可重试的抖动
      # 留在收敛窗口内解决，而不是在后面的验收证据阶段变成 health=unknown 的发布失败。
      [[ -n "$health" && "$state" == running && "$health" != starting && "$health" != unhealthy ]] && return 0
    fi
    if ((SECONDS >= effective_deadline)); then
      elapsed=$((SECONDS - started))
      [[ -z "$container" ]] || restarts="$(docker inspect "$container" --format '{{.RestartCount}}' 2>/dev/null || true)"
      printf '[验收失败] service=%s container=%s status=%s health=%s restarts=%s\n' \
        "$service" "${container:-missing}" "${state:-unknown}" "${health:-unknown}" "${restarts:-unknown}" >&2
      report_health_convergence_timeout "$service" "$elapsed" "$per_service_window" "$total_budget" "$binding"
      return 1
    fi
    sleep 2
  done
}

verify_current_services() {
  local health_timeout total_budget stability_seconds="${UIP_VERIFY_STABILITY_SECONDS:-10}"
  local service container state health restarts current_container current_state current_health current_restarts index need_wait started total_deadline
  local expected_image expected_native actual_image actual_native resolved
  local -a services=()
  local -a containers=()
  local -a healths=()
  local -a restarts_before=()

  read_health_budget || return 1
  health_timeout="$health_budget_window"
  total_budget="$health_budget_total"
  [[ "$stability_seconds" =~ ^[0-9]+$ ]] && ((stability_seconds >= 1 && stability_seconds <= 60)) || {
    printf '无探针 Worker 稳定观察时间必须是 1 到 60 之间的整数秒\n' >&2
    return 1
  }
  # 全体服务共享一个总预算（避免逐个服务串行等待放大总时长），但每个服务在自己的窗口内
  # 单独计算截止时间；两者取较早者，所以慢服务不会被前面服务消耗掉的预算饿死。
  started=$SECONDS
  total_deadline=$((started + total_budget))

  for service in "$@"; do
    container="$(compose ps -q "$service" 2>/dev/null || true)"
    if [[ -z "$container" ]]; then
      printf '[验收失败] service=%s container=missing；必需长期服务未运行\n' "$service" >&2
      return 1
    fi
    state="$(docker inspect "$container" --format '{{.State.Status}}' 2>/dev/null || true)"
    health="$(docker inspect "$container" --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' 2>/dev/null || true)"
    need_wait=0
    case "$state" in
      running) [[ -n "$health" && "$health" != starting && "$health" != unhealthy ]] || need_wait=1 ;;
      # 强制重建后的瞬时 created/restarting 也要等收敛，不能当成稳定状态。
      created|restarting) need_wait=1 ;;
      *)
        restarts="$(docker inspect "$container" --format '{{.RestartCount}}' 2>/dev/null || true)"
        printf '[验收失败] service=%s container=%s status=%s health=%s restarts=%s\n' \
          "$service" "$container" "${state:-unknown}" "${health:-unknown}" "${restarts:-unknown}" >&2
        return 1
        ;;
    esac
    if ((need_wait)); then
      wait_for_current_service_convergence "$service" "$health_timeout" "$total_deadline" "$total_budget" || return 1
      container="$(compose ps -q "$service" 2>/dev/null || true)"
      if [[ -z "$container" ]]; then
        printf '[验收失败] service=%s container=missing；必需长期服务未运行\n' "$service" >&2
        return 1
      fi
      state="$(docker inspect "$container" --format '{{.State.Status}}' 2>/dev/null || true)"
      health="$(docker inspect "$container" --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' 2>/dev/null || true)"
    fi
    # 空 health 只可能来自失败的 docker inspect（有 healthcheck 时至少是 starting/healthy/unhealthy，
    # 没有时是 none）。不能让它一路落到 health=unknown，把一次可重试的 inspect 抖动变成发布失败。
    [[ -n "$health" ]] || {
      printf '[验收失败] service=%s container=%s 无法读取健康状态（docker inspect 失败）\n' \
        "$service" "$container" >&2
      return 1
    }
    restarts="$(docker inspect "$container" --format '{{.RestartCount}}' 2>/dev/null || true)"
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
      healthy|none) ;;
      *)
        printf '[验收失败] service=%s container=%s status=running health=%s restarts=%s\n' \
          "$service" "$container" "${health:-unknown}" "$restarts" >&2
        return 1
        ;;
    esac
    services+=("$service")
    containers+=("$container")
    healths+=("$health")
    restarts_before+=("$restarts")
  done

  if ((${#services[@]} > 0)); then
    # 现场要求：外部依赖导致的历史重启不算不稳定，只判定验收窗口内是否新增重启。
    printf '观察 %d 个长期服务的 %s 秒稳定性窗口：容器不替换、状态与健康不退化、RestartCount 不新增（历史重启不判失败）。\n' \
      "${#services[@]}" "$stability_seconds"
    sleep "$stability_seconds"
    for ((index=0; index<${#services[@]}; index++)); do
      service="${services[$index]}"
      current_container="$(compose ps -q "$service" 2>/dev/null || true)"
      current_state="$(docker inspect "$current_container" --format '{{.State.Status}}' 2>/dev/null || true)"
      current_health="$(docker inspect "$current_container" --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' 2>/dev/null || true)"
      current_restarts="$(docker inspect "$current_container" --format '{{.RestartCount}}' 2>/dev/null || true)"
      if [[ "$current_container" != "${containers[$index]}" || "$current_state" != running || \
            "$current_health" != "${healths[$index]}" || "$current_restarts" != "${restarts_before[$index]}" ]]; then
        printf '[验收失败] service=%s container=%s->%s status=%s health=%s restarts=%s->%s stability=%ss\n' \
          "$service" "${containers[$index]}" "${current_container:-missing}" "${current_state:-unknown}" \
          "${current_health:-unknown}" "${restarts_before[$index]}" "${current_restarts:-unknown}" "$stability_seconds" >&2
        return 1
      fi
      if [[ "${restarts_before[$index]}" != 0 ]]; then
        printf '[验收证据] service=%s container=%s status=running health=%s restarts=%s（历史重启，验收窗口内未新增） stability=%ss\n' \
          "$service" "$current_container" "${healths[$index]}" "$current_restarts" "$stability_seconds"
      else
        printf '[验收证据] service=%s container=%s status=running health=%s restarts=0（无重启） stability=%ss\n' \
          "$service" "$current_container" "${healths[$index]}" "$stability_seconds"
      fi
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
    reimbursement) services+=(reimbursement-mysql) ;;
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

# 运行配置门禁：runtime/*.env 重复键 + 编排里的生产 worker 标志必须为字面量 false。
# 现场两次故障：contract.env 重复受管键 -> provisioner 报 validate runtime environment
# group；project-migrate 的 PROJECT_RUN_WORKER_WITH_API="true" -> 生产校验报
# PROJECT_RUN_WORKER_WITH_API must be false in production。两类问题都能在发布前静态发现，
# 因此这里 fail-closed：不通过就不打印成功结论（与许可运行态门禁同一位置/风格）。
# 参数：可选 --apps/--json/--dry-run/--deploy-dir（透传给门禁脚本）。
# 返回值：通过返回零；未通过返回非零，由调用方中止。
runtime_env_hygiene_gate() {
  local -a passthrough=()
  while (($# > 0)); do
    case "$1" in
      --deploy-dir|--apps)
        (($# > 1)) || die "$1 缺少取值"
        passthrough+=("$1" "$2")
        shift 2
        ;;
      --json|--dry-run)
        passthrough+=("$1")
        shift
        ;;
      *) die "不支持的运行配置门禁参数：$1" ;;
    esac
  done
  local -a gate_args=(--deploy-dir "$deploy_dir")
  ((${#passthrough[@]} == 0)) || gate_args+=("${passthrough[@]}")
  if python3 "$script_dir/check-runtime-env-hygiene.py" "${gate_args[@]}"; then
    return 0
  fi
  printf '错误：运行配置门禁未通过，先修配置再发布（未判定部署成功）。\n' >&2
  printf '处置：runtime/*.env 里每个键只保留一行——重复键会让 provisioner 报 validate runtime environment group；\n' >&2
  printf '      编排里 PROJECT_RUN_WORKER_WITH_API 全部写死 false（project-migrate 也不例外），\n' >&2
  printf '      否则生产校验报 PROJECT_RUN_WORKER_WITH_API must be false in production。\n' >&2
  printf '复核：%s runtime-env（只读，可反复执行）\n' "$0" >&2
  return 1
}

verify() {
  require_initialized
  local component="${1:-}"
  local -a required_services=(platform-api platform-worker file-gateway keycloak)
  # temporal 由 deploy-service.sh 按 temporal_consumers_enabled 条件启动：没有合同/项目/CRM
  # 消费子系统时它根本不会被启动，因此这里也必须条件化，否则平台-only 安装永远验收失败。
  if temporal_consumers_enabled; then required_services+=(temporal); fi
  required_services+=(subsystem-provisioner frontend)
  [[ -z "$component" ]] || scope_require "$component" || return
  # 先提示发布阶段遗留的数据库依赖刷新失败标记，再等待健康收敛：这样即使后面的门禁失败，
  # 操作员也能看到“平台发布当时已继续”的补救信息（该标记不阻断验收）。
  report_database_refresh_failures
  wait_for_required_service_health "${required_services[@]}" || return
  compose ps
  local api_port
  api_port="$(env_get "$runtime_file" PLATFORM_API_PORT)"; api_port="${api_port:-18080}"
  curl --fail --silent --show-error --connect-timeout 5 --max-time 20 "http://127.0.0.1:$api_port/readyz" >/dev/null
  curl --fail --silent --show-error --connect-timeout 5 --max-time 20 "$PUBLIC_PLATFORM_ORIGIN/healthz" >/dev/null
  # 公网 issuer 只用于判定“经入口能不能拿到 OIDC 元数据”，必须区分两种不同性质的失败：
  # tenant realm 尚未创建时公网返回 404（由 platform-worker 启动期 Keycloak bootstrap 负责，
  # KEYCLOAK_MANAGEMENT_ENABLED=false 时根本不会执行），这不是 Keycloak 故障，不能把验收卡死；
  # 连接拒绝/超时/5xx 才是真实故障，仍然失败关闭。裸 curl --fail 会把 404 与超时混成一个
  # 无诊断的失败，正是 uip_20261103_r2 现场同类误判的来源。
  keycloak_public_status="$(curl --silent --show-error --connect-timeout 5 --max-time 20 \
    --output /dev/null --write-out '%{http_code}' \
    "$PUBLIC_KEYCLOAK_ISSUER/.well-known/openid-configuration" || true)"
  case "$keycloak_public_status" in
    200|204) ;;
    404)
      printf '[警告] 公网 Keycloak issuer 返回 404（%s）：tenant realm 尚未创建或名称不一致。\n' \
        "$PUBLIC_KEYCLOAK_ISSUER" >&2
      printf '       该 realm 由 platform-worker 启动期的 Keycloak bootstrap（EnsureKeycloakBroker）创建；\n' >&2
      printf '       KEYCLOAK_MANAGEMENT_ENABLED=false 时不会执行，属预期配置而非故障。不阻断本次验收。\n' >&2
      ;;
    *)
      die "Keycloak 公网 OIDC 发现文档不可用：HTTP ${keycloak_public_status:-unknown}（${PUBLIC_KEYCLOAK_ISSUER}）；请检查前端入口、租户 realm 与 Keycloak 请求处理"
      ;;
  esac
  compose exec -T platform-api wget -qO- http://file-gateway:8086/readyz >/dev/null || die 'platform-api 容器无法访问 http://file-gateway:8086/readyz'
  gateway_proxy_status="$(curl --silent --show-error --connect-timeout 5 --max-time 20 --output /dev/null --write-out '%{http_code}' "$PUBLIC_PLATFORM_ORIGIN/file-gateway/api/v2/upload-sessions")"
  [[ "$gateway_proxy_status" == 405 || "$gateway_proxy_status" == 401 ]] || die "File Gateway 代理未到达受保护 API（HTTP ${gateway_proxy_status}）"
  echo 'File Gateway 代理到达受保护 API；匿名请求被正确拒绝。'
  # 登录链路的失败不会被健康检查发现：回跳地址/Secure Cookie 与部署声明不一致时必须失败关闭，
  # 否则“容器全绿、登录 401 死循环”会一路验收通过。
  verify_public_transport_consistency || return 1
  echo '基础平台核心服务健康检查已通过；不代表任何业务子系统已部署，也不代表外部客户端/安全组已验证。'
  if [[ -n "$component" ]]; then
    verify_subsystem_current_health "$component" || return
    # 运行配置门禁：runtime/*.env 重复键与生产 worker 标志。静态缺陷在容器重启或接入
    # 失败之前就拦住；必须先于许可运行态门禁与成功结论，失败即中止（fail-closed）。
    runtime_env_hygiene_gate || return
    # 许可运行态门禁：健康与受控状态都通过后，还要证明“许可运行态”没有把容器留在重启
    # 循环里（缺许可状态卷 / 许可实例与平台不一致）。必须在打印成功结论之前，并且失败即中止：
    # 现场故障正是通过这一层被漏掉，平台只显示「部署 Agent 执行失败」，重试完全无效。
    license_runtime_gate "$subsystem_app_code" || return
    printf '%s/prod 子系统当前数据库、API、Worker 与跨系统依赖验收已通过。\n' "$subsystem_app_code"
  else
    runtime_env_hygiene_gate || return
    license_runtime_gate || return
    echo '如需验收业务子系统，请执行：deploy.sh verify <component>。'
  fi
}
