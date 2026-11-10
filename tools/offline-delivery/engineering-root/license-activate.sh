#!/usr/bin/env bash
# 商业授权导入脚本（在已部署应用系统的服务器上以 root 运行）。
#
# 用途：
#   1. 生成签发申请（license-request.json），交给厂商签发工具出 JWS；
#   2. 拿到 JWS 后带 --license 重新运行本脚本完成导入。
#
# 用法：
#   ./license-activate.sh [--customer <客户标识>] [--license <commercial-license.jws>]
#                         [--deploy-dir /opt/unified-identity-platform] [--account admin]
#
# 说明：
#   - environment 自动取部署编排中的 APP_ENV（离线默认 production），无需手填；
#   - 申请与许可证按 customer_id + environment + instance_id 三重绑定，
#     换服务器、换环境均需重新签发；
#   - 需要平台管理员账号（具备 platform:license:read/manage 权限）。
set -Eeuo pipefail
umask 077

die() { printf '[错误] %s\n' "$*" >&2; exit 1; }
info() { printf '[信息] %s\n' "$*"; }

# 安全（SEC-6）：--deploy-dir 来自命令行，本脚本会 source/执行其中的脚本，
# 因此必须确认目录不可被第三方替换或改写：属主是 root 或当前用户，且 group/other
# 无写权限。当前用户自有的 0700/0750 目录仍然允许（本地受控安装），
# 只拒绝可被其他用户写入的路径，避免在 root 上下文里执行被替换的脚本。
# 当前用户用 bash 内建 EUID 判定，不依赖 PATH 上的 id，避免被同名命令影响判断。
dir_owner_uid() { stat -c '%u' "$1" 2>/dev/null || stat -f '%u' "$1" 2>/dev/null; }
dir_perm_bits() { stat -c '%a' "$1" 2>/dev/null || stat -f '%Lp' "$1" 2>/dev/null; }
assert_trusted_deploy_dir() {
  local path="$1" owner mode
  [[ -d "$path" && ! -L "$path" ]] || die "部署路径不存在、不是目录或是符号链接：${path}"
  owner="$(dir_owner_uid "$path")" || die "无法读取目录属主：${path}"
  mode="$(dir_perm_bits "$path")" || die "无法读取目录权限：${path}"
  [[ "$owner" == "0" || "$owner" == "$EUID" ]] ||
    die "部署目录属主既不是 root 也不是当前用户（uid=${owner}）：${path}；请先修正属主再授权"
  [[ "$mode" =~ ^[0-7]{3,4}$ ]] || die "目录权限不可识别（${mode}）：${path}"
  (( (8#${mode} & 8#022) == 0 )) ||
    die "部署目录对 group/other 可写（权限 ${mode}）：${path}；请先执行 chmod go-w 再授权"
}

customer=""
license_file=""
deploy_dir="/opt/unified-identity-platform"
account="${LICENSE_ADMIN_ACCOUNT:-admin}"
password="${LICENSE_ADMIN_PASSWORD:-}"
confirm_changes=false
confirm_replace_pending=false
confirm_initialize=false
replace_session=false
activation_timeout=900

while [[ $# -gt 0 ]]; do
  case "$1" in
    --customer) customer="${2:?--customer 缺少参数}"; shift 2 ;;
    --license) license_file="${2:?--license 缺少参数}"; shift 2 ;;
    --deploy-dir) deploy_dir="${2:?--deploy-dir 缺少参数}"; shift 2 ;;
    --account) account="${2:?--account 缺少参数}"; shift 2 ;;
    --password) die '不允许命令行密码；请交互输入或使用 --password-stdin' ;;
    --password-stdin) IFS= read -r password || [[ -n "$password" ]]; shift ;;
    --confirm-changes) confirm_changes=true; shift ;;
    --confirm-replace-pending) confirm_replace_pending=true; shift ;;
    --confirm-initialize) confirm_initialize=true; shift ;;
    --replace-session) replace_session=true; shift ;;
    --activation-timeout) activation_timeout="${2:?--activation-timeout 缺少参数}"; shift 2 ;;
    -h|--help) sed -n '2,14p' "$0"; printf '%s\n' '安全确认：--confirm-initialize、--confirm-changes、--confirm-replace-pending、--replace-session 均须显式指定；密码仅交互或 --password-stdin。API 测试/代理需显式 LICENSE_API_BASE 与 LICENSE_ORIGIN。'; exit 0 ;;
    *) die "未知参数：$1" ;;
  esac
done
[[ "$activation_timeout" =~ ^[1-9][0-9]*$ && ${#activation_timeout} -le 4 && "$activation_timeout" -le 3600 ]] || die '执行确认超时必须为 1～3600 秒'

[[ -z "$license_file" || ( -f "$license_file" && ! -L "$license_file" ) ]] || die '许可证必须是非符号链接常规文件'
# 已有受控安装走同一安全导入/激活路径，不创建管理员会话。
if [[ -n "$license_file" && -f "$deploy_dir/license-installation.json" && "$confirm_changes" == false && "$confirm_replace_pending" == false ]]; then
  [[ $(id -u) == 0 ]] || die '请以 root 运行受控离线授权'
  assert_trusted_deploy_dir "$deploy_dir"
  assert_trusted_deploy_dir "$deploy_dir/bin"
  [[ -f "$deploy_dir/bin/deploy.sh" && ! -L "$deploy_dir/bin/deploy.sh" ]] || die '缺少受控部署入口'
  bash "$deploy_dir/bin/deploy.sh" license-renew "$license_file" || die '授权导入/执行确认未完成；状态已保留'
  info '安全导入和必需组件执行确认已完成；有效期未重新起算。'
  exit 0
fi

api_base_override="${LICENSE_API_BASE:-}"
if [[ -z "$api_base_override" ]]; then
  [[ $(id -u) -eq 0 ]] || die "请以 root 运行（需要读取部署配置并访问本机 API 端口）"
  [[ -d "$deploy_dir" ]] || die "部署目录不存在：${deploy_dir}（可用 --deploy-dir 指定）"
  assert_trusted_deploy_dir "$deploy_dir"
  assert_trusted_deploy_dir "$deploy_dir/bin"
  compose_file="$deploy_dir/docker-compose.yml"
  [[ -f "$compose_file" ]] || die "缺少 $compose_file"

  # 使用已配置的公网网关，不从 Compose 占位默认值猜测暴露端口。
  app_env=production
  [[ -f "$deploy_dir/bin/public-transport.sh" && ! -L "$deploy_dir/bin/public-transport.sh" ]] || die '缺少公开传输配置工具，请更新部署资产'
  source "$deploy_dir/bin/public-transport.sh"
  [[ -n "$(public_transport_env_value "$deploy_dir/.env" PUBLIC_PLATFORM_HOST)" ]] || die '未配置公开平台地址，不猜测主机或端口'
  public_transport_prepare "$deploy_dir" "$deploy_dir/.env" || die '公开传输配置校验失败'
  origin="$PUBLIC_PLATFORM_ORIGIN"
  api_base_override="$origin/api/v1"
else
  # 测试钩子：直接指向已知的 API 与环境，跳过部署目录发现。
  app_env="${LICENSE_APP_ENV:-development}"
  origin="${LICENSE_ORIGIN:?使用 LICENSE_API_BASE 时必须提供 LICENSE_ORIGIN}"
fi
api="$api_base_override"
python3 -c 'import sys,urllib.parse as u; a,o=map(u.urlsplit,sys.argv[1:]); assert a.scheme in ("http","https") and o.scheme in ("http","https") and a.netloc and o.netloc and not a.username and not o.username and not a.query and not o.query and not a.fragment and not o.fragment' "$api" "$origin" || die 'API/Origin 必须是无凭据、无查询参数的 HTTP(S) 地址'

info "部署目录：$deploy_dir"
info "API 地址：${api}（APP_ENV=${app_env}，Origin=${origin}）"

command -v curl >/dev/null || die "缺少 curl"
# O3：单次请求的超时随剩余 deadline 收敛（下限 1 秒、上限 60 秒），
# 避免 deadline 已到仍在等一个 60 秒的请求。api_call 调用前设置、结束后恢复；
# 登录等不走 api_call 的调用保持默认 60 秒。
api_max_time=60
curl() { command curl --connect-timeout 5 --max-time "$api_max_time" --show-error "$@"; }
python3 --version >/dev/null 2>&1 || die "缺少 python3"

work="$(mktemp -d /tmp/license-activate.XXXXXX)"
cookie="$work/cookie"
trap 'rm -f -- "$work"/body.* "$work/preview.json" "$work/status.json" "$work/cookie" "$work/request.json"; rmdir -- "$work"' EXIT
command -v flock >/dev/null || die '缺少 flock，请先安装离线宿主依赖'
[[ -d "$deploy_dir" && ! -L "$deploy_dir" && ! -L "$deploy_dir/.license-helper.lock" ]] || die '部署目录或授权锁无效'
exec 9>"$deploy_dir/.license-helper.lock"
flock -n 9 || die '另一个授权辅助操作正在执行'

# ── O3：有界退避重试的 API 调用层 ───────────────────────────────────────────
# 统一走 api_call：不再使用 --fail（4xx/5xx 会丢响应体，无法区分“等一会就好”
# 与“重试无效”）。状态码、curl 退出码、响应体文件分开返回，由 api_classify 分类：
#   可重试 = curl 退出码 7/28/52/56、HTTP 5xx（少数确定性 code 除外）、
#            body code 属于 RETRY_TRANSIENT_CODES（运行未就绪、并发推进冲突）；
#   其余一律立即失败（400/403/404 与 LICENSE_CLOCK_RECOVERY_REQUIRED、
#   LICENSE_STATE_INVALID、LICENSE_VERSION_NOT_INCREASING、
#   LICENSE_BINDING_MISMATCH、LICENSE_CONFIRMATION_REQUIRED 等确定性错误）。
# 响应体只落临时文件；任何日志都不输出 body 原文、请求头、cookie 或 JWS。
RETRY_CURL_EXITS=" 7 28 52 56 "
# 服务端明确可重试的瞬时业务码：运行未就绪、并发推进导致的版本冲突。
RETRY_TRANSIENT_CODES=" LICENSE_RUNTIME_NOT_READY LICENSE_VERSION_CONFLICT "
RETRY_IMMEDIATE_CODES=" LICENSE_ACTIVATION_UNSUPPORTED LICENSE_TRUST_NOT_CONFIGURED "
last_http=""
last_curl_rc=""
last_code=""
last_request_id=""
last_message=""
API_BODY=""
API_VERDICT=""
# 激活轮询的共享状态；deadline 由提交前的初始化块赋值（backoff_delay 依赖它）。
deadline=0
started_at=0
attempts=0
poll_rounds=0
retry_seq=0
retry_stage="授权状态"
retry_app="-"
retry_reason="pending"
pending_apps=()
pending_lines=()
pending_codes=()
enforced_apps=()
activated_apps=()

# 逐个应用记录未完成原因，供超时报告使用（state/revision/ready/ack/snapshot_current）。
record_pending() { # <应用编码> <摘要> <最近code>
  local i=0
  while (( i < ${#pending_apps[@]} )); do
    if [[ "${pending_apps[$i]}" == "$1" ]]; then
      pending_lines[$i]="$2"
      pending_codes[$i]="$3"
      return 0
    fi
    i=$((i + 1))
  done
  pending_apps+=("$1")
  pending_lines+=("$2")
  pending_codes+=("$3")
  return 0
}

# 仅更新最近 code，保留已取得的执行状态摘要。
mark_pending_code() { # <应用编码> <最近code>
  local i=0
  while (( i < ${#pending_apps[@]} )); do
    if [[ "${pending_apps[$i]}" == "$1" ]]; then
      pending_codes[$i]="$2"
      return 0
    fi
    i=$((i + 1))
  done
  pending_apps+=("$1")
  pending_lines+=("执行状态未取得（等待组件受控接入、登记与就绪确认）")
  pending_codes+=("$2")
  return 0
}

clear_pending() { # <应用编码>
  local i=0 keep_apps=() keep_lines=() keep_codes=()
  while (( i < ${#pending_apps[@]} )); do
    if [[ "${pending_apps[$i]}" != "$1" ]]; then
      keep_apps+=("${pending_apps[$i]}")
      keep_lines+=("${pending_lines[$i]}")
      keep_codes+=("${pending_codes[$i]}")
    fi
    i=$((i + 1))
  done
  pending_apps=("${keep_apps[@]+"${keep_apps[@]}"}")
  pending_lines=("${keep_lines[@]+"${keep_lines[@]}"}")
  pending_codes=("${keep_codes[@]+"${keep_codes[@]}"}")
  return 0
}

# 超时报告：先逐应用给出未完成原因，再给出已完成/已发起激活清单，
# 最后用逐字保留前缀的主行失败退出。
pending_progress_line() { # 输出“已完成应用=… 已发起激活=…”（bash 3.2 空数组安全）
  local a done_list="" fired_list=""
  for a in ${enforced_apps[@]+"${enforced_apps[@]}"}; do
    done_list="${done_list:+$done_list,}$a"
  done
  for a in ${activated_apps[@]+"${activated_apps[@]}"}; do
    fired_list="${fired_list:+$fired_list,}$a"
  done
  printf '已完成应用=%s 已发起激活=%s' "${done_list:-none}" "${fired_list:-none}"
}

activation_timeout_report() {
  local i=0 elapsed
  elapsed=$((SECONDS - started_at))
  while (( i < ${#pending_apps[@]} )); do
    info "未完成：应用编码=${pending_apps[$i]} ${pending_lines[$i]} 最近code=${pending_codes[$i]:-none}"
    i=$((i + 1))
  done
  info "$(pending_progress_line)"
  die "授权执行确认超时；状态已保留，不得认定授权已生效；已等待 ${elapsed} 秒，上限 ${activation_timeout} 秒，共尝试 ${attempts} 次；最后错误：阶段=${retry_stage:-授权状态}/应用=${retry_app:--}/HTTP=${last_http:-000}/code=${last_code:-none}/curl退出码=${last_curl_rc:-0}/request_id=${last_request_id:-none}/message=${last_message:-none}"
}

# 中断报告：SIGINT/SIGTERM 时保留现场并明确“不得认定已授权”，复用未完成清单。
activation_interrupted_report() { # <信号名> <退出码>
  local signal="${1:-TERM}" code="${2:-143}" i=0 elapsed=0
  if (( started_at > 0 )); then elapsed=$((SECONDS - started_at)); fi
  while (( i < ${#pending_apps[@]} )); do
    info "未完成：应用编码=${pending_apps[$i]} ${pending_lines[$i]} 最近code=${pending_codes[$i]:-none}"
    i=$((i + 1))
  done
  info "$(pending_progress_line)"
  printf '[错误] 授权执行确认被中断（%s）；状态已保留，不得认定已授权；已等待 %s 秒，共尝试 %s 次；最后错误：阶段=%s/应用=%s/HTTP=%s/code=%s/curl退出码=%s/request_id=%s/message=%s\n' \
    "$signal" "$elapsed" "${attempts:-0}" \
    "${retry_stage:-授权状态}" "${retry_app:--}" "${last_http:-000}" "${last_code:-none}" \
    "${last_curl_rc:-0}" "${last_request_id:-none}" "${last_message:-none}" >&2
  exit "$code"
}

# deadline 是唯一的硬边界；首个激活轮先真实观测一次状态（极端小的
# --activation-timeout 下也不能“零观测即超时”），其后每轮与逐应用迭代都严格执行。
deadline_exceeded() { (( deadline > 0 )) && (( SECONDS >= deadline )); }
check_deadline() {
  (( poll_rounds >= 1 )) || return 0
  if deadline_exceeded; then activation_timeout_report; fi
}

# 单次请求预算：按剩余 deadline 收敛到 1～60 秒；未进入激活流程（deadline=0）时用 60。
api_budget() {
  local remaining=60
  if (( deadline > 0 )); then
    remaining=$((deadline - SECONDS))
    if (( remaining < 1 )); then remaining=1; fi
    if (( remaining > 60 )); then remaining=60; fi
  fi
  printf '%s' "$remaining"
}

api_call() { # <curl 参数…，最后一个必须是 URL> → 输出「状态码 curl退出码 body文件」
  local out status rc previous="$api_max_time"
  api_max_time="$(api_budget)"
  out="$(mktemp "$work/body.XXXXXX")"
  # set -e 下 curl 失败会直接终止脚本；这里显式接住退出码交给分类器判断。
  set +e
  status="$(curl -s -b "$cookie" -H "Origin: $origin" "$@" -o "$out" -w '%{http_code}')"
  rc=$?
  set -e
  api_max_time="$previous"
  printf '%s %s %s\n' "${status:-000}" "$rc" "$out"
}

# 读取响应体顶层字符串字段；响应体不是对象或字段缺失时输出空串。
body_field() { # <body文件> <字段名>
  python3 - "$1" "$2" <<'PY' 2>/dev/null || true
import json, sys
try:
    doc = json.load(open(sys.argv[1], encoding="utf-8"))
except Exception:
    sys.exit(0)
value = doc.get(sys.argv[2]) if isinstance(doc, dict) else None
print(value if isinstance(value, str) else "")
PY
}

# 只透出服务端诊断 message：去控制字符/换行、截断 300 字符，避免日志注入；
# 绝不输出响应体原文、请求头、cookie、密码或 JWS。
body_message() { # <body文件>
  python3 - "$1" <<'PY' 2>/dev/null || true
import json, sys
try:
    doc = json.load(open(sys.argv[1], encoding="utf-8"))
except Exception:
    sys.exit(0)
value = doc.get("message") if isinstance(doc, dict) else None
if not isinstance(value, str):
    sys.exit(0)
clean = "".join(ch for ch in value if ch.isprintable()).strip()
print(clean[:300])
PY
}

# 分类一次调用：结果写入 API_VERDICT（retry/fail），机器可对账字段写入 last_*。
api_classify() { # <状态码> <curl退出码> <body文件>
  local status="$1" rc="$2" body="$3" num=0
  last_http="$status"
  last_curl_rc="$rc"
  last_code="$(body_field "$body" code)"
  last_request_id="$(body_field "$body" request_id)"
  last_message="$(body_message "$body")"
  if [[ "$rc" != "0" ]]; then
    if [[ "$RETRY_CURL_EXITS" == *" $rc "* ]]; then API_VERDICT=retry; else API_VERDICT=fail; fi
    return 0
  fi
  if [[ "$status" =~ ^[0-9]{3}$ ]]; then num=$((10#${status})); fi
  # 服务端明确可重试的瞬时业务码（运行未就绪、并发推进冲突）允许退避重试。
  if [[ -n "$last_code" && "$RETRY_TRANSIENT_CODES" == *" $last_code "* ]]; then
    API_VERDICT=retry
    return 0
  fi
  if (( num >= 500 && num <= 599 )); then
    if [[ "$RETRY_IMMEDIATE_CODES" == *" $last_code "* ]]; then API_VERDICT=fail; else API_VERDICT=retry; fi
    return 0
  fi
  API_VERDICT=fail
  return 0
}

# 机器可对账的错误字段：阶段 / 应用编码 / HTTP / code / curl 退出码 / request_id / message。
# message 已去换行、截断 300 字符，仅用于定位服务端拒绝原因。
api_detail() { # <阶段> <应用编码>
  printf '阶段=%s 应用编码=%s HTTP=%s code=%s curl退出码=%s request_id=%s message=%s' \
    "$1" "$2" "${last_http:-000}" "${last_code:-none}" "${last_curl_rc:-0}" "${last_request_id:-none}" "${last_message:-none}"
}

# 确定性错误的归因与下一步。
api_remedy() { # <code>
  case "${1:-}" in
    LICENSE_CLOCK_RECOVERY_REQUIRED) printf '先校正系统时间，再用厂商签名时间恢复凭据完成恢复，然后重新运行本脚本' ;;
    LICENSE_STATE_INVALID) printf '保留现场并交授权管理员核查持久授权状态；重试无效' ;;
    LICENSE_VERSION_NOT_INCREASING) printf '让厂商签发更高版本的许可证后再导入' ;;
    LICENSE_VERSION_CONFLICT) printf '并发推进，按退避重读后确认；持续出现检查是否有其他授权操作并行' ;;
    LICENSE_EXPECTATION_MISMATCH) printf '预览与提交之间授权状态已变化或提交值与预览不一致；重新运行本脚本会重新预览并提交，不要重复提交同一预期值' ;;
    LICENSE_BINDING_MISMATCH) printf '按当前部署重新生成签发申请并让厂商重新签发，不能复用其他客户或环境的许可证' ;;
    LICENSE_CONFIRMATION_REQUIRED) printf '核对预览差异后重新运行，并显式加 --confirm-changes 或 --confirm-replace-pending' ;;
    LICENSE_RUNTIME_NOT_READY) printf '完成全部组件的受控接入、登记与就绪确认后再重试' ;;
    LICENSE_TRUST_NOT_CONFIGURED) printf '升级到已配置厂商可信公钥的发行版后再授权' ;;
    LICENSE_ACTIVATION_UNSUPPORTED) printf '同步升级控制面到支持绑定许可修订的激活版本后再重试' ;;
    *) printf '该错误不能通过重试恢复，请按平台审计与 request_id 排查后重新运行' ;;
  esac
}

api_die() { # <阶段> <应用编码>
  die "$(api_detail "$1" "$2")；下一步：$(api_remedy "${last_code:-}")"
}

# 单次调用：成功返回 0（响应体路径在 API_BODY）；
# 可重试错误返回 1（由调用方决定是否退避重试）；确定性错误立即 exit 1。
api_request() { # <阶段> <应用编码> <curl 参数…，最后一个必须是 URL>
  local stage="$1" app="$2" result status rc
  shift 2
  result="$(api_call "$@")"
  status="${result%% *}"
  result="${result#* }"
  rc="${result%% *}"
  API_BODY="${result#* }"
  api_classify "$status" "$rc" "$API_BODY"
  if [[ "$API_VERDICT" == retry ]]; then return 1; fi
  if [[ "$rc" != "0" ]]; then
    die "$(api_detail "$stage" "$app")；curl 连接中断且不属于可重试场景，请检查网络与 API 地址后重新运行"
  fi
  if [[ "${last_http:-}" != "200" || "${last_code:-}" != "OK" ]]; then
    api_die "$stage" "$app"
  fi
  return 0
}

# 退避阶梯 2/4/8/16/30 秒封顶（测试可用 RETRY_SLEEP=0 关闭等待），
# 且不超过 deadline 剩余时间，避免无意义等待。
retry_sleep_override="${RETRY_SLEEP:-}"
if [[ -n "$retry_sleep_override" && ! "$retry_sleep_override" =~ ^[0-9]+$ ]]; then
  die 'RETRY_SLEEP 必须是非负整数（单位：秒）'
fi
backoff_delay() { # <重试序号> → 本次退避秒数
  local n="$1" delay remaining
  case "$n" in
    1) delay=2 ;;
    2) delay=4 ;;
    3) delay=8 ;;
    4) delay=16 ;;
    *) delay=30 ;;
  esac
  if [[ -n "$retry_sleep_override" ]]; then delay="$retry_sleep_override"; fi
  remaining=$((deadline - SECONDS))
  if (( remaining < 1 )); then printf '0'; return 0; fi
  if (( delay > remaining )); then delay="$remaining"; fi
  printf '%s' "$delay"
}

backoff_sleep() { # <重试序号>
  local delay
  delay="$(backoff_delay "$1")"
  if ! [[ "$delay" =~ ^[0-9]+$ ]]; then die '退避等待秒数无效'; fi
  if (( delay > 0 )); then
    sleep "$delay" || die '退避等待被中断；授权执行确认未完成，状态已保留'
  fi
  return 0
}

# 重试日志限频：前 3 次必打，其后每 30 秒最多 1 条；单次运行最多打印 40 行。
# retry_attempts 统计重试次数，retry_log_count 只统计已打印行数，两者不能混用：
# 否则快速重试会把 40 行配额在几秒内耗尽，之后即使过了 30 秒窗口也不再输出。
retry_attempts=0
retry_log_count=0
retry_log_last_at=0
RETRY_LOG_LIMIT=40
log_retry() { # <阶段> <应用编码>
  local now
  retry_attempts=$((retry_attempts + 1))
  if (( retry_log_count >= RETRY_LOG_LIMIT )); then return 0; fi
  now=$SECONDS
  if (( retry_attempts <= 3 )) || (( now - retry_log_last_at >= 30 )); then
    retry_log_count=$((retry_log_count + 1))
    if [[ "$retry_reason" == error ]]; then
      info "$(api_detail "$1" "$2")；遇到可重试错误，按退避阶梯重试（第 ${retry_attempts} 次重试）"
    else
      info "$(api_detail "$1" "$2")；执行确认尚未完成，按退避阶梯重新核对（第 ${retry_attempts} 次重试）"
    fi
    retry_log_last_at=$now
  fi
  return 0
}

# O3：中断必须保留现场并明确“不得认定已授权”；临时文件清理仍由 EXIT trap 负责。
trap 'activation_interrupted_report INT 130' INT
trap 'activation_interrupted_report TERM 143' TERM

if [[ -z "$password" ]]; then
  printf '管理员账号 [%s]：' "$account"
  read -r input_account
  account="${input_account:-$account}"
  printf '管理员密码：'
  read -rs password
  echo
fi

[[ -n "$password" ]] || die '管理员密码为空'

login_payload="$(printf '%s' "$password" | python3 -c 'import json,sys; print(json.dumps({"account":sys.argv[1],"password":sys.stdin.read(),"login_type":"password","replace_existing_session":sys.argv[2]=="true"}))' "$account" "$replace_session")"
unset password LICENSE_ADMIN_PASSWORD
login_code="$(printf '%s' "$login_payload" | curl -s -c "$cookie" -X POST "$api/auth/login" \
  -H "Origin: $origin" -H 'Content-Type: application/json' \
  --data-binary @- \
  -o /dev/null -w '%{http_code}')" || die "无法连接平台 API：$api"
[[ "$login_code" == "200" ]] || die "管理员登录失败（HTTP ${login_code}），请核对账号密码"
unset login_payload
info "管理员登录成功"

if ! api_request '初始授权状态' '-' "$api/licenses/status"; then
  die "$(api_detail '初始授权状态' '-')；瞬时错误在此阶段不自动重试，请重新运行本脚本"
fi
status_json="$(<"$API_BODY")"
initialized="$(python3 -c 'import json,sys; d=json.load(sys.stdin); assert d.get("code")=="OK" and isinstance(d.get("data"),dict); print("true" if d["data"].get("initialized") else "false")' <<<"$status_json")" \
  || die '初始授权状态响应异常（缺少 data.initialized）'

if [[ "$initialized" != "true" ]]; then
  [[ "$confirm_initialize" == true ]] || die '首次初始化会持久化安装身份；确认客户后显式指定 --confirm-initialize'
  if [[ -z "$customer" ]]; then
    printf '客户标识（首次初始化，签发后不可更改）：'
    read -r customer
  fi
  [[ -n "$customer" ]] || die "客户标识不能为空"
  init_code="$(curl -s -b "$cookie" -H "Origin: $origin" -H 'Content-Type: application/json' \
    -X POST "$api/licenses/initialize" \
    -d "$(python3 -c 'import json,sys; print(json.dumps({"customer_id": sys.argv[1], "environment": sys.argv[2]}))' "$customer" "$app_env")" \
    -o /dev/null -w '%{http_code}')" || die "初始化请求失败"
  [[ "$init_code" == "200" ]] || die "初始化失败（HTTP ${init_code}）。environment 必须与部署一致（当前 ${app_env}）"
  info "部署实例已初始化：customer=$customer environment=$app_env"
else
  info "部署实例已初始化，跳过初始化"
fi

request_json="$deploy_dir/license-request.json"
[[ ! -L "$request_json" ]] || die '申请文件不能是符号链接'
curl --fail -s -b "$cookie" -H "Origin: $origin" "$api/licenses/request" >"$work/request.json" \
  || die "签发申请生成失败"
python3 -c 'import json,sys; d=json.load(sys.stdin); data=d.get("data") or {}; assert d.get("code")=="OK" and data.get("instance_id") and data.get("environment")' <"$work/request.json" || die '签发申请响应异常'
install -m 600 -- "$work/request.json" "$request_json"
info "签发申请已生成：$request_json"
info '申请内容保存在受保护文件中，不输出到部署日志。'

if [[ -z "$license_file" ]]; then
  info "尚未提供许可证。请将本申请交给厂商签发工具（上传申请 JSON 自动填充，勾选系统与期限后签发），"
  info "拿到 commercial-license.jws 后重新运行："
  info "  $0 --customer '$customer' --license /路径/commercial-license.jws"
  trap - INT TERM
  exit 0
fi


[[ -f "$license_file" && ! -L "$license_file" ]] || die '许可证必须是常规文件且非符号链接'

# O3：本地计算许可证摘要 sha256(去首尾空白的 raw JWS)，算法与 license-core 的
# Digest 一致（diff.go）。摘要只用于判断“同一许可证是否已导入”，不输出许可证内容。
local_digest="$(python3 - "$license_file" <<'PY'
import hashlib, sys
raw = open(sys.argv[1], encoding="utf-8").read().strip()
print(hashlib.sha256(raw.encode("utf-8")).hexdigest())
PY
)" || die '无法计算许可证摘要；请确认许可证为 UTF-8 文本后重新运行'
[[ "$local_digest" =~ ^[0-9a-f]{64}$ ]] || die '许可证摘要计算异常，无法安全判断是否续跑'

# 断点续跑判定只使用初始授权状态里的部署事实（current_digest / pending_digest /
# current.applications[].code），不打印许可证原文、JWS 或请求头。
printf '%s' "$status_json" >"$work/status.json"
resume_current="false"
resume_pending="false"
resume_apps=""
if [[ "$initialized" == "true" ]]; then
  resume_info="$(python3 - "$work/status.json" "$local_digest" <<'PY'
import json, re, sys
try:
    doc = json.load(open(sys.argv[1], encoding="utf-8"))
except Exception:
    sys.exit(1)
data = doc.get("data") if isinstance(doc, dict) else None
if not isinstance(data, dict):
    sys.exit(1)
dep = data.get("deployment") if isinstance(data.get("deployment"), dict) else {}
current = dep.get("current_digest") if isinstance(dep.get("current_digest"), str) else ""
pending = dep.get("pending_digest") if isinstance(dep.get("pending_digest"), str) else ""
apps = []
current_license = dep.get("current") if isinstance(dep.get("current"), dict) else {}
for item in current_license.get("applications") or []:
    code = item.get("code") if isinstance(item, dict) else None
    if isinstance(code, str) and re.fullmatch("[a-z_]+", code) and code not in apps:
        apps.append(code)
print("true" if current and current == sys.argv[2] else "false")
print("true" if pending and pending == sys.argv[2] else "false")
print(",".join(apps))
PY
)" || die '初始授权状态响应异常（缺少 data.deployment），无法判断是否续跑'
  { IFS= read -r resume_current || true; IFS= read -r resume_pending || true; IFS= read -r resume_apps || true; } <<<"$resume_info"
fi

# O3：deadline 与重试状态在提交前初始化，使“提交后当前状态”与激活轮询共享同一预算。
# 首轮激活必须先真实观测一次状态，其后每轮与逐应用迭代都严格执行 deadline。
IMPORT_RETRY_MAX=3
retry_stage="授权状态"
retry_app="-"
retry_reason="pending"
deadline=$((SECONDS + activation_timeout))
started_at=$SECONDS
attempts=0
poll_rounds=0
retry_seq=0
retry_attempts=0
retry_log_count=0
retry_log_last_at=0
pending_apps=()
pending_lines=()
pending_codes=()
enforced_apps=()
activated_apps=()

expected_digest=""
applications=""
if [[ "$resume_current" == "true" ]]; then
  # 同一许可证已是当前生效版本：跳过预览与提交，直接进入执行确认续跑。
  # 同证重跑会在预览阶段 409 LICENSE_VERSION_NOT_INCREASING，因此不能重放导入。
  applications="${resume_apps//,/ }"
  [[ -n "$applications" ]] || die '当前授权状态缺少覆盖应用清单，无法从执行确认续跑'
  expected_digest="$local_digest"
  info '已导入同一许可证，从执行确认续跑：跳过预览与提交，不重复导入同一许可证。'
elif [[ "$resume_pending" == "true" ]]; then
  die '该许可证已提交但尚未到生效时间；状态已保留，到生效时间后重新运行本脚本即可，不要重复提交'
else
  preview_payload="$(python3 -c 'import json,sys; print(json.dumps({"raw_jws": open(sys.argv[1], encoding="utf-8").read().strip()}))' "$license_file")"
  import_attempt=0
  commit_done=false
  preview_json=""
  while (( import_attempt < IMPORT_RETRY_MAX )); do
    import_attempt=$((import_attempt + 1))
    # 预览可重试错误（5xx / curl 7,28,52,56 / 运行未就绪 / 并发冲突）有界退避重试。
    preview_tries=0
    while :; do
      if api_request '许可证预览' '-' -X POST -H 'Content-Type: application/json' \
        --data-binary @- "$api/licenses/imports/preview" <<<"$preview_payload"; then
        break
      fi
      preview_tries=$((preview_tries + 1))
      retry_stage='许可证预览'
      retry_app='-'
      retry_reason='error'
      if (( preview_tries >= IMPORT_RETRY_MAX )); then
        die "$(api_detail '许可证预览' '-')；预览连续失败，已停止重试；重新运行本脚本会重新预览"
      fi
      log_retry '许可证预览' '-'
      backoff_sleep "$preview_tries"
    done
    preview_json="$(<"$API_BODY")"
    printf '%s' "$preview_json" >"$work/preview.json"
    info "许可证验签通过，授权明细："
    python3 -c 'import json,sys; d=json.load(sys.stdin); data=d["data"]; print(json.dumps({"systems":[{"code":a["code"],"kind":a["kind"],"expires_at":a["expires_at"]} for a in data["license"]["applications"]],"changes":data.get("changes",[]),"digest":data["digest"]}, ensure_ascii=False, indent=2))' <<<"$preview_json" \
      || die '许可证预览响应缺少 license.applications 或 digest，无法展示授权明细'

# O6：预览响应归一化后再生成提交请求。
#  - state = data.get("state") or data（兼容 State 外提与嵌套两种形态）；
#  - revision / current_version / pending_digest 必须存在且类型正确
#    （整数必须 type(x) is int，拒绝 bool），digest 非空；
#  - expected_current_version 取 current_version：平台 commit 比对的是
#    p.CurrentVersion（application/service.go:289），此前误取 highest_version，
#    存在待生效许可证时必然 409；
#  - preview 要求确认差异/替换待生效许可证时必须先给开关，不等 400；
#  - revision=0 首装合法。
preview_out=""
if ! preview_out="$(python3 - "$work/preview.json" "$license_file" "$confirm_changes" "$confirm_replace_pending" <<'PY'
import json, sys
path, license_path, confirm_changes, confirm_replace_pending = sys.argv[1:5]
try:
    doc = json.load(open(path, encoding="utf-8"))
except Exception:
    print("许可证预览响应不是合法 JSON")
    sys.exit(1)
if not isinstance(doc, dict) or doc.get("code") != "OK":
    code = doc.get("code") if isinstance(doc, dict) else "none"
    print("许可证预览响应 code 不是 OK（code=%s）" % code)
    sys.exit(1)
data = doc.get("data")
if not isinstance(data, dict):
    print("许可证预览响应 data 不是对象")
    sys.exit(1)
state = data.get("state") or data
if not isinstance(state, dict):
    print("许可证预览响应 state 不是对象")
    sys.exit(1)

bad = []
digest = data.get("digest")
if not isinstance(digest, str) or not digest.strip():
    bad.append("digest")


def need_int(name):
    if name not in state:
        bad.append(name)
        return None
    value = state[name]
    if type(value) is not int or value < 0:
        bad.append("%s（必须是 >=0 的整数，不接受布尔值）" % name)
        return None
    return value


revision = need_int("revision")
current_version = need_int("current_version")
pending_digest = state.get("pending_digest")
if "pending_digest" not in state:
    bad.append("pending_digest")
elif not isinstance(pending_digest, str):
    bad.append("pending_digest（必须是字符串）")
    pending_digest = None
if bad:
    print("许可证预览响应缺少必需字段或字段类型非法：%s" % "、".join(bad))
    sys.exit(1)

if data.get("requires_change_confirmation") and confirm_changes != "true":
    print("许可证预览要求确认授权差异：核对 changes 后重新运行并显式加 --confirm-changes")
    sys.exit(2)
if data.get("requires_pending_replacement") and confirm_replace_pending != "true":
    print("许可证预览要求替换待生效许可证：核对差异后重新运行并显式加 --confirm-replace-pending")
    sys.exit(2)

print(json.dumps({
    "raw_jws": open(license_path, encoding="utf-8").read().strip(),
    "digest": digest,
    "expected_revision": revision,
    "expected_current_version": current_version,
    "expected_pending_digest": pending_digest,
    "confirm_changes": confirm_changes == "true",
    "confirm_replace_pending": confirm_replace_pending == "true",
}, ensure_ascii=False))
PY
)"; then
      die "${preview_out:-许可证预览响应结构异常，无法生成导入请求}"
    fi
    if [[ "$preview_out" != "{"* ]]; then die '许可证预览响应结构异常，无法生成导入请求'; fi
    commit_payload="$preview_out"

    # 提交只允许“重新预览后重试”一种重放路径：仅并发冲突（409
    # LICENSE_VERSION_CONFLICT）会重新预览并生成新期望值；其它可重试错误
    # （5xx / curl 28 等）视为“提交结果不确定”，禁止重放同一 payload。
    if api_request '许可证导入提交' '-' -X POST -H 'Content-Type: application/json' \
      --data-binary @- "$api/licenses/imports/commit" <<<"$commit_payload"; then
      commit_done=true
      break
    fi
    retry_stage='许可证导入提交'
    retry_app='-'
    retry_reason='error'
    if [[ "${last_code:-}" == "LICENSE_VERSION_CONFLICT" ]]; then
      if (( import_attempt >= IMPORT_RETRY_MAX )); then
        die "$(api_detail '许可证导入提交' '-')；提交持续并发冲突，已停止重试；核对平台审计后重新运行本脚本"
      fi
      info '提交返回并发冲突（授权状态已变化）；重新预览后按新期望值重试，禁止重放同一请求。'
      log_retry '许可证导入提交' '-'
      backoff_sleep "$import_attempt"
      continue
    fi
    die "$(api_detail '许可证导入提交' '-')；提交结果不确定，禁止重放同一请求；重跑同一命令会按许可证摘要续跑（已生效则直接进入执行确认）"
  done
  [[ "$commit_done" == true ]] || die '许可证导入未完成：重试次数已用尽；重新运行本脚本会重新预览后提交'
  expected_digest="$(python3 -c 'import json,sys; print(json.load(sys.stdin)["data"]["digest"])' <<<"$preview_json")" \
    || die '许可证预览响应缺少 digest'
  applications="$(python3 -c 'import json,sys,re; a=json.load(sys.stdin)["data"]["license"]["applications"]; assert a; codes=[x["code"] for x in a]; assert all(re.fullmatch("[a-z_]+",x) for x in codes); print("\n".join(codes))' <<<"$preview_json")" || die '授权执行应用清单无效；许可证提交不等于已激活'
  info '许可证已提交，不等于授权已生效；必须完成受控组件接入并确认所有必需进程执行状态。'
fi

if [[ -f "$deploy_dir/license-installation.json" ]]; then
  bash "$deploy_dir/bin/deploy.sh" license-renew "$license_file" || die '执行确认未完成；状态已保留，请排查后重试'
  info '必需组件执行确认完成。'
  trap - INT TERM
else
  # 存量部署无随包计划时仍可使用真实协调接口；不伪造登记或 ACK。
  # 提交后当前状态同样受 deadline 与有界重试约束（可重试错误退避重试）。
  retry_stage='当前授权状态'
  retry_app='-'
  retry_reason='pending'
  current_status=""
  status_tries=0
  while :; do
    if api_request '当前授权状态' '-' "$api/licenses/status"; then
      current_status="$(<"$API_BODY")"
      break
    fi
    status_tries=$((status_tries + 1))
    retry_stage='当前授权状态'
    retry_app='-'
    retry_reason='error'
    check_deadline strict
    if (( status_tries >= IMPORT_RETRY_MAX )); then
      die "$(api_detail '当前授权状态' '-')；提交后状态核对连续失败，已停止重试；重新运行本脚本会按摘要续跑"
    fi
    log_retry '当前授权状态' '-'
    retry_seq=$((retry_seq + 1))
    backoff_sleep "$retry_seq"
  done
  python3 -c 'import json,sys; d=json.load(sys.stdin); s=d.get("data",{}); assert d.get("code")=="OK" and s.get("deployment",{}).get("current_digest")==sys.argv[1] and not s.get("deployment",{}).get("clock_blocked")' "$expected_digest" <<<"$current_status" || die '提交许可证尚未成为当前有效版本或时钟异常；不能用旧版本组件确认冒充本次激活'

  # O3：激活轮询改为有界退避重试。
  # 每一轮 = 一次「状态 + 逐应用执行状态（必要时激活）」；瞬时错误按 2/4/8/16/30
  # 秒退避后重开本轮（激活请求携带旧 revision 时不原地重放，避免 409 误判），
  # 确定性错误立即失败。deadline 是唯一的硬边界。
  record_enforced() { # <应用编码>
    local i=0
    while (( i < ${#enforced_apps[@]} )); do
      if [[ "${enforced_apps[$i]}" == "$1" ]]; then return 0; fi
      i=$((i + 1))
    done
    enforced_apps+=("$1")
    return 0
  }

  record_activated() { # <应用编码>
    local i=0
    while (( i < ${#activated_apps[@]} )); do
      if [[ "${activated_apps[$i]}" == "$1" ]]; then return 0; fi
      i=$((i + 1))
    done
    activated_apps+=("$1")
    return 0
  }

  activation_round() {
    local application execution v1 v2 v3 v4 round_complete
    # 每轮开头检查 deadline：任何路径都不能越过上限继续重试。
    check_deadline
    retry_stage='授权状态'
    retry_app='-'
    retry_reason='pending'
    if ! api_request '授权状态' '-' "$api/licenses/status"; then
      retry_stage='授权状态'
      retry_app='-'
      retry_reason='error'
      return 1
    fi
    if ! python3 -c 'import json,sys; d=json.load(sys.stdin); s=d.get("data",{}); assert d.get("code")=="OK" and s.get("deployment",{}).get("current_digest")==sys.argv[1] and not s.get("deployment",{}).get("clock_blocked"); systems=s.get("systems") or []; assert all(any(x.get("code")==a and x.get("status")=="VALID" for x in systems) for a in sys.argv[2:])' "$expected_digest" $applications <"$API_BODY"; then
      die '许可证已变化、未生效、到期或时钟异常；执行确认已停止'
    fi
    round_complete=true
    for application in $applications; do
      # 逐应用迭代同样受 deadline 约束：单次请求预算按剩余时间收缩。
      check_deadline
      retry_app="$application"
      retry_stage='执行状态'
      retry_reason='pending'
      if ! api_request '执行状态' "$application" "$api/licenses/enforcement/$application"; then
        retry_stage='执行状态'
        retry_app="$application"
        retry_reason='error'
        mark_pending_code "$application" "${last_code:-none}"
        return 1
      fi
      execution="$(<"$API_BODY")"
      v1=""; v2=""; v3=""; v4=""
      { IFS= read -r v1; IFS= read -r v2; IFS= read -r v3; IFS= read -r v4; } <<<"$(python3 -c 'import json,sys
d = json.load(sys.stdin)
assert d.get("code") == "OK"
s = d["data"]
members = s.get("members") or []
r = s["revision"]
state = s.get("state") or ""
snapshot = bool(s.get("snapshot_current"))
ready = sum(1 for m in members if m.get("ready_at"))
ack = sum(1 for m in members if m.get("ack_at") and m.get("ack_revision") == r and m.get("issued_revision") == r and not m.get("retired_at"))
complete = state == "ENFORCED" and snapshot and bool(members) and ready == len(members) and ack == len(members)
print("complete" if complete else "pending")
print(r)
print("state=%s revision=%s ready=%d/%d ack=%d/%d snapshot_current=%s" % (state or "none", r, ready, len(members), ack, len(members), "true" if snapshot else "false"))
print(state)' <<<"$execution")" || true
      if ! [[ "$v1" == "complete" || "$v1" == "pending" ]]; then
        die '执行状态响应异常（缺少 state/revision/members 或 code 非 OK）'
      fi
      if [[ "$v1" == complete ]]; then
        clear_pending "$application"
        record_enforced "$application"
        continue
      fi
      round_complete=false
      [[ "$v2" =~ ^[0-9]+$ ]] || die '执行版本无效'
      record_pending "$application" "$v3" "${last_code:-OK}"
      # APPLYING 表示服务端正在应用：只等待确认，不重复 POST 激活；
      # PENDING_ENFORCEMENT 或空态才发起激活。
      if [[ "$v4" == "APPLYING" ]]; then
        retry_stage='执行确认'
        retry_reason='pending'
        return 1
      fi
      record_activated "$application"
      if ! api_request '执行激活' "$application" -X POST -H 'Content-Type: application/json' \
        --data-binary @- "$api/licenses/enforcement/$application/activation" \
        <<<"$(printf '{"expected_revision":%s}' "$v2")"; then
        retry_stage='执行激活'
        retry_reason='error'
        return 1
      fi
    done
    if [[ "$round_complete" == true ]]; then return 0; fi
    retry_stage='执行确认'
    retry_app="-"
    retry_reason='pending'
    return 1
  }

  while :; do
    # deadline 检查是循环第一件事：任何路径都不能越过上限继续重试。
    check_deadline
    attempts=$((attempts + 1))
    if activation_round; then
      break
    fi
    poll_rounds=$((poll_rounds + 1))
    retry_seq=$((retry_seq + 1))
    log_retry "${retry_stage:-授权状态}" "${retry_app:--}"
    backoff_sleep "$retry_seq"
  done
  trap - INT TERM
  info '所有许可证覆盖应用均已 ENFORCED，且必需组件 READY/当前快照 ACK 已确认。'
fi
