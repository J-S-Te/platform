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

set -Eeuo pipefail
IFS=$'\n\t'
umask 077

# ============================ 全局默认值与共享状态 ============================
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
RETRY_MODE=false
PRESET=""
INTERACTIVE=false
ASSUME_YES=false
DRY_RUN=false
SHALLOW_OFFBOARD=false   # 旧版 offboard 行为：仅删 DB，不做容器/gateway 清理
DELETE_APPLICATION=false  # offboard 是否在 environment 删除后顺带删除 application
OWNS_SESSION=false
TEMP_DIR=""
RESPONSE_FILE=""
CONFIRMATION_CODE=""      # offboard 必填

# 库文件定位：入口被 source 时 BASH_SOURCE[0] 仍是本文件路径。
script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

source "${script_dir}/lib/subsystem-common.sh"
source "${script_dir}/lib/subsystem-http.sh"
source "${script_dir}/lib/subsystem-onboard.sh"
source "${script_dir}/lib/subsystem-update.sh"
source "${script_dir}/lib/subsystem-status.sh"
source "${script_dir}/lib/subsystem-offboard.sh"
source "${script_dir}/lib/subsystem-connect.sh"


# ============================ 子命令分发 ============================
dispatch() {
  if [[ $# -eq 0 ]]; then
    usage >&2
    exit 2
  fi
  SUBCOMMAND="$1"
  shift
  case "$SUBCOMMAND" in
    onboard)   cmd_onboard   "$@" ;;
    update)    RETRY_MODE=false; cmd_update "$@" ;;
    retry)     RETRY_MODE=true; cmd_update "$@" ;;
    status)    cmd_status    "$@" ;;
    connect)   cmd_connect   "$@" ;;
    offboard)  cmd_offboard  "$@" ;;
    -h|--help|help) usage; exit 0 ;;
    *)
      log "ERROR" "未知子命令：${SUBCOMMAND}。支持：connect、onboard、update、retry、status、offboard"
      usage >&2
      exit 2
      ;;
  esac
}

main() {
  require_command curl
  require_command python3
  # 通用认证/控制参数先扫一遍（包含子命令自身支持的相同参数），剩余的留给子命令解析。
  # 兼容做法：每个子命令的 parse_*_args 独立解析自己的参数集合。
  dispatch "$@"
}

# 直接执行时运行业务入口；被 source（如库化验收）时只加载定义，不产生副作用。
if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  main "$@"
fi
