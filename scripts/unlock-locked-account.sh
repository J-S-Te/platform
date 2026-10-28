#!/usr/bin/env bash
# 解锁被登录安全策略锁定的账号（走平台正式接口，不改数据库）。
#
# 前置条件：
#   1. 另有一个具备 platform:locked-account:read / :unlock 的账号
#      （platform-super-admin 或 platform-security-admin 角色持有这两个权限）；
#   2. 目标服务器可访问平台入口地址。
#
# 用法：
#   ./unlock-locked-account.sh --base-url https://host --admin localadmin --target admins
#   ./unlock-locked-account.sh --base-url https://host --admin localadmin --list
#   ./unlock-locked-account.sh ... --password-stdin < 密码文件
#   ./unlock-locked-account.sh --base-url https://host --probe
#   ./unlock-locked-account.sh --base-url https://host --cookie "bp_session=..." --target admins
#   （Cookie 名默认 bp_session，可用 --cookie-name 覆盖；从浏览器开发者工具复制）
#
# 生产环境通常开启凭据加密（uip-credential/v1），密码模式会被 422 拒绝；
# 此时用浏览器的会话 Cookie（--cookie）或直接在界面里解锁。
#
# 说明：
#   - 密码只从标准输入/终端读取，不接受命令行参数（避免进入 shell 历史与进程列表）；
#   - 只调用 POST /api/v1/security/locked-accounts/{account_id}/unlock；
#   - 解锁不会恢复锁定期间已被撤销的会话，目标账号需要重新登录；
#   - 该操作会被平台请求审计记录（操作者、来源地址、结果）。
set -euo pipefail

BASE_URL=""
ORIGIN=""
ADMIN=""
TARGET="admins"
ACCOUNT_ID=""
LIST_ONLY=0
ASSUME_YES=0
PASSWORD_STDIN=0
PASSWORD=""
COOKIE=""
COOKIE_NAME="bp_session"
PROBE=0

usage() {
    sed -n '2,22p' "$0" | sed 's/^# \{0,1\}//'
    exit "${1:-0}"
}

log() { printf '[unlock] %s\n' "$*" >&2; }
fail() { printf '[unlock] ERROR: %s\n' "$*" >&2; exit 1; }

while [[ $# -gt 0 ]]; do
    case "$1" in
        --base-url) BASE_URL="${2:-}"; shift 2 ;;
        --origin) ORIGIN="${2:-}"; shift 2 ;;
        --admin) ADMIN="${2:-}"; shift 2 ;;
        --target) TARGET="${2:-}"; shift 2 ;;
        --account-id) ACCOUNT_ID="${2:-}"; shift 2 ;;
        --list) LIST_ONLY=1; shift ;;
        --yes) ASSUME_YES=1; shift ;;
        --password-stdin) PASSWORD_STDIN=1; shift ;;
        --cookie) COOKIE="${2:-}"; shift 2 ;;
        --probe) PROBE=1; shift ;;
        --cookie-name) COOKIE_NAME="${2:-bp_session}"; shift 2 ;;
        -h|--help) usage 0 ;;
        *) usage 1 ;;
    esac
done

[[ -n "${BASE_URL}" ]] || fail "缺少 --base-url"
BASE_URL="${BASE_URL%/}"
[[ -n "${ORIGIN}" ]] || ORIGIN="${BASE_URL}"
if [[ "${PROBE}" -eq 1 ]]; then
    probe_code() {
        local path="$1"
        local code
        code="$(curl -sS -o /dev/null -w '%{http_code}' -H "Accept: application/json" "${BASE_URL}${path}" || true)"
        printf '%s' "${code}"
    }
    locked_code="$(probe_code "/api/v1/security/locked-accounts")"
    enc_code="$(probe_code "/api/v1/auth/credential-challenges")"
    printf 'entry=%s\n' "${BASE_URL}"
    printf 'locked-accounts endpoint: %s (%s)\n' "${locked_code}" \
        "$([[ "${locked_code}" == "401" || "${locked_code}" == "403" ]] && echo 存在·需认证 || echo 缺失/不可用)"
    case "${enc_code}" in
        404) enc_state="未启用·可用明文密码模式" ;;
        401|403|405) enc_state="已启用·需 --cookie 或界面解锁" ;;
        000) enc_state="入口不可达" ;;
        *) enc_state="未知（HTTP ${enc_code}）" ;;
    esac
    printf 'credential-encryption: %s (%s)\n' "${enc_code}" "${enc_state}"
    exit 0
fi

[[ -n "${ADMIN}" || -n "${COOKIE}" ]] || fail "缺少 --admin（有解锁权限的账号名）或 --cookie"
[[ "${BASE_URL}" == https://* ]] || log "警告：入口不是 https，密码与会话将以明文传输"

command -v curl >/dev/null 2>&1 || fail "需要 curl"

if [[ "${LIST_ONLY}" -eq 0 && -z "${ACCOUNT_ID}" && -z "${TARGET}" ]]; then
    fail "缺少 --target 或 --account-id"
fi

if [[ -z "${COOKIE}" ]]; then
    if [[ "${PASSWORD_STDIN}" -eq 1 ]]; then
        IFS= read -r PASSWORD || true
    else
        [[ -t 0 ]] || fail "非交互环境请使用 --password-stdin 或 --cookie"
        read -r -s -p "有解锁权限的账号 [${ADMIN}] 密码：" PASSWORD
        printf '\n' >&2
    fi
    [[ -n "${PASSWORD}" ]] || fail "密码为空"
fi

WORKDIR="$(mktemp -d "${TMPDIR:-/tmp}/unlock.XXXXXX")"
chmod 700 "${WORKDIR}"
COOKIE_JAR="${WORKDIR}/cookies"
BODY_FILE="${WORKDIR}/body"
trap 'rm -rf "${WORKDIR}"' EXIT

# api METHOD PATH [DATA_FILE] -> 打印 HTTP 状态码；响应体写入 BODY_FILE。
api() {
    local method="$1" path="$2" data_file="${3:-}"
    local args=(-sS -o "${BODY_FILE}" -w '%{http_code}'
        -b "${COOKIE_JAR}" -c "${COOKIE_JAR}"
        -H "Accept: application/json" -H "Origin: ${ORIGIN}"
        -X "${method}" "${BASE_URL}${path}")
    if [[ -n "${COOKIE}" ]]; then
        args+=(-H "Cookie: ${COOKIE}")
    fi
    if [[ -n "${data_file}" ]]; then
        args+=(-H "Content-Type: application/json" --data-binary "@${data_file}")
    fi
    curl "${args[@]}" || true
}

json_escape() {
    printf '%s' "$1" | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g'
}

# 从锁定列表响应提取行：account_id<TAB>account_name<TAB>locked_until<TAB>failure_count
locked_rows() {
    if command -v python3 >/dev/null 2>&1; then
        python3 -c '
import json,sys
doc=json.load(sys.stdin)
data=doc.get("data") or doc
for item in data.get("items") or []:
    print("\t".join([str(item.get("account_id","")), str(item.get("account_name","")),
                     str(item.get("locked_until") or ""), str(item.get("failure_count",""))]))
'
        return
    fi
    # 无 python3 时退化为按记录切分；账号名/ID 是 ASCII，足够定位目标账号。
    tr '{' '\n' | sed -n 's/.*"account_id":"\([^"]*\)".*"account_name":"\([^"]*\)".*/\1	\2		/p'
}

# 分页收集锁定账号，输出全部行。
collect_locked() {
    local page=1 code body total
    while :; do
        code="$(api GET "/api/v1/security/locked-accounts?page=${page}&page_size=100")"
        body="$(cat "${BODY_FILE}")"
        case "${code}" in
            200) ;;
            403) fail "当前账号缺少 platform:locked-account:read 权限" ;;
            *)   fail "查询锁定账号失败，HTTP ${code}：$(printf '%s' "${body}" | head -c 200)" ;;
        esac
        printf '%s' "${body}" | locked_rows
        total="$(printf '%s' "${body}" | sed -n 's/.*"total":\([0-9]*\).*/\1/p' | head -1)"
        if [[ -z "${total}" || "${total}" -le $((page * 100)) ]]; then
            break
        fi
        page=$((page + 1))
    done
}

# ---- 1. 认证 ----
if [[ -n "${COOKIE}" ]]; then
    me_code="$(api GET /api/v1/auth/me)"
    [[ "${me_code}" == "200" ]] || fail "提供的会话 Cookie 无效或已过期（HTTP ${me_code}），请重新登录后复制 ${COOKIE_NAME}"
    log "使用浏览器会话 Cookie 认证"
else
login_payload="${WORKDIR}/login.json"
printf '{"account":"%s","password":"%s","login_type":"password"}' \
    "$(json_escape "${ADMIN}")" "$(json_escape "${PASSWORD}")" > "${login_payload}"

login_code="$(api POST /api/v1/auth/login "${login_payload}")"
login_body="$(cat "${BODY_FILE}")"
rm -f "${login_payload}"
case "${login_code}" in
    200) log "登录成功：${ADMIN}" ;;
    401) fail "登录失败：账号或密码错误，或该账号也被锁定" ;;
    403) fail "登录被拒绝：Origin 校验或权限问题（检查 --origin 是否与平台对外地址一致）" ;;
    422) fail "平台要求凭据加密（uip-credential/v1），请改用 --cookie 或直接在界面解锁" ;;
    429) fail "登录被限流，请稍后重试" ;;
    *)   fail "登录失败，HTTP ${login_code}：$(printf '%s' "${login_body}" | head -c 200)" ;;
esac
fi

# ---- 2. 查锁定账号 ----
rows="$(collect_locked)"
if [[ "${LIST_ONLY}" -eq 1 ]]; then
    if [[ -z "${rows}" ]]; then
        log "当前没有被锁定的账号"
    else
        printf 'account_id\taccount_name\tlocked_until\tfailure_count\n'
        printf '%s\n' "${rows}"
    fi
    exit 0
fi

if [[ -z "${ACCOUNT_ID}" ]]; then
    ACCOUNT_ID="$(printf '%s\n' "${rows}" | awk -F'\t' -v name="${TARGET}" '$2 == name {print $1; exit}')"
    if [[ -z "${ACCOUNT_ID}" ]]; then
        log "锁定列表里没有 ${TARGET}：可能已自动过期，或账号名不一致"
        printf '%s\n' "${rows}" | sed -n '1,20p' >&2
        exit 3
    fi
fi

log "目标账号：${TARGET:-${ACCOUNT_ID}}（account_id=${ACCOUNT_ID}）"
if [[ "${ASSUME_YES}" -eq 0 ]]; then
    [[ -t 0 ]] || fail "非交互环境请加 --yes"
    read -r -p "确认解除该账号锁定？[y/N] " answer
    [[ "${answer}" == "y" || "${answer}" == "Y" ]] || { log "已取消"; exit 0; }
fi

# ---- 3. 解锁 ----
unlock_code="$(api POST "/api/v1/security/locked-accounts/${ACCOUNT_ID}/unlock")"
unlock_body="$(cat "${BODY_FILE}")"
case "${unlock_code}" in
    200) log "解锁成功：$(printf '%s' "${unlock_body}" | head -c 200)" ;;
    403) fail "缺少 platform:locked-account:unlock 权限" ;;
    404) fail "账号不存在或不属于当前租户" ;;
    409) fail "该账号当前没有生效中的锁定（可能已过期或被其他管理员解锁）" ;;
    *)   fail "解锁失败，HTTP ${unlock_code}：$(printf '%s' "${unlock_body}" | head -c 200)" ;;
esac

# ---- 4. 复核 ----
remaining="$(collect_locked | awk -F'\t' -v id="${ACCOUNT_ID}" '$1 == id {print}')"
if [[ -z "${remaining}" ]]; then
    log "复核通过：该账号已不在锁定列表中，可用原密码重新登录"
else
    fail "复核未通过，账号仍在锁定列表：${remaining}"
fi
