#!/usr/bin/env bash
# subsystem.sh HTTP/会话与错误诊断（批次 5 库化，逐字迁移）；仅含函数定义，无顶层副作用——由 scripts/subsystem.sh 入口在全局变量区之后 source。

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
  # macOS 的 Bash 3.2 配合 set -u 时，空数组展开须使用 ${arr[@]+...} 写法。
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
