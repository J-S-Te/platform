#!/usr/bin/env bash
# 通过基础平台管理 API 登记并触发部署一个子系统。
#
# 本脚本是“统一登录目标”的唯一运维入口。它会一次性创建：
#   Application、Environment、相对路径 LoginTarget、OAuth Client，
# 并由后端受控 provisioner 写入子系统 OIDC 配置、启动子系统、更新门户网关。
# 前端不再提供这些基础设施配置项。

set -Eeuo pipefail
IFS=$'\n\t'
umask 077

API_BASE_URL="${BASIC_PLATFORM_API_BASE_URL:-http://127.0.0.1:8081/api/v1}"
APPLICATION_CODE=""
APPLICATION_NAME=""
DESCRIPTION=""
ENVIRONMENT="prod"
PUBLIC_BASE_URL=""
UPSTREAM_URL=""
PATH_PREFIX=""
CLIENT_TYPE="confidential"
ACCOUNT=""
PASSWORD_STDIN=false
REPLACE_EXISTING_SESSION=false
COOKIE_FILE=""
OWNS_SESSION=false
TEMP_DIR=""
RESPONSE_FILE=""

log() {
  local level="$1"
  shift
  printf '[%s] [%s] %s\n' "$(date '+%Y-%m-%d %H:%M:%S')" "$level" "$*" >&2
}

usage() {
  cat <<'USAGE'
用法：
  subsystem-onboarding.sh [认证参数] [接入参数]

接入参数（对应后端 POST /api/v1/subsystem-onboarding）：
  --application-code CODE       Application.Code，必填，例如 contract_management
  --application-name NAME       Application.Name，必填，例如 合同管理系统
  --environment ENV             Environment.Environment：dev/test/staging/prod，默认 prod
  --public-base-url URL          Environment.BaseURL，对外统一入口，必填，例如 http://localhost:8081
  --upstream-url URL             Environment.UpstreamURL，子系统内网地址，必填，例如 http://contract-api:8081
  --path-prefix PATH             Environment.PathPrefix，默认 /<application-code>，例如 /contract_management
  --client-type TYPE             OAuth Client 类型：confidential/public，默认 confidential
  --description TEXT             应用说明；默认“门户路径接入：<path-prefix>”

派生配置（无需手工填写）：
  LoginTarget.TargetURI          固定使用相对路径 <path-prefix>/
  OAuth redirect_uri             <public-base-url><path-prefix>/auth/callback
  OAuth client_id                <application-code>-<environment>-web

认证参数：
  --api-base-url URL             平台 API 根地址；默认 BASIC_PLATFORM_API_BASE_URL，
                                 未设置时为 http://127.0.0.1:8081/api/v1
  --account ACCOUNT              具备子系统接入权限的平台管理员账号
  --password-stdin               从标准输入读取一行密码；未指定时安全交互输入
  --cookie-file FILE             复用已登录的平台 Cookie 文件；指定后不再使用账号口令登录
  --replace-existing-session     撤销该账号原有会话后登录。仅在明确需要时使用
  -h, --help                     显示帮助

示例（本地统一前端入口）：
  bash scripts/subsystem-onboarding.sh \
    --application-code contract_management \
    --application-name '合同管理系统' \
    --environment prod \
    --public-base-url http://localhost:8081 \
    --upstream-url http://contract-api:8081 \
    --path-prefix /contract_management \
    --client-type confidential \
    --account admin

CI 示例（密码不出现在命令行和进程列表中）：
  printf '%s\n' "$PLATFORM_ADMIN_PASSWORD" | bash scripts/subsystem-onboarding.sh \
    --password-stdin --account admin \
    --application-code contract_management \
    --application-name '合同管理系统' \
    --public-base-url https://portal.example.com \
    --upstream-url http://contract-api:8081

说明：
  1. 脚本自行登录时会在结束前主动退出，避免单终端登录策略留下占用会话。
  2. --cookie-file 代表复用调用者已有会话，脚本不会退出该会话。
  3. portal-gateway.sh 是底层网关维护工具，不替代本脚本的完整接入流程。
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
      --application-name)
        [[ $# -ge 2 ]] || { log "ERROR" "--application-name 缺少参数"; exit 2; }
        APPLICATION_NAME="$2"; shift 2 ;;
      --description)
        [[ $# -ge 2 ]] || { log "ERROR" "--description 缺少参数"; exit 2; }
        DESCRIPTION="$2"; shift 2 ;;
      --environment)
        [[ $# -ge 2 ]] || { log "ERROR" "--environment 缺少参数"; exit 2; }
        ENVIRONMENT="$2"; shift 2 ;;
      --public-base-url)
        [[ $# -ge 2 ]] || { log "ERROR" "--public-base-url 缺少参数"; exit 2; }
        PUBLIC_BASE_URL="$2"; shift 2 ;;
      --upstream-url)
        [[ $# -ge 2 ]] || { log "ERROR" "--upstream-url 缺少参数"; exit 2; }
        UPSTREAM_URL="$2"; shift 2 ;;
      --path-prefix)
        [[ $# -ge 2 ]] || { log "ERROR" "--path-prefix 缺少参数"; exit 2; }
        PATH_PREFIX="$2"; shift 2 ;;
      --client-type)
        [[ $# -ge 2 ]] || { log "ERROR" "--client-type 缺少参数"; exit 2; }
        CLIENT_TYPE="$2"; shift 2 ;;
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

  if ! python3 - \
    "$APPLICATION_CODE" "$APPLICATION_NAME" "$ENVIRONMENT" \
    "$PUBLIC_BASE_URL" "$UPSTREAM_URL" "$PATH_PREFIX" "$CLIENT_TYPE" \
    "$API_BASE_URL" "$normalized_file" <<'PY'
import re
import sys
from pathlib import Path
from urllib.parse import urlsplit

code, name, environment, public_base, upstream, path, client_type, api_base, output_path = sys.argv[1:]
code = code.strip().lower()
name = name.strip()
environment = environment.strip().lower() or "prod"
public_base = public_base.strip().rstrip("/")
upstream = upstream.strip().rstrip("/")
path = path.strip().rstrip("/") or (f"/{code}" if code else "")
client_type = client_type.strip().lower() or "confidential"
api_base = api_base.strip().rstrip("/")

errors = []
if not re.fullmatch(r"[a-z][a-z0-9._-]{0,63}", code):
    errors.append("application-code 必须匹配 ^[a-z][a-z0-9._-]{0,63}$")
if not name or len(name.encode("utf-8")) > 128:
    errors.append("application-name 必填且 UTF-8 长度不能超过 128 字节")
if any(ord(ch) < 32 or ord(ch) == 127 for ch in name):
    errors.append("application-name 不能包含换行或控制字符")
if environment not in {"dev", "test", "staging", "prod"}:
    errors.append("environment 只能是 dev、test、staging 或 prod")
if client_type not in {"confidential", "public"}:
    errors.append("client-type 只能是 confidential 或 public")

def validate_url(label, value, allow_path):
    if not value or len(value) > 512 or any(ch.isspace() for ch in value):
        errors.append(f"{label} 必填、最长 512 字符且不能包含空白")
        return
    try:
        parsed = urlsplit(value)
        port = parsed.port
    except ValueError:
        errors.append(f"{label} 端口或 URL 格式无效")
        return
    if parsed.scheme not in {"http", "https"} or not parsed.hostname:
        errors.append(f"{label} 必须是带主机名的 http/https URL")
    if parsed.username is not None or parsed.password is not None or parsed.query or parsed.fragment:
        errors.append(f"{label} 不能包含用户信息、query 或 fragment")
    if not allow_path and parsed.path not in {"", "/"}:
        errors.append(f"{label} 只能填写 origin，不能包含业务路径")
    if port is not None and not 1 <= port <= 65535:
        errors.append(f"{label} 端口必须在 1-65535 之间")

validate_url("public-base-url", public_base, False)
validate_url("upstream-url", upstream, True)
validate_url("api-base-url", api_base, True)

if (
    not path
    or len(path) > 128
    or path == "/"
    or not path.startswith("/")
    or "//" in path
    or not re.fullmatch(r"/[A-Za-z0-9._~!+/\-]*", path)
    or any(segment in {".", ".."} for segment in path.split("/"))
):
    errors.append("path-prefix 必须是非根绝对路径，且不能包含 //、编码字符或点路径段")

if errors:
    print("\n".join(errors), file=sys.stderr)
    raise SystemExit(2)

Path(output_path).write_text(
    "\n".join((code, name, environment, public_base, upstream, path, client_type, api_base)) + "\n",
    encoding="utf-8",
)
PY
  then
    exit 2
  fi

  # 兼容 macOS 自带 Bash 3.2，不依赖 mapfile/readarray。
  {
    IFS= read -r APPLICATION_CODE
    IFS= read -r APPLICATION_NAME
    IFS= read -r ENVIRONMENT
    IFS= read -r PUBLIC_BASE_URL
    IFS= read -r UPSTREAM_URL
    IFS= read -r PATH_PREFIX
    IFS= read -r CLIENT_TYPE
    IFS= read -r API_BASE_URL
  } <"$normalized_file"

  if [[ -z "$DESCRIPTION" ]]; then
    DESCRIPTION="门户路径接入：${PATH_PREFIX}"
  fi
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

write_onboarding_payload() {
  python3 - "$APPLICATION_CODE" "$APPLICATION_NAME" "$DESCRIPTION" "$ENVIRONMENT" \
    "$PUBLIC_BASE_URL" "$UPSTREAM_URL" "$PATH_PREFIX" "$CLIENT_TYPE" >"$1" <<'PY'
import json
import sys
code, name, description, environment, public_base, upstream, path, client_type = sys.argv[1:]
json.dump({
    "application_code": code,
    "application_name": name,
    "description": description,
    "environment": environment,
    "public_base_url": public_base,
    "upstream_url": upstream,
    "path_prefix": path,
    "client_type": client_type,
}, sys.stdout, ensure_ascii=False)
PY
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
    --connect-timeout 10 --max-time 900 \
    --cookie "$COOKIE_FILE" --cookie-jar "$COOKIE_FILE" \
    --header 'Accept: application/json' \
    --header 'Content-Type: application/json' \
    --request POST \
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
    if python3 - "$login_response" <<'PY' >/dev/null 2>&1
import json, sys
with open(sys.argv[1], encoding="utf-8") as handle:
    raise SystemExit(0 if json.load(handle).get("code") == "AUTH_CONCURRENT_SESSION" else 1)
PY
    then
      log "ERROR" "账号已有有效会话。请先在原终端退出，或明确添加 --replace-existing-session。"
    fi
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

print_success() {
  python3 - "$1" <<'PY'
import json
import sys
with open(sys.argv[1], encoding="utf-8") as handle:
    envelope = json.load(handle)
data = envelope.get("data") or {}
application = data.get("application") or {}
environment = data.get("environment") or {}
target = data.get("login_target") or {}
client = data.get("oauth_client") or {}
automation = data.get("automation") or {}

print("子系统接入完成：")
print(f"  应用：{application.get('name', '-')} ({application.get('code', '-')})")
print(f"  环境：{environment.get('environment', '-')}")
print(f"  BaseURL：{environment.get('base_url') or '-'}")
print(f"  UpstreamURL：{environment.get('upstream_url') or '-'}")
print(f"  PathPrefix：{environment.get('path_prefix') or '-'}")
print(f"  LoginTarget：{target.get('target_uri') or '-'}")
print(f"  OAuth Client ID：{client.get('client_id') or '-'}")
print(f"  对外访问地址：{automation.get('public_url') or '-'}")
print(f"  追踪号：{envelope.get('request_id') or '-'}")
PY
}

main() {
  parse_args "$@"
  require_command curl
  require_command python3

  TEMP_DIR="$(mktemp -d "${TMPDIR:-/tmp}/subsystem-onboarding.XXXXXX")"
  RESPONSE_FILE="$TEMP_DIR/onboarding-response.json"
  trap cleanup EXIT
  validate_and_normalize

  if [[ -n "$COOKIE_FILE" ]]; then
    if [[ ! -r "$COOKIE_FILE" ]]; then
      log "ERROR" "Cookie 文件不可读：$COOKIE_FILE"
      exit 2
    fi
    # curl 的 --cookie-jar 会回写文件。复制到私有临时目录，避免污染或覆盖调用者的 Cookie 文件。
    local source_cookie_file="$COOKIE_FILE"
    COOKIE_FILE="$TEMP_DIR/cookies.txt"
    cp "$source_cookie_file" "$COOKIE_FILE"
  else
    COOKIE_FILE="$TEMP_DIR/cookies.txt"
    : >"$COOKIE_FILE"
    login
  fi

  local payload_file="$TEMP_DIR/onboarding.json"
  local status=""
  write_onboarding_payload "$payload_file"

  log "INFO" "开始接入 ${APPLICATION_CODE}：${PUBLIC_BASE_URL}${PATH_PREFIX}/ -> ${UPSTREAM_URL}"
  status="$(http_post_json "/subsystem-onboarding" "$payload_file" "$RESPONSE_FILE")" || {
    log "ERROR" "无法连接子系统接入接口：${API_BASE_URL}/subsystem-onboarding"
    exit 1
  }

  if [[ "$status" != "201" ]]; then
    print_api_error "$status" "$RESPONSE_FILE"
    exit 1
  fi

  print_success "$RESPONSE_FILE"
}

main "$@"
