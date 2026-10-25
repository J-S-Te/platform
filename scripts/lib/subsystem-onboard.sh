#!/usr/bin/env bash
# subsystem.sh onboard 子命令（批次 5 库化，逐字迁移）；仅含函数定义，无顶层副作用——由 scripts/subsystem.sh 入口在全局变量区之后 source。

# ============================ onboard 子命令 ============================
onboard_usage() {
  cat <<'USAGE'
用法：subsystem.sh onboard [认证参数] [接入参数]

快捷预设：
  --preset contract-management-local
      填入合同管理系统本地 Docker 默认配置：
      contract_management / 合同管理系统 / dev /
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
      [[ "$ENVIRONMENT_EXPLICIT" == true ]] || ENVIRONMENT="dev"
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

# print_deployment_failed_summary 打印后台部署失败的真实错误与下一步指引，
# 不再伪装成 HTTP 503（避免误导操作者去查无关的依赖可用性）。
print_deployment_failed_summary() {
  python3 - "$1" <<'PY'
import json
import sys
try:
    with open(sys.argv[1], encoding="utf-8") as handle:
        envelope = json.load(handle)
except Exception:
    print("后台部署失败，且未能读取最终状态；请用 status 子命令查询。")
    sys.exit(0)
data = envelope.get("data") or {}
print("后台部署失败：")
print(f"  应用/环境：{data.get('application_code', '-')} / {data.get('environment', '-')}")
print(f"  状态：{data.get('status', '-')}")
print(f"  错误码：{data.get('last_error_code', '-')}")
print(f"  错误说明：{data.get('last_error') or data.get('last_error_message') or '-'}")
next_action = data.get('next_action') or ''
if next_action:
    print(f"  下一步：{next_action}")
print("  修复后使用 retry 子命令重试。")
PY
}

# onboard_print_accepted_and_final 打印受理回执与后台终态摘要（异步接入路径）。
onboard_print_accepted_and_final() {
  python3 - "$1" <<'PY'
import json
import sys
with open(sys.argv[1], encoding="utf-8") as handle:
    envelope = json.load(handle)
data = envelope.get("data") or {}
print("子系统接入完成（后台编排终态）：")
print(f"  应用/环境：{data.get('application_code', '-')} / {data.get('environment', '-')}")
print(f"  状态：{data.get('status', '-')}")
print(f"  追踪号：{envelope.get('request_id') or '-'}")
print("  提示：OAuth Client Secret 不回显；由受控 provisioner 写入子系统运行配置。")
PY
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

  if [[ "$status" == "202" ]]; then
    log "INFO" "接入请求已受理，部署正在后台执行；等待终态（DEPLOY_WAIT_TIMEOUT 可调，默认 900s）"
    if wait_for_deployment_completion "$APPLICATION_CODE" "$ENVIRONMENT" "$RESPONSE_FILE"; then
      onboard_print_accepted_and_final "$RESPONSE_FILE"
      exit 0
    fi
    print_deployment_failed_summary "${RESPONSE_FILE}.final"
    exit 1
  fi
  if [[ "$status" != "201" ]]; then
    print_api_error "$status" "$RESPONSE_FILE" "/subsystem-onboarding"
    exit 1
  fi

  onboard_print_success "$RESPONSE_FILE"
}
