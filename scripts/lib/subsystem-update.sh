#!/usr/bin/env bash
# subsystem.sh update 子命令（批次 5 库化，逐字迁移）；仅含函数定义，无顶层副作用——由 scripts/subsystem.sh 入口在全局变量区之后 source。

# ============================ update 子命令 ============================
update_usage() {
  cat <<'USAGE'
用法：subsystem.sh update|retry|adopt [认证参数] --application-code CODE --environment ENV

update/retry 保留已有应用、环境登记及业务数据，并按受控部署流程刷新运行配置和容器。
adopt 把"已登记但从未部署"的应用环境纳入部署状态机（首次部署前使用）。
重试可能重新下发接入凭据与组件授权凭据，部署状态会随执行结果更新。
要修改 BaseURL / UpstreamURL / OAuth 配置，请先通过 PATCH /environments 与 /oauth-clients
接口完成受控变更，然后调用本子命令让正在运行的子系统容器应用新配置。

必填参数：
  --application-code CODE   Application.Code，例如 contract_management
  --environment ENV         Environment.Environment：dev/test/staging/prod

可选参数：
  --issuer-alias ALIAS      认证提供方别名，例如 keycloak 或 platform；
                            仅在该参数非空时写入 update/retry/adopt 请求体（payload），
                            留空则沿用平台现有的认证提供方配置

执行控制：
  -y, --yes                 跳过最终确认
  -h, --help                显示本帮助

示例（正常重建）：
  bash scripts/subsystem.sh update \
    --application-code contract_management \
    --environment prod \
    --account admin

部署失败后的重试：
  bash scripts/subsystem.sh retry \
    --application-code contract_management \
    --environment prod \
    --account admin
USAGE
}

update_required_configuration_missing() {
  [[ -z "${APPLICATION_CODE//[[:space:]]/}" || -z "${ENVIRONMENT//[[:space:]]/}" ]]
}

update_write_payload() {
  python3 - "$APPLICATION_CODE" "$ENVIRONMENT" "$ISSUER_ALIAS" >"$1" <<'PY'
import json
import sys
code, environment, issuer_alias = sys.argv[1:]
payload = {
    "application_code": code.strip(),
    "environment": environment.strip(),
}
if issuer_alias.strip():
    payload["issuer_alias"] = issuer_alias.strip()
json.dump(payload, sys.stdout, ensure_ascii=False)
PY
}

update_print_summary() {
  cat >&2 <<EOF_SUMMARY

即将受控更新以下子系统（保留已有应用、环境登记及业务数据）：
  应用：${APPLICATION_CODE}
  环境：${ENVIRONMENT}
  平台 API：${API_BASE_URL}
EOF_SUMMARY
  if [[ -n "$COOKIE_FILE" ]]; then
    printf '  认证方式：复用 Cookie 文件（路径不回显）\n' >&2
  else
    printf '  认证方式：平台管理员账号 %s（密码不回显）\n' "$ACCOUNT" >&2
  fi
  printf '  提示：若已变更 BaseURL/UpstreamURL/PathPrefix/OAuth 配置，请先走 PATCH，再执行 update。\n' >&2
}

update_confirm() {
  [[ "$ASSUME_YES" == true ]] && return 0
  local answer=""
  printf '\n' >&2
  read -r -p '确认开始重建吗？输入 y 或 yes 继续，其他输入取消：' answer
  case "$answer" in
    y|Y|yes|Yes|YES)
      return 0
      ;;
    *)
      log "INFO" "已取消，未调用平台 API。"
      return 1
      ;;
  esac
}

# resolve_deploy_wait_timeout 校验并解析 DEPLOY_WAIT_TIMEOUT（单位：秒），默认 900。
# 非法值必须显式失败：bash 算术会把 "abc"、"5m" 求值为 0，从而把配置错误静默伪装成
# 「立即超时」。0 也一并拒绝——它意味着一次都等不了，只可能是误配置。
resolve_deploy_wait_timeout() {
  local raw="${DEPLOY_WAIT_TIMEOUT:-}"
  if [[ -z "${raw//[[:space:]]/}" ]]; then
    DEPLOY_WAIT_TIMEOUT_SECONDS=900
    return 0
  fi
  if [[ ! "$raw" =~ ^[0-9]+$ ]]; then
    log "ERROR" "DEPLOY_WAIT_TIMEOUT 必须是纯数字秒数（例如 900），当前值：${raw}"
    return 2
  fi
  local seconds=$(( 10#$raw ))
  if (( seconds < 1 )); then
    log "ERROR" "DEPLOY_WAIT_TIMEOUT 必须大于 0 秒（0 无法等待任何部署），当前值：${raw}"
    return 2
  fi
  DEPLOY_WAIT_TIMEOUT_SECONDS="$seconds"
}

# wait_for_deployment_completion 等待后台部署编排进入终态。平台异步化后（202 受理），
# 部署在服务端脱离请求执行；这里轮询 subsystem-status 直到 READY/OFFBOARDED（成功）或
# PROVISION_FAILED（失败，详情见 status 子命令）。DEPLOY_WAIT_TIMEOUT 可覆盖默认 900 秒。
# 轮询节奏：先查询、后等待——首次查询立即发出，之后每次间隔 5 秒，仍以 SECONDS 作截止。
#
# 旧终态保护：202 受理后平台状态可能尚未从上一轮终态翻转，首次查询会读到「旧的 READY /
# PROVISION_FAILED」。因此只有在**本轮观察到过非终态**、或**同一个终态连续出现两次**之后，
# 才把终态当作本次结果；否则多等一个轮询间隔再确认。代价是「极快完成」多一次 5 秒确认，
# 但不会把上一次的成功/失败误判为本次结果。
wait_for_deployment_completion() {
  local application_code="$1" environment="$2" output_file="$3"
  local timeout="${DEPLOY_WAIT_TIMEOUT_SECONDS:-900}"
  local deadline=$(( SECONDS + timeout ))
  local state="" code=""
  local saw_active=false deferred_terminal=false
  while :; do
    code="$(http_request GET "/subsystem-status?application_code=${application_code}&environment=${environment}" "" "$output_file" 30)" || {
      diagnose_connection_failure "/subsystem-status"
      return 2
    }
    if [[ "$code" != "200" ]]; then
      print_api_error "$code" "$output_file" "/subsystem-status"
      return 2
    fi
    state="$(python3 -c 'import json,sys
try:
    print((json.load(sys.stdin).get("data") or {}).get("status", ""))
except Exception:
    print("")' < "$output_file" 2>/dev/null)"
    log "INFO" "部署状态：${state:-unknown}"
    case "$state" in
      READY|OFFBOARDED)
        if [[ "$saw_active" == true || "$deferred_terminal" == true ]]; then
          cp "$output_file" "${output_file}.final" 2>/dev/null || true
          return 0
        fi
        deferred_terminal=true
        log "INFO" "状态为 ${state}，但本轮尚未观察到处理中状态；等待一次确认（避免把上一次的旧终态当成本次结果）"
        ;;
      PROVISION_FAILED)
        if [[ "$saw_active" == true || "$deferred_terminal" == true ]]; then
          cp "$output_file" "${output_file}.final" 2>/dev/null || true
          return 1
        fi
        deferred_terminal=true
        log "INFO" "状态为 PROVISION_FAILED，但本轮尚未观察到处理中状态；等待一次确认（避免把上一次的旧终态当成本次结果）"
        ;;
      *)
        saw_active=true
        deferred_terminal=false
        ;;
    esac
    # 未进终态：仅在预算内才 sleep，且 sleep 之后重新确认预算，避免越过截止时间再发一次查询。
    (( SECONDS < deadline )) || break
    sleep 5
    (( SECONDS < deadline )) || break
  done
  log "ERROR" "等待部署完成超时（${timeout}s）；部署仍在后台执行，可稍后用 status 子命令继续查询"
  return 2
}

update_print_success() {
  python3 - "$1" <<'PY'
import json
import sys
with open(sys.argv[1], encoding="utf-8") as handle:
    envelope = json.load(handle)
data = envelope.get("data") or {}
print("子系统已重新部署：")
print(f"  状态：{data.get('status', '-')}")
public_url = data.get("public_url") or "-"
print(f"  对外访问地址：{public_url}")
print(f"  追踪号：{envelope.get('request_id') or '-'}")
PY
}

parse_update_args() {
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --api-base-url)
        [[ $# -ge 2 ]] || { log "ERROR" "--api-base-url 缺少参数"; exit 2; }
        API_BASE_URL="$2"; shift 2 ;;
      --platform-origin)
        [[ $# -ge 2 ]] || { log "ERROR" "--platform-origin 缺少参数"; exit 2; }
        PLATFORM_ORIGIN="$2"; shift 2 ;;
      --account)
        [[ $# -ge 2 ]] || { log "ERROR" "--account 缺少参数"; exit 2; }
        ACCOUNT="$2"; shift 2 ;;
      --password-stdin)
        PASSWORD_STDIN=true; shift ;;
      --cookie-file)
        [[ $# -ge 2 ]] || { log "ERROR" "--cookie-file 缺少参数"; exit 2; }
        COOKIE_FILE="$2"; shift 2 ;;
      --replace-existing-session)
        REPLACE_EXISTING_SESSION=true; shift ;;
      -y|--yes)
        ASSUME_YES=true; shift ;;
      --application-code)
        [[ $# -ge 2 ]] || { log "ERROR" "--application-code 缺少参数"; exit 2; }
        APPLICATION_CODE="$2"; shift 2 ;;
      --environment)
        [[ $# -ge 2 ]] || { log "ERROR" "--environment 缺少参数"; exit 2; }
        ENVIRONMENT="$2"; shift 2 ;;
      --issuer-alias)
        [[ $# -ge 2 ]] || { log "ERROR" "--issuer-alias 缺少参数"; exit 2; }
        ISSUER_ALIAS="$2"; shift 2 ;;
      -h|--help)
        update_usage; exit 0 ;;
      --)
        shift; break ;;
      *)
        log "ERROR" "未知参数：$1"
        update_usage >&2
        exit 2 ;;
    esac
  done
}

update_validate_and_normalize() {
  local normalized_file="$TEMP_DIR/normalized.txt"
  if ! python3 - "$APPLICATION_CODE" "$ENVIRONMENT" "$API_BASE_URL" "$PLATFORM_ORIGIN" "$normalized_file" <<'PY'
import re
import sys
from pathlib import Path
from urllib.parse import urlsplit

code, environment, api_base, platform_origin, output_path = sys.argv[1:]
code = code.strip().lower()
environment = environment.strip().lower()
api_base = api_base.strip().rstrip("/")
platform_origin = platform_origin.strip().rstrip("/")

errors = []
if not re.fullmatch(r"[a-z][a-z0-9._-]{0,63}", code):
    errors.append("application-code 必须匹配 ^[a-z][a-z0-9._-]{0,63}$")
if environment not in {"dev", "test", "staging", "prod"}:
    errors.append("environment 只能是 dev、test、staging 或 prod")
if not api_base or len(api_base) > 512 or any(ch.isspace() for ch in api_base):
    errors.append("api-base-url 必填、最长 512 字符且不能包含空白")
else:
    try:
        parsed = urlsplit(api_base)
        port = parsed.port
    except ValueError:
        errors.append("api-base-url 端口或 URL 格式无效")
    else:
        if parsed.scheme not in {"http", "https"} or not parsed.hostname:
            errors.append("api-base-url 必须是带主机名的 http/https URL")
        if parsed.username is not None or parsed.password is not None or parsed.query or parsed.fragment:
            errors.append("api-base-url 不能包含用户信息、query 或 fragment")
        if port is not None and not 1 <= port <= 65535:
            errors.append("api-base-url 端口必须在 1-65535 之间")
        if not platform_origin:
            platform_origin = f"{parsed.scheme}://{parsed.netloc}"

if platform_origin:
    try:
        origin = urlsplit(platform_origin)
        origin_port = origin.port
    except ValueError:
        errors.append("platform-origin 端口或 URL 格式无效")
    else:
        if origin.scheme not in {"http", "https"} or not origin.hostname:
            errors.append("platform-origin 必须是带主机名的 http/https origin")
        if origin.username is not None or origin.password is not None or origin.query or origin.fragment or origin.path not in {"", "/"}:
            errors.append("platform-origin 只能填写 origin，不能包含用户信息、路径、query 或 fragment")
        if origin_port is not None and not 1 <= origin_port <= 65535:
            errors.append("platform-origin 端口必须在 1-65535 之间")
else:
    errors.append("无法从 api-base-url 推导 platform-origin，请显式传入 --platform-origin")

if errors:
    print("\n".join(errors), file=sys.stderr)
    raise SystemExit(2)

Path(output_path).write_text("\n".join((code, environment, api_base, platform_origin)) + "\n", encoding="utf-8")
PY
  then
    return 2
  fi
  {
    IFS= read -r APPLICATION_CODE
    IFS= read -r ENVIRONMENT
    IFS= read -r API_BASE_URL
    IFS= read -r PLATFORM_ORIGIN
  } <"$normalized_file"

  validate_api_transport || return 2

  if [[ -z "$COOKIE_FILE" && -z "${ACCOUNT//[[:space:]]/}" ]]; then
    log "ERROR" "未使用 --cookie-file 时必须提供 --account"
    return 2
  fi
  if [[ -n "$COOKIE_FILE" && "$PASSWORD_STDIN" == true ]]; then
    log "ERROR" "--cookie-file 与 --password-stdin 不能同时使用"
    return 2
  fi
  if [[ -n "$COOKIE_FILE" && "$REPLACE_EXISTING_SESSION" == true ]]; then
    log "ERROR" "--cookie-file 与 --replace-existing-session 不能同时使用"
    return 2
  fi
}

cmd_update() {
  parse_update_args "$@"
  require_command curl
  require_command python3

  # 在发出任何请求之前就校验等待预算：非法值不能等到 202 之后才暴露。
  if ! resolve_deploy_wait_timeout; then
    exit 2
  fi

  TEMP_DIR="$(mktemp -d "${TMPDIR:-/tmp}/subsystem-update.XXXXXX")"
  RESPONSE_FILE="$TEMP_DIR/update-response.json"
  trap cleanup EXIT

  if update_required_configuration_missing; then
    log "ERROR" "update/retry/adopt 必须提供 --application-code 和 --environment"
    update_usage >&2
    exit 2
  fi
  if ! update_validate_and_normalize; then
    exit 2
  fi

  update_print_summary
  if ! update_confirm; then
    exit 0
  fi

  ensure_authenticated

  local payload_file="$TEMP_DIR/update.json"
  local status=""
  update_write_payload "$payload_file"

  local endpoint="/subsystem-update"
  local action="重建"
  if [[ "${ADOPT_MODE:-false}" == true ]]; then
    endpoint="/subsystem-adoption"
    action="收编（纳入部署状态机）"
  elif [[ "$RETRY_MODE" == true ]]; then
    endpoint="/subsystem-retry"
    action="重试部署"
  fi
  log "INFO" "开始${action} ${APPLICATION_CODE}/${ENVIRONMENT} 容器"
  status="$(http_post_json "$endpoint" "$payload_file" "$RESPONSE_FILE" 900)" || {
    diagnose_connection_failure "$endpoint"
    exit 1
  }

  if [[ "$status" == "202" ]]; then
    log "INFO" "部署请求已受理，正在后台执行；等待终态（DEPLOY_WAIT_TIMEOUT 可调，默认 900s）"
    if wait_for_deployment_completion "$APPLICATION_CODE" "$ENVIRONMENT" "$RESPONSE_FILE"; then
      update_print_success "$RESPONSE_FILE"
      exit 0
    fi
    print_deployment_failed_summary "${RESPONSE_FILE}.final"
    exit 1
  fi
  if [[ "$status" != "200" ]]; then
    print_api_error "$status" "$RESPONSE_FILE" "$endpoint"
    exit 1
  fi

  update_print_success "$RESPONSE_FILE"
}
