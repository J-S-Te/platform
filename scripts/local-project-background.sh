#!/usr/bin/env bash
# Sourced by docker-local.sh. Uses the production state machine with a fixed
# local service and Docker config identity, never an invented registry digest.
local_project_background_image() {
  local reference image
  reference="$(compose --profile project config --format json project-api sla-notifier 2>/dev/null | python3 -c '
import json,sys
s=json.load(sys.stdin).get("services",{}); a=s.get("project-api",{}).get("image");b=s.get("sla-notifier",{}).get("image")
if not a or a!=b or any(c.isspace() for c in a):raise SystemExit("项目API和后台镜像配置不配套")
print(a)
')" || return
  image="$(docker image inspect --format '{{.Id}}' "$reference")" || return
  [[ "$image" =~ ^sha256:[a-f0-9]{64}$ ]] || { printf '本地项目镜像没有不可变config身份。\n' >&2; return 1; }
  printf '%s\n' "$image"
}

configure_local_project_background_build() {
  local_project_image="$(local_project_background_image)" || return
  export PROJECT_BACKGROUND_BUILD_ID="project-background-sha256-${local_project_image#sha256:}"
}

# 本地 dev 未导入厂商 JWS 是常态：后台 worker 此时按 PAUSED_LICENSE 刻意暂停执行，
# 生产状态机会拒绝完成受控切换并保留候选（stage=activating，不放宽其语义）。此函数
# 只在「唯一阻塞是授权暂停且 bootstrap/routing 均就绪」时为真；其他任何失败都不可
# 容忍，调用方必须按原样失败。
local_project_background_license_paused() {
  local operation state rc=1
  local state_file="$project_root/runtime/project-background/operation.json"
  [[ -f "$state_file" && ! -L "$state_file" ]] || return 1
  operation="$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1])).get("operation_id",""))' "$state_file" 2>/dev/null)" || return 1
  [[ -n "$operation" ]] || return 1
  state="$(mktemp "${TMPDIR:-/tmp}/uip-project-inspect.XXXXXX")" || return 1
  if compose exec -T sla-notifier /usr/local/bin/project-background-control --action inspect --operation-id "$operation" >"$state" 2>/dev/null; then
    if python3 -c 'import json,sys;d=json.load(open(sys.argv[1]));sys.exit(0 if d.get("paused_reason")=="PAUSED_LICENSE" and d.get("bootstrap_ready") is True and d.get("routing_ready") is True else 1)' "$state"; then
      rc=0
    fi
  fi
  rm -f -- "$state"
  return "$rc"
}

local_project_background() (
  local action="${1:-}"
  case "$action" in prepare|activate|verify|teardown|stopped) ;; *) return 2 ;; esac
  local deploy_dir="$project_root" release_file="$project_root/runtime/local-project-image" runtime_file="$env_file"
  # Define before source so production-only scope functions do not enter this shell.
  project_background_build_id() {
    [[ "$1" =~ ^sha256:[a-f0-9]{64}$ ]] || { printf '本地项目Build必须绑定config摘要。\n' >&2; return 1; }
    printf 'project-background-sha256-%s\n' "${1#sha256:}"
  }
  source "$project_root/deploy/production/bin/project-background.sh"
  _project_background_container() {
    local ids; ids="$(compose ps -q sla-notifier)" || return
    [[ -z "$ids" || "$ids" != *$'\n'* ]] || return 1
    printf '%s\n' "$ids"
  }
  _project_background_owned_container() {
    docker ps -aq --filter "label=com.docker.compose.project=$compose_project" --filter label=com.docker.compose.service=sla-notifier
  }
  _project_background_image() { docker inspect --format '{{.Image}}' "$1"; }
  _project_background_control() {
    compose exec -T sla-notifier /usr/local/bin/project-background-control --action "$1" --operation-id "$2" >"$3"
  }
  _project_background_stop() { compose stop --timeout 300 sla-notifier; }
  _project_background_preflight() {
    compose --profile project config --format json project-api sla-notifier 2>/dev/null | python3 -c '
import json,sys
s=json.load(sys.stdin).get("services",{}); b=sys.argv[1]
for name in ("project-api","sla-notifier"):
 if s.get(name,{}).get("environment",{}).get("PROJECT_BACKGROUND_BUILD_ID")!=b:raise SystemExit("项目镜像和Build配置不一致")
w=s["sla-notifier"];e=w.get("environment",{});h=w.get("healthcheck",{});d=w.get("depends_on",{}).get("temporal",{})
if not(e.get("TEMPORAL_ADDRESS") and e.get("TEMPORAL_NAMESPACE") and d.get("condition")=="service_healthy" and not h.get("disable") and "/healthz" in " ".join(h.get("test",[]))):raise SystemExit("本地项目缺少Temporal/bootstrap健康契约，请合并新版compose.local.yaml项目配置")
' "$PROJECT_BACKGROUND_BUILD_ID"
  }
  env_get() {
    case "$2" in
      PROJECT_IMAGE) printf '%s\n' "$local_project_image" ;;
      PROJECT_BACKGROUND_BUILD_ID) printf '%s\n' "$PROJECT_BACKGROUND_BUILD_ID" ;;
      OIDC_TENANT_ID|PLATFORM_ENVIRONMENT_CODE) env_value "$project_env_file" "$2" ;;
      COMPOSE_PROJECT_NAME) printf '%s\n' "$compose_project" ;;
      *) return 2 ;;
    esac
  }
  if [[ "$action" == teardown ]]; then
    local owned; owned="$(_project_background_owned_container)" || return
    if [[ -z "$owned" && ! -e "$deploy_dir/runtime/project-background/operation.json" ]]; then return 0; fi
  fi
  if [[ "$action" == stopped && ! -e "$deploy_dir/runtime/project-background/operation.json" ]]; then return 0; fi
  configure_local_project_background_build || return
  "project_background_$action"
)
