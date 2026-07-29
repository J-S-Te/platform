#!/usr/bin/env bash
# 通过基础平台管理 API 永久撤销一个已接入子系统的单个非开发环境。
#
# 该脚本只删除指定 Environment 派生的 LoginTarget 和 OAuth Client 配置；
# 不删除 Application、其他环境、容器镜像、网关进程或子系统业务数据。
# 绝不能用于子系统代码、镜像、功能模块或业务迁移的常规发布升级。

set -Eeuo pipefail
IFS=$'\n\t'
umask 077

API_BASE_URL="${BASIC_PLATFORM_API_BASE_URL:-http://127.0.0.1:8081/api/v1}"
APPLICATION_CODE=""
ENVIRONMENT=""
CONFIRMATION_CODE=""
ACCOUNT=""
PASSWORD_STDIN=false
REPLACE_EXISTING_SESSION=false
COOKIE_FILE=""
OWNS_SESSION=false
TEMP_DIR=""

log() {
  local level="$1"
  shift
  printf '[%s] [%s] %s\n' "$(date '+%Y-%m-%d %H:%M:%S')" "$level" "$*" >&2
}

usage() {
  cat <<'USAGE'
用法：
  subsystem-offboarding.sh [认证参数] [撤销参数]

撤销参数：
  --application-code CODE       要撤销的 Application.Code，例如 contract_management
  --environment ENV             要撤销的环境，只允许 test、staging 或 prod
  --confirm CODE/ENV            必填，必须精确等于 --application-code/--environment

认证参数：
  --api-base-url URL             平台 API 根地址；默认 BASIC_PLATFORM_API_BASE_URL，
                                 未设置时为 http://127.0.0.1:8081/api/v1
  --account ACCOUNT              具备删除应用环境权限的基础平台管理员账号
  --password-stdin               从标准输入读取一行密码；未指定时安全交互输入
  --cookie-file FILE             复用已登录的平台 Cookie 文件；指定后不再使用账号口令登录
  --replace-existing-session     撤销该账号原有会话后登录。仅在明确需要时使用
  -h, --help                     显示帮助

示例（仅撤销合同管理系统 prod，保留 Application 与 dev）：
  bash scripts/subsystem-offboarding.sh \
    --application-code contract_management \
    --environment prod \
    --confirm contract_management/prod \
    --account admin

CI 示例（密码不出现在命令行和进程列表中）：
  printf '%s\n' "$PLATFORM_ADMIN_PASSWORD" | bash scripts/subsystem-offboarding.sh \
    --password-stdin --account admin \
    --application-code contract_management \
    --environment prod \
    --confirm contract_management/prod

安全边界：
  1. 不允许删除 dev，也不允许删除基础平台 platform。
  2. 不提供 --force。若环境仍有关联配置命名空间或审计回执，平台会拒绝删除。
  3. 只删除该环境派生的登录目标和 OAuth Client 配置，不会删除 Docker、Nginx、Application 或业务数据。
  4. 只适用于该环境永久下线；绝不能为了代码、镜像、功能模块或日常发布而撤销后重建。
  5. 脚本自行登录时会在结束前主动退出，避免单终端登录策略留下占用会话。
USAGE
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || {
    log "ERROR" "缺少命令：$1"
    exit 2
  }
}

parse_args() {
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --application-code)
        [[ $# -ge 2 ]] || { log "ERROR" "--application-code 缺少参数"; exit 2; }
        APPLICATION_CODE="$2"; shift 2 ;;
      --environment)
        [[ $# -ge 2 ]] || { log "ERROR" "--environment 缺少参数"; exit 2; }
        ENVIRONMENT="$2"; shift 2 ;;
      --confirm)
        [[ $# -ge 2 ]] || { log "ERROR" "--confirm 缺少参数"; exit 2; }
        CONFIRMATION_CODE="$2"; shift 2 ;;
      --api-base-url)
        [[ $# -ge 2 ]] || { log "ERROR" "--api-base-url 缺少参数"; exit 2; }
        API_BASE_URL="$2"; shift 2 ;;
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
      -h|--help)
        usage; exit 0 ;;
      *)
        log "ERROR" "未知参数：$1"
        usage >&2
        exit 2 ;;
    esac
  done
}

validate_and_normalize() {
  local normalized_file="$TEMP_DIR/normalized.txt"

  if ! python3 - "$APPLICATION_CODE" "$ENVIRONMENT" "$CONFIRMATION_CODE" "$API_BASE_URL" "$normalized_file" <<'PY'
import re
import sys
from pathlib import Path
from urllib.parse import urlsplit

code, environment, confirmation, api_base, output_path = sys.argv[1:]
code = code.strip().lower()
environment = environment.strip().lower()
confirmation = confirmation.strip()
api_base = api_base.strip().rstrip("/")
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

if errors:
    print("\n".join(errors), file=sys.stderr)
    raise SystemExit(2)

Path(output_path).write_text("\n".join((code, environment, confirmation, api_base)) + "\n", encoding="utf-8")
PY
  then
    exit 2
  fi

  {
    IFS= read -r APPLICATION_CODE
    IFS= read -r ENVIRONMENT
    IFS= read -r CONFIRMATION_CODE
    IFS= read -r API_BASE_URL
  } <"$normalized_file"

  if [[ -z "$COOKIE_FILE" && -z "${ACCOUNT//[[:space:]]/}" ]]; then
    log "ERROR" "未使用 --cookie-file 时必须提供 --account"
    exit 2
  fi
  if [[ -n "$COOKIE_FILE" && "$PASSWORD_STDIN" == true ]]; then
    log "ERROR" "--cookie-file 与 --password-stdin 不能同时使用"
    exit 2
  fi
  if [[ -n "$COOKIE_FILE" && "$REPLACE_EXISTING_SESSION" == true ]]; then
    log "ERROR" "--cookie-file 与 --replace-existing-session 不能同时使用"
    exit 2
  fi
}

write_login_payload() {
  local destination="$1"
  local password="$2"
  local password_file="$TEMP_DIR/password.txt"

  printf '%s' "$password" >"$password_file"
  python3 - "$ACCOUNT" "$REPLACE_EXISTING_SESSION" "$password_file" >"$destination" <<'PY'
import json
import sys
from pathlib import Path

account, replace, password_path = sys.argv[1:]
password = Path(password_path).read_text(encoding="utf-8")
json.dump({
    "account": account.strip(),
    "password": password,
    "login_type": "password",
    "replace_existing_session": replace == "true",
}, sys.stdout, ensure_ascii=False)
PY
  : >"$password_file"
  rm -f "$password_file"
}

http_post_json() {
  local endpoint="$1"
  local payload_file="$2"
  local output_file="$3"
  curl --silent --show-error \
    --connect-timeout 10 --max-time 120 \
    --cookie "$COOKIE_FILE" --cookie-jar "$COOKIE_FILE" \
    --header 'Accept: application/json' \
    --header 'Content-Type: application/json' \
    --request POST \
    --data-binary "@${payload_file}" \
    --output "$output_file" \
    --write-out '%{http_code}' \
    "${API_BASE_URL}${endpoint}"
}

http_get() {
  local endpoint="$1"
  local output_file="$2"
  curl --silent --show-error \
    --connect-timeout 10 --max-time 120 \
    --cookie "$COOKIE_FILE" --cookie-jar "$COOKIE_FILE" \
    --header 'Accept: application/json' \
    --request GET \
    --output "$output_file" \
    --write-out '%{http_code}' \
    "${API_BASE_URL}${endpoint}"
}

http_delete_json() {
  local endpoint="$1"
  local payload_file="$2"
  local output_file="$3"
  curl --silent --show-error \
    --connect-timeout 10 --max-time 120 \
    --cookie "$COOKIE_FILE" --cookie-jar "$COOKIE_FILE" \
    --header 'Accept: application/json' \
    --header 'Content-Type: application/json' \
    --request DELETE \
    --data-binary "@${payload_file}" \
    --output "$output_file" \
    --write-out '%{http_code}' \
    "${API_BASE_URL}${endpoint}"
}

print_api_error() {
  local http_status="$1"
  local response_file="$2"
  python3 - "$http_status" "$response_file" <<'PY'
import json
import sys

status, path = sys.argv[1:]
try:
    with open(path, encoding="utf-8") as handle:
        payload = json.load(handle)
except Exception:
    print(f"HTTP {status}：平台返回了无法解析的响应", file=sys.stderr)
    raise SystemExit
code = payload.get("code") or "UNKNOWN"
message = payload.get("message") or "请求失败"
request_id = payload.get("request_id") or "-"
print(f"HTTP {status} {code}：{message}（追踪号：{request_id}）", file=sys.stderr)
if code == "AUTH_CONCURRENT_SESSION":
    print("账号已有有效会话。请先在原终端退出，或明确添加 --replace-existing-session。", file=sys.stderr)
elif code == "IAM_ENVIRONMENT_DELETE_BLOCKED":
    print("该环境仍有配置命名空间或审计回执。请先按保留策略处理，脚本不提供 --force。", file=sys.stderr)
elif code == "IAM_VERSION_CONFLICT":
    print("环境配置已变化；请重新执行脚本以重新读取版本后再确认删除。", file=sys.stderr)
elif code == "AUTH_FORBIDDEN":
    print("当前基础平台管理员缺少 platform:application-environment:delete 权限。", file=sys.stderr)
PY
}

login() {
  local password=""
  local login_payload="$TEMP_DIR/login.json"
  local login_response="$TEMP_DIR/login-response.json"
  local status=""

  if [[ "$PASSWORD_STDIN" == true ]]; then
    IFS= read -r password || true
  else
    if [[ ! -t 0 ]]; then
      log "ERROR" "标准输入不是终端；请使用 --password-stdin 或 --cookie-file"
      exit 2
    fi
    read -r -s -p "平台管理员密码: " password
    printf '\n' >&2
  fi
  if [[ -z "$password" ]]; then
    log "ERROR" "密码不能为空"
    exit 2
  fi

  write_login_payload "$login_payload" "$password"
  password=""
  unset password

  status="$(http_post_json "/auth/login" "$login_payload" "$login_response")" || {
    log "ERROR" "无法连接平台登录接口：${API_BASE_URL}/auth/login"
    exit 1
  }
  : >"$login_payload"

  if [[ "$status" != "200" ]]; then
    print_api_error "$status" "$login_response"
    exit 1
  fi
  OWNS_SESSION=true
  log "INFO" "平台管理员认证成功"
}

logout_owned_session() {
  [[ "$OWNS_SESSION" == true && -n "$COOKIE_FILE" && -f "$COOKIE_FILE" ]] || return 0
  curl --silent --show-error \
    --connect-timeout 5 --max-time 15 \
    --cookie "$COOKIE_FILE" --cookie-jar "$COOKIE_FILE" \
    --header 'Accept: application/json' \
    --request POST \
    --output /dev/null \
    "${API_BASE_URL}/auth/logout" >/dev/null 2>&1 || true
  OWNS_SESSION=false
}

cleanup() {
  local status=$?
  trap - EXIT
  logout_owned_session
  if [[ -n "$TEMP_DIR" && -d "$TEMP_DIR" ]]; then
    rm -rf "$TEMP_DIR"
  fi
  exit "$status"
}

application_list_endpoint() {
  python3 - "$APPLICATION_CODE" <<'PY'
import sys
from urllib.parse import quote
print("/applications?keyword=" + quote(sys.argv[1], safe="") + "&page=1&page_size=100")
PY
}

select_application_id() {
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

select_environment() {
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

write_delete_payload() {
  local version="$1"
  local destination="$2"
  python3 - "$CONFIRMATION_CODE" "$version" >"$destination" <<'PY'
import json
import sys
confirmation, version = sys.argv[1:]
json.dump({"confirmation_code": confirmation, "version": int(version)}, sys.stdout, ensure_ascii=False)
PY
}

print_success() {
  local response_file="$1"
  python3 - "$APPLICATION_CODE" "$ENVIRONMENT" "$response_file" <<'PY'
import json
import sys

code, environment, path = sys.argv[1:]
try:
    with open(path, encoding="utf-8") as handle:
        envelope = json.load(handle)
except Exception as exc:
    print(f"撤销完成，但无法解析响应：{exc}")
    raise SystemExit
print("子系统环境接入已撤销：")
print(f"  应用：{code}")
print(f"  环境：{environment}")
print("  已清理：该环境派生的 LoginTarget 与 OAuth Client 配置")
print("  保留：Application、其他环境（含 dev）、Docker/Nginx、子系统业务数据")
print(f"  追踪号：{envelope.get('request_id') or '-'}")
PY
}

main() {
  parse_args "$@"
  require_command curl
  require_command python3

  TEMP_DIR="$(mktemp -d "${TMPDIR:-/tmp}/subsystem-offboarding.XXXXXX")"
  trap cleanup EXIT
  validate_and_normalize

  if [[ -n "$COOKIE_FILE" ]]; then
    if [[ ! -r "$COOKIE_FILE" ]]; then
      log "ERROR" "Cookie 文件不可读：$COOKIE_FILE"
      exit 2
    fi
    local source_cookie_file="$COOKIE_FILE"
    COOKIE_FILE="$TEMP_DIR/cookies.txt"
    cp "$source_cookie_file" "$COOKIE_FILE"
  else
    COOKIE_FILE="$TEMP_DIR/cookies.txt"
    : >"$COOKIE_FILE"
    login
  fi

  local applications_response="$TEMP_DIR/applications.json"
  local environments_response="$TEMP_DIR/environments.json"
  local delete_payload="$TEMP_DIR/delete-environment.json"
  local delete_response="$TEMP_DIR/delete-environment-response.json"
  local status=""
  local application_id=""
  local environment_details=""
  local environment_id=""
  local environment_version=""

  status="$(http_get "$(application_list_endpoint)" "$applications_response")" || {
    log "ERROR" "无法连接应用查询接口：${API_BASE_URL}/applications"
    exit 1
  }
  if [[ "$status" != "200" ]]; then
    print_api_error "$status" "$applications_response"
    exit 1
  fi
  application_id="$(select_application_id "$applications_response")" || exit 1

  status="$(http_get "/applications/${application_id}/environments?page=1&page_size=100" "$environments_response")" || {
    log "ERROR" "无法连接环境查询接口：${API_BASE_URL}/applications/${application_id}/environments"
    exit 1
  }
  if [[ "$status" != "200" ]]; then
    print_api_error "$status" "$environments_response"
    exit 1
  fi
  environment_details="$(select_environment "$ENVIRONMENT" "$environments_response")" || exit 1
  {
    IFS= read -r environment_id
    IFS= read -r environment_version
  } <<<"$environment_details"

  write_delete_payload "$environment_version" "$delete_payload"
  log "INFO" "开始撤销 ${APPLICATION_CODE}/${ENVIRONMENT}（仅清理该环境的登录目标与 OAuth 配置）"
  status="$(http_delete_json "/applications/${application_id}/environments/${environment_id}" "$delete_payload" "$delete_response")" || {
    log "ERROR" "无法连接环境删除接口：${API_BASE_URL}/applications/${application_id}/environments/${environment_id}"
    exit 1
  }
  if [[ "$status" != "200" ]]; then
    print_api_error "$status" "$delete_response"
    exit 1
  fi

  print_success "$delete_response"
}

main "$@"
