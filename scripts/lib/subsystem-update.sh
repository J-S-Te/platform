#!/usr/bin/env bash
# subsystem.sh update 子命令（批次 5 库化，逐字迁移）；仅含函数定义，无顶层副作用——由 scripts/subsystem.sh 入口在全局变量区之后 source。

# ============================ update 子命令 ============================
update_usage() {
  cat <<'USAGE'
用法：subsystem.sh update|retry [认证参数] --application-code CODE --environment ENV

update/retry 保留已有应用、环境登记及业务数据，并按受控部署流程刷新运行配置和容器。
重试可能重新下发接入凭据与组件授权凭据，部署状态会随执行结果更新。
要修改 BaseURL / UpstreamURL / OAuth 配置，请先通过 PATCH /environments 与 /oauth-clients
接口完成受控变更，然后调用本子命令让正在运行的子系统容器应用新配置。

必填参数：
  --application-code CODE   Application.Code，例如 contract_management
  --environment ENV         Environment.Environment：dev/test/staging/prod
  --issuer-alias ALIAS     认证提供方别名，例如 keycloak 或 platform

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

# wait_for_deployment_completion 等待后台部署编排进入终态。平台异步化后（202 受理），
# 部署在服务端脱离请求执行；这里轮询 subsystem-status 直到 READY/OFFBOARDED（成功）或
# PROVISION_FAILED（失败，详情见 status 子命令）。DEPLOY_WAIT_TIMEOUT 可覆盖默认 900 秒。
wait_for_deployment_completion() {
  local application_code="$1" environment="$2" output_file="$3"
  local deadline=$(( SECONDS + ${DEPLOY_WAIT_TIMEOUT:-900} ))
  local state="" code=""
  while (( SECONDS < deadline )); do
    sleep 5
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
        cp "$output_file" "${output_file}.final" 2>/dev/null || true
        return 0
        ;;
      PROVISION_FAILED)
        cp "$output_file" "${output_file}.final" 2>/dev/null || true
        return 1
        ;;
    esac
  done
  log "ERROR" "等待部署完成超时（${DEPLOY_WAIT_TIMEOUT:-900}s）；部署仍在后台执行，可稍后用 status 子命令继续查询"
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

  TEMP_DIR="$(mktemp -d "${TMPDIR:-/tmp}/subsystem-update.XXXXXX")"
  RESPONSE_FILE="$TEMP_DIR/update-response.json"
  trap cleanup EXIT

  if update_required_configuration_missing; then
    log "ERROR" "update 必须提供 --application-code 和 --environment"
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
  if [[ "$RETRY_MODE" == true ]]; then
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
