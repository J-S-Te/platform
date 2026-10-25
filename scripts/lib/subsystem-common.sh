#!/usr/bin/env bash
# subsystem.sh 通用函数与通用参数解析（批次 5 库化，逐字迁移）；仅含函数定义，无顶层副作用——由 scripts/subsystem.sh 入口在全局变量区之后 source。

# ============================ 通用函数 ============================
log() {
  local level="$1"
  shift
  printf '[%s] [%s] %s\n' "$(date '+%Y-%m-%d %H:%M:%S')" "$level" "$*" >&2
}

usage() {
  cat <<'USAGE'
用法：
  subsystem.sh <connect|onboard|update|retry|status|offboard> [通用认证参数] [子命令参数]

子命令：
  connect    统一接入向导（预览 → 确认 → 自动执行 → 查看结果／继续恢复）；动作由服务端判定
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
