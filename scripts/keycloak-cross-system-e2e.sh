#!/usr/bin/env bash
# keycloak-cross-system-e2e.sh
# -----------------------------------------------------------------------------
# 只读的 Keycloak 跨系统接入检查。
#
# 本脚本不会启动/停止容器，不读取或写入运行时 .env，不创建 Keycloak Client、用户、
# 会话或任何业务数据。它只执行 HTTP GET：
#   1. Keycloak Realm discovery、issuer 一致性和 JWKS；
#   2. 基础平台 discovery 暴露的在线 authorization-context 端点；
#   3. 合同、项目、CRM、客户门户的统一网关健康检查与未登录会话端点；
#   4. （可选）携带现成 Keycloak access token 调用 authorization-context；
#   5. （可选）使用由 Keycloak 登录建立的项目管理会话，读取受限项目 API，并
#      在外部撤权后验证同一会话已被拒绝。
#
# 无凭据时，第 4 项会明确 SKIP，前 3 项仍可用于 CI/部署后的只读门禁。
# 不要通过命令行传 token；请使用 KEYCLOAK_E2E_ACCESS_TOKEN 环境变量或
# KEYCLOAK_E2E_ACCESS_TOKEN_FILE（权限应为 0600 的本地文件），避免泄漏到 history。
# -----------------------------------------------------------------------------

set -Eeuo pipefail

LOG_PREFIX="[keycloak-cross-system-e2e]"
BASE_URL="${KEYCLOAK_E2E_BASE_URL:-http://localhost:8081}"
ISSUER="${KEYCLOAK_E2E_ISSUER:-http://localhost:18090/realms/basic-platform}"
ACCESS_TOKEN="${KEYCLOAK_E2E_ACCESS_TOKEN:-}"
ACCESS_TOKEN_FILE="${KEYCLOAK_E2E_ACCESS_TOKEN_FILE:-}"
REQUIRE_AUTH_CONTEXT="${KEYCLOAK_E2E_REQUIRE_AUTH_CONTEXT:-false}"
PROJECT_SESSION_COOKIE="${KEYCLOAK_E2E_PROJECT_SESSION_COOKIE:-}"
PROJECT_SESSION_COOKIE_FILE="${KEYCLOAK_E2E_PROJECT_SESSION_COOKIE_FILE:-}"
PROJECT_SESSION_COOKIE_NAME="${KEYCLOAK_E2E_PROJECT_SESSION_COOKIE_NAME:-project_management_session}"
PROJECT_READ_PATH="${KEYCLOAK_E2E_PROJECT_READ_PATH:-/project_management/api/v1/projects}"
PROJECT_READ_EXPECTED_STATUS="${KEYCLOAK_E2E_PROJECT_READ_EXPECTED_STATUS:-200}"
PROJECT_EXPECT_REVOKED="${KEYCLOAK_E2E_PROJECT_EXPECT_REVOKED:-false}"
TIMEOUT_SECONDS="${KEYCLOAK_E2E_TIMEOUT_SECONDS:-10}"

usage() {
  cat <<'EOF'
用法：
  bash scripts/keycloak-cross-system-e2e.sh [选项]

选项：
  --base-url URL          统一网关公网地址，默认 http://localhost:8081
  --issuer URL            Keycloak Realm issuer，默认 http://localhost:18090/realms/basic-platform
  --access-token-file F   从本地受保护文件读取 Keycloak access token；不建议传命令行 token
  --require-auth-context  未提供 token 时将 authorization-context 检查视为失败而非跳过
  --timeout SECONDS       单次 HTTP 超时秒数，默认 10
  -h, --help              显示本说明

也可使用环境变量：
  KEYCLOAK_E2E_BASE_URL
  KEYCLOAK_E2E_ISSUER
  KEYCLOAK_E2E_ACCESS_TOKEN
  KEYCLOAK_E2E_ACCESS_TOKEN_FILE
  KEYCLOAK_E2E_REQUIRE_AUTH_CONTEXT=true
  KEYCLOAK_E2E_PROJECT_SESSION_COOKIE_FILE
  KEYCLOAK_E2E_PROJECT_SESSION_COOKIE_NAME=project_management_session
  KEYCLOAK_E2E_PROJECT_READ_PATH=/project_management/api/v1/projects
  KEYCLOAK_E2E_PROJECT_READ_EXPECTED_STATUS=200
  KEYCLOAK_E2E_PROJECT_EXPECT_REVOKED=true
  KEYCLOAK_E2E_TIMEOUT_SECONDS

前置条件：
  - curl 和 jq 已安装；目标环境的 Keycloak、基础平台网关及四个子系统已启动。
  - 可选的 access token 必须是已映射到基础平台人员/租户、且 aud/azp 对应已接入应用
    Client 的 Keycloak access token。client_credentials token 通常没有人员映射，不能
    通过 authorization-context，是预期的安全限制。
  - 项目管理受限 API 使用自己的持久会话而不是直接接受浏览器 access token。请在完成
    Keycloak 登录后，从受控测试会话导出 cookie 值至 0600 文件，并设置
    KEYCLOAK_E2E_PROJECT_SESSION_COOKIE_FILE。不要把 cookie 放入命令行或提交到仓库。

撤权契约（两次只读运行）：
  1. 使用仍有 project.read 权限的同一会话运行本脚本，预期受限 API 为 200（或通过
     KEYCLOAK_E2E_PROJECT_READ_EXPECTED_STATUS 指定实际允许状态）。
  2. 在基础平台撤销该人员的项目管理授权，等待在线 authorization revision 刷新；随后以
     相同 cookie 设置 KEYCLOAK_E2E_PROJECT_EXPECT_REVOKED=true 再运行。脚本仅接受
     401 或 403，任何 200 都会失败。脚本自身不会执行撤权或写入业务数据。

检查范围：
  Keycloak discovery / issuer / JWKS、平台 authorization-context、合同管理、项目管理、
  客户与商机管理（CRM）、客户自助门户的网关健康检查。

安全性：
  本脚本仅发起 GET 请求；不会打印 access token 或响应中可能包含的授权详情。
EOF
}

fail() {
  printf '%s ERROR: %s\n' "$LOG_PREFIX" "$*" >&2
  exit 1
}

log() {
  printf '%s %s\n' "$LOG_PREFIX" "$*"
}

while (($# > 0)); do
  case "$1" in
    --base-url)
      (($# >= 2)) || fail '--base-url 缺少参数'
      BASE_URL="$2"
      shift 2
      ;;
    --issuer)
      (($# >= 2)) || fail '--issuer 缺少参数'
      ISSUER="$2"
      shift 2
      ;;
    --access-token-file)
      (($# >= 2)) || fail '--access-token-file 缺少参数'
      ACCESS_TOKEN_FILE="$2"
      shift 2
      ;;
    --require-auth-context)
      REQUIRE_AUTH_CONTEXT=true
      shift
      ;;
    --timeout)
      (($# >= 2)) || fail '--timeout 缺少参数'
      TIMEOUT_SECONDS="$2"
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      fail "未知参数：$1"
      ;;
  esac
done

command -v curl >/dev/null 2>&1 || fail '未找到 curl'
command -v jq >/dev/null 2>&1 || fail '未找到 jq'
[[ "$TIMEOUT_SECONDS" =~ ^[1-9][0-9]*$ ]] || fail '--timeout 必须为正整数'

normalize_base_url() {
  local value="$1"
  value="${value%/}"
  [[ "$value" =~ ^https?://[^/?#]+(/[^?#]*)?$ ]] || fail "URL 非法：$1"
  printf '%s' "$value"
}

BASE_URL="$(normalize_base_url "$BASE_URL")"
ISSUER="$(normalize_base_url "$ISSUER")"

if [[ -n "$ACCESS_TOKEN_FILE" ]]; then
  [[ -f "$ACCESS_TOKEN_FILE" ]] || fail "access token 文件不存在：$ACCESS_TOKEN_FILE"
  [[ -z "$ACCESS_TOKEN" ]] || fail '不要同时设置 KEYCLOAK_E2E_ACCESS_TOKEN 和 access-token-file'
  ACCESS_TOKEN="$(tr -d '\r\n' < "$ACCESS_TOKEN_FILE")"
fi

if [[ -n "$PROJECT_SESSION_COOKIE_FILE" ]]; then
  [[ -f "$PROJECT_SESSION_COOKIE_FILE" ]] || fail "项目会话 cookie 文件不存在：$PROJECT_SESSION_COOKIE_FILE"
  [[ -z "$PROJECT_SESSION_COOKIE" ]] || fail '不要同时设置 KEYCLOAK_E2E_PROJECT_SESSION_COOKIE 和 session-cookie-file'
  PROJECT_SESSION_COOKIE="$(tr -d '\r\n' < "$PROJECT_SESSION_COOKIE_FILE")"
fi

if [[ -n "$ACCESS_TOKEN" && "$ACCESS_TOKEN" != *.*.* ]]; then
  fail '提供的 access token 不是预期的 JWT 格式'
fi

[[ "$PROJECT_SESSION_COOKIE_NAME" =~ ^[A-Za-z0-9_-]+$ ]] || fail '项目会话 cookie 名称非法'
[[ "$PROJECT_READ_PATH" =~ ^/[^?#]*$ ]] || fail '项目受限 API 路径必须以 / 开头且不能包含 query 或 fragment'
[[ "$PROJECT_READ_EXPECTED_STATUS" =~ ^[1-5][0-9][0-9]$ ]] || fail '项目受限 API 预期状态必须是 HTTP 状态码'
case "$PROJECT_EXPECT_REVOKED" in true|false) ;; *) fail 'KEYCLOAK_E2E_PROJECT_EXPECT_REVOKED 只能为 true 或 false' ;; esac

BODY_FILE="$(mktemp)"
trap 'rm -f -- "$BODY_FILE"' EXIT

http_status() {
  local url="$1"
  shift
  : > "$BODY_FILE"
  curl --silent --show-error --location --max-time "$TIMEOUT_SECONDS" \
    --output "$BODY_FILE" --write-out '%{http_code}' "$@" "$url"
}

json_get() {
  local url="$1"
  local label="$2"
  local status
  status="$(http_status "$url")" || fail "$label 无法连接：$url"
  [[ "$status" == '200' ]] || fail "$label 返回 HTTP $status：$url"
  jq -e . "$BODY_FILE" >/dev/null 2>&1 || fail "$label 未返回 JSON：$url"
}

check_health() {
  local label="$1"
  local path="$2"
  local status
  status="$(http_status "${BASE_URL}${path}")" || fail "$label 无法连接"
  [[ "$status" == '200' ]] || fail "$label 返回 HTTP $status（预期 200）：${BASE_URL}${path}"
  log "PASS ${label}"
}

check_anonymous_session_endpoint() {
  local label="$1"
  local path="$2"
  local status
  status="$(http_status "${BASE_URL}${path}")" || fail "$label 无法连接"
  case "$status" in
    401|503)
      log "PASS ${label}（未登录返回 HTTP ${status}）"
      ;;
    *)
      fail "$label 返回 HTTP $status（预期未登录为 401；CRM 未完成认证配置时允许 503）：${BASE_URL}${path}"
      ;;
  esac
}

log "开始只读检查：gateway=${BASE_URL} issuer=${ISSUER} timeout=${TIMEOUT_SECONDS}s"

# 1. Keycloak Realm discovery / issuer / JWKS。
keycloak_discovery_url="${ISSUER}/.well-known/openid-configuration"
json_get "$keycloak_discovery_url" 'Keycloak Realm discovery'
discovered_issuer="$(jq -er '.issuer | strings | select(length > 0)' "$BODY_FILE")" || fail 'Keycloak discovery 缺少 issuer'
[[ "$discovered_issuer" == "$ISSUER" ]] || fail "Keycloak issuer 不匹配：期望 ${ISSUER}，实际 ${discovered_issuer}"
jwks_uri="$(jq -er '.jwks_uri | strings | select(test("^https?://"))' "$BODY_FILE")" || fail 'Keycloak discovery 缺少合法 jwks_uri'
log 'PASS Keycloak Realm discovery 与 issuer'

json_get "$jwks_uri" 'Keycloak JWKS'
jq -e '(.keys | type == "array") and ([.keys[]? | select((.kty | type == "string") and (.kid | type == "string"))] | length > 0)' "$BODY_FILE" >/dev/null || \
  fail 'Keycloak JWKS 不含带 kid 的签名密钥'
log 'PASS Keycloak JWKS'

# 2. 基础平台 discovery 必须暴露在线授权上下文端点。它与 Keycloak discovery 是两个职责。
platform_discovery_url="${BASE_URL}/.well-known/openid-configuration"
json_get "$platform_discovery_url" '基础平台 discovery'
authorization_context_endpoint="$(jq -er '.authorization_context_endpoint | strings | select(test("^https?://"))' "$BODY_FILE")" || \
  fail '基础平台 discovery 缺少 authorization_context_endpoint'
expected_context_endpoint="${BASE_URL}/oauth2/authorization-context"
[[ "$authorization_context_endpoint" == "$expected_context_endpoint" ]] || \
  fail "authorization_context_endpoint 不匹配：期望 ${expected_context_endpoint}，实际 ${authorization_context_endpoint}"
log 'PASS 基础平台在线 authorization-context discovery'

# 未携带 token 时 401 是此受保护端点正常且必要的 fail-closed 行为。
context_anonymous_status="$(http_status "$authorization_context_endpoint")" || fail 'authorization-context 无法连接'
[[ "$context_anonymous_status" == '401' ]] || \
  fail "authorization-context 未携带 token 返回 HTTP ${context_anonymous_status}（预期 401）"
log 'PASS authorization-context 匿名请求被拒绝（HTTP 401）'

# 3. 子系统必须都能经同一网关达到各自 API；healthz 不要求登录，不读取业务数据。
check_health '合同管理 healthz' '/contract_management/healthz'
check_health '项目管理 healthz' '/project_management/healthz'
check_health '客户与商机管理（CRM）healthz' '/customer-opportunity/healthz'
check_health '客户自助门户 healthz' '/customer-portal/healthz'

# 该组额外验证 path-prefix 没有被网关吞掉；不读取会话内容。
check_anonymous_session_endpoint '合同管理 auth/me 路由' '/contract_management/api/v1/auth/me'
check_anonymous_session_endpoint '项目管理 auth/me 路由' '/project_management/api/v1/auth/me'
check_anonymous_session_endpoint 'CRM auth/me 路由' '/customer-opportunity/api/v1/auth/me'
check_anonymous_session_endpoint '客户门户 auth/me 路由' '/customer-portal/api/v1/auth/me'

# 4. 可选的真实授权链检查。令牌由调用方安全提供，脚本不申请、输出或持久化令牌。
if [[ -z "$ACCESS_TOKEN" ]]; then
  if [[ "$REQUIRE_AUTH_CONTEXT" == true ]]; then
    fail 'KEYCLOAK_E2E_REQUIRE_AUTH_CONTEXT=true，但未提供 Keycloak access token'
  fi
  log 'SKIP 携带 Keycloak token 的 authorization-context：未提供凭据（其余只读门禁已完成）'
else
  context_token_status="$(http_status "$authorization_context_endpoint" --header "Authorization: Bearer ${ACCESS_TOKEN}")" || \
    fail '携带 Keycloak token 的 authorization-context 无法连接'
  [[ "$context_token_status" == '200' ]] || \
    fail "携带 Keycloak token 的 authorization-context 返回 HTTP ${context_token_status}（需要已映射人员且 Client/环境授权有效）"
  jq -e '
    (.sub | type == "string" and length > 0) and
    (.identity_id | type == "string" and length > 0) and
    (.tenant_id | type == "string" and length > 0) and
    (.client_id | type == "string" and length > 0) and
    (.application_code | type == "string" and length > 0) and
    (.environment_code | type == "string" and length > 0) and
    (.roles | type == "array") and
    (.permissions | type == "array") and
    (.data_scopes | type == "array") and
    (.authorization_revision | type == "number")
  ' "$BODY_FILE" >/dev/null || fail 'authorization-context 返回体缺少 V2 在线授权字段'
  log 'PASS Keycloak token -> 基础平台 authorization-context（不输出授权详情）'
fi

# 5. 真实 Keycloak 登录后的项目管理受限 API。项目管理刻意使用持久会话而非把
# access token 直接转发给业务 API；cookie 对应的登录入口仍由 Keycloak 完成认证。
# 无受控测试会话时保持只读 SKIP，不会把“缺少凭据”伪装成系统故障。
if [[ -z "$PROJECT_SESSION_COOKIE" ]]; then
  if [[ "$PROJECT_EXPECT_REVOKED" == true ]]; then
    fail 'KEYCLOAK_E2E_PROJECT_EXPECT_REVOKED=true，但未提供项目管理测试会话 cookie'
  fi
  log 'SKIP 项目管理受限 API 与撤权契约：未提供 Keycloak 登录后的项目会话 cookie'
else
  project_read_status="$(http_status "${BASE_URL}${PROJECT_READ_PATH}" --header "Cookie: ${PROJECT_SESSION_COOKIE_NAME}=${PROJECT_SESSION_COOKIE}")" || \
    fail '项目管理受限 API 无法连接'
  if [[ "$PROJECT_EXPECT_REVOKED" == true ]]; then
    case "$project_read_status" in
      401|403)
        log "PASS 项目管理撤权后同一会话被拒绝（HTTP ${project_read_status}）"
        ;;
      *)
        fail "项目管理撤权后受限 API 返回 HTTP ${project_read_status}（预期 401 或 403）：${BASE_URL}${PROJECT_READ_PATH}"
        ;;
    esac
  elif [[ "$project_read_status" == "$PROJECT_READ_EXPECTED_STATUS" ]]; then
    log "PASS Keycloak 登录会话 -> 项目管理受限 API（HTTP ${project_read_status}）"
  else
    fail "项目管理受限 API 返回 HTTP ${project_read_status}（预期 ${PROJECT_READ_EXPECTED_STATUS}）：${BASE_URL}${PROJECT_READ_PATH}"
  fi
fi

log '完成：Keycloak、基础平台在线授权上下文、合同、项目、CRM、客户门户的只读跨系统检查均通过。'
