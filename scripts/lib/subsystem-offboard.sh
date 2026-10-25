#!/usr/bin/env bash
# subsystem.sh offboard 子命令（批次 5 库化，逐字迁移）；仅含函数定义，无顶层副作用——由 scripts/subsystem.sh 入口在全局变量区之后 source。

# ============================ offboard 子命令 ============================
offboard_usage() {
  cat <<'USAGE'
用法：subsystem.sh offboard [认证参数] [撤销参数]

offboard 是深清理子系统的入口。默认会依次：
  1. 调用 POST /subsystem-teardown 停止容器、删除 .env.local、移除门户网关入口、重新加载 nginx
  2. 调用 DELETE /applications/<id>/environments/<env_id> 删除 DB 记录
  3. 若指定 --delete-application，再调用 DELETE /applications/<id> 删除整个 application

撤销参数：
  --application-code CODE       要撤销的 Application.Code
  --environment ENV             要撤销的环境，只允许 test、staging 或 prod
  --confirm CODE/ENV            必填，必须精确等于 <application-code>/<environment>
  --shallow                     退回旧版语义：只删 DB 记录（仅供紧急修复使用）
  --delete-application          在 environment 删除后顺带删除 application（需 environment 已是最后一个）

安全边界：
  1. 不允许删除 dev，也不允许删除基础平台 platform。
  2. 不提供 --force。若环境仍有关联配置命名空间或审计回执，平台会拒绝删除。
  3. 默认深清理：停止容器 + 删除 .env.local + 删除门户网关入口 + 重新加载 nginx + 删除 DB 记录。
  4. 只适用于该环境永久下线；绝不能为了代码、镜像、功能模块或日常发布而撤销后重建。
USAGE
}

offboard_required_configuration_missing() {
  [[ -z "${APPLICATION_CODE//[[:space:]]/}" || -z "${ENVIRONMENT//[[:space:]]/}" || -z "${CONFIRMATION_CODE//[[:space:]]/}" ]]
}

offboard_application_list_endpoint() {
  python3 - "$APPLICATION_CODE" <<'PY'
import sys
from urllib.parse import quote
print("/applications?keyword=" + quote(sys.argv[1], safe="") + "&page=1&page_size=100")
PY
}

offboard_select_application_id() {
  local response_file="$1"
  python3 - "$APPLICATION_CODE" "$response_file" <<'PY'
import json
import sys

code, path = sys.argv[1:]
try:
    with open(path, encoding="utf-8") as handle:
        envelope = json.load(handle)
    data = envelope.get("data") or {}
    items = data.get("items") or []
except Exception as exc:
    print(f"无法解析应用列表响应：{exc}", file=sys.stderr)
    raise SystemExit(1)
matches = [item for item in items if isinstance(item, dict) and item.get("code") == code]
application_id = matches[0].get("application_id") if len(matches) == 1 else None
if not isinstance(application_id, str) or not application_id:
    print(f"未找到唯一的应用 {code}；请先确认接入状态。", file=sys.stderr)
    raise SystemExit(1)
print(application_id)
PY
}

offboard_select_environment() {
  local expected_environment="$1"
  local response_file="$2"
  python3 - "$expected_environment" "$response_file" <<'PY'
import json
import sys

environment, path = sys.argv[1:]
try:
    with open(path, encoding="utf-8") as handle:
        envelope = json.load(handle)
    data = envelope.get("data") or {}
    items = data.get("items") or []
except Exception as exc:
    print(f"无法解析环境列表响应：{exc}", file=sys.stderr)
    raise SystemExit(1)
matches = [item for item in items if isinstance(item, dict) and item.get("environment") == environment]
if len(matches) != 1:
    print(f"未找到唯一的环境 {environment}；不会执行删除。", file=sys.stderr)
    raise SystemExit(1)
item = matches[0]
environment_id = item.get("environment_id")
version = item.get("version")
if not isinstance(environment_id, str) or not environment_id or not isinstance(version, int) or version <= 0:
    print("环境记录缺少有效 id 或 version；不会执行删除。", file=sys.stderr)
    raise SystemExit(1)
print(environment_id)
print(version)
PY
}

offboard_count_remaining_environments() {
  local response_file="$1"
  python3 - "$response_file" <<'PY'
import json
import sys
with open(sys.argv[1], encoding="utf-8") as handle:
    envelope = json.load(handle)
data = envelope.get("data") or {}
items = data.get("items") or []
print(len([item for item in items if isinstance(item, dict) and item.get("environment")]))
PY
}

offboard_write_delete_payload() {
  local version="$1"
  local destination="$2"
  python3 - "$CONFIRMATION_CODE" "$version" >"$destination" <<'PY'
import json
import sys
confirmation, version = sys.argv[1:]
json.dump({"confirmation_code": confirmation, "version": int(version)}, sys.stdout, ensure_ascii=False)
PY
}

offboard_write_teardown_payload() {
  python3 - "$APPLICATION_CODE" "$ENVIRONMENT" >"$1" <<'PY'
import json
import sys
code, environment = sys.argv[1:]
json.dump({
    "application_code": code.strip(),
    "environment": environment.strip(),
}, sys.stdout, ensure_ascii=False)
PY
}

offboard_validate_and_normalize() {
  local normalized_file="$TEMP_DIR/normalized.txt"

  if ! python3 - "$APPLICATION_CODE" "$ENVIRONMENT" "$CONFIRMATION_CODE" "$API_BASE_URL" "$PLATFORM_ORIGIN" "$normalized_file" <<'PY'
import re
import sys
from pathlib import Path
from urllib.parse import urlsplit

code, environment, confirmation, api_base, platform_origin, output_path = sys.argv[1:]
code = code.strip().lower()
environment = environment.strip().lower()
confirmation = confirmation.strip()
api_base = api_base.strip().rstrip("/")
platform_origin = platform_origin.strip().rstrip("/")
errors = []

if not re.fullmatch(r"[a-z][a-z0-9._-]{0,63}", code):
    errors.append("application-code 必须匹配 ^[a-z][a-z0-9._-]{0,63}$")
if environment not in {"test", "staging", "prod"}:
    if environment == "dev":
        errors.append("为保护开发环境，environment 不允许使用 dev")
    else:
        errors.append("environment 只能是 test、staging 或 prod")
if code == "platform":
    errors.append("不允许通过本脚本删除基础平台 platform 的环境")
if confirmation != f"{code}/{environment}":
    errors.append("--confirm 必须精确等于 <application-code>/<environment>")
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

Path(output_path).write_text("\n".join((code, environment, confirmation, api_base, platform_origin)) + "\n", encoding="utf-8")
PY
  then
    return 2
  fi

  {
    IFS= read -r APPLICATION_CODE
    IFS= read -r ENVIRONMENT
    IFS= read -r CONFIRMATION_CODE
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

offboard_print_summary() {
  cat >&2 <<EOF_SUMMARY

即将撤销以下子系统接入（不可恢复）：
  应用：${APPLICATION_CODE}
  环境：${ENVIRONMENT}
  --confirm：${CONFIRMATION_CODE}
EOF_SUMMARY
  if [[ "$SHALLOW_OFFBOARD" == true ]]; then
    printf '  模式：shallow — 仅删除 DB 记录（环境派生的 LoginTarget 与 OAuth Client）\n' >&2
    printf '  容器 / .env.local / 门户网关：不会清理\n' >&2
  else
    printf '  模式：deep（默认）— 停止容器 + 删除 .env.local + 移除门户网关入口 + 重新加载 nginx + 删除 DB 记录\n' >&2
  fi
  if [[ "$DELETE_APPLICATION" == true ]]; then
    printf '  附加：--delete-application — 在 environment 删除后顺带删除 application\n' >&2
  fi
}

offboard_confirm() {
  [[ "$ASSUME_YES" == true ]] && return 0
  local answer=""
  printf '\n' >&2
  read -r -p '确认开始深清理吗？输入 y 或 yes 继续，其他输入取消：' answer
  case "$answer" in
    y|Y|yes|Yes|YES)
      return 0
      ;;
    *)
      log "INFO" "已取消，未调用平台 API，未删除任何子系统配置"
      return 1
      ;;
  esac
}

offboard_print_success() {
  python3 - "$APPLICATION_CODE" "$ENVIRONMENT" <<'PY'
import json
import sys
code, environment = sys.argv[1:]
print("子系统环境接入已撤销：")
print(f"  应用：{code}")
print(f"  环境：{environment}")
print("  已清理：停止容器 + 删除 .env.local + 移除门户网关入口 + 重新加载 nginx + 删除该环境派生的 LoginTarget 与 OAuth Client 配置")
print("  保留：Application（除非同时指定 --delete-application）、其他环境（含 dev）、子系统业务数据")
PY
}

parse_offboard_args() {
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
      --confirm)
        [[ $# -ge 2 ]] || { log "ERROR" "--confirm 缺少参数"; exit 2; }
        CONFIRMATION_CODE="$2"; shift 2 ;;
      --shallow)
        SHALLOW_OFFBOARD=true; shift ;;
      --delete-application)
        DELETE_APPLICATION=true; shift ;;
      -h|--help)
        offboard_usage; exit 0 ;;
      --)
        shift; break ;;
      *)
        log "ERROR" "未知参数：$1"
        offboard_usage >&2
        exit 2 ;;
    esac
  done
}

cmd_offboard() {
  parse_offboard_args "$@"
  require_command curl
  require_command python3

  TEMP_DIR="$(mktemp -d "${TMPDIR:-/tmp}/subsystem-offboard.XXXXXX")"
  trap cleanup EXIT

  if offboard_required_configuration_missing; then
    log "ERROR" "offboard 必须提供 --application-code、--environment、--confirm"
    offboard_usage >&2
    exit 2
  fi
  if ! offboard_validate_and_normalize; then
    exit 2
  fi

  offboard_print_summary
  if ! offboard_confirm; then
    exit 0
  fi

  ensure_authenticated

  local applications_response="$TEMP_DIR/applications.json"
  local environments_response="$TEMP_DIR/environments.json"
  local teardown_payload="$TEMP_DIR/teardown.json"
  local teardown_response="$TEMP_DIR/teardown-response.json"
  local delete_payload="$TEMP_DIR/delete-environment.json"
  local delete_response="$TEMP_DIR/delete-environment-response.json"
  local delete_app_response="$TEMP_DIR/delete-application-response.json"
  local status=""
  local application_id=""
  local environment_details=""
  local environment_id=""
  local environment_version=""

  status="$(http_get "$(offboard_application_list_endpoint)" "$applications_response")" || {
    log "ERROR" "无法连接应用查询接口：${API_BASE_URL}/applications"
    exit 1
  }
  if [[ "$status" != "200" ]]; then
    print_api_error "$status" "$applications_response"
    exit 1
  fi
  application_id="$(offboard_select_application_id "$applications_response")" || exit 1

  status="$(http_get "/applications/${application_id}/environments?page=1&page_size=100" "$environments_response")" || {
    log "ERROR" "无法连接环境查询接口：${API_BASE_URL}/applications/${application_id}/environments"
    exit 1
  }
  if [[ "$status" != "200" ]]; then
    print_api_error "$status" "$environments_response"
    exit 1
  fi
  environment_details="$(offboard_select_environment "$ENVIRONMENT" "$environments_response")" || exit 1
  {
    IFS= read -r environment_id
    IFS= read -r environment_version
  } <<<"$environment_details"

  if [[ "$SHALLOW_OFFBOARD" != true ]]; then
    offboard_write_teardown_payload "$teardown_payload"
    log "INFO" "深清理 ${APPLICATION_CODE}/${ENVIRONMENT}：停止容器、删除 .env.local、移除门户网关入口、重新加载 nginx"
    status="$(http_post_json "/subsystem-teardown" "$teardown_payload" "$teardown_response" 300)" || {
      diagnose_connection_failure "/subsystem-teardown"
      exit 1
    }
    if [[ "$status" != "200" ]]; then
      print_api_error "$status" "$teardown_response" "/subsystem-teardown"
      exit 1
    fi
    log "INFO" "容器与网关清理完成"
  fi

  offboard_write_delete_payload "$environment_version" "$delete_payload"
  log "INFO" "开始删除 ${APPLICATION_CODE}/${ENVIRONMENT} 的 DB 记录（环境派生的 LoginTarget 与 OAuth Client 配置）"
  status="$(http_delete_json "/applications/${application_id}/environments/${environment_id}" "$delete_payload" "$delete_response")" || {
    log "ERROR" "无法连接环境删除接口：${API_BASE_URL}/applications/${application_id}/environments/${environment_id}"
    exit 1
  }
  if [[ "$status" != "200" ]]; then
    print_api_error "$status" "$delete_response" "/applications/${application_id}/environments/${environment_id}"
    exit 1
  fi

  if [[ "$DELETE_APPLICATION" == true ]]; then
    # 删除 application 前必须保证它下面已经没有其他 environment；否则平台会拒绝。
    local remaining_response="$TEMP_DIR/environments-remaining.json"
    status="$(http_get "/applications/${application_id}/environments?page=1&page_size=100" "$remaining_response")" || {
      log "ERROR" "无法重新查询环境列表：${API_BASE_URL}/applications/${application_id}/environments"
      exit 1
    }
    if [[ "$status" != "200" ]]; then
      print_api_error "$status" "$remaining_response"
      exit 1
    fi
    local remaining_count
    remaining_count="$(offboard_count_remaining_environments "$remaining_response")" || exit 1
    if [[ "$remaining_count" != "0" ]]; then
      log "ERROR" "application 仍存在 ${remaining_count} 个环境，--delete-application 不会执行。请先清理其他环境。"
      exit 2
    fi
    log "INFO" "开始删除 application ${APPLICATION_CODE}"
    status="$(http_delete_json "/applications/${application_id}" "" "$delete_app_response" 60)" || {
      log "ERROR" "无法连接 application 删除接口：${API_BASE_URL}/applications/${application_id}"
      exit 1
    }
    if [[ "$status" != "200" ]]; then
      print_api_error "$status" "$delete_app_response" "/applications/${application_id}"
      exit 1
    fi
  fi

  offboard_print_success
}
