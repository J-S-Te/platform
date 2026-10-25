#!/usr/bin/env bash
# subsystem.sh connect 统一接入向导（批次 5 库化，逐字迁移）；仅含函数定义，无顶层副作用——由 scripts/subsystem.sh 入口在全局变量区之后 source。

# ============================ connect：统一接入向导 ============================
connect_usage() {
  cat <<'USAGE'
用法：
  subsystem.sh connect --application-code CODE --environment ENV [选项]

说明：
  统一接入向导（预览 → 确认 → 自动执行 → 查看结果／继续恢复）。
  动作（ONBOARD / ADOPT / UPDATE / RETRY）由服务端判定，调用方不选择动作；
  缺少批准、许可证、镜像或认证观察期未结束时进入"等待处理"。

选项：
  --application-code CODE   目标应用编码（如 contract_management、settlement）
  --environment ENV         目标环境（dev/test/staging/prod）
  --public-origin URL       对外公开地址（首次接入或公开入口变更时提供）
  --auth-provider ALIAS     认证提供方（platform/keycloak）；默认沿用服务端绑定
  --initial-admin USER_ID   首次接入的初始管理员
  --idempotency-key KEY     幂等键；默认按"目标+预览版本"生成，重复执行返回原操作
  --resume OPERATION_ID     材料补齐后继续该操作
  --no-watch                只提交/恢复，不轮询进度
  --watch-interval SECONDS  轮询间隔；默认 5
  --watch-max ATTEMPTS      最多轮询次数；默认 60
  -y, --yes                 跳过确认（非交互执行）

通用认证参数与其他子命令一致（--api-base-url/--platform-origin/--account/--password-stdin/--cookie-file）。
USAGE
}

parse_connect_args() {
  CONNECT_PUBLIC_ORIGIN=""
  CONNECT_INITIAL_ADMIN=""
  CONNECT_IDEMPOTENCY_KEY=""
  CONNECT_RESUME_ID=""
  CONNECT_WATCH=true
  CONNECT_WATCH_INTERVAL=5
  CONNECT_WATCH_MAX=60
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --api-base-url)
        [[ -n "${2:-}" ]] || { log "ERROR" "--api-base-url 需要参数"; exit 2; }
        API_BASE_URL="$2"; shift 2 ;;
      --platform-origin)
        [[ -n "${2:-}" ]] || { log "ERROR" "--platform-origin 需要参数"; exit 2; }
        PLATFORM_ORIGIN="$2"; shift 2 ;;
      --account)
        [[ -n "${2:-}" ]] || { log "ERROR" "--account 需要参数"; exit 2; }
        ACCOUNT="$2"; shift 2 ;;
      --password-stdin)
        PASSWORD_STDIN=true; shift ;;
      --cookie-file)
        [[ -n "${2:-}" ]] || { log "ERROR" "--cookie-file 需要参数"; exit 2; }
        COOKIE_FILE="$2"; shift 2 ;;
      --replace-existing-session)
        REPLACE_EXISTING_SESSION=true; shift ;;
      --application-code)
        [[ -n "${2:-}" ]] || { log "ERROR" "--application-code 需要参数"; exit 2; }
        APPLICATION_CODE="$2"; shift 2 ;;
      --environment)
        [[ -n "${2:-}" ]] || { log "ERROR" "--environment 需要参数"; exit 2; }
        ENVIRONMENT="$2"; shift 2 ;;
      --public-origin)
        [[ -n "${2:-}" ]] || { log "ERROR" "--public-origin 需要参数"; exit 2; }
        CONNECT_PUBLIC_ORIGIN="$2"; shift 2 ;;
      --auth-provider)
        [[ -n "${2:-}" ]] || { log "ERROR" "--auth-provider 需要参数"; exit 2; }
        ISSUER_ALIAS="$2"; shift 2 ;;
      --initial-admin)
        [[ -n "${2:-}" ]] || { log "ERROR" "--initial-admin 需要参数"; exit 2; }
        CONNECT_INITIAL_ADMIN="$2"; shift 2 ;;
      --idempotency-key)
        [[ -n "${2:-}" ]] || { log "ERROR" "--idempotency-key 需要参数"; exit 2; }
        CONNECT_IDEMPOTENCY_KEY="$2"; shift 2 ;;
      --resume)
        [[ -n "${2:-}" ]] || { log "ERROR" "--resume 需要参数"; exit 2; }
        CONNECT_RESUME_ID="$2"; shift 2 ;;
      --no-watch)
        CONNECT_WATCH=false; shift ;;
      --watch-interval)
        [[ -n "${2:-}" ]] || { log "ERROR" "--watch-interval 需要参数"; exit 2; }
        CONNECT_WATCH_INTERVAL="$2"; shift 2 ;;
      --watch-max)
        [[ -n "${2:-}" ]] || { log "ERROR" "--watch-max 需要参数"; exit 2; }
        CONNECT_WATCH_MAX="$2"; shift 2 ;;
      -y|--yes)
        ASSUME_YES=true; shift ;;
      -h|--help)
        connect_usage; exit 0 ;;
      *)
        log "ERROR" "未知参数：$1"
        connect_usage >&2
        exit 2 ;;
    esac
  done
}

connect_write_preview_payload() {
  # connect_write_preview_payload FILE
  python3 - "$1" "$APPLICATION_CODE" "$ENVIRONMENT" <<'PY'
import json
import sys
path, application_code, environment = sys.argv[1], sys.argv[2], sys.argv[3]
with open(path, "w", encoding="utf-8") as handle:
    json.dump({"application_code": application_code, "environment": environment}, handle, ensure_ascii=False)
PY
}

connect_write_submit_payload() {
  # connect_write_submit_payload FILE PREVIEW_VERSION IDEMPOTENCY_KEY
  python3 - "$1" "$APPLICATION_CODE" "$ENVIRONMENT" "$CONNECT_PUBLIC_ORIGIN" "$ISSUER_ALIAS" "$CONNECT_INITIAL_ADMIN" "$2" "$3" <<'PY'
import json
import sys
path, code, env, public_origin, auth_provider, initial_admin, preview_version, idempotency_key = sys.argv[1:9]
payload = {"application_code": code, "environment": env, "preview_version": preview_version, "idempotency_key": idempotency_key}
if public_origin:
    payload["public_origin"] = public_origin
if auth_provider:
    payload["auth_provider"] = auth_provider
if initial_admin:
    payload["initial_admin_id"] = initial_admin
with open(path, "w", encoding="utf-8") as handle:
    json.dump(payload, handle, ensure_ascii=False)
PY
}

connect_print_preview() {
  # connect_print_preview FILE：打印判定结果；stdout 末行输出 preview_version。
  python3 - "$1" <<'PY'
import json
import sys
with open(sys.argv[1], encoding="utf-8") as handle:
    envelope = json.load(handle)
data = envelope.get("data") or {}
action = data.get("action") or ""
labels = {"ONBOARD": "首次接入", "ADOPT": "接管运行时", "UPDATE": "受控更新", "RETRY": "重试失败部署", "NONE": "无需操作"}
print("接入条件检查：")
print("  判定动作：%s%s" % (action, ("（%s）" % labels[action]) if action in labels else ""))
print("  判定说明：%s" % (data.get("reason") or ""))
print("  起始阶段：%s" % (data.get("next_stage") or ""))
print("  执行入口：%s" % {"unified": "统一接入入口", "legacy": "既有受控更新入口", "none": "无需操作"}.get(data.get("execution_path") or "", "未知"))
legacy_hint = data.get("legacy_hint") or ""
if legacy_hint:
    print("  提示：%s" % legacy_hint)
if data.get("display_only"):
    print("  结果：目标已接入且配置一致，无需重复操作")
missing = data.get("missing") or []
if missing:
    print("  未满足条件：")
    for item in missing:
        print("    - [%s] %s" % (item.get("Code") or item.get("code") or "", item.get("Message") or item.get("message") or ""))
        next_action = item.get("NextAction") or item.get("next_action") or ""
        if next_action:
            print("      下一步：%s" % next_action)
required = data.get("required_permissions") or []
if required:
    print("  所需权限：%s" % ", ".join(required))
missing_permissions = data.get("missing_permissions") or []
if missing_permissions:
    print("  当前账号缺少权限：%s" % ", ".join(missing_permissions))
change_summary = data.get("change_summary") or []
if change_summary:
    print("  变更摘要：")
    for item in change_summary:
        print("    - %s" % item)
print(data.get("preview_version") or "")
PY
}

connect_print_operation() {
  # connect_print_operation FILE：打印操作状态与分项阶段结果；stdout 末行为状态。
  python3 - "$1" <<'PY'
import json
import sys
with open(sys.argv[1], encoding="utf-8") as handle:
    envelope = json.load(handle)
data = envelope.get("data") or {}
status_labels = {"PENDING": "待执行", "RUNNING": "执行中", "WAITING": "等待处理", "FAILED": "失败", "SUCCEEDED": "已完成"}
status = data.get("status") or ""
print("接入操作：%s" % (data.get("operation_id") or ""))
print("  动作：%s  当前阶段：%s  状态：%s%s" % (
    data.get("action") or "", data.get("stage") or "", status,
    ("（%s）" % status_labels[status]) if status in status_labels else ""))
print("  尝试次数：%s" % (data.get("attempt_count") if data.get("attempt_count") is not None else 0))
if data.get("error_code"):
    print("  错误编码：%s" % data.get("error_code"))
if data.get("error_message"):
    print("  错误说明：%s" % data.get("error_message"))
if data.get("result_ref"):
    print("  结果引用：%s" % data.get("result_ref"))
stages = data.get("stages") or []
if stages:
    print("  阶段结果：")
    for stage in stages:
        line = "    - %s：%s（尝试 %s）" % (stage.get("stage") or "", stage.get("status") or "", stage.get("attempt_count") or 0)
        if stage.get("error_code"):
            line += " 错误=%s" % stage.get("error_code")
        if stage.get("evidence_ref"):
            line += " 证据=%s" % stage.get("evidence_ref")
        print(line)
missing = data.get("missing") or []
if missing:
    print("  仍需处理：")
    for item in missing:
        print("    - [%s] %s" % (item.get("Code") or item.get("code") or "", item.get("Message") or item.get("message") or ""))
        next_action = item.get("NextAction") or item.get("next_action") or ""
        if next_action:
            print("      下一步：%s" % next_action)
if status == "WAITING":
    print("  继续方式：补齐上述条件后执行 subsystem.sh connect --resume %s" % (data.get("operation_id") or ""))
elif status == "FAILED":
    print("  继续方式：查看错误后重新执行 subsystem.sh connect --application-code %s --environment %s（服务端将判定为 RETRY）" % (
        data.get("application_code") or "", data.get("environment") or ""))
print(status)
PY
}

connect_watch_operation() {
  # connect_watch_operation OPERATION_ID：轮询到终态或"等待处理"；--no-watch 时只查一次。
  local operation_id="$1"
  local endpoint="/api/v1/subsystem-access/operations/${operation_id}"
  local attempt=0
  local state=""
  local max_attempts="$CONNECT_WATCH_MAX"
  if [[ "$CONNECT_WATCH" != "true" ]]; then
    max_attempts=1
  fi
  while [[ "$attempt" -lt "$max_attempts" ]]; do
    attempt=$((attempt + 1))
    local status=""
    status="$(http_get "$endpoint" "$RESPONSE_FILE")" || {
      diagnose_connection_failure "$endpoint"
      exit 1
    }
    if [[ "$status" != "200" ]]; then
      print_api_error "$status" "$RESPONSE_FILE" "$endpoint"
      exit 1
    fi
    state="$(connect_print_operation "$RESPONSE_FILE" | tail -n 1)"
    case "$state" in
      SUCCEEDED|FAILED|WAITING) return 0 ;;
    esac
    if [[ "$CONNECT_WATCH" != "true" ]]; then
      return 0
    fi
    sleep "$CONNECT_WATCH_INTERVAL"
  done
  log "WARN" "轮询达到上限（${max_attempts} 次）；操作仍在执行，可稍后重新查询或使用 --resume 继续"
  return 0
}

cmd_connect() {
  local argument=""
  for argument in "$@"; do
    if [[ "$argument" == "-h" || "$argument" == "--help" ]]; then
      connect_usage
      exit 0
    fi
  done
  parse_connect_args "$@"
  require_command curl
  require_command python3

  TEMP_DIR="$(mktemp -d "${TMPDIR:-/tmp}/subsystem-connect.XXXXXX")"
  RESPONSE_FILE="$TEMP_DIR/connect-response.json"
  trap cleanup EXIT

  if [[ -z "$APPLICATION_CODE" || -z "$ENVIRONMENT" ]]; then
    log "ERROR" "connect 必须提供 --application-code 和 --environment"
    connect_usage >&2
    exit 2
  fi

  ensure_authenticated

  # 恢复模式：材料补齐后继续既有操作（服务端会重新检查一次）。
  if [[ -n "$CONNECT_RESUME_ID" ]]; then
    local resume_endpoint="/api/v1/subsystem-access/operations/${CONNECT_RESUME_ID}/resume"
    local resume_status=""
    resume_status="$(http_post_json "$resume_endpoint" "" "$RESPONSE_FILE")" || {
      diagnose_connection_failure "$resume_endpoint"
      exit 1
    }
    if [[ "$resume_status" != "200" ]]; then
      print_api_error "$resume_status" "$RESPONSE_FILE" "$resume_endpoint"
      exit 1
    fi
    connect_watch_operation "$CONNECT_RESUME_ID"
    return 0
  fi

  # 1) 只读条件检查（不创建资源、不部署）。
  local preview_payload="$TEMP_DIR/preview.json"
  connect_write_preview_payload "$preview_payload"
  local preview_status=""
  preview_status="$(http_post_json "/api/v1/subsystem-access/preview" "$preview_payload" "$RESPONSE_FILE")" || {
    diagnose_connection_failure "/subsystem-access/preview"
    exit 1
  }
  if [[ "$preview_status" != "200" ]]; then
    print_api_error "$preview_status" "$RESPONSE_FILE" "/subsystem-access/preview"
    exit 1
  fi
  local preview_version=""
  preview_version="$(connect_print_preview "$RESPONSE_FILE" | tail -n 1)"
  local display_only=""
  # 执行入口为既有受控入口时，统一入口不得提交（与页面行为一致）：只给出提示并退出。
  local execution_path=""
  execution_path="$(python3 -c "import json,sys;print((json.load(open(sys.argv[1],encoding='utf-8')).get('data') or {}).get('execution_path') or '')" "$RESPONSE_FILE")"
  if [[ "$execution_path" == "legacy" ]]; then
    log "ERROR" "该动作当前由既有受控更新入口执行；请按上方提示执行，完成后可重新检查条件。"
    exit 2
  fi
  display_only="$(python3 -c "import json,sys;print((json.load(open(sys.argv[1],encoding='utf-8')).get('data') or {}).get('display_only'))" "$RESPONSE_FILE")"
  if [[ "$display_only" == "True" ]]; then
    return 0
  fi

  # 2) 确认执行（动作由服务端判定，CLI 不选择动作）。
  if [[ "${ASSUME_YES:-false}" != "true" ]]; then
    local answer=""
    read -r -p "确认按上述判定执行？[y/N] " answer || answer=""
    case "$answer" in
      y|Y|yes|YES) ;;
      *) log "INFO" "已取消；未创建操作"; return 0 ;;
    esac
  fi

  # 3) 提交操作（幂等键未显式提供时按"目标+预览版本"生成）。
  local idempotency_key="$CONNECT_IDEMPOTENCY_KEY"
  if [[ -z "$idempotency_key" ]]; then
    idempotency_key="connect:${APPLICATION_CODE}:${ENVIRONMENT}:${preview_version}"
  fi
  local submit_payload="$TEMP_DIR/submit.json"
  connect_write_submit_payload "$submit_payload" "$preview_version" "$idempotency_key"
  local submit_status=""
  submit_status="$(http_post_json "/api/v1/subsystem-access/operations" "$submit_payload" "$RESPONSE_FILE")" || {
    diagnose_connection_failure "/subsystem-access/operations"
    exit 1
  }
  if [[ "$submit_status" != "202" && "$submit_status" != "200" ]]; then
    if [[ "$submit_status" == "409" ]]; then
      log "ERROR" "条件已变化或该目标已有执行中操作；请重新执行 connect 以获取最新预览。"
    fi
    print_api_error "$submit_status" "$RESPONSE_FILE" "/subsystem-access/operations"
    exit 1
  fi
  local operation_id=""
  operation_id="$(python3 -c "import json,sys;print((json.load(open(sys.argv[1],encoding='utf-8')).get('data') or {}).get('operation_id') or '')" "$RESPONSE_FILE")"
  if [[ -z "$operation_id" ]]; then
    log "INFO" "服务端判定无需创建操作（目标已接入且配置一致）"
    return 0
  fi
  log "INFO" "接入操作已受理：${operation_id}"
  connect_watch_operation "$operation_id"
}
