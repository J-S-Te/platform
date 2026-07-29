#!/usr/bin/env bash
# 通过基础平台管理 API 首次登记并触发部署一个子系统。
#
# 本脚本只用于创建统一登录接入，不用于子系统代码、镜像、功能模块或业务迁移的日常发布。
# 已接入环境的统一登录配置会独立保留；常规发布不得先撤销或重复执行本脚本。
#
# 本脚本是“统一登录目标”的唯一创建入口。它会一次性创建：
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
INITIAL_ADMIN_USER_ID=""
ACCOUNT=""
PASSWORD_STDIN=false
REPLACE_EXISTING_SESSION=false
COOKIE_FILE=""
INTERACTIVE=false
ASSUME_YES=false
DRY_RUN=false
PRESET=""
CONFIRMATION_REQUIRED=false
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
  subsystem-onboarding.sh --interactive [认证参数] [接入参数]
  subsystem-onboarding.sh --preset contract-management-local [认证参数]

交互方式：
  直接运行脚本，或在缺少必填接入参数时，终端中会自动进入中文配置向导。
  向导会回显将要创建的配置并要求确认；不会显示密码、Cookie 或 OAuth Secret。

快捷预设：
  --preset contract-management-local
      填入合同管理系统本地 Docker 默认配置：
      contract_management / 合同管理系统 / prod /
      http://localhost:8081 / http://contract-api:8081 / /contract_management
      已显式传入的接入参数优先于预设值。

接入参数（对应后端 POST /api/v1/subsystem-onboarding）：
  --application-code CODE       Application.Code，必填，例如 contract_management
  --application-name NAME       Application.Name，必填，例如 合同管理系统
  --environment ENV             Environment.Environment：dev/test/staging/prod，默认 prod
  --public-base-url URL          Environment.BaseURL，对外统一入口，必填，例如 http://localhost:8081
  --upstream-url URL             Environment.UpstreamURL，子系统内网地址，必填，例如 http://contract-api:8081
  --path-prefix PATH             Environment.PathPrefix，默认 /<application-code>，例如 /contract_management
  --client-type TYPE             OAuth Client 类型：confidential/public，默认 confidential
  --initial-admin-user-id ID     首个业务管理员用户 ID；未指定时使用当前登录的平台操作者
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

执行控制：
  -i, --interactive              即使参数已完整，也进入中文配置向导并确认
  -y, --yes                      跳过向导/预设的最终确认；适用于 CI
  --dry-run                      仅校验并显示最终配置，不登录、不调用 API、不写入配置
  --preset NAME                  使用快捷预设；当前支持 contract-management-local
  -h, --help                     显示帮助

示例（自动进入向导）：
  bash scripts/subsystem-onboarding.sh

示例（合同管理本地快捷接入）：
  bash scripts/subsystem-onboarding.sh \
    --preset contract-management-local \
    --account admin

示例（完整非交互参数）：
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
    --password-stdin --yes --account admin \
    --application-code contract_management \
    --application-name '合同管理系统' \
    --public-base-url https://portal.example.com \
    --upstream-url http://contract-api:8081

说明：
  1. 本脚本仅用于首次创建应用环境；子系统代码、镜像、前后端功能模块和业务迁移的常规更新不需要、也不得执行接入或撤销脚本。
  2. 已接入环境的统一登录配置由基础平台持久化保存，正常重启、重新构建或发布子系统不会删除或重建该配置。
  3. 确需变更 BaseURL、UpstreamURL、PathPrefix 或 OAuth 回调时，走受控配置变更流程；不要通过撤销后重建绕过配置变更。
  4. 脚本自行登录时会在结束前主动退出，避免单终端登录策略留下占用会话。
  5. --cookie-file 代表复用调用者已有会话，脚本不会退出该会话。
  6. portal-gateway.sh 是底层网关维护工具，不替代本脚本的完整接入流程。
USAGE
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || {
    log "ERROR" "缺少命令：$1"
    exit 2
  }
}

can_interact() {
  [[ -t 0 && -t 1 ]]
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
      --initial-admin-user-id)
        [[ $# -ge 2 ]] || { log "ERROR" "--initial-admin-user-id 缺少参数"; exit 2; }
        INITIAL_ADMIN_USER_ID="$2"; shift 2 ;;
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
      -i|--interactive)
        INTERACTIVE=true; CONFIRMATION_REQUIRED=true; shift ;;
      -y|--yes)
        ASSUME_YES=true; shift ;;
      --dry-run)
        DRY_RUN=true; CONFIRMATION_REQUIRED=true; shift ;;
      --preset)
        [[ $# -ge 2 ]] || { log "ERROR" "--preset 缺少参数"; exit 2; }
        PRESET="$2"; CONFIRMATION_REQUIRED=true; shift 2 ;;
      -h|--help)
        usage; exit 0 ;;
      *)
        log "ERROR" "未知参数：$1"
        usage >&2
        exit 2 ;;
    esac
  done
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
    *)
      log "ERROR" "未知快捷预设：${PRESET}。当前仅支持 contract-management-local"
      exit 2 ;;
  esac
}

required_configuration_missing() {
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

lowercase() {
  # macOS 自带 Bash 3.2 不支持 ${value,,}，因此使用 tr 做兼容转换。
  printf '%s' "$1" | tr '[:upper:]' '[:lower:]'
}

suggest_application_name() {
  case "$(lowercase "$APPLICATION_CODE")" in
    contract_management|contract-management) printf '%s' '合同管理系统' ;;
    *) printf '%s' '' ;;
  esac
}

suggest_upstream_url() {
  case "$(lowercase "$APPLICATION_CODE")" in
    contract_management|contract-management) printf '%s' 'http://contract-api:8081' ;;
    *) printf '%s' '' ;;
  esac
}

collect_interactive_config() {
  printf '\n统一登录目标 — 子系统接入向导\n' >&2
  printf '请填写对外访问地址与子系统内网地址。密码、Cookie 和 OAuth Secret 不会回显。\n\n' >&2

  prompt_value API_BASE_URL '基础平台 API 地址' "$API_BASE_URL" '通常为 http://127.0.0.1:8081/api/v1；可通过 BASIC_PLATFORM_API_BASE_URL 覆盖。'
  prompt_value APPLICATION_CODE '应用编码' "$APPLICATION_CODE" '小写字母开头；可使用数字、.、_、-。例如 contract_management。'
  prompt_value APPLICATION_NAME '应用名称' "$(suggest_application_name)" '例如 合同管理系统。'
  prompt_value ENVIRONMENT '环境' "$ENVIRONMENT" '仅支持 dev、test、staging、prod。'
  prompt_value PUBLIC_BASE_URL '对外统一入口 BaseURL' "${PUBLIC_BASE_URL:-http://localhost:8081}" '填写用户浏览器访问的门户 origin，不要带 /api/v1 或业务路径。'
  prompt_value UPSTREAM_URL '子系统 UpstreamURL' "$(suggest_upstream_url)" '填写 Docker/内网中可被网关访问的地址，例如 http://contract-api:8081。'
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
  } <"$normalized_file"

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

write_onboarding_payload() {
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

diagnose_connection_failure() {
  local endpoint="$1"
  log "ERROR" "无法连接平台接口：${API_BASE_URL}${endpoint}"
  cat >&2 <<'EOF_DIAG'
可按以下顺序排查：
  1. 在 platform 目录执行：bash scripts/docker-local.sh ps
  2. 查看基础平台日志：bash scripts/docker-local.sh logs api subsystem-provisioner
  3. 确认 --api-base-url 指向平台 API（默认 http://127.0.0.1:8081/api/v1），不是子系统地址。
  4. 若刚更新了基础平台后端路由或迁移，执行：bash scripts/docker-local.sh refresh-api
EOF_DIAG
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
    print("处理建议：该环境已经完成接入。子系统代码、镜像、前端、后端、功能模块和业务迁移的正常更新均不需要执行接入或撤销脚本，现有统一登录配置会保留。只有永久下线该环境时才使用 subsystem-offboarding.sh；若确需变更 BaseURL、UpstreamURL、PathPrefix 或 OAuth 回调，请走受控配置变更流程，不能通过撤销后重建绕过。", file=sys.stderr)
elif code in {"IAM_CONFLICT", "IAM_VERSION_CONFLICT"}:
    print("处理建议：存在资源冲突或配置已变更。检查应用编码、环境、路径前缀和 OAuth Client 是否被其他接入占用；不要通过重复执行脚本覆盖现有配置。", file=sys.stderr)
elif code in {"PLATFORM_DEPENDENCY_UNAVAILABLE", "PLATFORM_INTERNAL_ERROR"} or status.startswith("5"):
    print("处理建议：基础平台 API 或受控 provisioner 当前不可用。执行 `bash scripts/docker-local.sh ps`，再执行 `bash scripts/docker-local.sh logs api subsystem-provisioner`；如刚更新后端，执行 `bash scripts/docker-local.sh refresh-api` 后重试。", file=sys.stderr)
elif code in {"AUTH_UNAUTHENTICATED", "AUTH_SESSION_EXPIRED"} or status == "401":
    print("处理建议：登录状态无效或 Cookie 已过期。不要复用旧 Cookie，改用 --account 重新登录；使用 --cookie-file 时请确认文件来自同一基础平台地址。", file=sys.stderr)
elif status == "403":
    print("处理建议：当前账号没有子系统接入所需的基础平台管理权限。请由超级管理员核对应用、环境、登录目标和 OAuth 客户端的创建权限。", file=sys.stderr)
elif status == "422":
    print("处理建议：检查参数：BaseURL 只能是对外 origin；UpstreamURL 必须为网关可达的内网 http/https 地址；PathPrefix 必须是非根绝对路径。", file=sys.stderr)
elif code == "PLATFORM_METHOD_NOT_ALLOWED" or status == "405":
    print("处理建议：运行中的 API 镜像可能未包含当前接口。执行 `bash scripts/docker-local.sh refresh-api` 后重试。", file=sys.stderr)
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

print_configuration_summary() {
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
  初始业务管理员：${INITIAL_ADMIN_USER_ID:-当前登录的平台操作者}
  平台 API：${API_BASE_URL}
EOF_SUMMARY
  if [[ "$DRY_RUN" == true ]]; then
    printf '  认证方式：不认证（dry-run 不调用平台 API）\n' >&2
  elif [[ -n "$COOKIE_FILE" ]]; then
    printf '  认证方式：复用 Cookie 文件（路径不回显）\n' >&2
  else
    printf '  认证方式：平台管理员账号 %s（密码不回显）\n' "$ACCOUNT" >&2
  fi
}

confirm_configuration() {
  [[ "$ASSUME_YES" == true ]] && return 0
  [[ "$CONFIRMATION_REQUIRED" == true ]] || return 0
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

run_interactive_wizard() {
  while true; do
    if ! collect_interactive_config; then
      log "WARN" "输入未完成，请重新填写。"
      continue
    fi
    if validate_and_normalize; then
      break
    fi
    log "WARN" "以上配置未通过校验，请根据提示重新输入。"
  done
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

main() {
  parse_args "$@"
  require_command curl
  require_command python3
  apply_preset

  TEMP_DIR="$(mktemp -d "${TMPDIR:-/tmp}/subsystem-onboarding.XXXXXX")"
  RESPONSE_FILE="$TEMP_DIR/onboarding-response.json"
  trap cleanup EXIT

  local needs_wizard=false
  if [[ "$INTERACTIVE" == true ]] || required_configuration_missing || \
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
      log "ERROR" "可使用快捷方式：bash scripts/subsystem-onboarding.sh --preset contract-management-local --account admin"
      exit 2
    fi
    CONFIRMATION_REQUIRED=true
    run_interactive_wizard
  else
    if ! validate_and_normalize; then
      exit 2
    fi
  fi

  print_configuration_summary
  if ! confirm_configuration; then
    exit 0
  fi

  if [[ "$DRY_RUN" == true ]]; then
    log "INFO" "配置校验通过（dry-run）。未登录、未调用平台 API，也未写入任何配置。"
    exit 0
  fi

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
    diagnose_connection_failure "/subsystem-onboarding"
    exit 1
  }

  if [[ "$status" != "201" ]]; then
    print_api_error "$status" "$RESPONSE_FILE"
    exit 1
  fi

  print_success "$RESPONSE_FILE"
}

main "$@"
