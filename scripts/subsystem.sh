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
#
# 实现分布（CLI 契约不变，仅实现语言不同）：
#   connect / status / update / retry / adopt / offboard → Python 包 scripts/lib/uipctl/
#   onboard                                            → 仍为 Shell（scripts/lib/subsystem-onboard.sh）
#
# 本文件只负责子命令识别与分发：已迁移的子命令 exec 到 `python3 -m uipctl`，
# 其余（onboard）继续 source Shell 库并原地执行，保证旧路径可用。

set -Eeuo pipefail
IFS=$'\n\t'
umask 077

# ============================ Shell 路径（onboard）默认值与共享状态 ============================
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
ISSUER_ALIAS=""

# 通用控制
SUBCOMMAND=""
PRESET=""
INTERACTIVE=false
ASSUME_YES=false
DRY_RUN=false
OWNS_SESSION=false
TEMP_DIR=""
RESPONSE_FILE=""

# 库文件定位：入口被 source 时 BASH_SOURCE[0] 仍是本文件路径。
script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# 顶层用法与 log 只依赖 common 库；Python 子命令不加载其余 Shell 库。
source "${script_dir}/lib/subsystem-common.sh"


# ============================ 子命令分发 ============================
# onboard 仍由 Shell 实现（下一轮迁移）：只加载它真正依赖的库。
shell_onboard() {
  require_command curl
  require_command python3
  # shellcheck source=lib/subsystem-http.sh
  source "${script_dir}/lib/subsystem-http.sh"
  # shellcheck source=lib/subsystem-update.sh
  source "${script_dir}/lib/subsystem-update.sh"
  # shellcheck source=lib/subsystem-onboard.sh
  source "${script_dir}/lib/subsystem-onboard.sh"
  cmd_onboard "$@"
}

# python_subcommand 已迁移到 scripts/lib/uipctl/：用同一份参数、同一份环境进入 Python 入口。
python_subcommand() {
  require_command python3
  PYTHONPATH="${script_dir}/lib${PYTHONPATH:+:${PYTHONPATH}}"
  export PYTHONPATH
  # -B 避免在仓库里生成 __pycache__，保持工作树干净。
  exec python3 -B -m uipctl "$@"
}

dispatch() {
  if [[ $# -eq 0 ]]; then
    usage >&2
    exit 2
  fi
  SUBCOMMAND="$1"
  shift
  case "$SUBCOMMAND" in
    onboard)   shell_onboard "$@" ;;
    connect|status|update|retry|adopt|offboard)
      python_subcommand "$SUBCOMMAND" "$@" ;;
    -h|--help|help) usage; exit 0 ;;
    *)
      log "ERROR" "未知子命令：${SUBCOMMAND}。支持：connect、onboard、update、retry、status、offboard"
      usage >&2
      exit 2
      ;;
  esac
}

main() {
  dispatch "$@"
}

# 直接执行时运行业务入口；被 source（如库化验收）时只加载定义，不产生副作用。
if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  main "$@"
fi
