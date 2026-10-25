#!/usr/bin/env bash
# subsystem.sh status 子命令（批次 5 库化，逐字迁移）；仅含函数定义，无顶层副作用——由 scripts/subsystem.sh 入口在全局变量区之后 source。

# ============================ status 子命令 ============================
status_usage() {
  cat <<'USAGE'
用法：subsystem.sh status [认证参数] --application-code CODE --environment ENV

查询基础平台记录的部署 Agent 状态。常见状态：
  PROVISIONING  首次接入记录已创建，Agent 尚未完成
  UPDATING      正在重建或重试
  READY         Agent 已成功完成，门户才会展示该子系统
  PROVISION_FAILED  最近一次 Agent 操作失败，可使用 retry 重试
  DRAINING      正在下线
  OFFBOARDED    基础设施已拆解，等待或已经完成 DB 清理
USAGE
}

cmd_status() {
  local argument=""
  for argument in "$@"; do
    if [[ "$argument" == "-h" || "$argument" == "--help" ]]; then
      status_usage
      exit 0
    fi
  done
  parse_update_args "$@"
  require_command curl
  require_command python3

  TEMP_DIR="$(mktemp -d "${TMPDIR:-/tmp}/subsystem-status.XXXXXX")"
  RESPONSE_FILE="$TEMP_DIR/status-response.json"
  trap cleanup EXIT

  if update_required_configuration_missing; then
    log "ERROR" "status 必须提供 --application-code 和 --environment"
    status_usage >&2
    exit 2
  fi
  if ! update_validate_and_normalize; then
    exit 2
  fi
  ensure_authenticated

  local endpoint="/subsystem-status?application_code=${APPLICATION_CODE}&environment=${ENVIRONMENT}"
  local status=""
  status="$(http_get "$endpoint" "$RESPONSE_FILE")" || {
    diagnose_connection_failure "/subsystem-status"
    exit 1
  }
  if [[ "$status" != "200" ]]; then
    print_api_error "$status" "$RESPONSE_FILE" "/subsystem-status"
    exit 1
  fi
  python3 - "$RESPONSE_FILE" <<'PY'
import json
import sys
with open(sys.argv[1], encoding="utf-8") as handle:
    envelope = json.load(handle)
data = envelope.get("data") or {}
print("子系统部署状态：")
for key, label in (("application_code", "应用"), ("environment", "环境"), ("status", "状态"),
                   ("operation", "最近操作"), ("generation", "代次"), ("attempt_count", "尝试次数"),
                   ("last_error_code", "错误编码"), ("last_error", "错误说明"),
                   ("updated_at", "更新时间")):
    value = data.get(key)
    if value not in (None, ""):
        print(f"  {label}：{value}")
print(f"  追踪号：{envelope.get('request_id') or '-'}")
PY
}
