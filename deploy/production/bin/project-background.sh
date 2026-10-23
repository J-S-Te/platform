#!/usr/bin/env bash
# 固定项目后台生命周期；调用者须持有已有部署锁且先完成备份。
# 只保存非秘密控制状态，不能代替许可证、运行批准或当前自然ACK。

if ! declare -F project_background_build_id >/dev/null; then
  source "$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)/compose-scope.sh"
fi

_project_background_state() {
  python3 - "$deploy_dir" "$@" <<'PY'
import json, os, secrets, stat, sys, tempfile
from pathlib import Path
root = Path(sys.argv[1])
if not root.is_absolute() or root.resolve() != root:
    raise SystemExit('项目后台部署根必须为真实绝对路径')
directory = root / 'runtime' / 'project-background'
for parent in (root / 'runtime', directory):
    if parent.is_symlink(): raise SystemExit('项目后台状态目录拒绝符号链接')
    if parent.exists() and not parent.is_dir(): raise SystemExit('项目后台状态目录无效')
directory.mkdir(mode=0o700, exist_ok=True)
owner = (root / 'runtime').stat()
if os.geteuid() == 0:
    os.chown(directory, owner.st_uid, owner.st_gid)
info = directory.stat()
if info.st_uid != owner.st_uid or stat.S_IMODE(info.st_mode) & 0o077:
    raise SystemExit('项目后台状态目录必须由部署用户拥有且权限0700')
path = directory / 'operation.json'
def load():
    if path.is_symlink(): raise SystemExit('项目后台状态拒绝符号链接')
    if not path.exists(): return None
    info = path.stat()
    if not stat.S_ISREG(info.st_mode) or info.st_uid != owner.st_uid or stat.S_IMODE(info.st_mode) & 0o077 or info.st_size > 262144:
        raise SystemExit('项目后台状态权限或大小无效')
    data = json.loads(path.read_text())
    if data.get('protocol') != 1: raise SystemExit('项目后台状态协议不受支持')
    return data
def save(data):
    fd, temporary = tempfile.mkstemp(prefix='.operation-', dir=directory)
    try:
        with os.fdopen(fd, 'w') as stream:
            json.dump(data, stream, ensure_ascii=False); stream.write('\n'); stream.flush(); os.fsync(stream.fileno())
        if os.geteuid() == 0: os.chown(temporary, owner.st_uid, owner.st_gid)
        os.replace(temporary, path)
    finally:
        if os.path.exists(temporary): os.unlink(temporary)
command = sys.argv[2]
if command == 'begin':
    image = sys.argv[3]; data = load()
    if data and data['stage'] == 'stopped':
        data['release_image'] = image; save(data)
    elif data and data['stage'] != 'complete':
        if data['release_image'] != image: raise SystemExit('存在未完成的项目后台操作，镜像指针不一致；保留现场，禁止猜测恢复')
    else:
        previous_engine = (data.get('current_engine') or data.get('engine','none')) if data else 'none'
        data = dict(protocol=1, operation_id='project-'+secrets.token_hex(16), release_image=image, stage='inspect', old_image='', old_build_id='', engine=previous_engine)
        save(data)
    print(data['operation_id'])
elif command == 'pending':
    # Completed installs still prove a consumer existed: only stopped permits absence.
    data = load(); print('true' if data and data['stage'] != 'stopped' else 'false')
elif command == 'get':
    data = load()
    if not data: raise SystemExit('缺少项目后台操作状态；请先prepare')
    value = data.get(sys.argv[3], '')
    if value is None: print('null')
    elif isinstance(value, (dict,list)): print(json.dumps(value))
    else: print(value)
elif command == 'prior':
    data = load(); payload = Path(sys.argv[5])
    previous = json.loads(payload.read_text()) if payload.stat().st_size else None
    data.update(old_image=sys.argv[3], engine=sys.argv[4], prior=previous)
    data['old_build_id'] = previous.get('build_id','') if previous else sys.argv[6]
    save(data)
elif command == 'observed':
    data = load()
    data.update(current_image=sys.argv[3], current_engine=sys.argv[4], current_build_id=sys.argv[5])
    save(data)
elif command == 'stage':
    data = load(); data['stage'] = sys.argv[3]; save(data)
elif command == 'rollback':
    data = load()
    if not data or not data['old_image']: raise SystemExit('没有已保存的旧项目镜像，不能自动回滚')
    data['release_image'] = data['old_image']; data['stage'] = 'rollback-drained'; save(data)
else: raise SystemExit('未知项目后台状态动作')
PY
}

_project_background_container() {
  local ids
  ids="$(compose ps -q project-sla-notifier)" || return 1
  [[ -z "$ids" || "$ids" != *$'\n'* ]] || {
    printf '项目后台存在多个容器，拒绝选择未知消费者。\n' >&2; return 1;
  }
  printf '%s\n' "$ids"
}

_project_background_stop() { compose stop --timeout 300 project-sla-notifier; }

_project_background_image() { docker inspect --format '{{.Config.Image}}' "$1"; }

# Include stopped/orphaned owned consumers even if a custom Compose omitted the service.
_project_background_owned_container() {
  local project
  project="$(env_get "${runtime_file:-$deploy_dir/.env}" COMPOSE_PROJECT_NAME)" || return
  project="${project:-basic-platform-production}"
  [[ "$project" =~ ^[a-z0-9][a-z0-9_-]*$ ]] || return 1
  docker ps -aq --filter "label=com.docker.compose.project=$project" --filter 'label=com.docker.compose.service=project-sla-notifier'
}

_project_background_control() {
  local action="$1" operation="$2" output="$3"
  compose exec -T project-sla-notifier /usr/local/bin/project-background-control \
    --action "$action" --operation-id "$operation" >"$output"
}

_project_background_validate() {
  local file="$1" build="$2" drained="${3:-false}"
  local readiness="${4:-false}"
  local tenant environment
  tenant="$(env_get "$deploy_dir/runtime/project.env" OIDC_TENANT_ID)" || return
  environment="$(env_get "$deploy_dir/runtime/project.env" PLATFORM_ENVIRONMENT_CODE)" || return
  python3 - "$file" "$build" "$drained" "$tenant" "$environment" "$readiness" <<'PY'
import json,sys
from pathlib import Path
p=Path(sys.argv[1])
if p.stat().st_size > 262144: raise SystemExit('后台控制响应过大')
d=json.loads(p.read_text())
if d.get('protocol') != 1 or d.get('engine') not in ('temporal','legacy') or d.get('build_id') != sys.argv[2] or not sys.argv[4] or not sys.argv[5] or d.get('tenant_id') != sys.argv[4] or d.get('environment') != sys.argv[5]:
    raise SystemExit('后台控制身份与实际镜像不一致')
if sys.argv[3] == 'true' and (d.get('drained') is not True or d.get('active_activities') != 0 or d.get('active_leases') != 0):
    raise SystemExit('后台仍有在途任务或缺少排空证据')
if sys.argv[6] == 'bootstrap' and d.get('bootstrap_ready') is not True:
    raise SystemExit('项目后台尚未完成bootstrap，禁止promote')
if sys.argv[6] == 'true' and (d.get('bootstrap_ready') is not True or d.get('routing_ready') is not True or (d.get('execution_ready') is not True and d.get('paused_reason') != 'PAUSED_OPERATOR')):
    raise SystemExit('后台尚未满足routing/execution或原人工暂停状态')
PY
}

# Config is projected into a private temporary file; never print resolved environment.
_project_background_preflight() (
  umask 077
  local projection; projection="$(mktemp "$deploy_dir/runtime/.project-compose.XXXXXX")" || return
  trap 'rm -f -- "$projection"' EXIT
  compose config --no-interpolate --format json >"$projection" 2>/dev/null || {
    printf '项目Compose兼容预检失败；请将docker-compose.yml.dist中的项目Build、Temporal和bootstrap健康字段合并后重试。\n' >&2; return 1;
  }
  python3 - "$projection" <<'CHECK'
import json,re,sys
from pathlib import Path
p=Path(sys.argv[1])
if p.stat().st_size>4194304: raise SystemExit('Compose预检投影过大')
d=json.loads(p.read_text()); s=d.get('services',{}).get('project-sla-notifier',{})
e=s.get('environment',{}); dep=s.get('depends_on',{}).get('temporal',{})
h=s.get('healthcheck',{}); test=' '.join(h.get('test',[]))
if not (isinstance(e,dict) and re.fullmatch(r'\$\{PROJECT_IMAGE(?::-.*?)?\}',s.get('image','')) and re.fullmatch(r'\$\{PROJECT_BACKGROUND_BUILD_ID(?::-.*?)?\}',str(e.get('PROJECT_BACKGROUND_BUILD_ID',''))) and e.get('TEMPORAL_ADDRESS') and e.get('TEMPORAL_NAMESPACE') and dep.get('condition')=='service_healthy' and not h.get('disable') and '/healthz' in test and '/readyz' not in test):
    raise SystemExit('项目Compose缺少Build/Temporal/bootstrap健康契约；请合并docker-compose.yml.dist的项目字段后重试')
CHECK
)

_project_background_legacy_replace_guard() {
  python3 - "$1" <<'CHECK'
import json,sys
from pathlib import Path
s=json.loads(Path(sys.argv[1]).read_text())
if s is None: raise SystemExit(0)
if s.get('engine')=='legacy' and (s.get('paused_reason') in ('PAUSED_OPERATOR','PAUSED_OPERATION','PAUSED_DEPLOYMENT') or any(x.get('operation_id') for x in s.get('schedules',[]))):
    raise SystemExit('legacy人工暂停或未完成操作仅在进程内，禁止替换；请在原进程受控完成/恢复该操作，确认执行策略后重新prepare或受控迁移Temporal')
CHECK
}

project_background_prepare() (
  umask 077
  local image operation id old_image marker identity state stage engine
  [[ "${1:-}" == teardown ]] || _project_background_preflight || return
  image="$(env_get "$release_file" PROJECT_IMAGE)" || return
  project_background_build_id "$image" >/dev/null || return
  operation="$(_project_background_state begin "$image")" || return
  stage="$(_project_background_state get stage)" || return
  id="$(_project_background_container)" || return
  state="$(mktemp "$deploy_dir/runtime/project-background/.inspect.XXXXXX")" || return
  trap 'rm -f -- "$state"' EXIT
  if [[ -z "$id" ]]; then
    if [[ "$stage" == inspect && "$(_project_background_state get engine)" == none && -z "$(_project_background_state get current_image)" ]]; then
      _project_background_state prior '' none "$state" '' || return
    elif [[ "$stage" != stopped && ( "$(_project_background_state get engine)" != none || -n "$(_project_background_state get current_image)" ) ]]; then
      printf '原项目后台已缺失，无法重新证明排空；保留恢复状态。\n' >&2; return 1
    fi
    if [[ "${1:-}" != teardown && "$(_project_background_state get engine)" == legacy ]]; then
      _project_background_state get prior >"$state" || return
      [[ ! -s "$state" || "$(cat "$state")" == null ]] || _project_background_legacy_replace_guard "$state" || return
    fi
    [[ "$stage" == stopped ]] || _project_background_state stage prepared
    return
  fi
  old_image="$(_project_background_image "$id")" || return
  project_background_build_id "$old_image" >/dev/null || return
  marker="$(docker inspect --format '{{index .Config.Labels "io.uip.project-background.protocol"}}' "$id")" || return
  if [[ "$marker" == 1 ]]; then
    _project_background_control inspect "$operation" "$state" || return
    _project_background_validate "$state" "$(project_background_build_id "$old_image")" || return
    engine="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["engine"])' "$state")" || return
    _project_background_state observed "$old_image" "$engine" "$(project_background_build_id "$old_image")" || return
    if [[ "$stage" == inspect ]]; then
      _project_background_state prior "$old_image" "$engine" "$state" || return
    fi
    if [[ "${1:-}" != teardown ]]; then
      _project_background_state get prior >"$state" || return
      _project_background_legacy_replace_guard "$state" || return
    fi
    _project_background_state stage drain-pending || return
    _project_background_control pause "$operation" "$state" || return
    _project_background_control drain "$operation" "$state" || return
    _project_background_validate "$state" "$(project_background_build_id "$old_image")" true || return
  elif [[ -z "$marker" || "$marker" == '<no value>' ]]; then
    identity="$(docker inspect --format '{{.Path}}' "$id")" || return
    case "$identity" in
      /usr/local/bin/sla-notifier|sla-notifier|./sla-notifier) ;;
      *) printf '旧项目后台身份未知，不能将缺失控制工具当成legacy。\n' >&2; return 1 ;;
    esac
    _project_background_state observed "$old_image" legacy "$(project_background_build_id "$old_image")" || return
    if [[ "$stage" == inspect ]]; then
      _project_background_state prior "$old_image" legacy "$state" "$(project_background_build_id "$old_image")" || return
    fi
    _project_background_state stage drain-pending || return
    _project_background_stop || return
    _project_background_state stage stopped
    return
  else
    printf '项目后台控制协议未知，保留当前进程与候选。\n' >&2; return 1
  fi
  _project_background_state stage prepared
)

project_background_activate() (
  umask 077
  local image expected operation id actual marker state engine activation_action
  image="$(env_get "$release_file" PROJECT_IMAGE)" || return
  [[ "$image" == "$(_project_background_state get release_image)" ]] || {
    printf '项目镜像指针与待恢复后台操作不一致。\n' >&2; return 1;
  }
  expected="$(project_background_build_id "$image")" || return
  operation="$(_project_background_state get operation_id)" || return
  id="$(_project_background_container)" || return
  [[ -n "$id" ]] || { printf '项目后台容器缺失。\n' >&2; return 1; }
  actual="$(_project_background_image "$id")" || return
  [[ "$actual" == "$image" ]] || { printf '项目后台实际镜像不匹配。\n' >&2; return 1; }
  marker="$(docker inspect --format '{{index .Config.Labels "io.uip.project-background.protocol"}}' "$id")" || return
  if [[ "$marker" != 1 ]]; then
    [[ "$(_project_background_state get stage)" == rollback-drained && "$(_project_background_state get engine)" == legacy ]] || {
      printf '新项目镜像没有受控后台协议，禁止报告完成。\n' >&2; return 1;
    }
    _project_background_state stage complete
    return
  fi
  [[ "$expected" == "$(env_get "$release_file" PROJECT_BACKGROUND_BUILD_ID)" ]] || {
    printf '项目镜像与后台 Build 指针不配套。\n' >&2; return 1;
  }
  state="$(mktemp "$deploy_dir/runtime/project-background/.activate.XXXXXX")" || return
  trap 'rm -f -- "$state"' EXIT
  _project_background_control inspect "$operation" "$state" || return
  _project_background_validate "$state" "$expected" || return
  engine="$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["engine"])' "$state")" || return
  _project_background_state observed "$image" "$engine" "$expected" || return
  _project_background_validate "$state" "$expected" false bootstrap || return
  activation_action=ensure
  if [[ "$(_project_background_state get stage)" == rollback-drained || "$(_project_background_state get stage)" == restoring ]]; then
    activation_action=restore
    _project_background_state stage restoring || return
  else
    _project_background_state stage activating || return
  fi
  _project_background_control "$activation_action" "$operation" "$state" || return
  _project_background_control resume "$operation" "$state" || return
  _project_background_control verify "$operation" "$state" || {
    printf '项目后台尚未满足routing/execution验收；保留候选和恢复记录，请检查许可或依赖。\n' >&2; return 1;
  }
  _project_background_validate "$state" "$expected" false true || return
  _project_background_state stage complete
)

project_background_verify() (
  umask 077
  local id image build state
  image="$(env_get "$release_file" PROJECT_IMAGE)" || return
  build="$(project_background_build_id "$image")" || return
  id="$(_project_background_container)" || return
  [[ -n "$id" && "$(_project_background_image "$id")" == "$image" ]] || return 1
  _project_background_state get operation_id >/dev/null || return
  state="$(mktemp "$deploy_dir/runtime/project-background/.verify.XXXXXX")" || return
  trap 'rm -f -- "$state"' EXIT
  _project_background_control verify "$(_project_background_state get operation_id)" "$state" || return
  _project_background_validate "$state" "$build" false true
)

project_background_teardown() (
  # ps is the selected Compose project's owned service inventory. Errors are fatal.
  local id pending
  id="$(_project_background_owned_container)" || return
  pending="$(_project_background_state pending)" || return
  [[ -n "$id" || "$pending" == true ]] || return 0
  _project_background_preflight || return
  project_background_prepare teardown || return
  [[ "$(_project_background_state get stage)" == stopped ]] || _project_background_state stage teardown-drained
)

# Call only AFTER successful actual stop/removal, never after drain alone.
project_background_stopped() (
  local pending stage
  pending="$(_project_background_state pending)" || return
  [[ "$pending" == true ]] || return 0
  stage="$(_project_background_state get stage)" || return
  [[ "$stage" == prepared || "$stage" == teardown-drained ]] || return 1
  _project_background_state stage stopped
)

# 必须在恢复旧镜像指针之前调用，新Build仍在运行时完成排空。
_project_background_restore_config() (
  umask 077
  local prior
  prior="$(mktemp "$deploy_dir/runtime/project-background/.prior.XXXXXX")" || return
  trap 'rm -f -- "$prior"' EXIT
  _project_background_state get prior >"$prior" || return
  python3 - "$deploy_dir/runtime/project.env" "$prior" "$(_project_background_state get old_build_id)" <<'PY'
import json, os, re, stat, sys, tempfile
from pathlib import Path
path, evidence = Path(sys.argv[1]), Path(sys.argv[2])
prior = json.loads(evidence.read_text())
if not isinstance(prior, dict) or prior.get('build_id') != sys.argv[3] or prior.get('engine') != 'temporal':
    raise SystemExit('旧后台配置身份不完整，保留候选并人工核对')
config = prior.get('runtime_config')
keys = {'sla_interval':'SLA_SCAN_INTERVAL', 'dispatch_interval':'NOTIFICATION_DISPATCH_INTERVAL', 'max_items':'PROJECT_BACKGROUND_MAX_ITEMS', 'run_budget':'PROJECT_BACKGROUND_RUN_BUDGET', 'activity_timeout':'PROJECT_BACKGROUND_ACTIVITY_TIMEOUT', 'sla_activity_timeout':'PROJECT_BACKGROUND_SLA_ACTIVITY_TIMEOUT', 'workflow_run_timeout':'PROJECT_BACKGROUND_WORKFLOW_RUN_TIMEOUT'}
bounds = {'sla_interval':2**63-1, 'dispatch_interval':2**63-1, 'max_items':50, 'run_budget':240000000000, 'activity_timeout':55000000000, 'sla_activity_timeout':120000000000, 'workflow_run_timeout':360000000000}
if not isinstance(config, dict) or set(config) != set(keys) or any(type(config[k]) is not int or not 0 < config[k] <= bounds[k] for k in keys):
    raise SystemExit('缺少可信旧后台参数或参数越界，禁止猜测回滚')
if config['workflow_run_timeout'] <= max(config[k] for k in ('run_budget','activity_timeout','sla_activity_timeout')):
    raise SystemExit('旧后台参数预算不一致')
if path.is_symlink() or path.parent.is_symlink(): raise SystemExit('运行配置拒绝符号链接')
info = path.stat(); owner = path.parent.stat()
if not stat.S_ISREG(info.st_mode) or info.st_uid != owner.st_uid or stat.S_IMODE(info.st_mode) & 0o077 or info.st_size > 1048576:
    raise SystemExit('运行配置属主、权限或大小不安全')
text = path.read_text(); lines = text.splitlines(keepends=True)
values = {}; counts = {}
for line in lines:
    match = re.match(r'^([A-Z][A-Z0-9_]*)=(.*?)(?:\r?\n)?$', line)
    if match:
        key, value = match.groups(); values[key] = value; counts[key] = counts.get(key, 0) + 1
if values.get('OIDC_TENANT_ID') != prior.get('tenant_id') or values.get('PLATFORM_ENVIRONMENT_CODE') != prior.get('environment') or any(counts.get(k,0)>1 for k in (*keys.values(), 'OIDC_TENANT_ID','PLATFORM_ENVIRONMENT_CODE')):
    raise SystemExit('运行配置身份或重复字段不一致')
replacement = {env: str(config[key]) + ('' if key == 'max_items' else 'ns') for key, env in keys.items()}
output = []
for line in lines:
    key = line.split('=',1)[0]
    if key in replacement:
        output.append(key+'='+replacement.pop(key)+'\n')
    else: output.append(line)
if output and not output[-1].endswith('\n'): output[-1] += '\n'
output.extend(key+'='+value+'\n' for key,value in replacement.items())
fd, temporary = tempfile.mkstemp(prefix='.project-rollback-', dir=path.parent)
try:
    with os.fdopen(fd,'w') as stream:
        stream.write(''.join(output)); stream.flush(); os.fsync(stream.fileno())
    os.chmod(temporary, stat.S_IMODE(info.st_mode))
    if os.geteuid() == 0: os.chown(temporary,info.st_uid,info.st_gid)
    current = path.lstat()
    if not stat.S_ISREG(current.st_mode) or (current.st_dev,current.st_ino,current.st_mtime_ns,current.st_size) != (info.st_dev,info.st_ino,info.st_mtime_ns,info.st_size):
        raise SystemExit('运行配置已变化，拒绝覆盖')
    os.replace(temporary,path)
finally:
    if os.path.exists(temporary): os.unlink(temporary)
PY
)

project_background_rollback() (
  local image operation state marker id
  # A first-install candidate is not a rollback target; refuse before changing it.
  [[ -n "$(_project_background_state get old_image)" ]] || {
    printf '没有操作前的旧项目运行态，禁止恢复发布指针；保留当前候选并修复依赖后继续激活。\n' >&2; return 1;
  }
  operation="$(_project_background_state get operation_id)" || return
  id="$(_project_background_container)" || return
  [[ -n "$id" ]] || return 1
  marker="$(docker inspect --format '{{index .Config.Labels "io.uip.project-background.protocol"}}' "$id")" || return
  if [[ "$marker" == 1 ]]; then
    image="$(_project_background_image "$id")" || return
    state="$(mktemp "$deploy_dir/runtime/project-background/.rollback.XXXXXX")" || return
    trap 'rm -f -- "$state"' EXIT
    _project_background_control pause "$operation" "$state" || return
    _project_background_control drain "$operation" "$state" || return
    _project_background_validate "$state" "$(project_background_build_id "$image")" true || return
  else
    [[ "$(_project_background_state get engine)" == legacy ]] || return 1
  fi
  if [[ "$(_project_background_state get engine)" == temporal ]]; then
    _project_background_restore_config || return
  fi
  _project_background_state rollback
)

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  set -Eeuo pipefail
  umask 077
  action="${1:-}"
  [[ $# -eq 1 ]] || { printf '用法：project-background.sh prepare|activate|verify|teardown|rollback\n' >&2; exit 2; }
  case "$action" in prepare|activate|verify|teardown|rollback) ;; *) exit 2 ;; esac
  helper_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
  deploy_dir="$(cd -- "$helper_dir/.." && pwd)"
  runtime_file="$deploy_dir/.env"; release_file="$deploy_dir/.release.env"
  # 不source运行配置；Compose和固定键读取器不会执行配置文本。
  env_get() { awk -F= -v key="$2" '$1==key {sub(/^[^=]*=/, ""); print; exit}' "$1"; }
  compose() { docker compose --project-directory "$deploy_dir" --file "$deploy_dir/docker-compose.yml" --env-file "$runtime_file" --env-file "$release_file" "$@"; }
  "project_background_$action"
fi
