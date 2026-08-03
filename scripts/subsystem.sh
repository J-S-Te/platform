#!/usr/bin/env bash
# 统一管理子系统的全生命周期：
#   onboard  — 首次接入子系统（创建 Application/Environment/LoginTarget/OAuth Client + 写入 .env.local + 启动容器 + 配置门户网关）
#   update   — 一键重建子系统容器，DB 字段更新请走 PATCH /environments 等受控接口
#   retry    — 重试失败的部署 Agent 操作，不重复创建接入记录
#   status   — 查询子系统部署 Agent 的持久化状态
#   offboard — 深清理子系统（停止容器 + 删除 .env.local + 删除门户网关入口 + 重新加载 nginx + 删除 DB 记录）
#
# 这是基础平台子系统接入的官方入口；请勿在子系统代码、镜像、功能模块或业务迁移的日常发布中重复执行。
# 旧版 subsystem-onboarding.sh / subsystem-offboarding.sh 现在改为薄壳，转发到本脚本以保持向后兼容。

set -Eeuo pipefail
IFS=$'\n\t'
umask 077

# ============================ 全局默认值与共享状态 ============================
API_BASE_URL="${BASIC_PLATFORM_API_BASE_URL:-http://localhost:8081/api/v1}"
PLATFORM_ORIGIN="${BASIC_PLATFORM_ORIGIN:-}"
ALLOW_INSECURE_HTTP_API="${BASIC_PLATFORM_ALLOW_INSECURE_HTTP_API:-false}"
INSECURE_HTTP_API_ALLOWED_HOSTS="${BASIC_PLATFORM_INSECURE_HTTP_API_ALLOWED_HOSTS:-}"

# 通用认证参数
ACCOUNT=""
PASSWORD_STDIN=false
REPLACE_EXISTING_SESSION=false
COOKIE_FILE=""

# onboard 专用
APPLICATION_CODE=""
APPLICATION_NAME=""
DESCRIPTION=""
ENVIRONMENT="prod"
ENVIRONMENT_EXPLICIT=false
PUBLIC_BASE_URL=""
UPSTREAM_URL=""
PATH_PREFIX=""
CLIENT_TYPE="confidential"
INITIAL_ADMIN_USER_ID=""

# 通用控制
SUBCOMMAND=""
RETRY_MODE=false
PRESET=""
INTERACTIVE=false
ASSUME_YES=false
DRY_RUN=false
SHALLOW_OFFBOARD=false   # 旧版 offboard 行为：仅删 DB，不做容器/gateway 清理
DELETE_APPLICATION=false  # offboard 是否在 environment 删除后顺带删除 application
OWNS_SESSION=false
TEMP_DIR=""
RESPONSE_FILE=""
CONFIRMATION_CODE=""      # offboard 必填

# ============================ 通用函数 ============================
log() {
  local level="$1"
  shift
  printf '[%s] [%s] %s\n' "$(date '+%Y-%m-%d %H:%M:%S')" "$level" "$*" >&2
}

usage() {
  cat <<'USAGE'
用法：
  subsystem.sh <onboard|update|retry|status|offboard> [通用认证参数] [子命令参数]

子命令：
  onboard    首次接入子系统（创建 DB 记录 + 写入 .env.local + 启动容器 + 配置门户网关）
  update     一键重建子系统容器；DB 字段变更请先走 PATCH
  retry      重试失败的部署 Agent 操作；不会重复创建 Application/OAuth Client
  status     查询部署 Agent 状态（PROVISIONING/READY/PROVISION_FAILED 等）
  offboard   深清理子系统（停止容器 + 删除 .env.local + 删除门户网关入口 + 删除 DB 记录）
             --shallow   退回旧版语义：只删 DB 记录（仅供紧急修复使用）

通用认证参数：
  --api-base-url URL             平台 API 根地址；默认 BASIC_PLATFORM_API_BASE_URL
  --platform-origin URL          Cookie 写请求的可信 Origin；默认 BASIC_PLATFORM_ORIGIN
  --account ACCOUNT              具备对应权限的平台管理员账号
  --password-stdin               从标准输入读取一行密码；未指定时安全交互输入
  --cookie-file FILE             复用已登录的平台 Cookie 文件
  --replace-existing-session     撤销该账号原有会话后登录

平台 API 传输保护：
  HTTPS 及 localhost/回环 HTTP 默认允许。可信局域网必须同时设置
  BASIC_PLATFORM_ALLOW_INSECURE_HTTP_API=true 和
  BASIC_PLATFORM_INSECURE_HTTP_API_ALLOWED_HOSTS=<精确主机列表>。
  此规则不限制 --public-base-url 或 OAuth HTTP 回调。

通用控制：
  -y, --yes                      跳过确认；适用于 CI
  -h, --help                     显示本帮助

执行 onboard / update / retry / status / offboard -h 查看子命令各自的参数说明。
USAGE
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || {
    log "ERROR" "缺少命令：$1"
    exit 2
  }
}

# 管理员口令和 Cookie 只能通过 HTTPS 或本机回环 HTTP 发送。局域网确需直接访问
# HTTP API 时必须显式开启并列出主机；该规则不限制 public-base-url/OAuth 回调地址。
validate_api_transport() {
  python3 - "$API_BASE_URL" "$ALLOW_INSECURE_HTTP_API" "$INSECURE_HTTP_API_ALLOWED_HOSTS" <<'PY'
import ipaddress
import sys
from urllib.parse import urlsplit

value, allow_insecure, allowed_hosts = sys.argv[1:]
parsed = urlsplit(value)
host = (parsed.hostname or "").rstrip(".").lower()
if parsed.scheme == "https":
    raise SystemExit(0)
is_loopback = host == "localhost"
try:
    is_loopback = is_loopback or ipaddress.ip_address(host).is_loopback
except ValueError:
    pass
if parsed.scheme == "http" and is_loopback:
    raise SystemExit(0)
enabled = allow_insecure.strip().lower() in {"1", "true", "yes", "on"}
allowed = {item.strip().rstrip(".").lower() for item in allowed_hosts.split(",") if item.strip()}
if parsed.scheme == "http" and enabled and host in allowed:
    raise SystemExit(0)
print(
    "非回环平台 API 必须使用 HTTPS；可信局域网确需 HTTP 时，请同时设置 "
    "BASIC_PLATFORM_ALLOW_INSECURE_HTTP_API=true 和 "
    "BASIC_PLATFORM_INSECURE_HTTP_API_ALLOWED_HOSTS=<精确主机列表>。"
    "该限制不影响 public-base-url 或 OAuth HTTP 回调。",
    file=sys.stderr,
)
raise SystemExit(2)
PY
}

can_interact() {
  [[ -t 0 && -t 1 ]]
}

lowercase() {
  # macOS 自带 Bash 3.2 不支持 ${value,,}，因此使用 tr 做兼容转换。
  printf '%s' "$1" | tr '[:upper:]' '[:lower:]'
}

# ============================ 通用参数解析 ============================
parse_common_args() {
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
      -h|--help)
        usage; exit 0 ;;
      --)
        shift; break ;;
      -*)
        log "ERROR" "未知参数：$1"
        usage >&2
        exit 2 ;;
      *)
        log "ERROR" "未预期的位置参数：$1（所有参数必须带选项名）"
        usage >&2
        exit 2 ;;
    esac
  done
}

# ============================ HTTP / 会话 ============================
write_login_payload() {
  local destination="$1"
  local password="$2"
  local password_file="$TEMP_DIR/password.txt"

  # 口令经权限受 umask 077 约束的临时文件传给 JSON 编码器，既不进入 argv，也不经 Shell 插值。
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

http_request() {
  # http_request METHOD ENDPOINT PAYLOAD_FILE OUTPUT_FILE [MAX_TIME_SECONDS]
  local method="$1"
  local endpoint="$2"
  local payload_file="${3:-}"
  local output_file="$4"
  local max_time="${5:-900}"
  local extra_args=()
  if [[ -n "$payload_file" && "$method" != "GET" ]]; then
    extra_args+=(--data-binary "@${payload_file}")
  fi
  # bash 3.2 (macOS) + set -u: empty array expansion needs the ${arr[@]+...} idiom.
  curl --silent --show-error \
    --connect-timeout 10 --max-time "$max_time" \
    --cookie "$COOKIE_FILE" --cookie-jar "$COOKIE_FILE" \
    --header 'Accept: application/json' \
    --header "Origin: ${PLATFORM_ORIGIN}" \
    --header 'Content-Type: application/json' \
    --request "$method" \
    ${extra_args[@]+"${extra_args[@]}"} \
    --output "$output_file" \
    --write-out '%{http_code}' \
    "${API_BASE_URL}${endpoint}"
}

http_post_json() {
  # http_post_json ENDPOINT PAYLOAD_FILE OUTPUT_FILE [MAX_TIME_SECONDS]
  http_request POST "$1" "$2" "$3" "${4:-900}"
}

http_get() {
  http_request GET "$1" "" "$2" "${3:-120}"
}

http_delete_json() {
  http_request DELETE "$1" "$2" "$3" "${4:-120}"
}

diagnose_connection_failure() {
  local endpoint="$1"
  log "ERROR" "无法连接平台接口：${API_BASE_URL}${endpoint}"
  cat >&2 <<'EOF_DIAG'
可按以下顺序排查：
  1. 在 platform 目录执行：bash scripts/docker-local.sh ps
  2. 查看基础平台日志：bash scripts/docker-local.sh logs api subsystem-provisioner
  3. 确认 --api-base-url 指向平台 API（默认 http://localhost:8081/api/v1），不是子系统地址。
  4. 确认 --platform-origin 与平台 APP_CORS_ALLOWED_ORIGINS/OIDC_ISSUER 中允许的 origin 一致。
  5. 若刚更新了基础平台后端路由或迁移，执行：bash scripts/docker-local.sh refresh-api
EOF_DIAG
}

print_api_error() {
  # print_api_error HTTP_STATUS RESPONSE_FILE ENDPOINT
  local http_status="$1"
  local response_file="$2"
  local endpoint="${3:-}"
  python3 - "$http_status" "$response_file" "$endpoint" "$PLATFORM_ORIGIN" <<'PY'
import json
import sys

status, path, endpoint, platform_origin = sys.argv[1:]
try:
    with open(path, encoding="utf-8") as handle:
        payload = json.load(handle)
except Exception:
    print(f"HTTP {status}：平台返回了无法解析的响应。请检查 API 地址、反向代理和 api 日志。", file=sys.stderr)
    raise SystemExit

code = payload.get("code") or "UNKNOWN"
message = payload.get("message") or "请求失败"
request_id = payload.get("request_id") or "-"
details = payload.get("details") or {}
print(f"HTTP {status} {code}：{message}（追踪号：{request_id}）", file=sys.stderr)

if code == "AUTH_CONCURRENT_SESSION":
    print("处理建议：该管理员账号已在另一终端保留有效会话。先在原终端退出；若原终端已关闭且确认需要抢占会话，使用同一命令追加 --replace-existing-session。该选项会使原会话立即失效。", file=sys.stderr)
elif code == "IAM_SUBSYSTEM_ALREADY_ONBOARDED":
    if isinstance(details, dict):
        application_code = details.get("application_code") or "-"
        environment = details.get("environment") or "-"
        state = details.get("status") or "-"
        next_action = details.get("next_action") or "请使用环境、登录目标或 OAuth 客户端的更新接口变更配置。"
        print(f"当前已存在：应用 {application_code}，环境 {environment}（状态：{state}）。{next_action}", file=sys.stderr)
    print("处理建议：该环境的控制面记录已经存在。子系统代码、镜像、前端、后端、功能模块和业务迁移的正常更新均不需要执行接入或撤销脚本，现有统一登录配置会保留。只有永久下线该环境时才使用 subsystem.sh offboard；若确需变更 BaseURL、UpstreamURL、PathPrefix 或 OAuth 回调，请走受控配置变更流程（subsystem.sh update 之前先 PATCH），不能通过撤销后重建绕过。若此前部署 Agent 失败，请先使用 subsystem.sh status 查询，再使用 subsystem.sh retry。", file=sys.stderr)
elif code in {"IAM_CONFLICT", "IAM_VERSION_CONFLICT"}:
    print("处理建议：存在资源冲突或配置已变更。检查应用编码、环境、路径前缀和 OAuth Client 是否被其他接入占用；不要通过重复执行脚本覆盖现有配置。", file=sys.stderr)
elif code in {"PLATFORM_DEPENDENCY_UNAVAILABLE", "PLATFORM_INTERNAL_ERROR"} or status.startswith("5"):
    print("处理建议：基础平台 API 或受控 provisioner 当前不可用。执行 `bash scripts/docker-local.sh ps`，再执行 `bash scripts/docker-local.sh logs api subsystem-provisioner`；如刚更新后端，执行 `bash scripts/docker-local.sh refresh-api` 后重试。", file=sys.stderr)
elif code in {"AUTH_UNAUTHENTICATED", "AUTH_SESSION_EXPIRED"} or status == "401":
    print("处理建议：登录状态无效或 Cookie 已过期。不要复用旧 Cookie，改用 --account 重新登录；使用 --cookie-file 时请确认文件来自同一基础平台地址。", file=sys.stderr)
elif status == "403" and endpoint == "/auth/login":
    print(f"处理建议：登录请求被同源保护拒绝。当前请求 Origin 为 {platform_origin}；请确认它已配置在平台 APP_CORS_ALLOWED_ORIGINS 中，并与 OIDC_ISSUER 的 origin 一致。可使用 --platform-origin 或 BASIC_PLATFORM_ORIGIN 修正。", file=sys.stderr)
elif status == "403":
    print("处理建议：当前账号没有子系统接入所需的基础平台管理权限。请由超级管理员核对应用、环境、登录目标和 OAuth 客户端的创建/更新/删除权限。", file=sys.stderr)
elif status == "422":
    print("处理建议：检查参数：BaseURL 只能是对外 origin；UpstreamURL 必须为网关可达的内网 http/https 地址；PathPrefix 必须是非根绝对路径。", file=sys.stderr)
elif code == "PLATFORM_METHOD_NOT_ALLOWED" or status == "405":
    print("处理建议：运行中的 API 镜像可能未包含当前接口。执行 `bash scripts/docker-local.sh refresh-api` 后重试。", file=sys.stderr)
elif code == "IAM_ENVIRONMENT_DELETE_BLOCKED":
    print("该环境仍有配置命名空间或审计回执。请先按保留策略处理，脚本不提供 --force。", file=sys.stderr)
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
    diagnose_connection_failure "/auth/login"
    exit 1
  }
  : >"$login_payload"

  if [[ "$status" != "200" ]]; then
    print_api_error "$status" "$login_response" "/auth/login"
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
    --header "Origin: ${PLATFORM_ORIGIN}" \
    --request POST \
    --output /dev/null \
    "${API_BASE_URL}/auth/logout" >/dev/null 2>&1 || true
  OWNS_SESSION=false
}

ensure_authenticated() {
  if [[ -n "$COOKIE_FILE" ]]; then
    if [[ ! -r "$COOKIE_FILE" ]]; then
      log "ERROR" "Cookie 文件不可读：$COOKIE_FILE"
      exit 2
    fi
    # 用户提供的 Cookie 文件只读复制到本次临时目录；登录、刷新或退出不能覆盖调用方的会话文件。
    local source_cookie_file="$COOKIE_FILE"
    COOKIE_FILE="$TEMP_DIR/cookies.txt"
    cp "$source_cookie_file" "$COOKIE_FILE"
  else
    COOKIE_FILE="$TEMP_DIR/cookies.txt"
    : >"$COOKIE_FILE"
    login
  fi
}

cleanup() {
  local status=$?
  # 先移除 EXIT trap，避免 logout 或临时目录清理失败时递归进入 cleanup；始终保留原命令退出码。
  trap - EXIT
  logout_owned_session
  if [[ -n "$TEMP_DIR" && -d "$TEMP_DIR" ]]; then
    rm -rf "$TEMP_DIR"
  fi
  exit "$status"
}

# ============================ onboard 子命令 ============================
onboard_usage() {
  cat <<'USAGE'
用法：subsystem.sh onboard [认证参数] [接入参数]

快捷预设：
  --preset contract-management-local
      填入合同管理系统本地 Docker 默认配置：
      contract_management / 合同管理系统 / prod /
      http://localhost:8081 / http://contract-api:8081 / /contract_management
      已显式传入的接入参数优先于预设值。
  --preset customer-portal-local
      填入客户自助门户本地 Docker 默认配置：
      customer_portal / 客户自助门户 / dev /
      http://localhost:8081 / http://portal-api:8091 / /customer-portal
      接入 Agent 会同时创建外部身份、角色和 CRM/Portal 通信所需的六个最小权限服务客户端。

接入参数（对应后端 POST /api/v1/subsystem-onboarding）：
  --application-code CODE       Application.Code，必填，例如 contract_management
  --application-name NAME       Application.Name，必填，例如 合同管理系统
  --environment ENV             Environment.Environment：dev/test/staging/prod，默认 prod
  --public-base-url URL         Environment.BaseURL，对外统一入口，必填
  --upstream-url URL            Environment.UpstreamURL，子系统内网地址，必填
  --path-prefix PATH            Environment.PathPrefix，默认 /<application-code>
  --client-type TYPE            OAuth Client 类型：confidential/public，默认 confidential
  --initial-admin-user-id ID    首个业务管理员用户 ID
  --description TEXT            应用说明；默认"门户路径接入：<path-prefix>"

执行控制：
  -i, --interactive             即使参数已完整，也进入中文配置向导并确认
  --dry-run                     仅校验并显示最终配置，不登录、不调用 API
  -h, --help                    显示本帮助
USAGE
}

apply_preset() {
  [[ -z "$PRESET" ]] && return 0
  case "$PRESET" in
    contract-management-local|contract_management_local)
      [[ -n "$APPLICATION_CODE" ]] || APPLICATION_CODE="contract_management"
      [[ -n "$APPLICATION_NAME" ]] || APPLICATION_NAME="合同管理系统"
      [[ -n "$PUBLIC_BASE_URL" ]] || PUBLIC_BASE_URL="http://localhost:8081"
      [[ -n "$UPSTREAM_URL" ]] || UPSTREAM_URL="http://contract-api:8081"
      [[ -n "$PATH_PREFIX" ]] || PATH_PREFIX="/contract_management"
      [[ -n "$DESCRIPTION" ]] || DESCRIPTION="合同创建、审批与客户管理系统"
      ;;
    customer-portal-local|customer_portal_local)
      [[ -n "$APPLICATION_CODE" ]] || APPLICATION_CODE="customer_portal"
      [[ -n "$APPLICATION_NAME" ]] || APPLICATION_NAME="客户自助门户"
      [[ "$ENVIRONMENT_EXPLICIT" == true ]] || ENVIRONMENT="dev"
      [[ -n "$PUBLIC_BASE_URL" ]] || PUBLIC_BASE_URL="http://localhost:8081"
      [[ -n "$UPSTREAM_URL" ]] || UPSTREAM_URL="http://portal-api:8091"
      [[ -n "$PATH_PREFIX" ]] || PATH_PREFIX="/customer-portal"
      [[ -n "$DESCRIPTION" ]] || DESCRIPTION="外部客户项目、报告、备案、评价与反馈自助门户"
      ;;
    *)
	  log "ERROR" "未知快捷预设：${PRESET}。支持 contract-management-local、customer-portal-local"
      exit 2 ;;
  esac
}

onboard_required_configuration_missing() {
  [[ -z "${APPLICATION_CODE//[[:space:]]/}" || -z "${APPLICATION_NAME//[[:space:]]/}" || \
    -z "${PUBLIC_BASE_URL//[[:space:]]/}" || -z "${UPSTREAM_URL//[[:space:]]/}" ]]
}

prompt_value() {
  local variable="$1"
  local label="$2"
  local default_value="$3"
  local hint="${4:-}"
  local current_value=""
  local entered=""

  current_value="${!variable}"
  [[ -n "$current_value" ]] || current_value="$default_value"
  [[ -n "$hint" ]] && printf '  提示：%s\n' "$hint" >&2
  if [[ -n "$current_value" ]]; then
    read -r -p "${label} [${current_value}]：" entered
  else
    read -r -p "${label}：" entered
  fi
  [[ -n "$entered" ]] || entered="$current_value"
  printf -v "$variable" '%s' "$entered"
}

onboard_suggest_application_name() {
  case "$(lowercase "$APPLICATION_CODE")" in
    contract_management|contract-management) printf '%s' '合同管理系统' ;;
    *) printf '%s' '' ;;
  esac
}

onboard_suggest_upstream_url() {
  case "$(lowercase "$APPLICATION_CODE")" in
    contract_management|contract-management) printf '%s' 'http://contract-api:8081' ;;
    *) printf '%s' '' ;;
  esac
}

onboard_collect_interactive_config() {
  printf '\n统一登录目标 — 子系统接入向导\n' >&2
  printf '请填写对外访问地址与子系统内网地址。密码、Cookie 和 OAuth Secret 不会回显。\n\n' >&2

  prompt_value API_BASE_URL '基础平台 API 地址' "$API_BASE_URL" '通常为 http://127.0.0.1:8081/api/v1；可通过 BASIC_PLATFORM_API_BASE_URL 覆盖。'
  prompt_value APPLICATION_CODE '应用编码' "$APPLICATION_CODE" '小写字母开头；可使用数字、.、_、-。例如 contract_management。'
  prompt_value APPLICATION_NAME '应用名称' "$(onboard_suggest_application_name)" '例如 合同管理系统。'
  prompt_value ENVIRONMENT '环境' "$ENVIRONMENT" '仅支持 dev、test、staging、prod。'
  prompt_value PUBLIC_BASE_URL '对外统一入口 BaseURL' "${PUBLIC_BASE_URL:-http://localhost:8081}" '填写用户浏览器访问的门户 origin，不要带 /api/v1 或业务路径。'
  prompt_value UPSTREAM_URL '子系统 UpstreamURL' "$(onboard_suggest_upstream_url)" '填写 Docker/内网中可被网关访问的地址，例如 http://contract-api:8081。'
  prompt_value PATH_PREFIX '门户路径前缀' "${PATH_PREFIX:-/${APPLICATION_CODE}}" '必须以 / 开头，例如 /contract_management；不要填写单独的 /。'
  prompt_value CLIENT_TYPE 'OAuth 客户端类型' "$CLIENT_TYPE" 'confidential（服务端子系统，推荐）或 public。'
  prompt_value DESCRIPTION '应用说明（可留空自动生成）' "$DESCRIPTION" '仅用于基础平台展示。'

  if [[ -z "$COOKIE_FILE" ]]; then
    if [[ "$PASSWORD_STDIN" == true ]]; then
      prompt_value ACCOUNT '平台管理员账号' "$ACCOUNT" '密码将从标准输入读取。'
    else
      local auth_mode="1"
      read -r -p '认证方式 [1=管理员账号口令，2=已有 Cookie 文件] [1]：' auth_mode
      auth_mode="${auth_mode:-1}"
      case "$auth_mode" in
        1)
          prompt_value ACCOUNT '平台管理员账号' "$ACCOUNT" '用于调用基础平台 API；不是子系统业务账号。'
          ;;
        2)
          prompt_value COOKIE_FILE '已登录 Cookie 文件路径' "$COOKIE_FILE" '脚本会复制该文件，不会修改原文件。'
          ;;
        *)
          log "ERROR" "认证方式只能输入 1 或 2"
          return 2
          ;;
      esac
    fi
  fi
}

onboard_validate_and_normalize() {
  local normalized_file="$TEMP_DIR/normalized.txt"

  if ! python3 - \
    "$APPLICATION_CODE" "$APPLICATION_NAME" "$ENVIRONMENT" \
    "$PUBLIC_BASE_URL" "$UPSTREAM_URL" "$PATH_PREFIX" "$CLIENT_TYPE" \
    "$API_BASE_URL" "$PLATFORM_ORIGIN" "$normalized_file" <<'PY'
import re
import sys
from pathlib import Path
from urllib.parse import urlsplit

code, name, environment, public_base, upstream, path, client_type, api_base, platform_origin, output_path = sys.argv[1:]
code = code.strip().lower()
name = name.strip()
environment = environment.strip().lower() or "prod"
public_base = public_base.strip().rstrip("/")
upstream = upstream.strip().rstrip("/")
path = path.strip().rstrip("/") or (f"/{code}" if code else "")
client_type = client_type.strip().lower() or "confidential"
api_base = api_base.strip().rstrip("/")
platform_origin = platform_origin.strip().rstrip("/") or public_base

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
validate_url("platform-origin", platform_origin, False)

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
    "\n".join((code, name, environment, public_base, upstream, path, client_type, api_base, platform_origin)) + "\n",
    encoding="utf-8",
)
PY
  then
    return 2
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
    IFS= read -r PLATFORM_ORIGIN
  } <"$normalized_file"

  validate_api_transport || return 2

  if [[ -z "$DESCRIPTION" ]]; then
    DESCRIPTION="门户路径接入：${PATH_PREFIX}"
  fi
  if [[ "$DRY_RUN" != true && -z "$COOKIE_FILE" && -z "${ACCOUNT//[[:space:]]/}" ]]; then
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

onboard_write_payload() {
  python3 - "$APPLICATION_CODE" "$APPLICATION_NAME" "$DESCRIPTION" "$ENVIRONMENT" \
    "$PUBLIC_BASE_URL" "$UPSTREAM_URL" "$PATH_PREFIX" "$CLIENT_TYPE" "$INITIAL_ADMIN_USER_ID" >"$1" <<'PY'
import json
import sys
code, name, description, environment, public_base, upstream, path, client_type, initial_admin_user_id = sys.argv[1:]
payload = {
    "application_code": code,
    "application_name": name,
    "description": description,
    "environment": environment,
    "public_base_url": public_base,
    "upstream_url": upstream,
    "path_prefix": path,
    "client_type": client_type,
}
if initial_admin_user_id.strip():
    payload["initial_admin_user_id"] = initial_admin_user_id.strip()
json.dump(payload, sys.stdout, ensure_ascii=False)
PY
}

onboard_print_configuration_summary() {
	local initial_access_summary="${INITIAL_ADMIN_USER_ID:-当前登录的平台操作者}"
	if [[ "$APPLICATION_CODE" == "customer_portal" ]]; then
		initial_access_summary="不授予内部管理员；由 CRM 邀请流程开通外部客户"
	fi
  cat >&2 <<EOF_SUMMARY

即将首次创建以下统一登录接入（不会覆盖已有环境）：
提示：子系统代码、镜像或功能模块的正常更新不需要执行本脚本。
  应用：${APPLICATION_NAME} (${APPLICATION_CODE})
  环境：${ENVIRONMENT}
  对外 BaseURL：${PUBLIC_BASE_URL}
  子系统 UpstreamURL：${UPSTREAM_URL}
  门户路径前缀：${PATH_PREFIX}
  对外访问地址：${PUBLIC_BASE_URL}${PATH_PREFIX}/
  OAuth 回调地址：${PUBLIC_BASE_URL}${PATH_PREFIX}/auth/callback
  OAuth Client ID：${APPLICATION_CODE}-${ENVIRONMENT}-web
  OAuth Client 类型：${CLIENT_TYPE}
  初始业务管理员：${initial_access_summary}
  平台 API：${API_BASE_URL}
  平台请求 Origin：${PLATFORM_ORIGIN}
EOF_SUMMARY
  if [[ "$DRY_RUN" == true ]]; then
    printf '  认证方式：不认证（dry-run 不调用平台 API）\n' >&2
  elif [[ -n "$COOKIE_FILE" ]]; then
    printf '  认证方式：复用 Cookie 文件（路径不回显）\n' >&2
  else
    printf '  认证方式：平台管理员账号 %s（密码不回显）\n' "$ACCOUNT" >&2
  fi
}

onboard_confirm_configuration() {
  [[ "$ASSUME_YES" == true ]] && return 0
  local answer=""
  printf '\n' >&2
  read -r -p '确认开始接入吗？输入 y 或 yes 继续，其他输入取消：' answer
  case "$answer" in
    y|Y|yes|Yes|YES)
      return 0
      ;;
    *)
      log "INFO" "已取消，未调用平台 API，未创建任何子系统配置"
      return 1
      ;;
  esac
}

onboard_run_interactive_wizard() {
  while true; do
    if ! onboard_collect_interactive_config; then
      log "WARN" "输入未完成，请重新填写。"
      continue
    fi
    if onboard_validate_and_normalize; then
      break
    fi
    log "WARN" "以上配置未通过校验，请根据提示重新输入。"
  done
}

onboard_print_success() {
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
authorization = data.get("authorization") or {}

print("子系统接入完成：")
print(f"  应用：{application.get('name', '-')} ({application.get('code', '-')})")
print(f"  环境：{environment.get('environment', '-')}")
print(f"  BaseURL：{environment.get('base_url') or '-'}")
print(f"  UpstreamURL：{environment.get('upstream_url') or '-'}")
print(f"  PathPrefix：{environment.get('path_prefix') or '-'}")
print(f"  LoginTarget：{target.get('target_uri') or '-'}")
print(f"  OAuth Client ID：{client.get('client_id') or '-'}")
print(f"  对外访问地址：{automation.get('public_url') or '-'}")
if authorization:
    print(f"  初始业务管理员：{authorization.get('initial_admin_user_id') or '-'}（角色：{authorization.get('role_code') or '-'}）")
print(f"  追踪号：{envelope.get('request_id') or '-'}")
print("  提示：OAuth Client Secret 不会回显；由受控 provisioner 写入子系统运行配置。")
PY
}

parse_onboard_args() {
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
      --application-name)
        [[ $# -ge 2 ]] || { log "ERROR" "--application-name 缺少参数"; exit 2; }
        APPLICATION_NAME="$2"; shift 2 ;;
      --description)
        [[ $# -ge 2 ]] || { log "ERROR" "--description 缺少参数"; exit 2; }
        DESCRIPTION="$2"; shift 2 ;;
      --environment)
        [[ $# -ge 2 ]] || { log "ERROR" "--environment 缺少参数"; exit 2; }
        ENVIRONMENT="$2"; ENVIRONMENT_EXPLICIT=true; shift 2 ;;
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
      --initial-admin-user-id)
        [[ $# -ge 2 ]] || { log "ERROR" "--initial-admin-user-id 缺少参数"; exit 2; }
        INITIAL_ADMIN_USER_ID="$2"; shift 2 ;;
      -i|--interactive)
        INTERACTIVE=true; shift ;;
      --dry-run)
        DRY_RUN=true; shift ;;
      --preset)
        [[ $# -ge 2 ]] || { log "ERROR" "--preset 缺少参数"; exit 2; }
        PRESET="$2"; shift 2 ;;
      -h|--help)
        onboard_usage; exit 0 ;;
      --)
        shift; break ;;
      *)
        log "ERROR" "未知参数：$1"
        onboard_usage >&2
        exit 2 ;;
    esac
  done
}

cmd_onboard() {
  parse_onboard_args "$@"
  require_command curl
  require_command python3
  apply_preset

  TEMP_DIR="$(mktemp -d "${TMPDIR:-/tmp}/subsystem-onboard.XXXXXX")"
  RESPONSE_FILE="$TEMP_DIR/onboarding-response.json"
  trap cleanup EXIT

  local needs_wizard=false
  if [[ "$INTERACTIVE" == true ]] || onboard_required_configuration_missing || \
    { [[ "$DRY_RUN" != true && -z "$COOKIE_FILE" && -z "${ACCOUNT//[[:space:]]/}" ]]; }; then
    needs_wizard=true
  fi

  if [[ "$needs_wizard" == true ]]; then
    if [[ "$PASSWORD_STDIN" == true ]]; then
      log "ERROR" "--password-stdin 不能与交互配置向导同时使用；请补齐所有接入参数后再使用该选项，或改用安全的终端密码输入。"
      exit 2
    fi
    if ! can_interact; then
      log "ERROR" "缺少必填接入参数且当前不是交互终端。请补齐参数，或在终端中运行脚本进入向导。"
      log "ERROR" "可使用快捷方式：bash scripts/subsystem.sh onboard --preset contract-management-local --account admin"
      exit 2
    fi
    onboard_run_interactive_wizard
  else
    if ! onboard_validate_and_normalize; then
      exit 2
    fi
  fi

  onboard_print_configuration_summary
  if ! onboard_confirm_configuration; then
    exit 0
  fi

  if [[ "$DRY_RUN" == true ]]; then
    log "INFO" "配置校验通过（dry-run）。未登录、未调用平台 API，也未写入任何配置。"
    exit 0
  fi

  ensure_authenticated

  local payload_file="$TEMP_DIR/onboarding.json"
  local status=""
  onboard_write_payload "$payload_file"

  log "INFO" "开始接入 ${APPLICATION_CODE}：${PUBLIC_BASE_URL}${PATH_PREFIX}/ -> ${UPSTREAM_URL}"
  status="$(http_post_json "/subsystem-onboarding" "$payload_file" "$RESPONSE_FILE")" || {
    diagnose_connection_failure "/subsystem-onboarding"
    exit 1
  }

  if [[ "$status" != "201" ]]; then
    print_api_error "$status" "$RESPONSE_FILE" "/subsystem-onboarding"
    exit 1
  fi

  onboard_print_success "$RESPONSE_FILE"
}

# ============================ update 子命令 ============================
update_usage() {
  cat <<'USAGE'
用法：subsystem.sh update|retry [认证参数] --application-code CODE --environment ENV

update 是一键重建子系统容器的快捷入口。它不会重新写入 .env.local，也不会变更 DB 记录。
要修改 BaseURL / UpstreamURL / OAuth 配置，请先通过 PATCH /environments 与 /oauth-clients
接口完成受控变更，然后调用本子命令让正在运行的子系统容器应用新配置。

必填参数：
  --application-code CODE   Application.Code，例如 contract_management
  --environment ENV         Environment.Environment：dev/test/staging/prod

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

update_print_summary() {
  cat >&2 <<EOF_SUMMARY

即将一键重建以下子系统的容器（不会重写 .env.local，也不会改 DB 字段）：
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

  if [[ "$status" != "200" ]]; then
    print_api_error "$status" "$RESPONSE_FILE" "$endpoint"
    exit 1
  fi

  update_print_success "$RESPONSE_FILE"
}

# ============================ status 子命令 ============================
status_usage() {
  cat <<'USAGE'
用法：subsystem.sh status [认证参数] --application-code CODE --environment ENV

查询基础平台记录的部署 Agent 状态。常见状态：
  PROVISIONING  首次接入记录已创建，Agent 尚未完成
  UPDATING      正在重建或重试
  READY         Agent 已成功完成，门户才会展示该子系统
  PROVISION_FAILED  最近一次 Agent 操作失败，可使用 retry 重试
  DRAINING      正在下线
  OFFBOARDED    基础设施已拆解，等待或已经完成 DB 清理
USAGE
}

cmd_status() {
  local argument=""
  for argument in "$@"; do
    if [[ "$argument" == "-h" || "$argument" == "--help" ]]; then
      status_usage
      exit 0
    fi
  done
  parse_update_args "$@"
  require_command curl
  require_command python3

  TEMP_DIR="$(mktemp -d "${TMPDIR:-/tmp}/subsystem-status.XXXXXX")"
  RESPONSE_FILE="$TEMP_DIR/status-response.json"
  trap cleanup EXIT

  if update_required_configuration_missing; then
    log "ERROR" "status 必须提供 --application-code 和 --environment"
    status_usage >&2
    exit 2
  fi
  if ! update_validate_and_normalize; then
    exit 2
  fi
  ensure_authenticated

  local endpoint="/subsystem-status?application_code=${APPLICATION_CODE}&environment=${ENVIRONMENT}"
  local status=""
  status="$(http_get "$endpoint" "$RESPONSE_FILE")" || {
    diagnose_connection_failure "/subsystem-status"
    exit 1
  }
  if [[ "$status" != "200" ]]; then
    print_api_error "$status" "$RESPONSE_FILE" "/subsystem-status"
    exit 1
  fi
  python3 - "$RESPONSE_FILE" <<'PY'
import json
import sys
with open(sys.argv[1], encoding="utf-8") as handle:
    envelope = json.load(handle)
data = envelope.get("data") or {}
print("子系统部署状态：")
for key, label in (("application_code", "应用"), ("environment", "环境"), ("status", "状态"),
                   ("operation", "最近操作"), ("generation", "代次"), ("attempt_count", "尝试次数"),
                   ("last_error_code", "错误编码"), ("last_error", "错误说明"),
                   ("updated_at", "更新时间")):
    value = data.get(key)
    if value not in (None, ""):
        print(f"  {label}：{value}")
print(f"  追踪号：{envelope.get('request_id') or '-'}")
PY
}

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

# ============================ 子命令分发 ============================
dispatch() {
  if [[ $# -eq 0 ]]; then
    usage >&2
    exit 2
  fi
  SUBCOMMAND="$1"
  shift
  case "$SUBCOMMAND" in
    onboard)   cmd_onboard   "$@" ;;
    update)    RETRY_MODE=false; cmd_update "$@" ;;
    retry)     RETRY_MODE=true; cmd_update "$@" ;;
    status)    cmd_status    "$@" ;;
    offboard)  cmd_offboard  "$@" ;;
    -h|--help|help) usage; exit 0 ;;
    *)
      log "ERROR" "未知子命令：${SUBCOMMAND}。支持：onboard、update、retry、status、offboard"
      usage >&2
      exit 2
      ;;
  esac
}

main() {
  require_command curl
  require_command python3
  # 通用认证/控制参数先扫一遍（包含子命令自身支持的相同参数），剩余的留给子命令解析。
  # 兼容做法：每个子命令的 parse_*_args 独立解析自己的参数集合。
  dispatch "$@"
}

main "$@"
