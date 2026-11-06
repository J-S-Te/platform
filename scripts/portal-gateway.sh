#!/usr/bin/env bash
# 把平台管理后台登记的子系 environment（path_prefix + upstream_url）渲染成 nginx
# 反向代理 location 配置，并 reload nginx。配合 applicationregistry 的
# Environment.UpstreamURL / Environment.PathPrefix 字段使用。
#
# 注意：这是底层网关维护工具。完整子系统接入请使用 scripts/subsystem.sh onboard，
# 不要仅执行 add/reload，否则不会创建 Application、Environment、LoginTarget 和 OAuth Client。
#
# 设计：所有用户访问都从门户统一入口进入，门户的 frontend nginx 把
# 配置的 path_prefix（例如 /external_system/）反代到子系真实内网地址。这样子系不需要对外暴露端口，
# 也不需要在 OAuth redirect_uri、LoginTarget.TargetURI 里写任何子系端口。
#
# 分工：本脚本只保留 shell 侧职责——CLI 分发与缺失参数校验、跨进程写锁
# （flock 优先，macOS 回退原子 mkdir 目录锁）、把渲染结果用 cat 覆盖写回 include
# 文件（保留 bind mount inode）、nginx -t / nginx -s reload。
# 渲染与解析逻辑在 scripts/lib/gateway_render.py（仅标准库，Python 3.9+）：
#   * nginx location 片段渲染、托管头、parse_entry 解析；
#   * curl --config 值转义、code/path_prefix/upstream_url/page_limit 校验；
#   * sync 的分页拉取、空页守卫、聚合与 path_prefix 去重判定。
# 解释器默认 python3，可用 PORTAL_GATEWAY_PYTHON 指定（例如固定到某个 python3.x）。
#
# 用法：
#   portal-gateway.sh add <code> <path_prefix> <upstream_url>
#   portal-gateway.sh remove <code>
#   portal-gateway.sh list
#   portal-gateway.sh sync   # 仅在部署管理 API 认证适配层后拉取并全量重写
#   portal-gateway.sh reload
#   portal-gateway.sh apply  # 仅在 sync 可认证时执行 sync + reload
#
# 维护的 nginx include 文件默认写入项目的 docker/portal-apps-locations.conf，由 compose 挂载到 frontend Nginx。
# 传统宿主机 Nginx 可通过 PORTAL_GATEWAY_NGINX_INCLUDE 覆盖该路径。

set -Eeuo pipefail
IFS=$'\n\t'
umask 077

PROJECT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
NGINX_INCLUDE="${PORTAL_GATEWAY_NGINX_INCLUDE:-${PROJECT_ROOT}/docker/portal-apps-locations.conf}"
NGINX_RELOAD_CMD="${PORTAL_GATEWAY_NGINX_RELOAD_CMD:-}"
COMPOSE_FILE="${PORTAL_GATEWAY_COMPOSE_FILE:-}"
API_BASE_URL="${PORTAL_GATEWAY_API_BASE_URL:-http://127.0.0.1:8080}"
API_TOKEN="${PORTAL_GATEWAY_API_TOKEN:-}"
PAGE_LIMIT="${PORTAL_GATEWAY_PAGE_LIMIT:-100}"
LOCK_FILE="${PORTAL_GATEWAY_LOCK_FILE:-${NGINX_INCLUDE}.lock}"
LOCK_TIMEOUT="${PORTAL_GATEWAY_LOCK_TIMEOUT:-60}"
# 这些模块已经编译进统一 Vue 前端，只允许代理其后端 API 子路径，不能生成整站反代。
# 判定逻辑在 gateway_render.is_integrated_frontend_code，这里只负责取值与透传。
INTEGRATED_FRONTEND_CODES="${PORTAL_GATEWAY_INTEGRATED_FRONTEND_CODES:-contract_management customer_and_opportunity customer-opportunity}"
PYTHON_BIN="${PORTAL_GATEWAY_PYTHON:-python3}"
RENDER_SCRIPT="${PROJECT_ROOT}/scripts/lib/gateway_render.py"
# 渲染器退出码约定（见 gateway_render.py 模块文档）：
#   0=stdout 为需要写入的 include 全文；1=运行时失败；2=输入非法；3=幂等跳过无需写入。
RENDER_NO_WRITE=3

log() {
  local level="$1"
  shift
  printf '[%s] [%s] %s\n' "$(date '+%Y-%m-%d %H:%M:%S')" "$level" "$*" >&2
}

usage() {
  cat <<'USAGE'
用法：
  portal-gateway.sh <command> [args]

完整接入：
  bash scripts/subsystem.sh onboard --help
  本脚本仅维护网关，不创建应用、环境、登录目标或 OAuth 客户端。

命令：
  add <code> <path_prefix> <upstream_url>
      追加或替换一个子系的反代 location。
  remove <code>
      移除一个子系的反代 location。
  list
      列出当前已注册的子系（按 code 字母序）。
  sync
      从平台管理后台拉所有 ACTIVE application/environment，全量重写 include 文件。
      当前管理接口默认只接受平台会话 Cookie，而本命令发送 Bearer Token；默认部署不可直接使用。
      只有部署了受控认证适配层后，才可通过 PORTAL_GATEWAY_API_BASE_URL 和
      PORTAL_GATEWAY_API_TOKEN 提供 application/environment 读权限。
  reload
      触发 nginx 重载配置。
  apply
      在 sync 已具备受控认证适配层时同步配置，并在成功后立即重载 nginx。

环境变量：
  PORTAL_GATEWAY_NGINX_INCLUDE     include 文件绝对路径
  PORTAL_GATEWAY_NGINX_RELOAD_CMD  已废弃：安全策略（SEC-F10）不再执行环境变量命令串，设置后仅告警并忽略
  PORTAL_GATEWAY_COMPOSE_FILE       frontend 所在 Compose 文件；未设置时自动探测
  PORTAL_GATEWAY_API_BASE_URL      sync 认证适配层的平台 API 入口
  PORTAL_GATEWAY_API_TOKEN         认证适配层接受的 Bearer token
  PORTAL_GATEWAY_PAGE_LIMIT        列表接口单页大小
  PORTAL_GATEWAY_LOCK_FILE         配置写锁文件；默认 <include>.lock
  PORTAL_GATEWAY_LOCK_TIMEOUT      等待配置写锁的秒数；默认 60
USAGE
}

# Linux 优先使用 util-linux flock；macOS 等缺少 flock 的环境回退到原子 mkdir 锁。
# 锁覆盖完整的 read/merge/write 流程，避免 CI、provisioner 与人工操作并发时丢路由。
run_with_gateway_lock() {
  local operation="$1"
  shift
  [[ "$LOCK_TIMEOUT" =~ ^[1-9][0-9]*$ ]] || {
    log "ERROR" "PORTAL_GATEWAY_LOCK_TIMEOUT 必须是正整数：$LOCK_TIMEOUT"
    return 2
  }
  mkdir -p -- "$(dirname -- "$LOCK_FILE")"

  if command -v flock >/dev/null 2>&1; then
    local rc
    exec 9>"$LOCK_FILE"
    if ! flock -w "$LOCK_TIMEOUT" 9; then
      # 全角括号紧跟变量时必须用 ${var} 形式：bash 3.2 会把非 ASCII 字节当作变量名的一部分，
      # 退化成 unbound variable，导致这条超时诊断整行丢失。
      log "ERROR" "等待网关配置锁超时：${LOCK_FILE}（操作：${operation}）"
      exec 9>&-
      return 1
    fi
    if ( "$@" ); then rc=0; else rc=$?; fi
    flock -u 9 || true
    exec 9>&-
    return "$rc"
  fi

  # macOS 没有系统 flock 时，mkdir 的原子性提供互斥；锁目录只使用固定后缀，不拼入操作参数。
  local lock_dir="${LOCK_FILE}.d" waited=0 rc
  while ! mkdir -- "$lock_dir" 2>/dev/null; do
    if (( waited >= LOCK_TIMEOUT )); then
      log "ERROR" "等待网关配置目录锁超时：${lock_dir}（操作：${operation}）"
      return 1
    fi
    sleep 1
    waited=$((waited + 1))
  done
  # 子进程隔离可确保被调用函数中的 exit 不会跳过目录锁清理。
  if ( "$@" ); then rc=0; else rc=$?; fi
  rmdir -- "$lock_dir" 2>/dev/null || true
  return "$rc"
}

require_arg() {
  if [[ -z "${1:-}" ]]; then
    log "ERROR" "$2"
    exit 2
  fi
}

# 调用 Python 渲染器：stdout 是 include 全文（add/remove/sync）或列表输出（list），
# stderr 是日志，退出码见 RENDER_NO_WRITE 约定。
# 环境变量显式透传，保持与旧脚本相同的注入面（默认值在 shell 侧解析）。
run_renderer() {
  if ! command -v "$PYTHON_BIN" >/dev/null 2>&1; then
    log "ERROR" "未找到 Python 3 解释器：${PYTHON_BIN}（渲染与解析由 scripts/lib/gateway_render.py 承担）"
    return 1
  fi
  PORTAL_GATEWAY_NGINX_INCLUDE="$NGINX_INCLUDE" \
    PORTAL_GATEWAY_API_BASE_URL="$API_BASE_URL" \
    PORTAL_GATEWAY_API_TOKEN="$API_TOKEN" \
    PORTAL_GATEWAY_PAGE_LIMIT="$PAGE_LIMIT" \
    PORTAL_GATEWAY_INTEGRATED_FRONTEND_CODES="$INTEGRATED_FRONTEND_CODES" \
    "$PYTHON_BIN" -B "$RENDER_SCRIPT" "$@"
}

# 把渲染器输出写入 include 文件：先落到 0600 临时文件，再 cat 覆盖。
# 该文件通常以单文件方式 bind mount 到 Nginx 容器，不能用 mv 替换 inode，
# 否则运行中的容器仍会读取旧文件；外层互斥锁保证覆盖期间没有另一写者。
# 返回 0 表示已写入，RENDER_NO_WRITE 表示渲染器判定无需写入，其余为渲染器错误码。
render_into_include() {
  local tmp rc
  tmp="$(mktemp)"
  if run_renderer "$@" >"$tmp"; then
    mkdir -p -- "$(dirname -- "$NGINX_INCLUDE")"
    if [[ ! -f "$NGINX_INCLUDE" ]]; then
      install -m 0644 /dev/null "$NGINX_INCLUDE"
    fi
    if cat -- "$tmp" >"$NGINX_INCLUDE"; then
      chmod 0644 "$NGINX_INCLUDE"
      rc=0
    else
      rc=$?
    fi
  else
    rc=$?
  fi
  rm -f -- "$tmp"
  return "$rc"
}

do_add() {
  local code="$1" path_prefix="$2" upstream_url="$3" rc=0
  render_into_include add "$code" "$path_prefix" "$upstream_url" || rc=$?
  if (( rc == RENDER_NO_WRITE )); then
    # 内置统一前端模块：渲染器已清理历史整站代理并记录 INFO，按既有语义成功返回。
    return 0
  fi
  if (( rc != 0 )); then
    return "$rc"
  fi
  log "INFO" "已注册子系 ${code} -> ${path_prefix} -> ${upstream_url}"
}

do_remove() {
  local code="$1" rc=0
  render_into_include remove "$code" || rc=$?
  if (( rc == RENDER_NO_WRITE )); then
    # include 不存在或目标条目不存在：渲染器已记录幂等 INFO，不改动文件。
    return 0
  fi
  if (( rc != 0 )); then
    return "$rc"
  fi
  log "INFO" "已移除子系 ${code}"
}

do_list() {
  local rc=0
  run_renderer list || rc=$?
  if (( rc == RENDER_NO_WRITE )); then
    return 0
  fi
  return "$rc"
}

do_sync() {
  local rc=0
  render_into_include sync || rc=$?
  if (( rc != 0 )); then
    return "$rc"
  fi
  log "INFO" "sync 完成，请执行 reload 让 nginx 生效"
}

resolve_compose_file() {
  if [[ -n "$COMPOSE_FILE" ]]; then
    if [[ ! -f "$COMPOSE_FILE" ]]; then
      log "ERROR" "PORTAL_GATEWAY_COMPOSE_FILE 不存在：$COMPOSE_FILE"
      return 1
    fi
    printf '%s' "$COMPOSE_FILE"
    return
  fi

  local candidate="${PROJECT_ROOT}/compose.local.yaml"
  if [[ -f "$candidate" ]] && docker compose -f "$candidate" ps --status running --services 2>/dev/null | grep -Fxq 'frontend'; then
    printf '%s' "$candidate"
    return
  fi

  log "ERROR" "未找到 compose.local.yaml 中正在运行的 frontend 服务；请先执行 docker-local.sh up，或设置 PORTAL_GATEWAY_COMPOSE_FILE"
  return 1
}

do_reload() {
  # 安全（SEC-F10）：不再执行来自环境变量的命令串。本脚本可能由持有 docker.sock 的
  # Agent 容器以 os.Environ() 启动，对 NGINX_RELOAD_CMD 执行 bash -c 等于把特权命令执行交给
  # 环境变量注入者，并绕过内置路径的 nginx -t 校验；现统一走内置 reload 路径，
  # nginx -t 校验是 reload 的必经步骤。
  if [[ -n "$NGINX_RELOAD_CMD" ]]; then
    log "WARN" "已忽略 NGINX_RELOAD_CMD 环境变量钩子（安全策略禁止执行环境变量命令串），改用内置 reload 路径"
  fi

  local compose_file
  compose_file="$(resolve_compose_file)"
  log "INFO" "校验 nginx 配置: docker compose -f ${compose_file} exec -T frontend nginx -t"
  # 先校验后平滑 reload；非法配置不能影响当前仍在服务的 Nginx worker。
  docker compose -f "$compose_file" exec -T frontend nginx -t
  log "INFO" "触发 nginx reload: docker compose -f ${compose_file} exec -T frontend nginx -s reload"
  docker compose -f "$compose_file" exec -T frontend nginx -s reload
}

main() {
  local command="${1:-}"
  if [[ -z "$command" ]]; then
    usage
    exit 1
  fi
  case "$command" in
    add) shift; require_arg "${1:-}" "缺少 <code>"; require_arg "${2:-}" "缺少 <path_prefix>"; require_arg "${3:-}" "缺少 <upstream_url>"; run_with_gateway_lock add do_add "$1" "$2" "$3" ;;
    remove) shift; require_arg "${1:-}" "缺少 <code>"; run_with_gateway_lock remove do_remove "$1" ;;
    list) run_with_gateway_lock list do_list ;;
    sync) run_with_gateway_lock sync do_sync ;;
    reload) do_reload ;;
    apply) run_with_gateway_lock sync do_sync && do_reload ;;
    -h|--help|help) usage ;;
    *) log "ERROR" "未知命令: $command"; usage; exit 1 ;;
  esac
}

main "$@"
