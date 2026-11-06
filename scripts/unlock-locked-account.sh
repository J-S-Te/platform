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
#   ./unlock-locked-account.sh --base-url https://host --cookie-stdin --target admins < 会话Cookie.txt
#   ./unlock-locked-account.sh --base-url https://host --cookie-file ~/bp.cookie --target admins
#   ./unlock-locked-account.sh --base-url https://host --cookie "bp_session=..." --target admins
#   （Cookie 名默认 bp_session，可用 --cookie-name 覆盖；从浏览器开发者工具复制）
#
# 生产环境通常开启凭据加密（uip-credential/v1），密码模式会被 422 拒绝；
# 此时用浏览器的会话 Cookie（优先 --cookie-stdin / --cookie-file）或直接在界面里解锁。
#
# 说明：
#   - 密码只从标准输入/终端读取，不接受命令行参数（避免进入 shell 历史与进程列表）；
#   - 会话 Cookie 优先用 --cookie-stdin（标准输入）或 --cookie-file（须为 0600 常规文件）：
#     值只写入 0700 临时目录内的 0600 文件，再以 curl -b <file> 发送，不进入命令行；
#   - --cookie 保留兼容旧用法，但值会进入 shell 历史与进程列表，脚本会打印警告；
#   - 只调用 POST /api/v1/security/locked-accounts/{account_id}/unlock；
#   - 账号/密码进入登录体前按 JSON 字符串转义：有 python3 时等价于
#     json.dumps(text, ensure_ascii=False)（覆盖 \" \\ \n \r \t \b \f 与全部 <0x20 控制字符，
#     中文等非 ASCII 原样保留）；无 python3 时降级实现只覆盖 \ " \n \r \t，
#     口令含 \b \f 或其他控制字符时仍可能拼出非法 JSON（平台回 400/422）；
#   - 临时目录 0700，其中的 Cookie jar、响应体、登录体一律 0600（启动即 umask 077）；
#   - 解锁不会恢复锁定期间已被撤销的会话，目标账号需要重新登录；
#   - 该操作会被平台请求审计记录（操作者、来源地址、结果）。
#
# 退出码：
#   0  成功（解锁完成并通过复核；--list 无锁定账号；--probe 探测完成；用户取消）
#   1  错误（登录/权限/接口失败、复核未通过、Cookie 文件校验失败等）
#   2  用法错误（未知参数、缺少必填参数、互斥参数同时指定）
#   3  目标未锁定或未找到（--target 指定的账号不在锁定列表中）
set -euo pipefail
# SEC：临时 cookie jar、响应体、登录体不依赖调用方 umask，统一 0600。
umask 077

BASE_URL=""
ORIGIN=""
ADMIN=""
TARGET="admins"
TARGET_SET=0
ACCOUNT_ID=""
LIST_ONLY=0
ASSUME_YES=0
PASSWORD_STDIN=0
PASSWORD=""
COOKIE=""
COOKIE_STDIN=0
COOKIE_FILE_IN=""
COOKIE_NAME="bp_session"
PROBE=0
WORKDIR=""
COOKIE_JAR=""
COOKIE_SOURCE_FILE=""
BODY_FILE=""

# 打印本文件顶部的用法注释块（遇到第一行非注释即停止），避免行号漂移导致帮助被截断。
usage() {
    awk 'NR > 1 && /^#/ { sub(/^# ?/, ""); print; next } NR > 1 { exit }' "$0"
    exit "${1:-0}"
}

log() { printf '[unlock] %s\n' "$*" >&2; }
fail() { printf '[unlock] ERROR: %s\n' "$*" >&2; exit 1; }
# 用法错误统一退出码 2（与 usage 注释块的退出码说明一致）。
usage_error() {
    printf '[unlock] ERROR: %s\n' "$*" >&2
    printf '[unlock] 提示：--help 可查看用法与退出码说明\n' >&2
    exit 2
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        --base-url) BASE_URL="${2:-}"; shift 2 ;;
        --origin) ORIGIN="${2:-}"; shift 2 ;;
        --admin) ADMIN="${2:-}"; shift 2 ;;
        --target) TARGET="${2:-}"; TARGET_SET=1; shift 2 ;;
        --account-id) ACCOUNT_ID="${2:-}"; shift 2 ;;
        --list) LIST_ONLY=1; shift ;;
        --yes) ASSUME_YES=1; shift ;;
        --password-stdin) PASSWORD_STDIN=1; shift ;;
        --cookie) COOKIE="${2:-}"; shift 2 ;;
        --cookie-stdin) COOKIE_STDIN=1; shift ;;
        --cookie-file) COOKIE_FILE_IN="${2:-}"; shift 2 ;;
        --probe) PROBE=1; shift ;;
        --cookie-name) COOKIE_NAME="${2:-bp_session}"; shift 2 ;;
        -h|--help) usage 0 ;;
        *) usage 2 ;;
    esac
done

[[ -n "${BASE_URL}" ]] || usage_error "缺少 --base-url"
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

[[ -n "${ADMIN}" || -n "${COOKIE}" || "${COOKIE_STDIN}" -eq 1 || -n "${COOKIE_FILE_IN}" ]] ||
    usage_error "缺少 --admin（有解锁权限的账号名）或会话 Cookie（--cookie-stdin / --cookie-file / --cookie）"
[[ "${BASE_URL}" == https://* ]] || log "警告：入口不是 https，密码与会话将以明文传输"

command -v curl >/dev/null 2>&1 || fail "需要 curl"

if [[ "${LIST_ONLY}" -eq 0 && -z "${ACCOUNT_ID}" && -z "${TARGET}" ]]; then
    usage_error "缺少 --target 或 --account-id"
fi

# 安全（SEC-4）：会话 Cookie 不进入 curl 的 argv（会留在 shell 历史与 ps 输出里）。
# 三种来源互斥；--cookie-stdin / --cookie-file 的值只落到 0700 临时目录内的 0600
# Netscape Cookie 文件，再以 curl -b <file> 发送。
cookie_sources=0
[[ -z "${COOKIE}" ]] || cookie_sources=$((cookie_sources + 1))
[[ "${COOKIE_STDIN}" -eq 0 ]] || cookie_sources=$((cookie_sources + 1))
[[ -z "${COOKIE_FILE_IN}" ]] || cookie_sources=$((cookie_sources + 1))
[[ "${cookie_sources}" -le 1 ]] || usage_error "--cookie / --cookie-stdin / --cookie-file 只能指定一个"

file_mode() { # 输出八进制权限位；Linux 用 stat -c，macOS 用 stat -f
    stat -c '%a' "$1" 2>/dev/null || stat -f '%Lp' "$1" 2>/dev/null
}

if [[ "${COOKIE_STDIN}" -eq 1 ]]; then
    IFS= read -r COOKIE || true
    [[ -n "${COOKIE}" ]] || fail "--cookie-stdin 未从标准输入读到会话 Cookie"
elif [[ -n "${COOKIE_FILE_IN}" ]]; then
    [[ -f "${COOKIE_FILE_IN}" && ! -L "${COOKIE_FILE_IN}" ]] ||
        fail "--cookie-file 必须是非符号链接的常规文件：${COOKIE_FILE_IN}"
    cookie_file_mode="$(file_mode "${COOKIE_FILE_IN}")" ||
        fail "无法读取 --cookie-file 权限：${COOKIE_FILE_IN}"
    [[ "${cookie_file_mode}" =~ ^[0-7]{3,4}$ ]] || fail "--cookie-file 权限不可识别：${cookie_file_mode}"
    (( (8#${cookie_file_mode} & 8#077) == 0 )) ||
        fail "--cookie-file 对 group/other 开放（权限 ${cookie_file_mode}），请先 chmod 600 <文件>"
    (( (8#${cookie_file_mode} & 8#400) != 0 )) ||
        fail "--cookie-file 当前用户不可读（权限 ${cookie_file_mode}）"
    COOKIE="$(<"${COOKIE_FILE_IN}")"
    [[ -n "${COOKIE}" ]] || fail "--cookie-file 内容为空：${COOKIE_FILE_IN}"
else
    [[ -z "${COOKIE}" ]] ||
        log "警告：--cookie 会把会话 Cookie 写进 shell 历史与进程列表；建议改用 --cookie-stdin 或 --cookie-file"
fi
if [[ -n "${COOKIE}" ]]; then
    COOKIE="${COOKIE#"${COOKIE%%[![:space:]]*}"}"
    COOKIE="${COOKIE%"${COOKIE##*[![:space:]]}"}"
    [[ -n "${COOKIE}" ]] || fail "会话 Cookie 为空"
fi

if [[ -z "${COOKIE}" ]]; then
    if [[ "${PASSWORD_STDIN}" -eq 1 ]]; then
        IFS= read -r PASSWORD || true
    else
        [[ -t 0 ]] || fail "非交互环境请使用 --password-stdin 或 --cookie-stdin"
        read -r -s -p "有解锁权限的账号 [${ADMIN}] 密码：" PASSWORD
        printf '\n' >&2
    fi
    [[ -n "${PASSWORD}" ]] || fail "密码为空"
fi

WORKDIR="$(mktemp -d "${TMPDIR:-/tmp}/unlock.XXXXXX")"
chmod 700 "${WORKDIR}"
COOKIE_JAR="${WORKDIR}/cookies"
COOKIE_SOURCE_FILE="${WORKDIR}/session-cookie.txt"
BODY_FILE="${WORKDIR}/body"
trap 'rm -rf "${WORKDIR}"' EXIT

# 把 --cookie / --cookie-stdin / --cookie-file 的值写成 Netscape Cookie 文件，
# 供 curl -b 读取；文件始终 0600，进程退出即随临时目录删除。
if [[ -n "${COOKIE}" ]]; then
    cookie_host="${BASE_URL#*://}"
    cookie_host="${cookie_host%%/*}"
    if [[ "${cookie_host}" == \[* ]]; then
        cookie_host="${cookie_host%%]*}]"
    else
        cookie_host="${cookie_host%%:*}"
    fi
    [[ -n "${cookie_host}" ]] || fail "无法从 --base-url 解析 Cookie 域"
    cookie_secure=FALSE
    [[ "${BASE_URL}" == https://* ]] && cookie_secure=TRUE
    ( umask 077; : > "${COOKIE_SOURCE_FILE}" ) || fail "无法创建临时会话 Cookie 文件"
    printf '# Netscape HTTP Cookie File\n' >> "${COOKIE_SOURCE_FILE}"
    IFS=';' read -r -a cookie_pairs <<< "${COOKIE}"
    cookie_count=0
    for cookie_pair in "${cookie_pairs[@]}"; do
        cookie_pair="${cookie_pair#"${cookie_pair%%[![:space:]]*}"}"
        cookie_pair="${cookie_pair%"${cookie_pair##*[![:space:]]}"}"
        [[ -n "${cookie_pair}" ]] || continue
        [[ "${cookie_pair}" == *=* ]] || fail "会话 Cookie 条目缺少 =：${cookie_pair%%=*}"
        cookie_name="${cookie_pair%%=*}"
        cookie_value="${cookie_pair#*=}"
        [[ "${cookie_name}" =~ ^[A-Za-z0-9_.!#$%+^|~-]+$ ]] ||
            fail "会话 Cookie 名含非法字符：${cookie_name}"
        [[ "${cookie_value}" != *[$'\t\n\r']* ]] || fail "会话 Cookie 值含控制字符"
        printf '%s\tFALSE\t/\t%s\t0\t%s\t%s\n' \
            "${cookie_host}" "${cookie_secure}" "${cookie_name}" "${cookie_value}" >> "${COOKIE_SOURCE_FILE}"
        cookie_count=$((cookie_count + 1))
    done
    [[ "${cookie_count}" -gt 0 ]] || fail "会话 Cookie 未包含任何 name=value 条目"
    chmod 600 "${COOKIE_SOURCE_FILE}"
fi

# api METHOD PATH [DATA_FILE] -> 打印 HTTP 状态码；响应体写入 BODY_FILE。
api() {
    local method="$1" path="$2" data_file="${3:-}"
    local args=(-sS -o "${BODY_FILE}" -w '%{http_code}'
        -b "${COOKIE_JAR}" -c "${COOKIE_JAR}"
        -H "Accept: application/json" -H "Origin: ${ORIGIN}"
        -X "${method}" "${BASE_URL}${path}")
    if [[ -f "${COOKIE_SOURCE_FILE}" ]]; then
        args+=(-b "${COOKIE_SOURCE_FILE}")
    fi
    if [[ -n "${data_file}" ]]; then
        args+=(-H "Content-Type: application/json" --data-binary "@${data_file}")
    fi
    curl "${args[@]}" || true
}

# json_escape <text> -> 打印可直接嵌入 JSON 双引号字符串的转义文本（不含外层引号）。
#
# 有 python3 时等价于 json.dumps(text, ensure_ascii=False)[1:-1]：
#   - 转义 " 与 \，以及 \n \r \t \b \f；
#   - 其余 <0x20 控制字符转成 \u00XX；
#   - UTF-8 中文/emoji 等非 ASCII 字符原样保留（JSON 允许字面量）；
#   - 以字节读入、UTF-8 解码，不受调用方 locale 影响（非法字节用 surrogateescape 原样输出）。
# 无 python3 时降级为确定性纯 bash 实现，只覆盖 \ " \n \r \t；
#   降级边界：\b \f 与其他控制字符（如 \x01）不会被转义，含这类字符的账号/口令
#   仍会拼出非法 JSON（平台回 400/422）；需要完整覆盖时请安装 python3。
json_escape() {
    if command -v python3 >/dev/null 2>&1; then
        printf '%s' "$1" | python3 -c '
import json,sys
text = sys.stdin.buffer.read().decode("utf-8", "surrogateescape")
out = json.dumps(text, ensure_ascii=False)[1:-1]
sys.stdout.buffer.write(out.encode("utf-8", "surrogateescape"))
'
        return
    fi
    local escaped="$1"
    escaped="${escaped//\\/\\\\}"
    escaped="${escaped//\"/\\\"}"
    escaped="${escaped//$'\n'/\\n}"
    escaped="${escaped//$'\r'/\\r}"
    escaped="${escaped//$'\t'/\\t}"
    printf '%s' "${escaped}"
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

# json_total <body> -> 打印响应信封的分页总数（.data.total；取不到时回退顶层 total，仍无则空）。
#
# 有 python3 时按 JSON 结构精确取值，避免响应里出现多个 "total" 字段（如顶层/meta/分页块）
# 时被贪心匹配误判成别的数字；无 python3 时退化为 grep 取第一个 "total" 数字字段。
json_total() {
    if command -v python3 >/dev/null 2>&1; then
        printf '%s' "$1" | python3 -c '
import json,sys
try:
    doc = json.load(sys.stdin)
except ValueError:
    sys.exit(0)
total = None
if isinstance(doc, dict):
    data = doc.get("data")
    if isinstance(data, dict):
        total = data.get("total")
    if total is None:
        total = doc.get("total")
if isinstance(total, int) and not isinstance(total, bool):
    sys.stdout.write("%d\n" % total)
'
        return
    fi
    printf '%s' "$1" |
        grep -oE '"total"[[:space:]]*:[[:space:]]*[0-9]+' |
        sed -e 's/[^0-9]//g' -e '1!d' || true
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
        total="$(json_total "${body}")"
        # 保持既有语义：total > page*page_size 时继续翻页；取不到总数则视为最后一页。
        if [[ ! "${total}" =~ ^[0-9]+$ || "${total}" -le $((page * 100)) ]]; then
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
chmod 600 "${login_payload}"

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
    log "目标账号：${TARGET}（account_id=${ACCOUNT_ID}）"
else
    # 显式 --account-id：文案必须反映真实目标——在锁定列表里就打印 account_name，
    # 不在列表（已解锁/超出分页/ID 写错）时如实说明，但仍按指定 ID 尝试解锁，
    # 由解锁响应的 404/409 兜底给出原因。
    explicit_name="$(printf '%s\n' "${rows}" | awk -F'\t' -v id="${ACCOUNT_ID}" '$1 == id {print $2; exit}')"
    if [[ -n "${explicit_name}" ]]; then
        explicit_desc="${explicit_name}（account_id=${ACCOUNT_ID}，在锁定列表中）"
    else
        explicit_desc="account_id=${ACCOUNT_ID} 未在锁定列表中（可能已解锁/超出分页），仍按指定 ID 尝试"
    fi
    if [[ "${TARGET_SET}" -eq 1 && -n "${TARGET}" ]]; then
        log "目标账号：--target ${TARGET}；--account-id ${explicit_desc}"
    else
        log "目标账号：${explicit_desc}"
    fi
fi
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
